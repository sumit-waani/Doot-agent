package project

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
)

// Timeouts for Daytona operations. Generous: creating a sandbox pulls a
// snapshot, and starting an archived one restores it from object storage.
const (
	provisionTimeout = 8 * time.Minute
	startTimeout     = 5 * time.Minute
	stopTimeout      = 3 * time.Minute
	deleteTimeout    = 3 * time.Minute
	shortOpTimeout   = 60 * time.Second
)

// ErrBusy is returned when a sandbox operation is already in progress.
var ErrBusy = errors.New("project: a sandbox operation is already running")

// Service owns the project and its sandbox lifecycle.
type Service struct {
	db     *db.DB
	cfg    *config.Store
	events *events.Log

	// busy serialises sandbox operations. One project, one sandbox, one
	// operator — concurrent provisioning and deleting is never wanted.
	busyMu sync.Mutex
	busyOp string

	// clientMu guards the cached Daytona client. It is rebuilt when the
	// credential or URL changes, so rotating the API key from Settings takes
	// effect without a restart.
	clientMu    sync.Mutex
	client      *daytona.Client
	clientPrint string

	// heartbeat tracking, keyed by nothing since there is only ever one.
	hbMu     sync.Mutex
	hbCancel context.CancelFunc
}

// NewService builds a Service.
func NewService(d *db.DB, cfg *config.Store, ev *events.Log) *Service {
	return &Service{db: d, cfg: cfg, events: ev}
}

// Load reads the project.
func (s *Service) Load(ctx context.Context) (Project, error) {
	return Load(ctx, s.db)
}

// ---------------------------------------------------------------- daytona client

// daytonaConfig assembles Daytona configuration from settings and secrets.
func (s *Service) daytonaConfig(ctx context.Context) (daytona.Config, error) {
	apiKey, err := s.cfg.Secret(ctx, config.SecretDaytonaAPIKey)
	if err != nil {
		if errors.Is(err, config.ErrNoSecret) {
			return daytona.Config{}, daytona.ErrNoAPIKey
		}
		return daytona.Config{}, err
	}

	return daytona.Config{
		APIKey:             apiKey,
		APIURL:             s.cfg.GetOr("daytona.api_url", daytona.DefaultAPIURL),
		Target:             s.cfg.Get("daytona.target"),
		Snapshot:           s.cfg.GetOr(config.KeySandboxSnapshot, daytona.RequiredSnapshot),
		VNCResolution:      s.cfg.GetOr(config.KeySandboxVNCRes, "1280x800"),
		AutoStopMinutes:    s.cfg.Int(config.KeySandboxAutoStopMin, 30),
		AutoArchiveMinutes: s.cfg.Int("sandbox.auto_archive_minutes", 0),
		AutoDeleteMinutes:  s.cfg.Int("sandbox.auto_delete_minutes", -1),
		TTLMinutes:         s.cfg.Int("sandbox.ttl_minutes", 0),
		Public:             s.cfg.Bool("sandbox.public", false),
		ScreenshotFormat:   s.cfg.GetOr("computeruse.screenshot_format", "jpeg"),
		ScreenshotQuality:  s.cfg.Int("computeruse.screenshot_quality", 80),
		ScreenshotScale:    s.cfg.Float("computeruse.screenshot_scale", 1),
		PreviewTTL:         time.Duration(s.cfg.Int("sandbox.preview_ttl_seconds", 3600)) * time.Second,
	}, nil
}

// Client returns a Daytona client, rebuilding it if the credentials changed.
func (s *Service) Client(ctx context.Context) (*daytona.Client, error) {
	cfg, err := s.daytonaConfig(ctx)
	if err != nil {
		return nil, err
	}

	// Fingerprint only the fields that require a new client; screenshot and
	// interval settings are read per call.
	print := cfg.APIURL + "|" + cfg.Target + "|" + fingerprint(cfg.APIKey)

	s.clientMu.Lock()
	defer s.clientMu.Unlock()

	if s.client != nil && s.clientPrint == print {
		return s.client, nil
	}

	if s.client != nil {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		_ = s.client.Close(closeCtx)
		cancel()
		s.client = nil
	}

	client, err := daytona.New(cfg)
	if err != nil {
		return nil, err
	}
	s.client, s.clientPrint = client, print
	return client, nil
}

