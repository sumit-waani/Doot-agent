// Package auth handles the single operator's login and session lifecycle.
//
// There is one user, so this is deliberately small: no roles, no invites, no
// email reset, no 2FA. Recovery is the DOOT_RESET_ADMIN environment variable.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/sumit-waani/doot/internal/db"
	"github.com/sumit-waani/doot/internal/passwd"
)

const (
	// CookieName is the session cookie.
	CookieName = "doot_session"

	// SessionTTL is deliberately long. An installed PWA that logs the operator
	// out every week would be unusable, and there is exactly one user.
	SessionTTL = 90 * 24 * time.Hour

	// touchInterval bounds how often last_seen_at is rewritten, so a burst of
	// requests does not mean a database write each.
	touchInterval = time.Hour

	tokenBytes = 32
)

var (
	ErrInvalidCredentials = errors.New("auth: invalid username or password")
	ErrNoSession          = errors.New("auth: no valid session")
)

// User is the authenticated operator.
type User struct {
	ID       int64
	Username string
}

// Service issues and validates sessions.
type Service struct {
	db     *db.DB
	secure bool
}

// NewService builds a Service. secure controls the cookie's Secure attribute;
// it is disabled only for local HTTP runs.
func NewService(d *db.DB, secure bool) *Service {
	return &Service{db: d, secure: secure}
}

// Login verifies credentials and returns the user.
//
// A missing user and a wrong password both return ErrInvalidCredentials, and
// the password is verified against a dummy hash when the user does not exist so
// that response timing does not reveal whether a username is valid.
func (s *Service) Login(ctx context.Context, username, password string) (User, error) {
	var u User
	var hash string

	const q = `SELECT id, username, password_hash FROM users WHERE username = ?`
	err := s.db.QueryRowContext(ctx, q, username).Scan(&u.ID, &u.Username, &hash)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		_ = passwd.Verify(password, dummyHashValue())
		return User{}, ErrInvalidCredentials
	case err != nil:
		return User{}, fmt.Errorf("auth: look up user: %w", err)
	}

	if err := passwd.Verify(password, hash); err != nil {
		if errors.Is(err, passwd.ErrMismatch) {
			return User{}, ErrInvalidCredentials
		}
		return User{}, fmt.Errorf("auth: verify password: %w", err)
	}

	return u, nil
}

// CreateSession issues a new session and returns the raw token for the cookie.
// Only the token's hash is stored, so a database leak does not hand over live
// sessions.
func (s *Service) CreateSession(ctx context.Context, userID int64, userAgent string) (string, time.Time, error) {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", time.Time{}, fmt.Errorf("auth: generate session token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)
	expires := time.Now().UTC().Add(SessionTTL)

	const q = `
INSERT INTO sessions (token_hash, user_id, user_agent, expires_at)
VALUES (?, ?, ?, ?)`
	if _, err := s.db.ExecContext(ctx, q, hashToken(token), userID, truncate(userAgent, 256), formatTime(expires)); err != nil {
		return "", time.Time{}, fmt.Errorf("auth: create session: %w", err)
	}

	return token, expires, nil
}

// Authenticate resolves a raw token to a user, rejecting expired sessions.
func (s *Service) Authenticate(ctx context.Context, token string) (User, error) {
	if token == "" {
		return User{}, ErrNoSession
	}

	var (
		u            User
		expiresAt    string
		lastSeenAt   string
		tokenHashHex = hashToken(token)
	)

	const q = `
SELECT u.id, u.username, s.expires_at, s.last_seen_at
  FROM sessions s
  JOIN users u ON u.id = s.user_id
 WHERE s.token_hash = ?`

	err := s.db.QueryRowContext(ctx, q, tokenHashHex).Scan(&u.ID, &u.Username, &expiresAt, &lastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNoSession
	}
	if err != nil {
		return User{}, fmt.Errorf("auth: load session: %w", err)
	}

	expiry, err := parseTime(expiresAt)
	if err != nil {
		return User{}, fmt.Errorf("auth: parse session expiry: %w", err)
	}
	if time.Now().UTC().After(expiry) {
		_ = s.DeleteSession(ctx, token)
		return User{}, ErrNoSession
	}

	s.maybeTouch(ctx, tokenHashHex, lastSeenAt)
	return u, nil
}

