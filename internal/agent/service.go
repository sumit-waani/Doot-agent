// Package agent is the agent loop: one primary agent, two subagents invoked as
// tools, and the run lifecycle around them.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/sumit-waani/doot/internal/config"
	"github.com/sumit-waani/doot/internal/daytona"
	"github.com/sumit-waani/doot/internal/db"
	"github.com/sumit-waani/doot/internal/events"
	"github.com/sumit-waani/doot/internal/github"
	"github.com/sumit-waani/doot/internal/llm"
	"github.com/sumit-waani/doot/internal/project"
)

// Event types emitted by the loop. These mirror the SSE contract in the UI doc.
const (
	eventTypeStatus       = events.TypeRunStatus
	eventTypeMessage      = events.TypeMessageAppend
	eventTypeDelta        = events.TypeMessageDelta
	eventTypeToolStart    = events.TypeToolStart
	eventTypeToolEnd      = events.TypeToolEnd
	eventTypePlanProposed = events.TypePlanProposed
	eventTypePlanUpdated  = events.TypePlanUpdated
	eventTypeTaskUpdated  = events.TypeTaskUpdated
	eventTypeScreenshot   = events.TypeScreenshot
	eventTypeUsage        = events.TypeUsage
	eventTypeEpoch        = events.TypeEpochChanged
	eventTypePreview      = "preview"
)

const (
	epochReasonClear   = "clear"
	epochReasonCompact = "compact"
)

// Service is the public surface the web layer calls.
type Service struct {
	db       *db.DB
	cfg      *config.Store
	events   *events.Log
	projects *project.Service

	store     *store
	registry  *registry
	artifacts *artifactStore

	// llmMu guards the cached client, rebuilt when credentials or model change so
	// editing them in Settings takes effect without a restart.
	llmMu    sync.Mutex
	llm      *llm.Client
	llmPrint string

	// cancels holds the stop function for the in-flight run, so shutdown can
	// unwind it promptly. Pause itself goes through the database.
	cancelMu sync.Mutex
	cancels  map[int64]context.CancelFunc
}

// NewService builds a Service.
func NewService(d *db.DB, cfg *config.Store, ev *events.Log, projects *project.Service) *Service {
	return &Service{
		db:        d,
		cfg:       cfg,
		events:    ev,
		projects:  projects,
		store:     &store{db: d},
		registry:  newRegistry(),
		artifacts: newArtifactStore(40),
		cancels:   map[int64]context.CancelFunc{},
	}
}

// ---------------------------------------------------------------- llm client

// LLM returns the model client, rebuilding it when configuration changes.
func (s *Service) LLM(ctx context.Context) (*llm.Client, error) {
	apiKey, err := s.cfg.Secret(ctx, config.SecretLLMAPIKey)
	if err != nil {
		if errors.Is(err, config.ErrNoSecret) {
			return nil, llm.ErrNoAPIKey
		}
		return nil, err
	}

	cfg := llm.Config{
		APIKey:             apiKey,
		BaseURL:            s.cfg.Get(config.KeyLLMBaseURL),
		Model:              s.cfg.Get(config.KeyLLMModel),
		ContextWindow:      s.cfg.Int(config.KeyLLMContextWindow, 200_000),
		MaxOutputTokens:    s.cfg.Int(config.KeyLLMMaxOutputTokens, 8192),
		InputPerMtok:       s.cfg.Float(config.KeyPricingInputPerMtok, 0),
		CachedInputPerMtok: s.cfg.Float(config.KeyPricingCachedInputPerMtok, 0),
		OutputPerMtok:      s.cfg.Float(config.KeyPricingOutputPerMtok, 0),
	}

	print := fmt.Sprintf("%s|%s|%d|%d|%.6f|%.6f|%.6f|%s",
		cfg.BaseURL, cfg.Model, cfg.ContextWindow, cfg.MaxOutputTokens,
		cfg.InputPerMtok, cfg.CachedInputPerMtok, cfg.OutputPerMtok, fingerprint(apiKey))

	s.llmMu.Lock()
	defer s.llmMu.Unlock()

	if s.llm != nil && s.llmPrint == print {
		return s.llm, nil
	}

	client, err := llm.New(cfg, s.recordLLMCall)
	if err != nil {
		return nil, err
	}
	s.llm, s.llmPrint = client, print
	return client, nil
}

func (s *Service) llmContextWindow() int {
	return s.cfg.Int(config.KeyLLMContextWindow, 200_000)
}

