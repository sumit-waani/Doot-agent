package web

import (
	"context"
	"database/sql"
	"log/slog"
	"net/http"

	"github.com/sumit-waani/doot/internal/agent"
	"github.com/sumit-waani/doot/internal/auth"
	"github.com/sumit-waani/doot/internal/config"
	"github.com/sumit-waani/doot/internal/project"
)

// basePage carries what the layout needs on every screen.
type basePage struct {
	Title string
	// Nav is the active tab: chat, desktop, project, settings.
	Nav string
	// Chrome controls whether the tab bar renders. Login and the offline page
	// have none.
	Chrome           bool
	User             auth.User
	UsingDefaultPass bool
	HasProject       bool
	Flash            string
	Error            string
}

func (s *Server) base(ctx context.Context, r *http.Request, user auth.User, nav, title string) basePage {
	p, _ := s.project.Load(ctx)
	return basePage{
		Title:            title,
		Nav:              nav,
		Chrome:           true,
		User:             user,
		UsingDefaultPass: s.usingDefaultPass,
		HasProject:       p.Exists,
		Flash:            r.URL.Query().Get("ok"),
		Error:            r.URL.Query().Get("err"),
	}
}

// ---------------------------------------------------------------- chat

type chatPage struct {
	basePage
	Project   project.Project
	Status    agent.Status
	Messages  []messageView
	LastEvent int64

	// Active reports whether a run is doing work, which decides whether the
	// composer shows Send or Pause.
	Active bool
	// Waiting reports that the run is parked on the human, so the next message is
	// an answer rather than a new instruction.
	Waiting bool
}

type messageView struct {
	Role      string
	Content   string
	At        string
	IsSummary bool
	Tools     []toolCallView
}

type toolCallView struct {
	Name     string
	Status   string
	Preview  string
	Duration int
}

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request, user auth.User) {
	ctx := r.Context()

	p, err := s.project.Load(ctx)
	if err != nil {
		slog.Error("could not load project", "err", err)
	}

	// A fresh client starts from the current head, so it does not replay the
	// whole log on first connect.
	lastEvent, err := s.events.LatestID(ctx)
	if err != nil {
		slog.Error("could not read latest event id", "err", err)
	}

	messages, err := s.loadMessages(ctx, p.CurrentEpoch)
	if err != nil {
		slog.Error("could not load messages", "err", err)
	}

	status, err := s.agent.Status(ctx)
	if err != nil {
		slog.Error("could not load agent status", "err", err)
	}

	page := chatPage{
		basePage:  s.base(ctx, r, user, "chat", "Chat"),
		Project:   p,
		Status:    status,
		Messages:  messages,
		LastEvent: lastEvent,
		Active:    status.RunStatus == agent.StatusRunning,
		Waiting: status.RunStatus == agent.StatusAwaitingHuman ||
			status.RunStatus == agent.StatusAwaitingApproval,
	}
	s.render.render(w, http.StatusOK, "chat", page)
}