// maybeTouch refreshes last_seen_at at most once per touchInterval. Failures
// are ignored: this is bookkeeping, and losing it must never fail a request.
func (s *Service) maybeTouch(ctx context.Context, tokenHash, lastSeenAt string) {
	seen, err := parseTime(lastSeenAt)
	if err == nil && time.Since(seen) < touchInterval {
		return
	}
	const q = `UPDATE sessions SET last_seen_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE token_hash = ?`
	_, _ = s.db.ExecContext(ctx, q, tokenHash)
}

// DeleteSession revokes one session.
func (s *Service) DeleteSession(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, hashToken(token)); err != nil {
		return fmt.Errorf("auth: delete session: %w", err)
	}
	return nil
}

// DeleteAllSessions revokes every session, used after a password change so a
// stolen cookie cannot outlive the credential it was issued against.
func (s *Service) DeleteAllSessions(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions`); err != nil {
		return fmt.Errorf("auth: delete all sessions: %w", err)
	}
	return nil
}

// ChangePassword updates the operator's password and revokes all sessions.
func (s *Service) ChangePassword(ctx context.Context, userID int64, currentPassword, newPassword string) error {
	var hash string
	err := s.db.QueryRowContext(ctx, `SELECT password_hash FROM users WHERE id = ?`, userID).Scan(&hash)
	if err != nil {
		return fmt.Errorf("auth: load user for password change: %w", err)
	}

	if err := passwd.Verify(currentPassword, hash); err != nil {
		if errors.Is(err, passwd.ErrMismatch) {
			return ErrInvalidCredentials
		}
		return fmt.Errorf("auth: verify current password: %w", err)
	}

	newHash, err := passwd.Hash(newPassword)
	if err != nil {
		return fmt.Errorf("auth: hash new password: %w", err)
	}

	const q = `
UPDATE users
   SET password_hash = ?,
       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
 WHERE id = ?`
	if _, err := s.db.ExecContext(ctx, q, newHash, userID); err != nil {
		return fmt.Errorf("auth: update password: %w", err)
	}

	return s.DeleteAllSessions(ctx)
}

// ChangeUsername updates the operator's username.
func (s *Service) ChangeUsername(ctx context.Context, userID int64, username string) error {
	const q = `
UPDATE users
   SET username = ?,
       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
 WHERE id = ?`
	if _, err := s.db.ExecContext(ctx, q, username, userID); err != nil {
		return fmt.Errorf("auth: update username: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- cookies

// SetCookie writes the session cookie.
func (s *Service) SetCookie(w http.ResponseWriter, token string, expires time.Time) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		MaxAge:   int(SessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// ClearCookie expires the session cookie.
func (s *Service) ClearCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// TokenFromRequest reads the session token from the request cookie.
func TokenFromRequest(r *http.Request) string {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// ---------------------------------------------------------------- helpers

// dummyHash is a real Argon2id hash of a random value that nothing will match.
// Verifying against it makes a login attempt for a nonexistent user cost the
// same as one for a real user, so response timing does not leak whether a
// username exists.
//
// It is computed rather than hardcoded so it cannot drift out of sync with the
// current Argon2id parameters, and so there is no risk of a malformed literal.
var dummyHashValue = sync.OnceValue(func() string {
	raw := make([]byte, tokenBytes)
	if _, err := rand.Read(raw); err != nil {
		// Only reachable if the system CSPRNG fails, in which case sessions
		// cannot be issued either. A fixed value still costs the right work.
		return mustHash("doot-timing-equalizer")
	}
	return mustHash(base64.RawURLEncoding.EncodeToString(raw))
})

func mustHash(s string) string {
	h, err := passwd.Hash(s)
	if err != nil {
		return ""
	}
	return h
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func formatTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

func parseTime(s string) (time.Time, error) {
	for _, layout := range []string{
		"2006-01-02T15:04:05.000Z",
		"2006-01-02T15:04:05Z",
		time.RFC3339Nano,
		time.RFC3339,
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("auth: unrecognised timestamp %q", s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