// recordLLMCall is the audit hook handed to the llm package, so every call is
// costed in exactly one place.
func (s *Service) recordLLMCall(ctx context.Context, rec llm.AuditRecord) {
	// Detached: the audit row must land even when the run's context has just been
	// cancelled, or a failed call would go unrecorded and appear free.
	auditCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	epoch, err := s.store.currentEpoch(auditCtx)
	if err != nil {
		epoch = 0
	}

	runID := runIDFromContext(ctx)
	if err := s.store.recordLLMCall(auditCtx, runID, epoch, rec); err != nil {
		slog.Error("could not record llm call", "err", err)
	}
}

// ---------------------------------------------------------------- public api

// Status describes the conversation state for the chat screen.
type Status struct {
	RunID      int64
	RunStatus  string
	Kind       string
	Resumable  bool
	Plan       *Plan
	Epoch      int
	SessionUSD float64
}

// Status returns the current state.
func (s *Service) Status(ctx context.Context) (Status, error) {
	epoch, err := s.store.currentEpoch(ctx)
	if err != nil {
		return Status{RunStatus: "idle"}, nil
	}

	out := Status{RunStatus: "idle", Epoch: epoch}

	if run, err := s.store.activeRun(ctx); err == nil {
		out.RunID = run.ID
		out.RunStatus = run.Status
		out.Kind = run.Kind
		out.Resumable = run.Resumable()
	}

	if plan, err := s.store.activePlan(ctx, epoch); err == nil {
		out.Plan = &plan
	}

	if cost, err := s.store.sessionCost(ctx, epoch); err == nil {
		out.SessionUSD = cost
	}

	return out, nil
}

// SendMessage records the operator's message and starts or continues a run.
//
// A message arriving while the run waits on a human is the answer to ask_human:
// the same run resumes rather than a new one starting, so the model keeps its
// context.
func (s *Service) SendMessage(ctx context.Context, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("agent: message is empty")
	}

	p, err := s.projects.Load(ctx)
	if err != nil {
		return err
	}
	if !p.Exists {
		return errNoProject
	}

	// Fail before recording anything if the model is unreachable, so the operator
	// is not left with a message that will never be answered.
	if _, err := s.LLM(ctx); err != nil {
		return err
	}

	epoch, err := s.store.currentEpoch(ctx)
	if err != nil {
		return err
	}

	msgID, err := s.store.appendMessage(ctx, epoch, nil, llm.Message{
		Role:    "user",
		Content: text,
	}, false)
	if err != nil {
		return err
	}

	s.emit(ctx, nil, eventTypeMessage, map[string]any{"role": "user", "content": text})

	if existing, err := s.store.activeRun(ctx); err == nil {
		switch existing.Status {
		case StatusAwaitingHuman, StatusAwaitingApproval, StatusPaused, StatusInterrupted:
			return s.resume(ctx, existing.ID)
		case StatusRunning:
			// Mid-run input is not dropped: it is already in the transcript and the
			// model sees it on its next turn.
			return nil
		}
	}

	return s.start(ctx, epoch, KindChat, &msgID)
}

// CreatePlan asks the agent to produce a goal plan.
func (s *Service) CreatePlan(ctx context.Context) error {
	p, err := s.projects.Load(ctx)
	if err != nil {
		return err
	}
	if !p.Exists {
		return errNoProject
	}
	if _, err := s.LLM(ctx); err != nil {
		return err
	}

	epoch, err := s.store.currentEpoch(ctx)
	if err != nil {
		return err
	}

	const ask = "Create a goal plan for the work we have discussed. " +
		"Call create_goal_plan with concrete, ordered tasks. Do not start any work yet."

	msgID, err := s.store.appendMessage(ctx, epoch, nil, llm.Message{Role: "user", Content: ask}, false)
	if err != nil {
		return err
	}
	s.emit(ctx, nil, eventTypeMessage, map[string]any{"role": "user", "content": ask})

	if existing, err := s.store.activeRun(ctx); err == nil {
		if existing.Resumable() {
			return s.resume(ctx, existing.ID)
		}
		return ErrRunActive
	}

	return s.start(ctx, epoch, KindPlan, &msgID)
}

