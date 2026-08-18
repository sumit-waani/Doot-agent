package daytona

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	sdk "github.com/daytona/clients/sdk-go/pkg/daytona"
	"github.com/daytona/clients/sdk-go/pkg/types"
)

// State mirrors the sandbox lifecycle states Doot cares about.
type State string

const (
	StateUnknown   State = "unknown"
	StateCreating  State = "creating"
	StateStarting  State = "starting"
	StateStarted   State = "started"
	StateStopping  State = "stopping"
	StateStopped   State = "stopped"
	StateArchived  State = "archived"
	StateError     State = "error"
	StateDestroyed State = "destroyed"
)

// Info is a snapshot of a sandbox's observable state.
type Info struct {
	ID           string
	State        State
	Snapshot     string
	CPU          float32
	MemoryGiB    float32
	DiskGiB      float32
	Public       bool
	Recoverable  bool
	ErrorReason  string
	LastActivity string
}

// Sandbox is a handle to one project sandbox.
type Sandbox struct {
	client *Client
	sb     *sdk.Sandbox
}

// Provision creates the project sandbox.
//
// Every lifecycle interval is passed explicitly. Left to Daytona's defaults,
// the sandbox would auto-stop after 15 minutes of what it considers inactivity,
// and a missing auto-delete value is the difference between a sandbox that
// survives and one that takes the working tree with it.
func (c *Client) Provision(ctx context.Context, name string) (*Sandbox, error) {
	autoStop := c.cfg.AutoStopMinutes
	autoArchive := c.cfg.AutoArchiveMinutes
	autoDelete := c.cfg.AutoDeleteMinutes
	ttl := c.cfg.TTLMinutes

	params := types.SnapshotParams{
		Snapshot: c.cfg.Snapshot,
		SandboxBaseParams: types.SandboxBaseParams{
			Name: name,
			// VNC_RESOLUTION is only honoured at creation time.
			EnvVars: map[string]string{
				"VNC_RESOLUTION": c.cfg.VNCResolution,
			},
			Labels: map[string]string{
				"managed-by": "doot",
			},
			Public:           c.cfg.Public,
			AutoStopInterval: &autoStop,
			// AutoPauseInterval is deliberately left nil: it is mutually
			// exclusive with auto-stop, and container sandboxes cannot pause.
			AutoArchiveInterval: &autoArchive,
			AutoDeleteInterval:  &autoDelete,
			TtlMinutes:          &ttl,
		},
	}

	slog.Info("provisioning sandbox",
		"snapshot", c.cfg.Snapshot,
		"resolution", c.cfg.VNCResolution,
		"auto_stop_min", autoStop,
		"auto_delete_min", autoDelete,
		"ttl_min", ttl,
	)

	sb, err := c.sdk.Create(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("daytona: create sandbox: %w", err)
	}
	return &Sandbox{client: c, sb: sb}, nil
}

// Attach returns a handle to an existing sandbox.
func (c *Client) Attach(ctx context.Context, sandboxID string) (*Sandbox, error) {
	if strings.TrimSpace(sandboxID) == "" {
		return nil, ErrNoSandbox
	}
	sb, err := c.sdk.Get(ctx, sandboxID)
	if err != nil {
		return nil, fmt.Errorf("daytona: get sandbox %s: %w", sandboxID, err)
	}
	return &Sandbox{client: c, sb: sb}, nil
}

// ID returns the sandbox identifier.
func (s *Sandbox) ID() string { return s.sb.ID }

// Info returns the current observable state, refreshed from the API.
func (s *Sandbox) Info(ctx context.Context) (Info, error) {
	if err := s.sb.RefreshData(ctx); err != nil {
		return Info{}, fmt.Errorf("daytona: refresh sandbox: %w", err)
	}
	return s.info(), nil
}

// CachedInfo returns state from the last refresh, without an API call.
func (s *Sandbox) CachedInfo() Info { return s.info() }

func (s *Sandbox) info() Info {
	in := Info{
		ID:        s.sb.ID,
		State:     State(string(s.sb.State)),
		CPU:       s.sb.Cpu,
		MemoryGiB: s.sb.Memory,
		DiskGiB:   s.sb.Disk,
		Public:    s.sb.Public,
	}
	if s.sb.Snapshot != nil {
		in.Snapshot = *s.sb.Snapshot
	}
	if s.sb.Recoverable != nil {
		in.Recoverable = *s.sb.Recoverable
	}
	if s.sb.ErrorReason != nil {
		in.ErrorReason = *s.sb.ErrorReason
	}
	if s.sb.LastActivityAt != nil {
		in.LastActivity = *s.sb.LastActivityAt
	}
	return in
}