// Close releases the cached client.
func (s *Service) Close(ctx context.Context) error {
	s.stopHeartbeat()

	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	if s.client == nil {
		return nil
	}
	err := s.client.Close(ctx)
	s.client = nil
	return err
}

// sandbox attaches to the project's sandbox.
func (s *Service) sandbox(ctx context.Context) (*daytona.Sandbox, Project, error) {
	p, err := s.Load(ctx)
	if err != nil {
		return nil, Project{}, err
	}
	if !p.Exists {
		return nil, p, ErrNoProject
	}
	if !p.HasSandbox() {
		return nil, p, daytona.ErrNoSandbox
	}

	client, err := s.Client(ctx)
	if err != nil {
		return nil, p, err
	}

	sb, err := client.Attach(ctx, p.SandboxID)
	if err != nil {
		return nil, p, err
	}
	return sb, p, nil
}

// ---------------------------------------------------------------- create / delete

// Create inserts the project and provisions its sandbox in the background.
//
// Provisioning is asynchronous because it pulls a snapshot and clones a repo,
// which is far longer than a request should block. Progress is streamed as
// events, and the sandbox state on the project row is the durable record — so a
// machine restart mid-provision leaves an observable state rather than a lie.
func (s *Service) Create(ctx context.Context, name, repoURL string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("project: name is required")
	}

	normalized, owner, repo, err := NormalizeRepoURL(repoURL)
	if err != nil {
		return err
	}

	// Fail before touching the database if Daytona is not configured, rather
	// than leaving a project row with no sandbox.
	if _, err := s.Client(ctx); err != nil {
		return err
	}

	existing, err := s.Load(ctx)
	if err != nil {
		return err
	}
	if existing.Exists {
		return ErrProjectExists
	}

	const q = `
INSERT INTO project (id, name, repo_url, repo_owner, repo_name, base_branch, work_branch,
                     sandbox_snapshot, vnc_resolution, sandbox_state, current_epoch)
VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, 'provisioning',
        (SELECT MAX(epoch) FROM conversation_epochs))`

	_, err = s.db.ExecContext(ctx, q,
		name, normalized, owner, repo,
		s.cfg.GetOr("git.base_branch", "main"),
		s.cfg.GetOr(config.KeyGitWorkBranch, "doot"),
		s.cfg.GetOr(config.KeySandboxSnapshot, daytona.RequiredSnapshot),
		s.cfg.GetOr(config.KeySandboxVNCRes, "1280x800"),
	)
	if err != nil {
		// The CHECK (id = 1) / primary key constraint is what actually enforces
		// one project; translate it rather than leaking SQL at the user.
		if isConstraintViolation(err) {
			return ErrProjectExists
		}
		return fmt.Errorf("project: create: %w", err)
	}

	s.emitState(ctx, "provisioning", "Creating sandbox…")
	s.goProvision()
	return nil
}

// goProvision runs provisioning detached from the request.
func (s *Service) goProvision() {
	if !s.acquire("provision") {
		slog.Warn("provision requested while another sandbox operation is running")
		return
	}

	go func() {
		defer s.release()

		ctx, cancel := context.WithTimeout(context.Background(), provisionTimeout)
		defer cancel()

		if err := s.provision(ctx); err != nil {
			slog.Error("provisioning failed", "err", err)
			s.setSandboxState(ctx, "error", err.Error())
			s.emitState(ctx, "error", err.Error())
			return
		}
		s.emitState(ctx, "started", "Sandbox ready")
	}()
}