// ApprovePlan approves the pending plan and begins execution.
func (s *Service) ApprovePlan(ctx context.Context, planID int64) error {
	plan, err := s.store.loadPlan(ctx, planID)
	if err != nil {
		return err
	}
	if plan.Status != PlanAwaitingApproval {
		return fmt.Errorf("agent: plan %d is not awaiting approval (it is %s)", planID, plan.Status)
	}

	if err := s.store.setPlanStatus(ctx, planID, PlanApproved); err != nil {
		return err
	}

	const note = "Plan approved. Begin work, starting with task 1."
	if _, err := s.store.appendMessage(ctx, plan.Epoch, nil,
		llm.Message{Role: "user", Content: note}, false); err != nil {
		return err
	}

	refreshed, _ := s.store.loadPlan(ctx, planID)
	s.emit(ctx, nil, eventTypePlanUpdated, planPayload(refreshed))
	s.emit(ctx, nil, eventTypeMessage, map[string]any{"role": "user", "content": note})

	if existing, err := s.store.activeRun(ctx); err == nil {
		if err := s.store.setPlanRun(ctx, planID, existing.ID); err != nil {
			slog.Debug("could not link plan to run", "err", err)
		}
		return s.resume(ctx, existing.ID)
	}

	return s.start(ctx, plan.Epoch, KindExecute, nil)
}

// RejectPlan discards the pending plan.
func (s *Service) RejectPlan(ctx context.Context, planID int64, reason string) error {
	plan, err := s.store.loadPlan(ctx, planID)
	if err != nil {
		return err
	}
	if err := s.store.setPlanStatus(ctx, planID, PlanRejected); err != nil {
		return err
	}

	note := "Plan rejected."
	if strings.TrimSpace(reason) != "" {
		note += " " + reason
	}
	if _, err := s.store.appendMessage(ctx, plan.Epoch, nil,
		llm.Message{Role: "user", Content: note}, false); err != nil {
		return err
	}

	refreshed, _ := s.store.loadPlan(ctx, planID)
	s.emit(ctx, nil, eventTypePlanUpdated, planPayload(refreshed))
	s.emit(ctx, nil, eventTypeMessage, map[string]any{"role": "user", "content": note})

	// The run stays parked: rejecting a plan is a conversation turn, not a
	// failure, and the operator will usually follow it with what they wanted
	// instead.
	return nil
}

// Pause asks the active run to stop at the next safe point.
func (s *Service) Pause(ctx context.Context) error {
	run, err := s.store.activeRun(ctx)
	if err != nil {
		return err
	}
	if run.Status != StatusRunning {
		return fmt.Errorf("agent: run is %s, not running", run.Status)
	}

	// Written to the database rather than signalled in memory, so the pause
	// survives a restart and the loop observes it at its next checkpoint.
	if err := s.store.parkRun(ctx, run.ID, StatusPaused); err != nil {
		return err
	}

	s.emitRunStatus(ctx, run.ID, StatusPaused, "Pausing at the next safe point…")
	return nil
}

// Resume continues a paused, interrupted or parked run.
func (s *Service) Resume(ctx context.Context) error {
	run, err := s.store.activeRun(ctx)
	if err != nil {
		return err
	}
	if !run.Resumable() {
		return fmt.Errorf("agent: run is %s and cannot be resumed", run.Status)
	}
	return s.resume(ctx, run.ID)
}

// Cancel abandons the active run.
func (s *Service) Cancel(ctx context.Context) error {
	run, err := s.store.activeRun(ctx)
	if err != nil {
		return err
	}

	s.stopRun(run.ID)

	if err := s.store.finishRun(ctx, run.ID, StatusCancelled, nil); err != nil {
		return err
	}
	s.emitRunStatus(ctx, run.ID, StatusCancelled, "Cancelled")
	return nil
}

// ClearConversation rolls onto a fresh epoch.
//
// History is retained: the epoch is closed, not deleted. The sandbox is
// deliberately untouched — repo, branch and filesystem all survive.
func (s *Service) ClearConversation(ctx context.Context) error {
	if run, err := s.store.activeRun(ctx); err == nil {
		s.stopRun(run.ID)
		if err := s.store.finishRun(ctx, run.ID, StatusCancelled, nil); err != nil {
			return err
		}
	}

	epoch, err := s.store.currentEpoch(ctx)
	if err != nil {
		return err
	}

	newEpoch, err := s.store.rollEpoch(ctx, epochReasonClear, "")
	if err != nil {
		return err
	}

	s.emit(ctx, nil, eventTypeEpoch, map[string]any{
		"epoch":    newEpoch,
		"previous": epoch,
		"reason":   epochReasonClear,
	})
	slog.Info("conversation cleared", "from_epoch", epoch, "to_epoch", newEpoch)
	return nil
}

// Artifact returns a stored screenshot for the UI.
func (s *Service) Artifact(id string) ([]byte, string, bool) {
	a, ok := s.artifacts.get(id)
	if !ok {
		return nil, "", false
	}
	return a.Data, a.ContentType, true
}

