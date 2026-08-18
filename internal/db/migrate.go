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
//   - Transactional per file, under BEGIN IMMEDIATE so two machines booting at
//     once (as happens briefly during a deploy) cannot double-apply.
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
		if err := applyMigration(ctx, d, m); err != nil {
			return err
		}
		slog.Info("migration applied", "version", m.Version, "name", m.Name)
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

// applyMigration runs one file inside an immediate transaction.
//
// BEGIN/COMMIT are issued manually on a single pinned connection rather than
// via sql.Tx, because database/sql offers no way to request IMMEDIATE and the
// write lock it takes is the whole point.
func applyMigration(ctx context.Context, d *DB, m migration) error {
	conn, err := d.Conn(ctx)
	if err != nil {
		return fmt.Errorf("db: acquire connection for migration %04d: %w", m.Version, err)
	}
	defer conn.Close()

	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return fmt.Errorf("db: begin migration %04d: %w", m.Version, err)
	}

	rollback := func(cause error) error {
		if _, rbErr := conn.ExecContext(ctx, "ROLLBACK"); rbErr != nil {
			return errors.Join(cause, fmt.Errorf("rollback failed: %w", rbErr))
		}
		return cause
	}

	// Statements are executed one at a time: the libsql HTTP driver does not
	// accept multiple statements per Exec.
	for i, stmt := range splitStatements(m.SQL) {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return rollback(fmt.Errorf(
				"db: migration %04d (%s) statement %d failed: %w\n--- statement ---\n%s",
				m.Version, m.Name, i+1, err, stmt))
		}
	}

	const record = `INSERT INTO schema_migrations (version, name, checksum) VALUES (?, ?, ?)`
	if _, err := conn.ExecContext(ctx, record, m.Version, m.Name, m.Checksum); err != nil {
		return rollback(fmt.Errorf("db: record migration %04d: %w", m.Version, err))
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return rollback(fmt.Errorf("db: commit migration %04d: %w", m.Version, err))
	}
	return nil
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
