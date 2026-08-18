// Package db owns the database connection and schema migrations.
//
// All durable state lives in Turso. The machine running Doot is treated as
// disposable and holds nothing of value, so there is no local-disk fallback in
// production. A local file database is supported purely so the binary can be
// run and exercised without a Turso account.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	_ "github.com/tursodatabase/libsql-client-go/libsql" // driver: libsql
	_ "modernc.org/sqlite"                               // driver: sqlite
)

// Kind identifies which backend a connection is talking to.
type Kind string

const (
	KindTurso Kind = "turso"
	KindLocal Kind = "local"
)

// DB wraps *sql.DB with the backend kind, which a few operations need to
// branch on (pragmas and locking behaviour differ between the two).
type DB struct {
	*sql.DB
	Kind Kind
}

// remoteSchemes are the URL schemes handled by the libsql driver.
var remoteSchemes = map[string]bool{
	"libsql": true,
	"wss":    true,
	"ws":     true,
	"https":  true,
	"http":   true,
}

// Open connects to the database identified by dsn.
//
// The driver is chosen by URL scheme rather than by configuration, so there is
// no way to point the local driver at a remote URL or vice versa:
//
//	libsql:// wss:// ws:// https:// http://  -> libsql   (Turso)
//	anything else                            -> sqlite   (local file)
//
// authToken is appended to remote URLs when the DSN does not already carry one.
func Open(ctx context.Context, dsn, authToken string) (*DB, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("db: empty DSN (set TURSO_DATABASE_URL)")
	}

	driver, resolved, kind, err := resolveDSN(dsn, authToken)
	if err != nil {
		return nil, err
	}

	sqlDB, err := sql.Open(driver, resolved)
	if err != nil {
		return nil, fmt.Errorf("db: open (%s): %w", driver, err)
	}

	tunePool(sqlDB, kind)

	pingCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := sqlDB.PingContext(pingCtx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("db: ping (%s): %w", driver, err)
	}

	d := &DB{DB: sqlDB, Kind: kind}
	if err := d.applyPragmas(ctx); err != nil {
		sqlDB.Close()
		return nil, err
	}

	slog.Info("database connected", "backend", kind, "driver", driver)
	return d, nil
}

// resolveDSN picks the driver and normalises the DSN for it.
func resolveDSN(dsn, authToken string) (driver, resolved string, kind Kind, err error) {
	u, parseErr := url.Parse(dsn)
	if parseErr == nil && remoteSchemes[u.Scheme] {
		if authToken != "" && u.Query().Get("authToken") == "" {
			q := u.Query()
			q.Set("authToken", authToken)
			u.RawQuery = q.Encode()
		}
		return "libsql", u.String(), KindTurso, nil
	}

	// Local file. Strip a file: prefix if present, then attach the pragmas
	// modernc/sqlite needs as DSN parameters (it has no separate pragma API).
	path := strings.TrimPrefix(dsn, "file:")
	if path == "" {
		return "", "", "", errors.New("db: empty local database path")
	}
	params := []string{
		"_pragma=foreign_keys(1)",
		"_pragma=busy_timeout(5000)",
		"_pragma=journal_mode(WAL)",
	}
	return "sqlite", path + "?" + strings.Join(params, "&"), KindLocal, nil
}

// tunePool sizes the connection pool for the backend.
func tunePool(sqlDB *sql.DB, kind Kind) {
	switch kind {
	case KindLocal:
		// WAL allows concurrent readers with a single writer. Keep the pool
		// small; this path exists for local runs, not for load.
		sqlDB.SetMaxOpenConns(4)
		sqlDB.SetMaxIdleConns(2)
	default:
		sqlDB.SetMaxOpenConns(8)
		sqlDB.SetMaxIdleConns(4)
	}
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)
}

// applyPragmas enables foreign key enforcement.
//
// For local files the pragma is set per connection via the DSN, since a pragma
// executed on one pooled connection would not apply to the others. Remote
// libSQL enables foreign keys server-side and rejects the pragma over HTTP, so
// a failure there is logged rather than fatal.
func (d *DB) applyPragmas(ctx context.Context) error {
	if d.Kind == KindLocal {
		var on int
		if err := d.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&on); err != nil {
			return fmt.Errorf("db: read foreign_keys pragma: %w", err)
		}
		if on != 1 {
			return errors.New("db: foreign_keys pragma did not take effect")
		}
		return nil
	}

	if _, err := d.ExecContext(ctx, "PRAGMA foreign_keys = ON"); err != nil {
		slog.Debug("foreign_keys pragma not settable on this backend; relying on server default", "err", err)
	}
	return nil
}

// Now returns the current UTC time in the format used by every timestamp
// column, so Go-side writes and SQL DEFAULTs produce identical strings.
func Now() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
}
