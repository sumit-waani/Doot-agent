package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sumit-waani/doot/internal/llm"
)

// defaultMaxTurns bounds one run's tool-calling turns.
//
// This is a runaway guard, not a policy: the design deliberately has no retry cap
// on the reviewer loop, because trusting the agent to self-manage has held up in
// practice. This exists only so a pathological loop cannot bill indefinitely
// while nobody is watching.
const defaultMaxTurns = 200

// errPaused unwinds the loop when the operator pauses.
var errPaused = errors.New("agent: paused")

// errParked unwinds the loop when the run is waiting on the human.
var errParked = errors.New("agent: awaiting human")

// run executes the loop until it finishes, parks, or is paused.
//
// The context here is the run's own, not a request's: a run outlives the HTTP
// call that started it.
func (s *Service) run(ctx context.Context, runID int64) {
	defer s.clearCancel(runID)

	err := s.loop(ctx, runID)

	switch {
	case err == nil:
		if finErr := s.store.finishRun(ctx, runID, StatusDone, nil); finErr != nil {
			slog.Error("could not finish run", "run", runID, "err", finErr)
		}
		s.emitRunStatus(ctx, runID, StatusDone, "")

	case errors.Is(err, errPaused):
		// Paused runs stay active: the slot is still theirs, and resuming must
		// pick up exactly where they stopped.
		if parkErr := s.store.parkRun(ctx, runID, StatusPaused); parkErr != nil {
			slog.Error("could not park paused run", "run", runID, "err", parkErr)
		}
		s.emitRunStatus(ctx, runID, StatusPaused, "Paused")

	case errors.Is(err, errParked):
		if parkErr := s.store.parkRun(ctx, runID, StatusAwaitingHuman); parkErr != nil {
			slog.Error("could not park run", "run", runID, "err", parkErr)
		}
		s.emitRunStatus(ctx, runID, StatusAwaitingHuman, "Waiting for you")

	case errors.Is(err, errAwaitingApproval):
		if parkErr := s.store.parkRun(ctx, runID, StatusAwaitingApproval); parkErr != nil {
			slog.Error("could not park run", "run", runID, "err", parkErr)
		}
		s.emitRunStatus(ctx, runID, StatusAwaitingApproval, "Plan awaiting your approval")

	case errors.Is(err, context.Canceled):
		// Process shutdown, not a failure. Marked interrupted so the boot
		// reconciler and the UI agree it can be resumed.
		if finErr := s.store.finishRun(ctx, runID, StatusInterrupted, err); finErr != nil {
			slog.Error("could not mark run interrupted", "run", runID, "err", finErr)
		}
		s.emitRunStatus(ctx, runID, StatusInterrupted, "Interrupted")

	default:
		slog.Error("run failed", "run", runID, "err", err)
		if finErr := s.store.finishRun(ctx, runID, StatusError, err); finErr != nil {
			slog.Error("could not mark run failed", "run", runID, "err", finErr)
		}
		s.appendAssistantNote(ctx, s.epochOf(ctx, runID), runID,
			"This run stopped with an error:\n\n"+err.Error())
		s.emitRunStatus(ctx, runID, StatusError, err.Error())
	}

	// Let auto-stop reclaim the sandbox now that nothing is running.
	s.projects.StopHeartbeat()
}

// errAwaitingApproval unwinds the loop when a plan needs sign-off.
var errAwaitingApproval = errors.New("agent: awaiting plan approval")

// loop is the turn loop.
func (s *Service) loop(ctx context.Context, runID int64) error {
	client, err := s.LLM(ctx)
	if err != nil {
		return err
	}

	maxTurns := s.cfg.Int("agent.max_turns", defaultMaxTurns)
	if maxTurns <= 0 {
		maxTurns = defaultMaxTurns
	}

	for turn := 1; turn <= maxTurns; turn++ {
		if err := s.checkStop(ctx, runID); err != nil {
			return err
		}

		tc, err := s.buildToolContext(ctx, runID)
		if err != nil {
			return err
		}

		if err := s.store.touchRun(ctx, runID); err != nil {
			slog.Debug("could not touch run", "err", err)
		}

		messages, err := s.assembleMessages(ctx, tc)
		if err != nil {
			return err
		}

		hasPlan := s.planInProgress(ctx, tc.Epoch)
		tools := s.registry.schemas(hasPlan)

		resp, callErr := s.streamTurn(ctx, client, runID, messages, tools)
		if callErr != nil {
			return callErr
		}

		// Persist the assistant turn before running anything: a tool that runs
		// without its request recorded would leave an unreplayable transcript.
		assistantID, err := s.store.appendMessage(ctx, tc.Epoch, &runID, llm.Message{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		}, false)
		if err != nil {
			return err
		}
		if err := s.store.setMessageTokens(ctx, assistantID,
			resp.Usage.PromptTokens, resp.Usage.CompletionTokens); err != nil {
			slog.Debug("could not record message tokens", "err", err)
		}

		if strings.TrimSpace(resp.Content) != "" {
			s.emit(ctx, &runID, eventTypeMessage, map[string]any{
				"role":    "assistant",
				"content": resp.Content,
			})
		}
		s.emitUsage(ctx, tc.Epoch, resp)

		// No tool calls means the model is done talking for this turn.
		if len(resp.ToolCalls) == 0 {
			return nil
		}

		outcome, err := s.executeToolCalls(ctx, tc, assistantID, resp.ToolCalls)
		if err != nil {
			return err
		}

		switch {
		case outcome.parkForHuman:
			return errParked
		case outcome.awaitApproval:
			return errAwaitingApproval
		case outcome.goalComplete:
			return nil
		}

		// Compaction happens between turns, which is the natural checkpoint: a
		// tool call is never left half-answered across an epoch boundary.
		if s.shouldCompact(resp.PromptTokens) {
			newEpoch, err := s.compact(ctx, &runID, tc.Epoch, "threshold")
			if err != nil {
				// A failed compaction is not fatal on its own; the next turn will
				// simply be large. Failing the run would lose more.
				slog.Error("compaction failed; continuing", "err", err)
			} else if newEpoch != tc.Epoch {
				// Keep the run row pointing at where its conversation now lives, so
				// anything querying runs and messages together stays coherent.
				if err := s.store.setRunEpoch(ctx, runID, newEpoch); err != nil {
					slog.Warn("could not update run epoch after compaction", "err", err)
				}
			}
		}
	}

	return fmt.Errorf("agent: stopped after %d turns without finishing", maxTurns)
}