// Close stops any in-flight run so shutdown is prompt.
func (s *Service) Close() {
	s.cancelMu.Lock()
	for _, cancel := range s.cancels {
		cancel()
	}
	s.cancels = map[int64]context.CancelFunc{}
	s.cancelMu.Unlock()
}

// ---------------------------------------------------------------- run control

// start creates a run and launches the loop.
func (s *Service) start(ctx context.Context, epoch int, kind string, triggerMessageID *int64) error {
	runID, err := s.store.startRun(ctx, epoch, kind, triggerMessageID)
	if err != nil {
		return err
	}

	s.launch(runID)
	s.emitRunStatus(ctx, runID, StatusRunning, "")
	return nil
}

// resume puts a parked run back to work.
func (s *Service) resume(ctx context.Context, runID int64) error {
	if err := s.store.resumeRun(ctx, runID); err != nil {
		return err
	}
	s.launch(runID)
	s.emitRunStatus(ctx, runID, StatusRunning, "Resumed")
	return nil
}

// launch runs the loop in the background.
//
// Detached from the request context on purpose: a run outlives the HTTP call
// that started it, and tying it to the request would kill it as soon as the
// browser navigated away.
func (s *Service) launch(runID int64) {
	runCtx, cancel := context.WithCancel(withRunID(context.Background(), runID))

	s.cancelMu.Lock()
	if existing, ok := s.cancels[runID]; ok {
		existing()
	}
	s.cancels[runID] = cancel
	s.cancelMu.Unlock()

	// Keep the sandbox awake for the run's duration. Daytona's inactivity timer
	// ignores work happening inside the sandbox, so without this a long build
	// gets its sandbox stopped mid-flight.
	if err := s.projects.StartHeartbeat(runCtx); err != nil {
		slog.Warn("could not start sandbox heartbeat", "err", err)
	}

	go s.run(runCtx, runID)
}

func (s *Service) stopRun(runID int64) {
	s.cancelMu.Lock()
	if cancel, ok := s.cancels[runID]; ok {
		cancel()
		delete(s.cancels, runID)
	}
	s.cancelMu.Unlock()
}

func (s *Service) clearCancel(runID int64) {
	s.cancelMu.Lock()
	delete(s.cancels, runID)
	s.cancelMu.Unlock()
}

// ---------------------------------------------------------------- push and pr

// pushAndOpenPR pushes the branch and attempts a pull request.
//
// PR creation is best-effort by design: an existing PR is the normal case on the
// second and later pushes, and any other failure is recorded and stepped past
// rather than failing the run.
func (s *Service) pushAndOpenPR(ctx context.Context, tc toolContext, title, body string) (toolResult, error) {
	setup := tc.Setup
	if setup.RepoURL == "" {
		return toolResult{}, errors.New("git configuration is unavailable")
	}

	push, err := tc.Sandbox.Push(ctx, setup)
	if err != nil {
		return toolResult{}, err
	}

	branchURL := fmt.Sprintf("https://github.com/%s/%s/tree/%s",
		tc.Project.RepoOwner, tc.Project.RepoName, push.Branch)

	var out strings.Builder
	fmt.Fprintf(&out, "Pushed %s to origin/%s.\n%s\n", shortSHA(push.HeadSHA), push.Branch, branchURL)

	if !s.cfg.Bool(config.KeyGitHubCreatePR, true) {
		_ = s.store.recordPush(ctx, &tc.RunID, push.Branch, push.HeadSHA, 0, "", "skipped", "")
		out.WriteString("\nPull request creation is disabled in settings.")
		return toolResult{Content: out.String(), Preview: "pushed " + shortSHA(push.HeadSHA)}, nil
	}

	token, err := s.cfg.Secret(ctx, config.SecretGitHubPAT)
	if err != nil {
		_ = s.store.recordPush(ctx, &tc.RunID, push.Branch, push.HeadSHA, 0, "", "failed",
			"no GitHub token configured")
		out.WriteString("\nNo GitHub token configured, so no pull request was opened. " +
			"The branch is pushed and can be merged by hand.")
		return toolResult{Content: out.String(), Preview: "pushed, no PR"}, nil
	}

	if title == "" {
		title = orDefault(planTitle(ctx, s, tc.Epoch), "Doot changes")
	}

	gh := github.New(token)
	pr, err := gh.CreatePullRequest(ctx, github.PullRequestInput{
		Owner: tc.Project.RepoOwner,
		Repo:  tc.Project.RepoName,
		Head:  push.Branch,
		Base:  tc.Project.BaseBranch,
		Title: title,
		Body:  body,
	})

	switch {
	case err == nil && pr.AlreadyExisted:
		_ = s.store.recordPush(ctx, &tc.RunID, push.Branch, push.HeadSHA, pr.Number, pr.URL, "exists", "")
		fmt.Fprintf(&out, "\nExisting pull request updated: %s", pr.URL)

	case err == nil:
		_ = s.store.recordPush(ctx, &tc.RunID, push.Branch, push.HeadSHA, pr.Number, pr.URL, "created", "")
		fmt.Fprintf(&out, "\nOpened pull request #%d: %s", pr.Number, pr.URL)

	default:
		// Recorded and reported, never fatal.
		slog.Warn("could not open pull request", "err", err)
		_ = s.store.recordPush(ctx, &tc.RunID, push.Branch, push.HeadSHA, 0, "", "failed", err.Error())
		fmt.Fprintf(&out, "\nCould not open a pull request (%v). The branch is pushed; "+
			"it can be merged by hand.", err)
	}

	return toolResult{Content: out.String(), Preview: "pushed " + shortSHA(push.HeadSHA)}, nil
}

