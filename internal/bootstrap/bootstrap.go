// Package bootstrap performs the fixed startup sequence described in
// docs/02-database.md. Each step must succeed before the next runs, and the
// server does not serve until all of them have.
package bootstrap

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"github.com/sumit-waani/doot/internal/db"
	"github.com/sumit-waani/doot/internal/passwd"
)

// DefaultUsername and DefaultPassword are the credentials created when the
// users table is empty. Changeable from the Settings screen afterwards.
const (
	DefaultUsername = "doot"
	DefaultPassword = "doot"
)

// Result reports what the startup sequence did, so main can log a single
// accurate summary and the UI can show the default-password banner.
type Result struct {
	CreatedDefaultUser bool
	ResetDefaultUser   bool
	InterruptedRuns    int
	PrunedEvents       int64
	UsingDefaultPass   bool
}

// Run executes the full startup sequence after migrations have been applied.
func Run(ctx context.Context, d *db.DB, resetAdmin bool) (Result, error) {
	var res Result

	created, err := EnsureDefaultUser(ctx, d)
	if err != nil {
		return res, err
	}
	res.CreatedDefaultUser = created

	if resetAdmin {
		if err := ResetDefaultUser(ctx, d); err != nil {
			return res, err
		}
		res.ResetDefaultUser = true
	}

	usingDefault, err := UsingDefaultPassword(ctx, d)
	if err != nil {
		return res, err
	}
	res.UsingDefaultPass = usingDefault

	interrupted, err := ReconcileInterruptedRuns(ctx, d)
	if err != nil {
		return res, err
	}
	res.InterruptedRuns = interrupted

	pruned, err := PruneEvents(ctx, d)
	if err != nil {
		return res, err
	}
	res.PrunedEvents = pruned

	if err := PruneExpiredSessions(ctx, d); err != nil {
		return res, err
	}

	return res, nil
}

// EnsureDefaultUser creates the default user when, and only when, no user
// exists. Idempotent by construction: with a user present it does nothing.
func EnsureDefaultUser(ctx context.Context, d *db.DB) (bool, error) {
	var count int
	if err := d.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return false, fmt.Errorf("bootstrap: count users: %w", err)
	}
	if count > 0 {
		return false, nil
	}

	hash, err := passwd.Hash(DefaultPassword)
	if err != nil {
		return false, fmt.Errorf("bootstrap: hash default password: %w", err)
	}

	const q = `INSERT INTO users (username, password_hash) VALUES (?, ?)`
	if _, err := d.ExecContext(ctx, q, DefaultUsername, hash); err != nil {
		return false, fmt.Errorf("bootstrap: create default user: %w", err)
	}

	slog.Warn("created default user; change the password from Settings",
		"username", DefaultUsername)
	return true, nil
}

// ResetDefaultUser restores the default credentials. Break-glass only, driven
// by DOOT_RESET_ADMIN, for when the password is lost and there is no other way
// in. Every session is dropped so an attacker-held cookie cannot outlive it.
func ResetDefaultUser(ctx context.Context, d *db.DB) error {
	hash, err := passwd.Hash(DefaultPassword)
	if err != nil {
		return fmt.Errorf("bootstrap: hash default password: %w", err)
	}

	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("bootstrap: begin admin reset: %w", err)
	}
	defer tx.Rollback()

	const upsert = `
INSERT INTO users (username, password_hash, updated_at)
VALUES (?, ?, strftime('%Y-%m-%dT%H:%M:%fZ','now'))
ON CONFLICT(username) DO UPDATE SET
  password_hash = excluded.password_hash,
  updated_at    = excluded.updated_at`
	if _, err := tx.ExecContext(ctx, upsert, DefaultUsername, hash); err != nil {
		return fmt.Errorf("bootstrap: reset default user: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions`); err != nil {
		return fmt.Errorf("bootstrap: clear sessions on reset: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("bootstrap: commit admin reset: %w", err)
	}

	slog.Warn("DOOT_RESET_ADMIN was set: credentials reset to defaults and all sessions cleared",
		"username", DefaultUsername)
	return nil
}

// UsingDefaultPassword reports whether the default user still has the default
// password, which drives the dismissible banner. It nags; it does not force.
func UsingDefaultPassword(ctx context.Context, d *db.DB) (bool, error) {
	var hash string
	const q = `SELECT password_hash FROM users WHERE username = ?`

	err := d.QueryRowContext(ctx, q, DefaultUsername).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("bootstrap: read default user: %w", err)
	}

	switch err := passwd.Verify(DefaultPassword, hash); {
	case err == nil:
		return true, nil
	case errors.Is(err, passwd.ErrMismatch):
		return false, nil
	default:
		// An unusable stored hash is a real problem, but not one worth
		// blocking startup over: it would lock the operator out entirely.
		slog.Error("stored password hash is unusable", "err", err)
		return false, nil
	}
}

// ReconcileInterruptedRuns marks runs that were still executing when the
// process died.
//
// A Fly machine can be restarted for host maintenance at any time, so a run
// left in 'running' is a lie once the process is gone. It becomes 'interrupted'
// and inactive, freeing the one-active-run slot. Interrupted runs are offered
// for resume, never silently resurrected.
func ReconcileInterruptedRuns(ctx context.Context, d *db.DB) (int, error) {
	const q = `
UPDATE runs
   SET status = 'interrupted',
       active = 0,
       ended_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
 WHERE active = 1
   AND status = 'running'`

	res, err := d.ExecContext(ctx, q)
	if err != nil {
		return 0, fmt.Errorf("bootstrap: reconcile interrupted runs: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil // not all drivers report this; not worth failing over
	}
	if n > 0 {
		slog.Warn("marked runs as interrupted after restart", "count", n)
	}
	return int(n), nil
}

// PruneEvents trims the SSE event log: keep 7 days or the most recent 5,000
// rows, whichever is larger. This is the only table Doot ever deletes from.
func PruneEvents(ctx context.Context, d *db.DB) (int64, error) {
	const q = `
DELETE FROM events
 WHERE created_at < strftime('%Y-%m-%dT%H:%M:%fZ','now','-7 days')
   AND id NOT IN (SELECT id FROM events ORDER BY id DESC LIMIT 5000)`

	res, err := d.ExecContext(ctx, q)
	if err != nil {
		return 0, fmt.Errorf("bootstrap: prune events: %w", err)
	}

	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	if n > 0 {
		slog.Info("pruned old SSE events", "count", n)
	}
	return n, nil
}

// PruneExpiredSessions deletes sessions past their expiry.
func PruneExpiredSessions(ctx context.Context, d *db.DB) error {
	const q = `DELETE FROM sessions WHERE expires_at < strftime('%Y-%m-%dT%H:%M:%fZ','now')`
	if _, err := d.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("bootstrap: prune expired sessions: %w", err)
	}
	return nil
}