// streamTurn performs one model call, streaming deltas to the UI.
func (s *Service) streamTurn(
	ctx context.Context,
	client *llm.Client,
	runID int64,
	messages []llm.Message,
	tools []llm.Tool,
) (llm.Response, error) {
	var pending strings.Builder
	lastFlush := time.Now()

	// Deltas are batched rather than emitted per token: every event is a database
	// write, and a token-per-row event log would dominate the run's cost in I/O.
	flush := func(force bool) {
		if pending.Len() == 0 {
			return
		}
		if !force && time.Since(lastFlush) < 400*time.Millisecond && pending.Len() < 240 {
			return
		}
		s.emit(ctx, &runID, eventTypeDelta, map[string]any{"delta": pending.String()})
		pending.Reset()
		lastFlush = time.Now()
	}

	stream := &llm.Stream{
		OnContentDelta: func(delta string) {
			pending.WriteString(delta)
			flush(false)
		},
		OnToolCall: func(call llm.ToolCall) {
			flush(true)
			s.emit(ctx, &runID, eventTypeToolStart, map[string]any{
				"name": call.Name,
				"id":   call.ID,
			})
		},
	}

	resp, err := client.Complete(ctx, llm.PurposePrimary, messages, tools, stream)
	flush(true)

	if err != nil {
		return resp, err
	}
	return resp, nil
}

// toolOutcome collects the control signals from a batch of tool calls.
type toolOutcome struct {
	parkForHuman  bool
	awaitApproval bool
	goalComplete  bool
}

// executeToolCalls runs each requested tool in order.
//
// Every call gets a tool message back, including failures: the protocol requires
// one response per call, and an unanswered tool call makes the next request
// invalid.
func (s *Service) executeToolCalls(
	ctx context.Context,
	tc toolContext,
	assistantID int64,
	calls []llm.ToolCall,
) (toolOutcome, error) {
	var outcome toolOutcome

	for _, call := range calls {
		// Pause is checked between calls rather than mid-call, so a tool either
		// runs to completion or does not start.
		if err := s.checkStop(ctx, tc.RunID); err != nil {
			return outcome, err
		}

		result, runErr := s.executeOne(ctx, tc, assistantID, call)

		content := result.Content
		if runErr != nil {
			// The error goes back as the tool result so the model can adapt,
			// rather than failing the run over a recoverable mistake.
			content = "Error: " + runErr.Error()
		}
		if strings.TrimSpace(content) == "" {
			content = "(no output)"
		}

		if _, err := s.store.appendMessage(ctx, tc.Epoch, &tc.RunID, llm.Message{
			Role:       "tool",
			Content:    content,
			ToolCallID: call.ID,
			Name:       call.Name,
		}, false); err != nil {
			return outcome, err
		}

		if result.ParkForHuman {
			outcome.parkForHuman = true
		}
		if result.PlanCreated > 0 {
			outcome.awaitApproval = true
		}
		if result.GoalComplete {
			outcome.goalComplete = true
		}
	}

	return outcome, nil
}