func planTitle(ctx context.Context, s *Service, epoch int) string {
	if plan, err := s.store.activePlan(ctx, epoch); err == nil {
		return plan.Title
	}
	return ""
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// ---------------------------------------------------------------- helpers

// appendAssistantNote records an assistant message and pushes it to the UI.
// Used by tools that speak to the human directly, such as ask_human.
func (s *Service) appendAssistantNote(ctx context.Context, epoch int, runID int64, body string) error {
	if strings.TrimSpace(body) == "" {
		return nil
	}
	if _, err := s.store.appendMessage(ctx, epoch, &runID,
		llm.Message{Role: "assistant", Content: body}, false); err != nil {
		return err
	}
	s.emit(ctx, &runID, eventTypeMessage, map[string]any{"role": "assistant", "content": body})
	return nil
}

// storeScreenshot keeps a frame available for the live timeline.
func (s *Service) storeScreenshot(ctx context.Context, runID int64, shot daytona.Screenshot) (string, error) {
	if len(shot.Data) == 0 {
		return "", errors.New("agent: empty screenshot")
	}
	id := s.artifacts.put(runID, contentTypeFor(shot.Format), shot.Data)
	return "/artifacts/" + id, nil
}

// emit records an event and publishes it.
func (s *Service) emit(ctx context.Context, runID *int64, eventType string, payload any) {
	if s.events == nil {
		return
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		slog.Debug("could not encode event", "type", eventType, "err", err)
		return
	}

	// Detached: an event describing why a run stopped must still be recorded when
	// the run's own context is already cancelled.
	emitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	if _, err := s.events.Append(emitCtx, runID, eventType, string(encoded)); err != nil {
		slog.Debug("could not record event", "type", eventType, "err", err)
	}
}

func (s *Service) emitRunStatus(ctx context.Context, runID int64, status, message string) {
	s.emit(ctx, &runID, eventTypeStatus, map[string]any{
		"run":     runID,
		"status":  status,
		"message": message,
	})
}

func (s *Service) emitUsage(ctx context.Context, epoch int, resp llm.Response) {
	cost, err := s.store.sessionCost(ctx, epoch)
	if err != nil {
		return
	}
	s.emit(ctx, nil, eventTypeUsage, map[string]any{
		"cost_usd":    cost,
		"context_pct": resp.ContextUsedPct(s.llmContextWindow()),
	})
}

func (s *Service) emitTaskUpdate(ctx context.Context, runID int64, epoch int) {
	plan, err := s.store.activePlan(ctx, epoch)
	if err != nil {
		return
	}
	s.emit(ctx, &runID, eventTypeTaskUpdated, planPayload(plan))
}

func fingerprint(secret string) string {
	if len(secret) <= 6 {
		return fmt.Sprintf("len%d", len(secret))
	}
	return fmt.Sprintf("len%d:%s", len(secret), secret[len(secret)-4:])
}

// runIDKey carries the run id through to the audit hook, which is called from
// inside the llm package and has no other way to know which run it belongs to.
type runIDKey struct{}

func withRunID(ctx context.Context, runID int64) context.Context {
	return context.WithValue(ctx, runIDKey{}, runID)
}

func runIDFromContext(ctx context.Context) *int64 {
	if v, ok := ctx.Value(runIDKey{}).(int64); ok {
		return &v
	}
	return nil
}