// loadMessages returns the live conversation: the current epoch only. Earlier
// epochs are retained in the database but are not part of live context.
//
// Tool results are not rendered as messages. They are attached to the assistant
// turn that requested them, collapsed, so the timeline stays scannable on a
// phone instead of being buried in build output.
func (s *Server) loadMessages(ctx context.Context, epoch int) ([]messageView, error) {
	if epoch == 0 {
		return nil, nil
	}

	const q = `
SELECT id, role, COALESCE(content,''), created_at, is_summary
  FROM messages
 WHERE epoch = ?
   AND role IN ('user','assistant')
 ORDER BY seq`

	rows, err := s.db.QueryContext(ctx, q, epoch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var (
		out []messageView
		ids []int64
	)
	byID := map[int64]int{}

	for rows.Next() {
		var (
			id        int64
			m         messageView
			isSummary int
		)
		if err := rows.Scan(&id, &m.Role, &m.Content, &m.At, &isSummary); err != nil {
			return nil, err
		}
		m.IsSummary = isSummary == 1
		byID[id] = len(out)
		ids = append(ids, id)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(ids) == 0 {
		return out, nil
	}

	tools, err := s.loadToolCalls(ctx, epoch)
	if err != nil {
		return out, err
	}
	for messageID, calls := range tools {
		if idx, ok := byID[messageID]; ok {
			out[idx].Tools = calls
		}
	}

	return out, nil
}

// loadToolCalls groups tool calls by the assistant message that requested them.
func (s *Server) loadToolCalls(ctx context.Context, epoch int) (map[int64][]toolCallView, error) {
	const q = `
SELECT tc.message_id, tc.name, tc.status, COALESCE(tc.result_preview,''), COALESCE(tc.duration_ms,0)
  FROM tool_calls tc
  JOIN messages m ON m.id = tc.message_id
 WHERE m.epoch = ?
 ORDER BY tc.id`

	rows, err := s.db.QueryContext(ctx, q, epoch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[int64][]toolCallView{}
	for rows.Next() {
		var (
			messageID sql.NullInt64
			v         toolCallView
		)
		if err := rows.Scan(&messageID, &v.Name, &v.Status, &v.Preview, &v.Duration); err != nil {
			return nil, err
		}
		if messageID.Valid {
			out[messageID.Int64] = append(out[messageID.Int64], v)
		}
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- desktop

type desktopPage struct {
	basePage
	Project project.Project
	VNCURL  string
	Ready   bool
	Busy    string
}

func (s *Server) handleDesktop(w http.ResponseWriter, r *http.Request, user auth.User) {
	ctx := r.Context()

	p, err := s.project.Load(ctx)
	if err != nil {
		slog.Error("could not load project", "err", err)
	}

	page := desktopPage{
		basePage: s.base(ctx, r, user, "desktop", "Desktop"),
		Project:  p,
		Busy:     s.project.BusyOp(),
	}

	// Only issue a preview link when the desktop is actually up. Embedding a
	// frame that cannot connect looks identical to a broken sandbox.
	if p.DesktopReady() {
		url, err := s.project.VNCURL(ctx)
		if err != nil {
			slog.Warn("could not get VNC url", "err", err)
			page.Error = "Could not get a desktop link: " + err.Error()
		} else {
			page.VNCURL = url
			page.Ready = true
		}
	}

	s.render.render(w, http.StatusOK, "desktop", page)
}

// ---------------------------------------------------------------- project

type projectPage struct {
	basePage
	Project   project.Project
	Usage     usageSummary
	Busy      string
	Heartbeat bool
}

// usageSummary is the cost breakdown by purpose, which is the one cost question
// worth answering: whether the E2E verifier is where the money goes.
type usageSummary struct {
	Rows  []usageRow
	Total usageRow
}

type usageRow struct {
	Purpose          string
	Calls            int
	PromptTokens     int64
	CompletionTokens int64
	CostUSD          float64
}

func (s *Server) handleProject(w http.ResponseWriter, r *http.Request, user auth.User) {
	ctx := r.Context()

	p, err := s.project.Load(ctx)
	if err != nil {
		slog.Error("could not load project", "err", err)
	}

	// Refresh from Daytona when nothing is in flight: auto-stop can stop the
	// sandbox without Doot being told.
	if p.HasSandbox() && s.project.BusyOp() == "" && !p.SandboxBusy() {
		if refreshed, err := s.project.Refresh(ctx); err != nil {
			slog.Debug("could not refresh sandbox state", "err", err)
		} else {
			p = refreshed
		}
	}

	usage, err := s.loadUsage(ctx)
	if err != nil {
		slog.Error("could not load usage", "err", err)
	}

	page := projectPage{
		basePage:  s.base(ctx, r, user, "project", "Project"),
		Project:   p,
		Usage:     usage,
		Busy:      s.project.BusyOp(),
		Heartbeat: s.project.HeartbeatRunning(),
	}
	s.render.render(w, http.StatusOK, "project", page)
}

func (s *Server) loadUsage(ctx context.Context) (usageSummary, error) {
	const q = `
SELECT purpose,
       COUNT(*)                    AS calls,
       SUM(prompt_tokens)          AS prompt_tokens,
       SUM(completion_tokens)      AS completion_tokens,
       SUM(cost_usd)               AS cost_usd
  FROM llm_calls
 GROUP BY purpose
 ORDER BY purpose`

	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return usageSummary{}, err
	}
	defer rows.Close()

	var out usageSummary
	for rows.Next() {
		var r usageRow
		if err := rows.Scan(&r.Purpose, &r.Calls, &r.PromptTokens, &r.CompletionTokens, &r.CostUSD); err != nil {
			return usageSummary{}, err
		}
		out.Rows = append(out.Rows, r)
		out.Total.Calls += r.Calls
		out.Total.PromptTokens += r.PromptTokens
		out.Total.CompletionTokens += r.CompletionTokens
		out.Total.CostUSD += r.CostUSD
	}
	out.Total.Purpose = "total"
	return out, rows.Err()
}

// ---------------------------------------------------------------- settings

type settingsPage struct {
	basePage
	Settings map[string]string
	Secrets  []secretField
	Saved    string
}

type secretField struct {
	Name  string
	Label string
	Set   bool
}

var secretLabels = map[string]string{
	config.SecretLLMAPIKey:         "LLM API key",
	config.SecretDaytonaAPIKey:     "Daytona API key",
	config.SecretGitHubPAT:         "GitHub PAT",
	config.SecretR2AccessKeyID:     "R2 access key ID",
	config.SecretR2SecretAccessKey: "R2 secret access key",
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request, user auth.User) {
	ctx := r.Context()

	present, err := s.cfg.SecretsPresent(ctx)
	if err != nil {
		slog.Error("could not read secret presence", "err", err)
	}

	fields := make([]secretField, 0, len(config.AllSecretNames))
	for _, name := range config.AllSecretNames {
		fields = append(fields, secretField{
			Name:  name,
			Label: secretLabels[name],
			Set:   present[name],
		})
	}

	page := settingsPage{
		basePage: s.base(ctx, r, user, "settings", "Settings"),
		Settings: s.cfg.All(),
		Secrets:  fields,
		Saved:    r.URL.Query().Get("saved"),
	}
	s.render.render(w, http.StatusOK, "settings", page)
}
