package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/sumit-waani/doot/internal/db"
	"github.com/sumit-waani/doot/internal/llm"
)

// Run statuses. These mirror the CHECK constraint on the runs table.
const (
	StatusRunning          = "running"
	StatusAwaitingApproval = "awaiting_approval"
	StatusAwaitingHuman    = "awaiting_human"
	StatusPaused           = "paused"
	StatusDone             = "done"
	StatusError            = "error"
	StatusInterrupted      = "interrupted"
	StatusCancelled        = "cancelled"
)

// Run kinds.
const (
	KindChat    = "chat"
	KindPlan    = "plan"
	KindExecute = "execute"
)

// ErrRunActive is returned when a run is already in flight.
//
// Enforced by a partial unique index on runs(active) rather than a mutex, so it
// holds across a machine restart.
var ErrRunActive = errors.New("agent: a run is already active")

// ErrNoRun is returned when no active run exists.
var ErrNoRun = errors.New("agent: no active run")

// Run is one execution of the agent loop.
type Run struct {
	ID     int64
	Epoch  int
	Kind   string
	Status string
	Active bool
	Error  string
}

// Terminal reports whether the run has finished for good.
func (r Run) Terminal() bool {
	switch r.Status {
	case StatusDone, StatusError, StatusCancelled:
		return true
	}
	return false
}

// Resumable reports whether the run can be picked back up.
func (r Run) Resumable() bool {
	switch r.Status {
	case StatusPaused, StatusInterrupted, StatusAwaitingHuman, StatusAwaitingApproval:
		return true
	}
	return false
}

// store is the agent's database access. Kept separate from the loop so the loop
// reads as a sequence of decisions rather than SQL.
type store struct {
	db *db.DB
}

// ---------------------------------------------------------------- epochs

// currentEpoch returns the project's live epoch.
func (s *store) currentEpoch(ctx context.Context) (int, error) {
	var epoch int
	err := s.db.QueryRowContext(ctx, `SELECT current_epoch FROM project WHERE id = 1`).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, errNoProject
	}
	if err != nil {
		return 0, fmt.Errorf("agent: read current epoch: %w", err)
	}
	return epoch, nil
}