// EnsureRunning brings the sandbox to a usable state, whatever it starts from.
//
// Container sandboxes preserve their filesystem across a stop but not their
// memory, so nothing may be assumed to still be running afterwards: dev servers
// and computer-use processes both need starting again.
func (s *Sandbox) EnsureRunning(ctx context.Context, timeout time.Duration) (Info, error) {
	info, err := s.Info(ctx)
	if err != nil {
		return Info{}, err
	}

	switch info.State {
	case StateStarted:
		return info, nil

	case StateStopped, StateArchived:
		// Archived sandboxes restore from object storage, so this can be slow.
		slog.Info("starting sandbox", "id", s.sb.ID, "from", info.State)
		if err := s.sb.StartWithTimeout(ctx, timeout); err != nil {
			return Info{}, fmt.Errorf("daytona: start sandbox: %w", err)
		}

	case StateStarting, StateCreating:
		if err := s.sb.WaitForStart(ctx, timeout); err != nil {
			return Info{}, fmt.Errorf("daytona: wait for sandbox start: %w", err)
		}

	case StateStopping:
		// Let the stop finish, then start again; a start issued mid-stop is
		// rejected.
		if err := s.sb.WaitForStop(ctx, timeout); err != nil {
			return Info{}, fmt.Errorf("daytona: wait for sandbox stop: %w", err)
		}
		if err := s.sb.StartWithTimeout(ctx, timeout); err != nil {
			return Info{}, fmt.Errorf("daytona: start sandbox after stop: %w", err)
		}

	case StateError:
		if !info.Recoverable {
			return info, fmt.Errorf("daytona: sandbox is in an unrecoverable error state: %s", info.ErrorReason)
		}
		slog.Warn("recovering sandbox", "id", s.sb.ID, "reason", info.ErrorReason)
		if err := s.recover(ctx); err != nil {
			return info, err
		}
		if err := s.sb.WaitForStart(ctx, timeout); err != nil {
			return Info{}, fmt.Errorf("daytona: wait for sandbox start after recover: %w", err)
		}

	case StateDestroyed:
		return info, fmt.Errorf("daytona: sandbox %s has been destroyed; reset the sandbox to rebuild it", s.sb.ID)

	default:
		// Any transitional state: wait it out rather than guessing.
		if err := s.sb.WaitForStart(ctx, timeout); err != nil {
			return Info{}, fmt.Errorf("daytona: wait for sandbox from state %q: %w", info.State, err)
		}
	}

	return s.Info(ctx)
}

// recover asks Daytona to recover a recoverable error state.
func (s *Sandbox) recover(ctx context.Context) error {
	_, _, err := s.client.control.SandboxAPI.
		RecoverSandbox(s.client.authContext(ctx), s.sb.ID).
		Execute()
	if err != nil {
		return fmt.Errorf("daytona: recover sandbox: %w", err)
	}
	return nil
}

// Stop stops the sandbox, preserving its filesystem.
func (s *Sandbox) Stop(ctx context.Context, timeout time.Duration) error {
	if err := s.sb.StopWithTimeout(ctx, timeout, false); err != nil {
		return fmt.Errorf("daytona: stop sandbox: %w", err)
	}
	return nil
}

// Delete destroys the sandbox and everything in it.
func (s *Sandbox) Delete(ctx context.Context, timeout time.Duration) error {
	if err := s.sb.DeleteAndWait(ctx, timeout); err != nil {
		// A sandbox that is already gone is the desired end state, not a
		// failure worth surfacing.
		if isNotFound(err) {
			slog.Info("sandbox already gone", "id", s.sb.ID)
			return nil
		}
		return fmt.Errorf("daytona: delete sandbox: %w", err)
	}
	return nil
}

// Exec runs a shell command in the sandbox.
func (s *Sandbox) Exec(ctx context.Context, command string) (string, int, error) {
	res, err := s.sb.Process.ExecuteCommand(ctx, command)
	if err != nil {
		return "", -1, fmt.Errorf("daytona: exec %q: %w", firstLine(command), err)
	}
	return res.Result, res.ExitCode, nil
}

// ExecCheck runs a command and turns a non-zero exit into an error, with the
// output attached — otherwise a failed setup script reports nothing useful.
func (s *Sandbox) ExecCheck(ctx context.Context, command string) (string, error) {
	out, code, err := s.Exec(ctx, command)
	if err != nil {
		return out, err
	}
	if code != 0 {
		return out, fmt.Errorf("daytona: command %q exited %d: %s",
			firstLine(command), code, truncate(strings.TrimSpace(out), 2000))
	}
	return out, nil
}

// Resources reads the sandbox's real CPU and memory limits from cgroups.
//
// nproc, free, top and /proc all report the *host's* values inside a Daytona
// sandbox, so anything that sizes a build from them will be wrong. cgroup files
// are the only honest source.
func (s *Sandbox) Resources(ctx context.Context) (cores float64, memoryBytes int64, err error) {
	out, execErr := s.ExecCheck(ctx, "cat /sys/fs/cgroup/cpu.max /sys/fs/cgroup/memory.max")
	if execErr != nil {
		return 0, 0, execErr
	}

	lines := strings.Fields(strings.TrimSpace(out))
	// cpu.max is "<quota> <period>"; memory.max follows on the next line.
	if len(lines) < 3 {
		return 0, 0, fmt.Errorf("daytona: unexpected cgroup output: %q", truncate(out, 200))
	}

	var quota, period float64
	if lines[0] == "max" {
		quota = -1
	} else if _, err := fmt.Sscanf(lines[0], "%f", &quota); err != nil {
		return 0, 0, fmt.Errorf("daytona: bad cpu.max quota %q", lines[0])
	}
	if _, err := fmt.Sscanf(lines[1], "%f", &period); err != nil || period == 0 {
		return 0, 0, fmt.Errorf("daytona: bad cpu.max period %q", lines[1])
	}
	if quota > 0 {
		cores = quota / period
	}

	if lines[2] != "max" {
		if _, err := fmt.Sscanf(lines[2], "%d", &memoryBytes); err != nil {
			return cores, 0, fmt.Errorf("daytona: bad memory.max %q", lines[2])
		}
	}
	return cores, memoryBytes, nil
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "404") ||
		strings.Contains(msg, "not found") ||
		errors.Is(err, ErrNoSandbox)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
