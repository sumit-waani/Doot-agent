package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migration is one numbered SQL file embedded in the binary.
type migration struct {
	Version  int
	Name     string
	SQL      string
	Checksum string
}

// Migrate applies every pending migration, in order.
//
// Properties, per docs/02-database.md:
//   - Runs on every boot. Deploying is the only action required.
//   - Idempotent: applied versions are skipped by version number.
//   - Transactional per file, claiming the version up front so two machines
//     booting at once (as happens briefly during a deploy) cannot double-apply.
//   - Forward-only. There are no down migrations.
//   - Checksummed: editing an already-applied file is a startup error, because
//     it means schema history was rewritten.
func Migrate(ctx context.Context, d *DB) error {
	if err := ensureMigrationsTable(ctx, d); err != nil {
		return err
	}

	all, err := loadMigrations()
	if err != nil {
		return err
	}

	applied, err := appliedMigrations(ctx, d)
	if err != nil {
		return err
	}

	for _, m := range all {
		prev, done := applied[m.Version]
		if done {
			if prev != m.Checksum {
				return fmt.Errorf(
					"db: migration %04d (%s) was modified after being applied "+
						"(recorded %s, embedded %s); migrations are forward-only",
					m.Version, m.Name, short(prev), short(m.Checksum))
			}
			continue
		}
		applied, err := applyMigrationWithRetry(ctx, d, m)
		if err != nil {
			return err
		}
		if applied {
			slog.Info("migration applied", "version", m.Version, "name", m.Name)
		}
	}

	return nil
}

// ensureMigrationsTable creates the tracking table. It is created by the runner
// rather than by a migration, since it has to exist before any can be recorded.
func ensureMigrationsTable(ctx context.Context, d *DB) error {
	const q = `
CREATE TABLE IF NOT EXISTS schema_migrations (
  version    INTEGER PRIMARY KEY,
  name       TEXT NOT NULL,
  checksum   TEXT NOT NULL,
  applied_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
)`
	if _, err := d.ExecContext(ctx, q); err != nil {
		return fmt.Errorf("db: create schema_migrations: %w", err)
	}
	return nil
}