// provision creates the sandbox, clones the repo, and runs setup.
func (s *Service) provision(ctx context.Context) error {
	p, err := s.Load(ctx)
	if err != nil {
		return err
	}
	if !p.Exists {
		return ErrNoProject
	}

	client, err := s.Client(ctx)
	if err != nil {
		return err
	}

	sb := (*daytona.Sandbox)(nil)

	// Reuse an existing sandbox if one was already created before a failure,
	// so a retry does not leak sandboxes.
	if p.HasSandbox() {
		if attached, attachErr := client.Attach(ctx, p.SandboxID); attachErr == nil {
			sb = attached
		} else {
			slog.Warn("recorded sandbox is unreachable; creating a new one",
				"sandbox", p.SandboxID, "err", attachErr)
		}
	}

	if sb == nil {
		s.emitState(ctx, "creating", "Creating sandbox…")
		created, createErr := client.Provision(ctx, sandboxName(p.RepoName))
		if createErr != nil {
			return createErr
		}
		sb = created

		// Record the ID immediately: if the next step fails, the sandbox still
		// exists and must be recoverable rather than orphaned.
		if err := s.setSandboxID(ctx, sb.ID()); err != nil {
			return err
		}
	}

	s.emitState(ctx, "starting", "Waiting for sandbox…")
	if _, err := sb.EnsureRunning(ctx, startTimeout); err != nil {
		return err
	}

	s.emitState(ctx, "cloning", "Cloning repository…")
	setup, err := s.repoSetup(ctx, p)
	if err != nil {
		return err
	}
	if err := sb.SetupRepo(ctx, setup); err != nil {
		return err
	}

	if strings.TrimSpace(p.SetupScript) != "" {
		s.emitState(ctx, "setup", "Running setup script…")
		if out, err := sb.RunSetupScript(ctx, setup.WorkDir, p.SetupScript); err != nil {
			return fmt.Errorf("setup script failed: %w (output: %s)", err, out)
		}
	}

	return s.setSandboxState(ctx, "started", "")
}

