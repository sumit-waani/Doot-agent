// Package events is the durable event log behind SSE.
//
// Every event is persisted before it is delivered, and the row's primary key is
// used as the SSE event id. That is what makes the stream trustworthy on a
// phone: mobile browsers kill background connections, so on reconnect the
// client sends Last-Event-ID and the server replays from the log. An in-memory
// ring buffer could not do this, and would also be wiped by a Fly restart.
package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/sumit-waani/doot/internal/db"
)

// Event types. Payloads are pre-rendered HTML fragments where they target the
// DOM, so htmx can swap them directly with no client-side rendering layer.
const (
	TypeRunStatus     = "run.status"
	TypeMessageAppend = "message.append"
	TypeMessageDelta  = "message.delta"
	TypeToolStart     = "tool.start"
	TypeToolEnd       = "tool.end"
	TypeDiff          = "diff"
	TypePlanProposed  = "plan.proposed"
	TypePlanUpdated   = "plan.updated"
	TypeTaskUpdated   = "task.updated"
	TypeScreenshot    = "screenshot"
	TypeSandboxState  = "sandbox.state"
	TypeUsage         = "usage"
	TypeEpochChanged  = "epoch.changed"

	// TypeReload tells the client to re-fetch instead of replaying, used when
	// the requested Last-Event-ID is older than the retained log.
	TypeReload = "reload"
)

// Event is one row of the log.
type Event struct {
	ID        int64  `json:"id"`
	RunID     *int64 `json:"run_id,omitempty"`
	Type      string `json:"type"`
	Payload   string `json:"payload"`
	CreatedAt string `json:"created_at"`
}

// Log persists and replays events.
type Log struct {
	db     *db.DB
	broker *Broker
}

// NewLog builds a Log with its own broker.
func NewLog(d *db.DB) *Log {
	return &Log{db: d, broker: NewBroker()}
}

// Broker exposes the fanout side, for handlers that subscribe.
func (l *Log) Broker() *Broker { return l.broker }

// Append persists an event and then publishes it to live subscribers.
//
// The order matters: persisting first means a client that reconnects
// immediately after cannot miss an event that was delivered but never logged.
func (l *Log) Append(ctx context.Context, runID *int64, eventType, payload string) (Event, error) {
	const q = `INSERT INTO events (run_id, type, payload) VALUES (?, ?, ?) RETURNING id, created_at`

	var ev = Event{RunID: runID, Type: eventType, Payload: payload}
	err := l.db.QueryRowContext(ctx, q, runID, eventType, payload).Scan(&ev.ID, &ev.CreatedAt)
	if err != nil {
		return Event{}, fmt.Errorf("events: append %q: %w", eventType, err)
	}

	l.broker.Publish(ev)
	return ev, nil
}

// AppendJSON marshals payload before appending, for events that carry data
// rather than a rendered fragment.
func (l *Log) AppendJSON(ctx context.Context, runID *int64, eventType string, payload any) (Event, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return Event{}, fmt.Errorf("events: marshal %q payload: %w", eventType, err)
	}
	return l.Append(ctx, runID, eventType, string(encoded))
}

// Since returns events with an id greater than afterID, oldest first, for
// replay after a reconnect.
func (l *Log) Since(ctx context.Context, afterID int64, limit int) ([]Event, error) {
	const q = `
SELECT id, run_id, type, payload, created_at
  FROM events
 WHERE id > ?
 ORDER BY id
 LIMIT ?`

	rows, err := l.db.QueryContext(ctx, q, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("events: replay after %d: %w", afterID, err)
	}
	defer rows.Close()

	var out []Event
	for rows.Next() {
		var (
			ev    Event
			runID sql.NullInt64
		)
		if err := rows.Scan(&ev.ID, &runID, &ev.Type, &ev.Payload, &ev.CreatedAt); err != nil {
			return nil, fmt.Errorf("events: scan event: %w", err)
		}
		if runID.Valid {
			id := runID.Int64
			ev.RunID = &id
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// LatestID returns the highest event id, or 0 when the log is empty. A client
// connecting fresh starts here so it does not replay the entire log.
func (l *Log) LatestID(ctx context.Context) (int64, error) {
	var id sql.NullInt64
	if err := l.db.QueryRowContext(ctx, `SELECT MAX(id) FROM events`).Scan(&id); err != nil {
		return 0, fmt.Errorf("events: latest id: %w", err)
	}
	if !id.Valid {
		return 0, nil
	}
	return id.Int64, nil
}

// OldestID returns the lowest retained event id, or 0 when the log is empty.
// Used to detect a Last-Event-ID that predates pruning, where replay would
// silently skip events and a reload is the honest response.
func (l *Log) OldestID(ctx context.Context) (int64, error) {
	var id sql.NullInt64
	if err := l.db.QueryRowContext(ctx, `SELECT MIN(id) FROM events`).Scan(&id); err != nil {
		return 0, fmt.Errorf("events: oldest id: %w", err)
	}
	if !id.Valid {
		return 0, nil
	}
	return id.Int64, nil
}

// CanReplay reports whether every event after afterID is still retained.
func (l *Log) CanReplay(ctx context.Context, afterID int64) (bool, error) {
	if afterID <= 0 {
		return true, nil
	}
	oldest, err := l.OldestID(ctx)
	if err != nil {
		return false, err
	}
	if oldest == 0 {
		return true, nil // empty log, nothing was missed
	}
	// Replay is complete only if the next event after afterID is still present,
	// i.e. pruning has not removed anything in the gap.
	return afterID+1 >= oldest, nil
}

// ---------------------------------------------------------------- broker

// Broker fans out live events to connected SSE clients.
type Broker struct {
	mu     sync.RWMutex
	nextID int64
	subs   map[int64]chan Event
}

// NewBroker builds an empty Broker.
func NewBroker() *Broker {
	return &Broker{subs: map[int64]chan Event{}}
}

// Subscribe registers a subscriber and returns its channel plus a cancel func.
func (b *Broker) Subscribe() (<-chan Event, func()) {
	// Buffered so a briefly-slow client does not block the agent loop.
	ch := make(chan Event, 64)

	b.mu.Lock()
	b.nextID++
	id := b.nextID
	b.subs[id] = ch
	b.mu.Unlock()

	return ch, func() {
		b.mu.Lock()
		if existing, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(existing)
		}
		b.mu.Unlock()
	}
}

// Publish delivers an event to all subscribers.
//
// Delivery is non-blocking: a subscriber whose buffer is full is skipped rather
// than stalling the publisher. That is safe because the event is already in the
// database, so the client recovers it on reconnect via Last-Event-ID.
func (b *Broker) Publish(ev Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()

	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// Subscribers reports the current subscriber count, for diagnostics.
func (b *Broker) Subscribers() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}
