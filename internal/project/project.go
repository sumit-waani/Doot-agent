// Package project owns the single project and its sandbox.
//
// It sits between the web handlers and the Daytona client: handlers never talk
// to Daytona directly, and this package never renders anything.
package project

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/sumit-waani/doot/internal/db"
)

// Project is the single project row.
type Project struct {
	Exists bool

	Name       string
	RepoURL    string
	RepoOwner  string
	RepoName   string
	BaseBranch string
	WorkBranch string
	WorkDir    string

	SetupScript string
	DevCommand  string
	DevPort     int

	SandboxID       string
	SandboxState    string
	SandboxSnapshot string

	VNCResolution string
	VNCWidth      int
	VNCHeight     int
	DesktopStatus string

	PreviewURL       string
	PreviewExpiresAt string

	CurrentEpoch int
	LastError    string
}

// HasSandbox reports whether a sandbox has been provisioned.
func (p Project) HasSandbox() bool { return p.SandboxID != "" }

// SandboxBusy reports whether the sandbox is mid-transition, so the UI can
// disable controls rather than firing conflicting operations.
func (p Project) SandboxBusy() bool {
	switch p.SandboxState {
	case "creating", "starting", "stopping", "restoring", "archiving", "provisioning", "resetting", "deleting":
		return true
	}
	return false
}

// DesktopReady reports whether computer-use is believed to be running.
func (p Project) DesktopReady() bool {
	return p.SandboxState == "started" && p.DesktopStatus == "running"
}

// ErrNoProject is returned when no project exists.
var ErrNoProject = errors.New("project: no project exists")

// ErrProjectExists is returned when one already does.
//
// Doot supports exactly one project. The database enforces this with
// CHECK (id = 1); this error is the friendly version of that constraint.
var ErrProjectExists = errors.New("project: a project already exists; delete it before creating another")

const projectColumns = `
  name, repo_url, repo_owner, repo_name, base_branch, work_branch, work_dir,
  COALESCE(setup_script,''), COALESCE(dev_command,''), COALESCE(dev_port,0),
  COALESCE(sandbox_id,''), COALESCE(sandbox_state,''), sandbox_snapshot,
  vnc_resolution, COALESCE(vnc_width,0), COALESCE(vnc_height,0),
  COALESCE(desktop_status,''), COALESCE(preview_url,''), COALESCE(preview_expires_at,''),
  current_epoch, COALESCE(last_error,'')`

func scanProject(row *sql.Row) (Project, error) {
	var p Project
	err := row.Scan(
		&p.Name, &p.RepoURL, &p.RepoOwner, &p.RepoName, &p.BaseBranch, &p.WorkBranch, &p.WorkDir,
		&p.SetupScript, &p.DevCommand, &p.DevPort,
		&p.SandboxID, &p.SandboxState, &p.SandboxSnapshot,
		&p.VNCResolution, &p.VNCWidth, &p.VNCHeight,
		&p.DesktopStatus, &p.PreviewURL, &p.PreviewExpiresAt,
		&p.CurrentEpoch, &p.LastError,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Project{}, nil
	}
	if err != nil {
		return Project{}, fmt.Errorf("project: load: %w", err)
	}
	p.Exists = true
	return p, nil
}

// Load reads the project, returning a zero Project when none exists.
func Load(ctx context.Context, d *db.DB) (Project, error) {
	return scanProject(d.QueryRowContext(ctx,
		`SELECT`+projectColumns+` FROM project WHERE id = 1`))
}

// repoIdentity extracts owner and repo name from a GitHub URL.
func repoIdentity(rawURL string) (owner, name string, err error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return "", "", errors.New("project: repository URL is required")
	}

	// The sandbox has no SSH keys, and a PAT must never travel over plaintext,
	// so anything that is not HTTPS is rejected. This is checked before parsing
	// because an SSH-style address (git@host:owner/repo) fails url.Parse with a
	// message about path segments, which tells the operator nothing useful.
	if !strings.HasPrefix(strings.ToLower(trimmed), "https://") {
		return "", "", errors.New(
			"project: repository URL must start with https:// (the sandbox has no SSH keys)")
	}

	u, parseErr := url.Parse(trimmed)
	if parseErr != nil {
		return "", "", fmt.Errorf("project: could not read that repository URL: %w", parseErr)
	}

	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("project: repository URL must look like https://github.com/owner/repo")
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), nil
}

// NormalizeRepoURL canonicalises a repository URL and returns its identity.
func NormalizeRepoURL(rawURL string) (normalized, owner, name string, err error) {
	owner, name, err = repoIdentity(rawURL)
	if err != nil {
		return "", "", "", err
	}
	return fmt.Sprintf("https://github.com/%s/%s.git", owner, name), owner, name, nil
}