// executeOne runs a single tool call and records it.
func (s *Service) executeOne(
	ctx context.Context,
	tc toolContext,
	assistantID int64,
	call llm.ToolCall,
) (toolResult, error) {
	started := time.Now()

	recordID, err := s.store.recordToolCall(ctx, tc.RunID, &assistantID, call)
	if err != nil {
		slog.Error("could not record tool call", "err", err)
	}

	finish := func(result toolResult, toolErr error) (toolResult, error) {
		status := "ok"
		preview := result.Preview
		if toolErr != nil {
			status = "error"
			preview = toolErr.Error()
		}

		duration := int(time.Since(started).Milliseconds())
		if recordID > 0 {
			if err := s.store.finishToolCall(ctx, recordID, status, preview, duration, toolErr); err != nil {
				slog.Debug("could not finish tool call record", "err", err)
			}
		}

		s.emit(ctx, &tc.RunID, eventTypeToolEnd, map[string]any{
			"name":     call.Name,
			"id":       call.ID,
			"status":   status,
			"preview":  truncate(preview, 400),
			"duration": duration,
		})

		return result, toolErr
	}

	def, ok := s.registry.get(call.Name)
	if !ok {
		return finish(toolResult{}, fmt.Errorf("unknown tool %q", call.Name))
	}

	if def.NeedsSandbox && tc.Sandbox == nil {
		return finish(toolResult{}, errors.New(
			"the sandbox is not available; it may be stopped or still provisioning"))
	}

	result, toolErr := def.Run(ctx, tc, json.RawMessage(call.Arguments))
	return finish(result, toolErr)
}

// assembleMessages builds the request: system prompt plus the live epoch.
func (s *Service) assembleMessages(ctx context.Context, tc toolContext) ([]llm.Message, error) {
	history, err := s.store.loadContext(ctx, tc.Epoch)
	if err != nil {
		return nil, err
	}

	messages := make([]llm.Message, 0, len(history)+2)
	messages = append(messages, llm.Message{Role: "system", Content: s.systemPrompt(tc.Project)})

	if plan, err := s.store.activePlan(ctx, tc.Epoch); err == nil && plan.Status != PlanAwaitingApproval {
		// Plan state is injected fresh each turn rather than relying on the
		// transcript, so progress survives compaction and a restart.
		messages = append(messages, llm.Message{
			Role:    "system",
			Content: renderPlanState(plan),
		})
	}

	messages = append(messages, history...)
	return messages, nil
}

func renderPlanState(p Plan) string {
	done, total := p.Progress()

	var b strings.Builder
	fmt.Fprintf(&b, "# Current plan (%d/%d complete)\n\n%s\n%s\n\n", done, total, p.Title, p.Goal)
	for _, t := range p.Tasks {
		marker := " "
		switch t.Status {
		case TaskDone:
			marker = "x"
		case TaskInProgress:
			marker = ">"
		case TaskSkipped:
			marker = "-"
		case TaskFailed:
			marker = "!"
		}
		fmt.Fprintf(&b, "[%s] %d. %s\n", marker, t.Seq, t.Title)
		if t.Detail != "" && t.Status != TaskDone {
			fmt.Fprintf(&b, "      %s\n", singleLine(t.Detail))
		}
	}

	if next := p.PendingTask(); next != nil {
		fmt.Fprintf(&b, "\nWork on task %d next.", next.Seq)
	} else {
		b.WriteString("\nAll tasks are finished. Verify end to end, push, then call goal_complete.")
	}
	return b.String()
}

// planInProgress reports whether an approved plan is being executed.
func (s *Service) planInProgress(ctx context.Context, epoch int) bool {
	plan, err := s.store.activePlan(ctx, epoch)
	if err != nil {
		return false
	}
	return plan.Status == PlanApproved || plan.Status == PlanInProgress
}

// checkStop returns errPaused when the operator has asked the run to stop.
//
// Cooperative rather than abrupt: the loop checks between turns and between tool
// calls, so a pause never leaves a tool half-run or a tool call unanswered.
func (s *Service) checkStop(ctx context.Context, runID int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	run, err := s.store.getRun(ctx, runID)
	if err != nil {
		return err
	}
	if run.Status == StatusPaused || run.Status == StatusCancelled {
		return errPaused
	}
	return nil
}

// buildToolContext gathers what tools need for this turn.
func (s *Service) buildToolContext(ctx context.Context, runID int64) (toolContext, error) {
	p, err := s.projects.Load(ctx)
	if err != nil {
		return toolContext{}, err
	}
	if !p.Exists {
		return toolContext{}, errNoProject
	}

	// The project's current epoch, not the run's starting epoch. Compaction rolls
	// the epoch mid-run, and reading the run's original value here would make the
	// next turn reload the pre-compaction transcript — compacting would cost a
	// summarisation call and change nothing.
	tc := toolContext{
		Service: s,
		RunID:   runID,
		Epoch:   p.CurrentEpoch,
		Project: p,
	}

	// A missing sandbox is not fatal: conversation-only turns are useful, and the
	// tools that need one report that clearly instead of the run dying.
	sb, _, err := s.projects.Sandbox(ctx)
	if err != nil {
		slog.Warn("sandbox unavailable for this turn", "err", err)
		return tc, nil
	}
	tc.Sandbox = sb

	setup, _, err := s.projects.RepoSetup(ctx)
	if err != nil {
		slog.Warn("could not build repo setup", "err", err)
	} else {
		tc.Setup = setup
	}

	return tc, nil
}

// epochOf returns a run's epoch, falling back to the project's current epoch.
func (s *Service) epochOf(ctx context.Context, runID int64) int {
	if run, err := s.store.getRun(ctx, runID); err == nil {
		return run.Epoch
	}
	if epoch, err := s.store.currentEpoch(ctx); err == nil {
		return epoch
	}
	return 0
}
