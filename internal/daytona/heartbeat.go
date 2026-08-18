package daytona

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Touch tells Daytona the sandbox is in use.
//
// This exists because of a specific trap: Daytona's inactivity timer is reset
// only by *external* interaction — lifecycle changes, preview traffic, SSH, and
// Toolbox API calls. It is explicitly not reset by work happening inside the
// sandbox. A twenty-minute build, a long test suite, or the agent thinking all
// count as idle, so without this a run gets its sandbox stopped mid-flight.
//
// The purpose-built control-plane endpoint is used first. If that fails, a
// cheap Toolbox read is used instead, since Toolbox calls also reset the timer.
// The fallback is worth the few lines: the failure it covers (an endpoint or
// auth change) would otherwise be invisible until a sandbox died mid-build.
func (s *Sandbox) Touch(ctx context.Context) error {
	_, err := s.client.control.SandboxAPI.
		UpdateLastActivity(s.client.authContext(ctx), s.sb.ID).
		Execute()
	if err == nil {
		return nil
	}

	slog.Debug("activity endpoint failed; falling back to a toolbox call", "err", err)

	if _, dirErr := s.sb.GetWorkingDir(ctx); dirErr != nil {
		return fmt.Errorf("daytona: could not signal activity (endpoint: %v; toolbox: %w)", err, dirErr)
	}
	return nil
}

// Heartbeat keeps the sandbox awake until ctx is cancelled.
//
// Intended to run for the lifetime of an active agent run and no longer: an
// always-on sandbox costs money for a tool used opportunistically from a phone,
// so auto-stop is left in place to catch genuinely idle time.
//
// It touches immediately, then on every tick, so a run that starts just before
// the inactivity deadline does not race it.
func (s *Sandbox) Heartbeat(ctx context.Context, every time.Duration, onError func(error)) {
	if every <= 0 {
		every = 5 * time.Minute
	}

	touch := func() {
		// Bounded independently of ctx: a hung request must not stall the next
		// tick, and a cancelled run should not report a spurious failure.
		callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
		defer cancel()

		if err := s.Touch(callCtx); err != nil {
			if ctx.Err() != nil {
				return // run ended mid-call; not an error worth reporting
			}
			slog.Warn("sandbox heartbeat failed", "id", s.sb.ID, "err", err)
			if onError != nil {
				onError(err)
			}
			return
		}
		slog.Debug("sandbox heartbeat", "id", s.sb.ID)
	}

	touch()

	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			touch()
		}
	}
}