// rollEpoch closes the current epoch and opens a new one.
//
// This is the single mechanism behind both "clear conversation" and compaction.
// Messages are never deleted: only the epoch pointer moves, so history stays
// queryable while the live context query stays trivial.
func (s *store) rollEpoch(ctx context.Context, reason, summary string) (newEpoch int, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("agent: begin epoch roll: %w", err)
	}
	defer tx.Rollback()

	var current int
	if err := tx.QueryRowContext(ctx, `SELECT current_epoch FROM project WHERE id = 1`).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, errNoProject
		}
		return 0, fmt.Errorf("agent: read epoch for roll: %w", err)
	}

	// Record what the closed epoch contained, so the history list is readable
	// without re-counting messages later.
	const closeQ = `
UPDATE conversation_epochs
   SET reason = ?,
       summary = ?,
       ended_at = strftime('%Y-%m-%dT%H:%M:%fZ','now'),
       message_count = (SELECT COUNT(*) FROM messages WHERE epoch = ?),
       total_tokens  = (SELECT COALESCE(SUM(COALESCE(prompt_tokens,0) + COALESCE(completion_tokens,0)), 0)
                          FROM messages WHERE epoch = ?)
 WHERE epoch = ?`
	if _, err := tx.ExecContext(ctx, closeQ, reason, nullableString(summary), current, current, current); err != nil {
		return 0, fmt.Errorf("agent: close epoch %d: %w", current, err)
	}

	var next int
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(epoch), 0) + 1 FROM conversation_epochs`).Scan(&next); err != nil {
		return 0, fmt.Errorf("agent: allocate epoch: %w", err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO conversation_epochs (epoch) VALUES (?)`, next); err != nil {
		return 0, fmt.Errorf("agent: create epoch %d: %w", next, err)
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE project SET current_epoch = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = 1`,
		next); err != nil {
		return 0, fmt.Errorf("agent: point project at epoch %d: %w", next, err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("agent: commit epoch roll: %w", err)
	}
	return next, nil
}

// ---------------------------------------------------------------- messages

// storedMessage is a message row plus its identity.
type storedMessage struct {
	ID  int64
	Seq int
	llm.Message
	IsSummary bool
}

// appendMessage writes one message, allocating its sequence number.
//
// The sequence is allocated inside the INSERT so it is atomic: a subquery in a
// single statement cannot interleave with another writer the way a
// read-then-write would.
func (s *store) appendMessage(ctx context.Context, epoch int, runID *int64, m llm.Message, isSummary bool) (int64, error) {
	var toolCallsJSON any
	if len(m.ToolCalls) > 0 {
		encoded, err := json.Marshal(m.ToolCalls)
		if err != nil {
			return 0, fmt.Errorf("agent: encode tool calls: %w", err)
		}
		toolCallsJSON = string(encoded)
	}

	const q = `
INSERT INTO messages (epoch, seq, role, content, tool_calls, tool_call_id, name, run_id, is_summary)
VALUES (?,
        (SELECT COALESCE(MAX(seq), 0) + 1 FROM messages WHERE epoch = ?),
        ?, ?, ?, ?, ?, ?, ?)
RETURNING id`

	var id int64
	err := s.db.QueryRowContext(ctx, q,
		epoch, epoch,
		m.Role,
		nullableString(m.Content),
		toolCallsJSON,
		nullableString(m.ToolCallID),
		nullableString(m.Name),
		runID,
		boolToInt(isSummary),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("agent: append %s message: %w", m.Role, err)
	}
	return id, nil
}

// setMessageTokens records what a turn cost, for the epoch rollup.
func (s *store) setMessageTokens(ctx context.Context, id int64, prompt, completion int64) error {
	const q = `UPDATE messages SET prompt_tokens = ?, completion_tokens = ? WHERE id = ?`
	if _, err := s.db.ExecContext(ctx, q, prompt, completion, id); err != nil {
		return fmt.Errorf("agent: record message tokens: %w", err)
	}
	return nil
}

// loadContext returns the live conversation for the model.
//
// Only the current epoch: earlier epochs are retained on disk but are not part
// of live context. That is the whole point of the epoch model.
func (s *store) loadContext(ctx context.Context, epoch int) ([]llm.Message, error) {
	const q = `
SELECT role, COALESCE(content,''), COALESCE(tool_calls,''), COALESCE(tool_call_id,''), COALESCE(name,'')
  FROM messages
 WHERE epoch = ?
 ORDER BY seq`

	rows, err := s.db.QueryContext(ctx, q, epoch)
	if err != nil {
		return nil, fmt.Errorf("agent: load context: %w", err)
	}
	defer rows.Close()

	var out []llm.Message
	for rows.Next() {
		var (
			m             llm.Message
			toolCallsJSON string
		)
		if err := rows.Scan(&m.Role, &m.Content, &toolCallsJSON, &m.ToolCallID, &m.Name); err != nil {
			return nil, fmt.Errorf("agent: scan context message: %w", err)
		}
		if toolCallsJSON != "" {
			if err := json.Unmarshal([]byte(toolCallsJSON), &m.ToolCalls); err != nil {
				return nil, fmt.Errorf("agent: decode tool calls: %w", err)
			}
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// loadTranscript returns every message in an epoch, for summarisation.
func (s *store) loadTranscript(ctx context.Context, epoch int) ([]storedMessage, error) {
	const q = `
SELECT id, seq, role, COALESCE(content,''), COALESCE(tool_calls,''),
       COALESCE(tool_call_id,''), COALESCE(name,''), is_summary
  FROM messages
 WHERE epoch = ?
 ORDER BY seq`

	rows, err := s.db.QueryContext(ctx, q, epoch)
	if err != nil {
		return nil, fmt.Errorf("agent: load transcript: %w", err)
	}
	defer rows.Close()

	var out []storedMessage
	for rows.Next() {
		var (
			sm            storedMessage
			toolCallsJSON string
			isSummary     int
		)
		if err := rows.Scan(&sm.ID, &sm.Seq, &sm.Role, &sm.Content, &toolCallsJSON,
			&sm.ToolCallID, &sm.Name, &isSummary); err != nil {
			return nil, fmt.Errorf("agent: scan transcript message: %w", err)
		}
		if toolCallsJSON != "" {
			_ = json.Unmarshal([]byte(toolCallsJSON), &sm.ToolCalls)
		}
		sm.IsSummary = isSummary == 1
		out = append(out, sm)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- runs

// startRun creates an active run.
//
// A conflict on the partial unique index means another run is already active,
// which is reported as ErrRunActive rather than a raw SQL error.
func (s *store) startRun(ctx context.Context, epoch int, kind string, triggerMessageID *int64) (int64, error) {
	const q = `
INSERT INTO runs (epoch, kind, status, active, trigger_message_id)
VALUES (?, ?, 'running', 1, ?)
RETURNING id`

	var id int64
	err := s.db.QueryRowContext(ctx, q, epoch, kind, triggerMessageID).Scan(&id)
	if err != nil {
		if isUniqueViolation(err) {
			return 0, ErrRunActive
		}
		return 0, fmt.Errorf("agent: start run: %w", err)
	}
	return id, nil
}

// activeRun returns the in-flight run, if any.
func (s *store) activeRun(ctx context.Context) (Run, error) {
	const q = `
SELECT id, epoch, kind, status, active, COALESCE(error,'')
  FROM runs
 WHERE active = 1
 LIMIT 1`

	var (
		r      Run
		active int
	)
	err := s.db.QueryRowContext(ctx, q).Scan(&r.ID, &r.Epoch, &r.Kind, &r.Status, &active, &r.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNoRun
	}
	if err != nil {
		return Run{}, fmt.Errorf("agent: read active run: %w", err)
	}
	r.Active = active == 1
	return r, nil
}

// getRun loads a run by id.
func (s *store) getRun(ctx context.Context, id int64) (Run, error) {
	const q = `
SELECT id, epoch, kind, status, active, COALESCE(error,'')
  FROM runs
 WHERE id = ?`

	var (
		r      Run
		active int
	)
	err := s.db.QueryRowContext(ctx, q, id).Scan(&r.ID, &r.Epoch, &r.Kind, &r.Status, &active, &r.Error)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, ErrNoRun
	}
	if err != nil {
		return Run{}, fmt.Errorf("agent: read run %d: %w", id, err)
	}
	r.Active = active == 1
	return r, nil
}

// parkRun moves a run to a waiting state while keeping it active.
//
// Awaiting approval or a human answer is still "the run holding the slot": it
// has not finished, and no other run may start in the meantime.
func (s *store) parkRun(ctx context.Context, id int64, status string) error {
	const q = `UPDATE runs SET status = ? WHERE id = ?`
	if _, err := s.db.ExecContext(ctx, q, status, id); err != nil {
		return fmt.Errorf("agent: park run %d as %s: %w", id, status, err)
	}
	return nil
}

// resumeRun puts a parked run back into the running state.
func (s *store) resumeRun(ctx context.Context, id int64) error {
	const q = `UPDATE runs SET status = 'running', active = 1, ended_at = NULL WHERE id = ?`
	if _, err := s.db.ExecContext(ctx, q, id); err != nil {
		if isUniqueViolation(err) {
			return ErrRunActive
		}
		return fmt.Errorf("agent: resume run %d: %w", id, err)
	}
	return nil
}

// finishRun ends a run and frees the active slot.
func (s *store) finishRun(ctx context.Context, id int64, status string, runErr error) error {
	var errText any
	if runErr != nil {
		errText = runErr.Error()
	}

	const q = `
UPDATE runs
   SET status = ?, active = 0, error = ?,
       ended_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
 WHERE id = ?`
	if _, err := s.db.ExecContext(ctx, q, status, errText, id); err != nil {
		return fmt.Errorf("agent: finish run %d: %w", id, err)
	}
	return nil
}

// setRunEpoch moves a run onto a new epoch, after compaction rolled it.
func (s *store) setRunEpoch(ctx context.Context, id int64, epoch int) error {
	const q = `UPDATE runs SET epoch = ? WHERE id = ?`
	if _, err := s.db.ExecContext(ctx, q, epoch, id); err != nil {
		return fmt.Errorf("agent: move run %d to epoch %d: %w", id, epoch, err)
	}
	return nil
}

// touchRun records liveness, which is what lets a restart tell a genuinely
// running run apart from one whose process died.
func (s *store) touchRun(ctx context.Context, id int64) error {
	const q = `UPDATE runs SET heartbeat_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = ?`
	if _, err := s.db.ExecContext(ctx, q, id); err != nil {
		return fmt.Errorf("agent: touch run %d: %w", id, err)
	}
	return nil
}

// ---------------------------------------------------------------- tool calls

func (s *store) recordToolCall(ctx context.Context, runID int64, messageID *int64, call llm.ToolCall) (int64, error) {
	const q = `
INSERT INTO tool_calls (run_id, message_id, tool_call_id, name, args, status)
VALUES (?, ?, ?, ?, ?, 'running')
RETURNING id`

	var id int64
	err := s.db.QueryRowContext(ctx, q, runID, messageID, call.ID, call.Name,
		nullableString(call.Arguments)).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("agent: record tool call %s: %w", call.Name, err)
	}
	return id, nil
}

func (s *store) finishToolCall(ctx context.Context, id int64, status, preview string, durationMS int, toolErr error) error {
	var errText any
	if toolErr != nil {
		errText = toolErr.Error()
	}

	const q = `
UPDATE tool_calls
   SET status = ?, result_preview = ?, error = ?, duration_ms = ?
 WHERE id = ?`
	if _, err := s.db.ExecContext(ctx, q, status, nullableString(preview), errText, durationMS, id); err != nil {
		return fmt.Errorf("agent: finish tool call %d: %w", id, err)
	}
	return nil
}

// ---------------------------------------------------------------- llm audit

func (s *store) recordLLMCall(ctx context.Context, runID *int64, epoch int, rec llm.AuditRecord) error {
	var errText any
	if rec.Err != nil {
		errText = rec.Err.Error()
	}

	const q = `
INSERT INTO llm_calls (run_id, epoch, purpose, model, prompt_tokens, cached_prompt_tokens,
                       completion_tokens, cost_usd, latency_ms, finish_reason, error)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	_, err := s.db.ExecContext(ctx, q,
		runID, epoch, string(rec.Purpose), rec.Model,
		rec.Usage.PromptTokens, rec.Usage.CachedPromptTokens, rec.Usage.CompletionTokens,
		rec.Usage.CostUSD, rec.LatencyMS, nullableString(rec.FinishReason), errText,
	)
	if err != nil {
		return fmt.Errorf("agent: record llm call: %w", err)
	}
	return nil
}

// sessionCost totals spend and tokens for an epoch, for the chat header.
func (s *store) sessionCost(ctx context.Context, epoch int) (costUSD float64, err error) {
	const q = `SELECT COALESCE(SUM(cost_usd), 0) FROM llm_calls WHERE epoch = ?`
	if err := s.db.QueryRowContext(ctx, q, epoch).Scan(&costUSD); err != nil {
		return 0, fmt.Errorf("agent: read session cost: %w", err)
	}
	return costUSD, nil
}

// ---------------------------------------------------------------- helpers

func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "unique constraint") || strings.Contains(msg, "unique")
}