// Delete destroys the sandbox and clears the project.
//
// Conversation history, audit logs and artifacts survive: deleting and
// recreating is the intended way to switch projects, so destroying the record of
// prior work would be the wrong default.
func (s *Service) Delete(ctx context.Context) error {
	if !s.acquire("delete") {
		return ErrBusy
	}
	defer s.release()

	s.stopHeartbeat()

	p, err := s.Load(ctx)
	if err != nil {
		return err
	}
	if !p.Exists {
		return ErrNoProject
	}

	if p.HasSandbox() {
		s.emitState(ctx, "deleting", "Destroying sandbox…")
		if client, clientErr := s.Client(ctx); clientErr == nil {
			if sb, attachErr := client.Attach(ctx, p.SandboxID); attachErr == nil {
				if err := sb.Delete(ctx, deleteTimeout); err != nil {
					// Do not strand the project row because Daytona is
					// unreachable; the sandbox can be cleaned up by hand.
					slog.Error("could not delete sandbox; clearing the project anyway",
						"sandbox", p.SandboxID, "err", err)
				}
			}
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("project: begin delete: %w", err)
	}
	defer tx.Rollback()

	// End the epoch so the conversation is not carried into the next project,
	// while the messages themselves remain on disk.
	if _, err := tx.ExecContext(ctx, `
UPDATE conversation_epochs
   SET reason = 'clear',
       ended_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
 WHERE epoch = (SELECT current_epoch FROM project WHERE id = 1)
   AND ended_at IS NULL`); err != nil {
		return fmt.Errorf("project: end epoch: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
INSERT OR IGNORE INTO conversation_epochs (epoch)
VALUES ((SELECT MAX(epoch) + 1 FROM conversation_epochs))`); err != nil {
		return fmt.Errorf("project: open epoch: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM project WHERE id = 1`); err != nil {
		return fmt.Errorf("project: delete row: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("project: commit delete: %w", err)
	}

	s.emitState(ctx, "", "Project deleted")
	return nil
}

// Reset destroys the sandbox and rebuilds it from the repository.
//
// Uncommitted work in the old sandbox is lost. That is acceptable because
// commits happen per subtask, so the exposure is at most one subtask. The
// conversation is untouched.
func (s *Service) Reset(ctx context.Context) error {
	p, err := s.Load(ctx)
	if err != nil {
		return err
	}
	if !p.Exists {
		return ErrNoProject
	}

	if !s.acquire("reset") {
		return ErrBusy
	}

	s.stopHeartbeat()

	go func() {
		defer s.release()

		bg, cancel := context.WithTimeout(context.Background(), provisionTimeout)
		defer cancel()

		s.emitState(bg, "resetting", "Destroying sandbox…")

		if p.HasSandbox() {
			if client, clientErr := s.Client(bg); clientErr == nil {
				if sb, attachErr := client.Attach(bg, p.SandboxID); attachErr == nil {
					if err := sb.Delete(bg, deleteTimeout); err != nil {
						slog.Error("could not delete sandbox during reset", "err", err)
					}
				}
			}
		}

		if err := s.clearSandbox(bg); err != nil {
			slog.Error("could not clear sandbox fields", "err", err)
		}

		if err := s.provision(bg); err != nil {
			slog.Error("reset provisioning failed", "err", err)
			s.setSandboxState(bg, "error", err.Error())
			s.emitState(bg, "error", err.Error())
			return
		}
		s.emitState(bg, "started", "Sandbox rebuilt")
	}()

	return nil
}

// ---------------------------------------------------------------- lifecycle

// Start wakes the sandbox.
func (s *Service) Start(ctx context.Context) error {
	if !s.acquire("start") {
		return ErrBusy
	}

	go func() {
		defer s.release()

		bg, cancel := context.WithTimeout(context.Background(), startTimeout)
		defer cancel()

		sb, _, err := s.sandbox(bg)
		if err != nil {
			s.emitState(bg, "error", err.Error())
			return
		}

		s.emitState(bg, "starting", "Starting sandbox…")
		info, err := sb.EnsureRunning(bg, startTimeout)
		if err != nil {
			slog.Error("could not start sandbox", "err", err)
			s.setSandboxState(bg, "error", err.Error())
			s.emitState(bg, "error", err.Error())
			return
		}

		// Computer-use processes never survive a stop, so whatever was believed
		// about the desktop is now stale.
		s.setDesktopStatus(bg, "")
		s.setSandboxState(bg, string(info.State), "")
		s.emitState(bg, string(info.State), "Sandbox running")
	}()

	return nil
}

// Stop stops the sandbox, preserving the filesystem.
func (s *Service) Stop(ctx context.Context) error {
	if !s.acquire("stop") {
		return ErrBusy
	}

	s.stopHeartbeat()

	go func() {
		defer s.release()

		bg, cancel := context.WithTimeout(context.Background(), stopTimeout)
		defer cancel()

		sb, _, err := s.sandbox(bg)
		if err != nil {
			s.emitState(bg, "error", err.Error())
			return
		}

		s.emitState(bg, "stopping", "Stopping sandbox…")
		if err := sb.Stop(bg, stopTimeout); err != nil {
			slog.Error("could not stop sandbox", "err", err)
			s.emitState(bg, "error", err.Error())
			return
		}

		s.setDesktopStatus(bg, "")
		s.setSandboxState(bg, "stopped", "")
		s.emitState(bg, "stopped", "Sandbox stopped")
	}()

	return nil
}

// Refresh re-reads sandbox state from Daytona and stores it.
//
// Worth doing on demand because Daytona can stop a sandbox on its own via
// auto-stop, which Doot would otherwise not notice.
func (s *Service) Refresh(ctx context.Context) (Project, error) {
	sb, p, err := s.sandbox(ctx)
	if err != nil {
		return p, err
	}

	info, err := sb.Info(ctx)
	if err != nil {
		return p, err
	}

	if string(info.State) != p.SandboxState {
		if err := s.setSandboxState(ctx, string(info.State), info.ErrorReason); err != nil {
			return p, err
		}
		if info.State != daytona.StateStarted {
			s.setDesktopStatus(ctx, "")
		}
		s.emitState(ctx, string(info.State), "")
	}

	return s.Load(ctx)
}

// ---------------------------------------------------------------- desktop

// StartDesktop brings up computer-use and resolves the real screen geometry.
func (s *Service) StartDesktop(ctx context.Context) error {
	if !s.acquire("desktop") {
		return ErrBusy
	}

	go func() {
		defer s.release()

		bg, cancel := context.WithTimeout(context.Background(), startTimeout)
		defer cancel()

		sb, _, err := s.sandbox(bg)
		if err != nil {
			s.emitState(bg, "error", err.Error())
			return
		}

		s.emitState(bg, "starting", "Waking sandbox…")
		if _, err := sb.EnsureRunning(bg, startTimeout); err != nil {
			s.emitState(bg, "error", err.Error())
			return
		}

		s.emitState(bg, "started", "Starting desktop…")
		if err := sb.EnsureDesktop(bg); err != nil {
			slog.Error("could not start computer-use", "err", err)
			s.setDesktopStatus(bg, "error")
			s.emitState(bg, "started", "Desktop failed: "+err.Error())
			return
		}

		// Record the geometry the X server actually allocated. The configured
		// resolution is only a request, and a mismatch displaces every click.
		if display, err := sb.Display(bg); err != nil {
			slog.Warn("could not read display geometry", "err", err)
		} else {
			if err := s.setDisplay(bg, display.Width, display.Height); err != nil {
				slog.Warn("could not store display geometry", "err", err)
			}
			slog.Info("desktop ready", "width", display.Width, "height", display.Height)
		}

		s.setDesktopStatus(bg, "running")
		if _, err := s.refreshVNCPreview(bg, sb); err != nil {
			slog.Warn("could not issue VNC preview link", "err", err)
		}
		s.emitState(bg, "started", "Desktop ready")
	}()

	return nil
}

// StopDesktop stops the computer-use processes but leaves the sandbox running.
func (s *Service) StopDesktop(ctx context.Context) error {
	sb, _, err := s.sandbox(ctx)
	if err != nil {
		return err
	}

	callCtx, cancel := context.WithTimeout(ctx, shortOpTimeout)
	defer cancel()

	if err := sb.StopDesktop(callCtx); err != nil {
		return err
	}
	s.setDesktopStatus(ctx, "")
	s.emitState(ctx, "started", "Desktop stopped")
	return nil
}

// VNCURL returns a usable noVNC link, re-issuing it when close to expiry.
func (s *Service) VNCURL(ctx context.Context) (string, error) {
	p, err := s.Load(ctx)
	if err != nil {
		return "", err
	}
	if p.PreviewURL != "" && !previewExpiringSoon(p.PreviewExpiresAt) {
		return p.PreviewURL, nil
	}

	sb, _, err := s.sandbox(ctx)
	if err != nil {
		return "", err
	}
	return s.refreshVNCPreview(ctx, sb)
}

func (s *Service) refreshVNCPreview(ctx context.Context, sb *daytona.Sandbox) (string, error) {
	port := s.cfg.Int("sandbox.vnc_port", 6080)

	callCtx, cancel := context.WithTimeout(ctx, shortOpTimeout)
	defer cancel()

	preview, err := sb.SignedPreview(callCtx, port)
	if err != nil {
		return "", err
	}

	const q = `
UPDATE project
   SET preview_url = ?, preview_expires_at = ?,
       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
 WHERE id = 1`
	if _, err := s.db.ExecContext(ctx, q, preview.URL,
		preview.ExpiresAt.Format("2006-01-02T15:04:05.000Z")); err != nil {
		return preview.URL, fmt.Errorf("project: store preview url: %w", err)
	}
	return preview.URL, nil
}

// PreviewURL returns a signed link to an arbitrary port, for the dev server.
func (s *Service) PreviewURL(ctx context.Context, port int) (string, error) {
	sb, _, err := s.sandbox(ctx)
	if err != nil {
		return "", err
	}

	callCtx, cancel := context.WithTimeout(ctx, shortOpTimeout)
	defer cancel()

	preview, err := sb.SignedPreview(callCtx, port)
	if err != nil {
		return "", err
	}
	return preview.URL, nil
}

// ---------------------------------------------------------------- heartbeat

// StartHeartbeat keeps the sandbox awake for the duration of an active run.
//
// Daytona's inactivity timer ignores work happening inside the sandbox, so a
// long build or a slow model call reads as idle and the sandbox gets stopped
// mid-run. Auto-stop is deliberately left enabled, so this runs only while a run
// is active rather than keeping the sandbox up permanently.
func (s *Service) StartHeartbeat(ctx context.Context) error {
	sb, _, err := s.sandbox(ctx)
	if err != nil {
		return err
	}

	interval := time.Duration(s.cfg.Int("sandbox.heartbeat_seconds", 300)) * time.Second

	s.hbMu.Lock()
	defer s.hbMu.Unlock()

	if s.hbCancel != nil {
		return nil // already beating
	}

	hbCtx, cancel := context.WithCancel(context.Background())
	s.hbCancel = cancel

	go func() {
		defer func() {
			s.hbMu.Lock()
			if s.hbCancel != nil {
				s.hbCancel = nil
			}
			s.hbMu.Unlock()
		}()

		sb.Heartbeat(hbCtx, interval, func(err error) {
			slog.Warn("sandbox may auto-stop: heartbeat is failing", "err", err)
		})
	}()

	slog.Info("sandbox heartbeat started", "interval", interval)
	return nil
}

// StopHeartbeat stops the heartbeat, letting auto-stop reclaim an idle sandbox.
func (s *Service) StopHeartbeat() { s.stopHeartbeat() }

func (s *Service) stopHeartbeat() {
	s.hbMu.Lock()
	defer s.hbMu.Unlock()
	if s.hbCancel != nil {
		s.hbCancel()
		s.hbCancel = nil
		slog.Info("sandbox heartbeat stopped")
	}
}

// HeartbeatRunning reports whether the heartbeat is active.
func (s *Service) HeartbeatRunning() bool {
	s.hbMu.Lock()
	defer s.hbMu.Unlock()
	return s.hbCancel != nil
}

// ---------------------------------------------------------------- settings

// UpdateBuild saves the setup script and dev command.
func (s *Service) UpdateBuild(ctx context.Context, setupScript, devCommand string, devPort int) error {
	const q = `
UPDATE project
   SET setup_script = ?, dev_command = ?, dev_port = ?,
       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
 WHERE id = 1`
	res, err := s.db.ExecContext(ctx, q, setupScript, devCommand, nullableInt(devPort))
	if err != nil {
		return fmt.Errorf("project: update build settings: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNoProject
	}
	return nil
}

// RunSetupScript re-runs the setup script against the live sandbox.
func (s *Service) RunSetupScript(ctx context.Context) (string, error) {
	sb, p, err := s.sandbox(ctx)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(p.SetupScript) == "" {
		return "", errors.New("project: no setup script configured")
	}

	if _, err := sb.EnsureRunning(ctx, startTimeout); err != nil {
		return "", err
	}
	return sb.RunSetupScript(ctx, p.WorkDir, p.SetupScript)
}

// ---------------------------------------------------------------- helpers

// repoSetup assembles the git configuration for the sandbox.
func (s *Service) repoSetup(ctx context.Context, p Project) (daytona.RepoSetup, error) {
	token, err := s.cfg.Secret(ctx, config.SecretGitHubPAT)
	if err != nil && !errors.Is(err, config.ErrNoSecret) {
		return daytona.RepoSetup{}, err
	}
	// A missing PAT is tolerated: a public repo clones without one, and the
	// failure on push is clearer than refusing to provision at all.
	if errors.Is(err, config.ErrNoSecret) {
		slog.Warn("no GitHub PAT configured; private repos and pushes will fail")
	}

	return daytona.RepoSetup{
		RepoURL:        p.RepoURL,
		WorkDir:        p.WorkDir,
		BaseBranch:     p.BaseBranch,
		WorkBranch:     p.WorkBranch,
		AuthorName:     s.cfg.GetOr(config.KeyGitAuthorName, "doot"),
		AuthorEmail:    s.cfg.GetOr(config.KeyGitAuthorEmail, "doot@local"),
		GitHubUsername: s.cfg.Get(config.KeyGitHubUsername),
		Token:          token,
	}, nil
}

// RepoSetup exposes the git configuration for the agent loop.
func (s *Service) RepoSetup(ctx context.Context) (daytona.RepoSetup, Project, error) {
	p, err := s.Load(ctx)
	if err != nil {
		return daytona.RepoSetup{}, p, err
	}
	if !p.Exists {
		return daytona.RepoSetup{}, p, ErrNoProject
	}
	setup, err := s.repoSetup(ctx, p)
	return setup, p, err
}

// Sandbox returns a ready sandbox handle for the agent loop, waking it if needed.
func (s *Service) Sandbox(ctx context.Context) (*daytona.Sandbox, Project, error) {
	sb, p, err := s.sandbox(ctx)
	if err != nil {
		return nil, p, err
	}
	if _, err := sb.EnsureRunning(ctx, startTimeout); err != nil {
		return nil, p, err
	}
	return sb, p, nil
}

func (s *Service) acquire(op string) bool {
	s.busyMu.Lock()
	defer s.busyMu.Unlock()
	if s.busyOp != "" {
		return false
	}
	s.busyOp = op
	return true
}

func (s *Service) release() {
	s.busyMu.Lock()
	s.busyOp = ""
	s.busyMu.Unlock()
}

// BusyOp reports the running sandbox operation, or "".
func (s *Service) BusyOp() string {
	s.busyMu.Lock()
	defer s.busyMu.Unlock()
	return s.busyOp
}

func (s *Service) setSandboxID(ctx context.Context, id string) error {
	const q = `UPDATE project SET sandbox_id = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = 1`
	if _, err := s.db.ExecContext(ctx, q, id); err != nil {
		return fmt.Errorf("project: store sandbox id: %w", err)
	}
	return nil
}

func (s *Service) setSandboxState(ctx context.Context, state, errMsg string) error {
	const q = `
UPDATE project
   SET sandbox_state = ?, last_error = ?,
       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
 WHERE id = 1`
	if _, err := s.db.ExecContext(ctx, q, state, nullableString(errMsg)); err != nil {
		return fmt.Errorf("project: store sandbox state: %w", err)
	}
	return nil
}

func (s *Service) setDesktopStatus(ctx context.Context, status string) {
	const q = `UPDATE project SET desktop_status = ? WHERE id = 1`
	if _, err := s.db.ExecContext(ctx, q, nullableString(status)); err != nil {
		slog.Warn("could not store desktop status", "err", err)
	}
}

func (s *Service) setDisplay(ctx context.Context, width, height int) error {
	const q = `UPDATE project SET vnc_width = ?, vnc_height = ? WHERE id = 1`
	if _, err := s.db.ExecContext(ctx, q, width, height); err != nil {
		return fmt.Errorf("project: store display geometry: %w", err)
	}
	return nil
}

// emitState publishes a sandbox state change to the UI.
func (s *Service) emitState(ctx context.Context, state, message string) {
	if s.events == nil {
		return
	}

	payload, err := json.Marshal(map[string]string{"state": state, "message": message})
	if err != nil {
		return
	}

	// Detached from the request: an event must still be logged when the caller
	// that triggered it has already returned.
	emitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	if _, err := s.events.Append(emitCtx, nil, events.TypeSandboxState, string(payload)); err != nil {
		slog.Warn("could not record sandbox state event", "err", err)
	}
}

func (s *Service) clearSandbox(ctx context.Context) error {
	const q = `
UPDATE project
   SET sandbox_id = NULL, sandbox_state = 'provisioning', desktop_status = NULL,
       vnc_width = NULL, vnc_height = NULL,
       preview_url = NULL, preview_expires_at = NULL, last_error = NULL,
       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
 WHERE id = 1`
	if _, err := s.db.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("project: clear sandbox: %w", err)
	}
	return nil
}

func sandboxName(repoName string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + 32
		default:
			return '-'
		}
	}, repoName)
	clean = strings.Trim(clean, "-")
	if clean == "" {
		clean = "project"
	}
	return fmt.Sprintf("doot-%s", clean)
}

func previewExpiringSoon(expiresAt string) bool {
	if expiresAt == "" {
		return true
	}
	for _, layout := range []string{"2006-01-02T15:04:05.000Z", time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, expiresAt); err == nil {
			// Re-issue with margin: a link that expires while the page is open
			// is indistinguishable from a broken sandbox.
			return time.Now().UTC().Add(5 * time.Minute).After(t)
		}
	}
	return true
}

func isConstraintViolation(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "constraint")
}

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func fingerprint(secret string) string {
	if secret == "" {
		return ""
	}
	if len(secret) <= 8 {
		return fmt.Sprintf("len%d", len(secret))
	}
	return fmt.Sprintf("len%d:%s", len(secret), secret[len(secret)-4:])
}