func appliedMigrations(ctx context.Context, d *DB) (map[int]string, error) {
	rows, err := d.QueryContext(ctx, `SELECT version, checksum FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("db: read schema_migrations: %w", err)
	}
	defer rows.Close()

	out := make(map[int]string)
	for rows.Next() {
		var v int
		var sum string
		if err := rows.Scan(&v, &sum); err != nil {
			return nil, fmt.Errorf("db: scan schema_migrations: %w", err)
		}
		out[v] = sum
	}
	return out, rows.Err()
}

// migrationLockAttempts bounds how long we wait for a peer holding the write
// lock. Deploys overlap for seconds, not minutes.
const migrationLockAttempts = 6

// applyMigrationWithRetry applies one migration, waiting out a peer that holds
// the write lock.
//
// Two machines briefly overlap on every deploy, and only one can hold the write
// lock. Without this the loser exits, Fly restarts it, and the rollover carries a
// crash for no reason: a transient lock is expected here, not exceptional.
//
// Reports whether this process was the one that applied it.
func applyMigrationWithRetry(ctx context.Context, d *DB, m migration) (bool, error) {
	delay := 250 * time.Millisecond

	for attempt := 1; ; attempt++ {
		err := applyMigration(ctx, d, m)
		if err == nil {
			return true, nil
		}
		if !isLocked(err) {
			return false, err
		}

		// The peer may have finished while we were waiting.
		if done, checkErr := alreadyApplied(ctx, d, m); checkErr == nil && done {
			slog.Info("migration applied by another process while waiting",
				"version", m.Version, "name", m.Name)
			return false, nil
		}

		if attempt >= migrationLockAttempts {
			return false, fmt.Errorf(
				"db: migration %04d (%s) could not get the write lock after %d attempts: %w",
				m.Version, m.Name, attempt, err)
		}

		slog.Warn("migration is waiting for the database write lock",
			"version", m.Version, "attempt", attempt, "retry_in", delay.String())

		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
	}
}

// alreadyApplied reports whether this exact migration is now recorded.
func alreadyApplied(ctx context.Context, d *DB, m migration) (bool, error) {
	applied, err := appliedMigrations(ctx, d)
	if err != nil {
		return false, err
	}
	recorded, ok := applied[m.Version]
	if !ok {
		return false, nil
	}
	if recorded != m.Checksum {
		return false, fmt.Errorf(
			"db: migration %04d (%s) was applied elsewhere with a different checksum "+
				"(recorded %s, embedded %s)",
			m.Version, m.Name, short(recorded), short(m.Checksum))
	}
	return true, nil
}

// isLocked reports whether err is a transient write-lock contention error.
// Matched on the message because neither driver exposes a sentinel for it.
func isLocked(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") ||
		strings.Contains(msg, "sqlite_busy") ||
		strings.Contains(msg, "is busy")
}

// applyMigration runs one file in a single transaction.
//
// Two constraints shape this, both learned the hard way:
//
//  1. It must use sql.Tx, never a pinned sql.Conn with a hand-written BEGIN.
//     Over Hrana-HTTP a request that leaves no transaction open returns no
//     baton, and the driver then marks that connection dead. database/sql
//     silently retries such a connection for pool-level calls, which is why
//     Exec and Query work — but a pinned sql.Conn gets no retry, so the raw
//     BEGIN failed with "stream is closed: driver: bad connection" against
//     Turso while passing against an embedded driver locally.
//
//  2. Statements are executed one at a time, because the libSQL HTTP driver
//     does not accept multiple statements per Exec.
//
// The version is claimed *before* the migration body runs. That is deliberate:
// the INSERT turns the transaction into a write transaction straight away, which
// is the portable equivalent of BEGIN IMMEDIATE, and it makes the claim atomic
// so an overlapping deploy fails fast on the primary key instead of racing to
// the commit. If the migration body then fails, the rollback releases the claim.
func applyMigration(ctx context.Context, d *DB, m migration) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("db: begin migration %04d: %w", m.Version, err)
	}
	defer tx.Rollback()

	const claim = `INSERT INTO schema_migrations (version, name, checksum) VALUES (?, ?, ?)`
	if _, err := tx.ExecContext(ctx, claim, m.Version, m.Name, m.Checksum); err != nil {
		if isConstraintViolation(err) {
			// Another process claimed this version between our read and now,
			// which happens when two machines overlap during a deploy.
			return confirmPeerApplied(ctx, d, m, err)
		}
		return fmt.Errorf("db: claim migration %04d: %w", m.Version, err)
	}

	for i, stmt := range splitStatements(m.SQL) {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf(
				"db: migration %04d (%s) statement %d failed: %w\n--- statement ---\n%s",
				m.Version, m.Name, i+1, err, stmt)
		}
	}

	if err := tx.Commit(); err != nil {
		if isConstraintViolation(err) {
			return confirmPeerApplied(ctx, d, m, err)
		}
		return fmt.Errorf("db: commit migration %04d: %w", m.Version, err)
	}
	return nil
}

// confirmPeerApplied checks whether another process applied this exact
// migration, so an overlapping deploy resolves quietly instead of crash-looping.
func confirmPeerApplied(ctx context.Context, d *DB, m migration, cause error) error {
	applied, err := appliedMigrations(ctx, d)
	if err != nil {
		return errors.Join(cause, err)
	}

	recorded, ok := applied[m.Version]
	if !ok {
		// The conflict was not this version being taken, so it is a real error.
		return fmt.Errorf("db: claim migration %04d: %w", m.Version, cause)
	}
	if recorded != m.Checksum {
		return fmt.Errorf(
			"db: migration %04d (%s) was applied elsewhere with a different checksum "+
				"(recorded %s, embedded %s)",
			m.Version, m.Name, short(recorded), short(m.Checksum))
	}

	slog.Info("migration already applied by another process; continuing",
		"version", m.Version, "name", m.Name)
	return nil
}

// isConstraintViolation reports whether err is a uniqueness or primary key
// conflict. Matched on the message because the two drivers report it with
// different error types and neither exposes a sentinel.
func isConstraintViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") ||
		strings.Contains(msg, "unique_violation") ||
		strings.Contains(msg, "primary key") ||
		strings.Contains(msg, "sqlite_constraint_primarykey") ||
		strings.Contains(msg, "constraint failed")
}

// loadMigrations reads and sorts the embedded files.
// Filenames must be NNNN_name.sql.
func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("db: read embedded migrations: %w", err)
	}

	var out []migration
	seen := make(map[int]string)

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		version, name, err := parseMigrationName(e.Name())
		if err != nil {
			return nil, err
		}
		if other, dup := seen[version]; dup {
			return nil, fmt.Errorf("db: duplicate migration version %04d (%s and %s)", version, other, e.Name())
		}
		seen[version] = e.Name()

		body, err := migrationFS.ReadFile("migrations/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("db: read migration %s: %w", e.Name(), err)
		}
		sum := sha256.Sum256(body)

		out = append(out, migration{
			Version:  version,
			Name:     name,
			SQL:      string(body),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}

	if len(out) == 0 {
		return nil, errors.New("db: no embedded migrations found")
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

func parseMigrationName(filename string) (int, string, error) {
	base := strings.TrimSuffix(filename, ".sql")
	idx := strings.Index(base, "_")
	if idx <= 0 {
		return 0, "", fmt.Errorf("db: migration %q must be named NNNN_name.sql", filename)
	}
	version, err := strconv.Atoi(base[:idx])
	if err != nil {
		return 0, "", fmt.Errorf("db: migration %q has a non-numeric version: %w", filename, err)
	}
	return version, base[idx+1:], nil
}

// splitStatements splits a SQL file on semicolons at statement level, ignoring
// semicolons inside string literals, quoted identifiers, and comments.
func splitStatements(src string) []string {
	var (
		out       []string
		cur       strings.Builder
		inSingle  bool // '...'
		inDouble  bool // "..."
		inBracket bool // [...]
		inLine    bool // -- ...
		inBlock   bool // /* ... */
	)

	runes := []rune(src)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		var next rune
		if i+1 < len(runes) {
			next = runes[i+1]
		}

		switch {
		case inLine:
			if c == '\n' {
				inLine = false
				cur.WriteRune(c)
			}
			continue

		case inBlock:
			if c == '*' && next == '/' {
				inBlock = false
				i++
			}
			continue

		case inSingle:
			cur.WriteRune(c)
			if c == '\'' {
				if next == '\'' { // escaped quote
					cur.WriteRune(next)
					i++
				} else {
					inSingle = false
				}
			}
			continue

		case inDouble:
			cur.WriteRune(c)
			if c == '"' {
				inDouble = false
			}
			continue

		case inBracket:
			cur.WriteRune(c)
			if c == ']' {
				inBracket = false
			}
			continue
		}

		switch {
		case c == '-' && next == '-':
			inLine = true
			i++
		case c == '/' && next == '*':
			inBlock = true
			i++
		case c == '\'':
			inSingle = true
			cur.WriteRune(c)
		case c == '"':
			inDouble = true
			cur.WriteRune(c)
		case c == '[':
			inBracket = true
			cur.WriteRune(c)
		case c == ';':
			if s := strings.TrimSpace(cur.String()); s != "" {
				out = append(out, s)
			}
			cur.Reset()
		default:
			cur.WriteRune(c)
		}
	}

	if s := strings.TrimSpace(cur.String()); s != "" {
		out = append(out, s)
	}
	return out
}

func short(sum string) string {
	if len(sum) <= 12 {
		return sum
	}
	return sum[:12]
}

// AppliedVersions reports the applied migration versions, for diagnostics.
func AppliedVersions(ctx context.Context, d *DB) ([]int, error) {
	rows, err := d.QueryContext(ctx, `SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()

	var out []int
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
