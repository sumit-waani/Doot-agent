package web

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"

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
	RunStatus string
	Messages  []messageView
	LastEvent int64
}

type messageView struct {
	Role    string
	Content string
	At      string
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

	page := chatPage{
		basePage:  s.base(ctx, r, user, "chat", "Chat"),
		Project:   p,
		RunStatus: s.currentRunStatus(ctx),
		Messages:  messages,
		LastEvent: lastEvent,
	}
	s.render.render(w, http.StatusOK, "chat", page)
}

// loadMessages returns the live conversation: the current epoch only. Earlier
// epochs are retained in the database but are not part of live context.
func (s *Server) loadMessages(ctx context.Context, epoch int) ([]messageView, error) {
	if epoch == 0 {
		return nil, nil
	}

	const q = `
SELECT role, COALESCE(content,''), created_at
  FROM messages
 WHERE epoch = ?
   AND role IN ('user','assistant')
 ORDER BY seq`

	rows, err := s.db.QueryContext(ctx, q, epoch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []messageView
	for rows.Next() {
		var m messageView
		if err := rows.Scan(&m.Role, &m.Content, &m.At); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// currentRunStatus reports the active run's status, or "idle".
func (s *Server) currentRunStatus(ctx context.Context) string {
	var status string
	const q = `SELECT status FROM runs WHERE active = 1 LIMIT 1`

	err := s.db.QueryRowContext(ctx, q).Scan(&status)
	if errors.Is(err, sql.ErrNoRows) {
		return "idle"
	}
	if err != nil {
		slog.Error("could not read run status", "err", err)
		return "unknown"
	}
	return status
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
