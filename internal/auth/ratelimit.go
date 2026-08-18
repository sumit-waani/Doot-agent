package auth

import (
	"sync"
	"time"
)

// LoginLimiter throttles failed login attempts.
//
// In-memory is the right scope here: there is one machine, and a limiter that
// survives restarts would only matter if an attacker could also restart the
// process. Successful logins clear the counter, so the operator's own typos
// never accumulate into a lockout.
type LoginLimiter struct {
	mu       sync.Mutex
	attempts map[string]*attemptRecord

	max    int
	window time.Duration
}

type attemptRecord struct {
	count       int
	windowStart time.Time
}

// NewLoginLimiter allows max failures per key within window.
func NewLoginLimiter(max int, window time.Duration) *LoginLimiter {
	return &LoginLimiter{
		attempts: map[string]*attemptRecord{},
		max:      max,
		window:   window,
	}
}

// Allow reports whether another attempt may be made for key, and how long to
// wait if not.
func (l *LoginLimiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	rec, ok := l.attempts[key]
	if !ok {
		return true, 0
	}

	if now.Sub(rec.windowStart) >= l.window {
		delete(l.attempts, key)
		return true, 0
	}

	if rec.count >= l.max {
		return false, l.window - now.Sub(rec.windowStart)
	}
	return true, 0
}

// RecordFailure counts a failed attempt.
func (l *LoginLimiter) RecordFailure(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	rec, ok := l.attempts[key]
	if !ok || now.Sub(rec.windowStart) >= l.window {
		l.attempts[key] = &attemptRecord{count: 1, windowStart: now}
		return
	}
	rec.count++
}

// Reset clears the counter for key, called on a successful login.
func (l *LoginLimiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, key)
}

// Sweep drops stale records so the map cannot grow without bound.
func (l *LoginLimiter) Sweep() {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	for k, rec := range l.attempts {
		if now.Sub(rec.windowStart) >= l.window {
			delete(l.attempts, k)
		}
	}
}
