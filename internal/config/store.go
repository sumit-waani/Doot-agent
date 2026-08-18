package config

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"

	"github.com/sumit-waani/doot/internal/db"
	"github.com/sumit-waani/doot/internal/secretbox"
)

// Settings keys. Constants rather than bare strings so a typo is a compile
// error instead of a silently-missing value.
const (
	KeyLLMModel           = "llm.model"
	KeyLLMBaseURL         = "llm.base_url"
	KeyLLMContextWindow   = "llm.context_window"
	KeyLLMMaxOutputTokens = "llm.max_output_tokens"

	KeyCompactThresholdPct = "agent.compact_threshold_pct"
	KeySystemPrompt        = "agent.system_prompt"
	KeyReviewerEnabled     = "agent.reviewer_enabled"
	KeyE2EEnabled          = "agent.e2e_enabled"

	KeySandboxSnapshot    = "sandbox.snapshot"
	KeySandboxAutoStopMin = "sandbox.auto_stop_minutes"
	KeySandboxVNCRes      = "sandbox.vnc_resolution"

	KeyGitWorkBranch  = "git.work_branch"
	KeyGitAuthorName  = "git.author_name"
	KeyGitAuthorEmail = "git.author_email"

	KeyGitHubUsername = "github.username"
	KeyGitHubCreatePR = "github.create_pr"

	KeyPricingInputPerMtok       = "pricing.input_per_mtok"
	KeyPricingCachedInputPerMtok = "pricing.cached_input_per_mtok"
	KeyPricingOutputPerMtok      = "pricing.output_per_mtok"
)

// ErrNoSecret is returned when a secret has no stored value.
var ErrNoSecret = errors.New("config: secret is not set")

// Store reads and writes settings and secrets.
//
// Settings are cached in memory. There is exactly one machine and one operator,
// so the cache cannot go stale behind our back, and it keeps page rendering
// from costing a round trip to Turso per lookup.
type Store struct {
	db  *db.DB
	box *secretbox.Box

	mu    sync.RWMutex
	cache map[string]string
}

// NewStore builds a Store and loads the settings cache.
func NewStore(ctx context.Context, d *db.DB, box *secretbox.Box) (*Store, error) {
	s := &Store{db: d, box: box, cache: map[string]string{}}
	if err := s.Reload(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

// Reload repopulates the settings cache from the database.
func (s *Store) Reload(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return fmt.Errorf("config: load settings: %w", err)
	}
	defer rows.Close()

	fresh := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return fmt.Errorf("config: scan setting: %w", err)
		}
		fresh[k] = v
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("config: iterate settings: %w", err)
	}

	s.mu.Lock()
	s.cache = fresh
	s.mu.Unlock()
	return nil
}

// ---------------------------------------------------------------- settings

// Get returns a setting, or "" if absent.
func (s *Store) Get(key string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cache[key]
}

// GetOr returns a setting, falling back to def when unset or empty.
func (s *Store) GetOr(key, def string) string {
	if v := s.Get(key); v != "" {
		return v
	}
	return def
}

// Int returns a setting parsed as an int, or def if absent or unparseable.
func (s *Store) Int(key string, def int) int {
	v := s.Get(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		slog.Warn("setting is not an integer; using default", "key", key, "value", v, "default", def)
		return def
	}
	return n
}

// Float returns a setting parsed as a float64, or def if absent or unparseable.
func (s *Store) Float(key string, def float64) float64 {
	v := s.Get(key)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		slog.Warn("setting is not a number; using default", "key", key, "value", v, "default", def)
		return def
	}
	return f
}

// Bool returns a setting parsed as a boolean. Values are stored as "1"/"0".
func (s *Store) Bool(key string, def bool) bool {
	switch strings.ToLower(strings.TrimSpace(s.Get(key))) {
	case "":
		return def
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return def
	}
}

// Set writes one setting and updates the cache.
func (s *Store) Set(ctx context.Context, key, value string) error {
	return s.SetMany(ctx, map[string]string{key: value})
}

// SetMany writes several settings. The Settings screen saves a whole section at
// a time, and a partially-applied section would be worse than a failed save.
func (s *Store) SetMany(ctx context.Context, values map[string]string) error {
	if len(values) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("config: begin settings write: %w", err)
	}
	defer tx.Rollback()

	const q = `
INSERT INTO settings (key, value, updated_at)
VALUES (?, ?, strftime('%Y-%m-%dT%H:%M:%fZ','now'))
ON CONFLICT(key) DO UPDATE SET
  value = excluded.value,
  updated_at = excluded.updated_at`

	for k, v := range values {
		if _, err := tx.ExecContext(ctx, q, k, v); err != nil {
			return fmt.Errorf("config: write setting %q: %w", k, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("config: commit settings: %w", err)
	}

	s.mu.Lock()
	for k, v := range values {
		s.cache[k] = v
	}
	s.mu.Unlock()

	return nil
}

// All returns a copy of every setting, for rendering the Settings screen.
func (s *Store) All() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string]string, len(s.cache))
	for k, v := range s.cache {
		out[k] = v
	}
	return out
}

// ---------------------------------------------------------------- secrets

// Secret decrypts and returns a stored credential.
// Secrets are never cached, so a rotated value takes effect immediately and
// plaintext credentials do not sit in memory between uses.
func (s *Store) Secret(ctx context.Context, name string) (string, error) {
	var ciphertext, nonce []byte
	const q = `SELECT ciphertext, nonce FROM secrets WHERE name = ?`

	err := s.db.QueryRowContext(ctx, q, name).Scan(&ciphertext, &nonce)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: %s", ErrNoSecret, name)
	}
	if err != nil {
		return "", fmt.Errorf("config: read secret %q: %w", name, err)
	}

	plaintext, err := s.box.Open(name, ciphertext, nonce)
	if err != nil {
		return "", fmt.Errorf("config: open secret %q: %w", name, err)
	}
	return plaintext, nil
}

// SetSecret encrypts and stores a credential.
func (s *Store) SetSecret(ctx context.Context, name, value string) error {
	ciphertext, nonce, err := s.box.Seal(name, value)
	if err != nil {
		return fmt.Errorf("config: seal secret %q: %w", name, err)
	}

	const q = `
INSERT INTO secrets (name, ciphertext, nonce, updated_at)
VALUES (?, ?, ?, strftime('%Y-%m-%dT%H:%M:%fZ','now'))
ON CONFLICT(name) DO UPDATE SET
  ciphertext = excluded.ciphertext,
  nonce      = excluded.nonce,
  updated_at = excluded.updated_at`

	if _, err := s.db.ExecContext(ctx, q, name, ciphertext, nonce); err != nil {
		return fmt.Errorf("config: write secret %q: %w", name, err)
	}
	return nil
}

// DeleteSecret removes a credential. Clearing a secret is an explicit action;
// submitting a blank field on the Settings screen must not reach this.
func (s *Store) DeleteSecret(ctx context.Context, name string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM secrets WHERE name = ?`, name); err != nil {
		return fmt.Errorf("config: delete secret %q: %w", name, err)
	}
	return nil
}

// SecretsPresent reports which secrets have a stored value, without decrypting
// any of them. This is what the Settings screen renders: whether a credential
// is set, never the credential itself.
func (s *Store) SecretsPresent(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name FROM secrets`)
	if err != nil {
		return nil, fmt.Errorf("config: list secrets: %w", err)
	}
	defer rows.Close()

	out := make(map[string]bool, len(AllSecretNames))
	for _, n := range AllSecretNames {
		out[n] = false
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("config: scan secret name: %w", err)
		}
		out[n] = true
	}
	return out, rows.Err()
}

// SeedSecretsFromEnv stores any seed value whose secret is not yet present.
//
// Existing secrets are left alone: once a credential has been set (or rotated
// from the UI) a stale environment variable must not overwrite it.
func (s *Store) SeedSecretsFromEnv(ctx context.Context, seeds map[string]string) error {
	if len(seeds) == 0 {
		return nil
	}

	present, err := s.SecretsPresent(ctx)
	if err != nil {
		return err
	}

	for name, value := range seeds {
		if present[name] {
			continue
		}
		if err := s.SetSecret(ctx, name, value); err != nil {
			return err
		}
		slog.Info("seeded secret from environment", "name", name)
	}
	return nil
}
