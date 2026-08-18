package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// Goal plan statuses, mirroring the CHECK constraint on goal_plans.
const (
	PlanAwaitingApproval = "awaiting_approval"
	PlanApproved         = "approved"
	PlanRejected         = "rejected"
	PlanInProgress       = "in_progress"
	PlanCompleted        = "completed"
	PlanAbandoned        = "abandoned"
)

// Task statuses.
const (
	TaskPending    = "pending"
	TaskInProgress = "in_progress"
	TaskReview     = "review"
	TaskDone       = "done"
	TaskSkipped    = "skipped"
	TaskFailed     = "failed"
)

// Review verdicts. "dismissed" records that the primary agent judged a reviewer
// finding to be a false positive — worth keeping, since that judgement is
// exactly what is worth auditing later.
const (
	VerdictClean     = "clean"
	VerdictIssues    = "issues"
	VerdictDismissed = "dismissed"
)

// ErrNoPlan is returned when no plan is awaiting or in progress.
var ErrNoPlan = errors.New("agent: no goal plan")

// Plan is a structured goal plan.
type Plan struct {
	ID           int64
	Epoch        int
	RunID        *int64
	Title        string
	Goal         string
	Deliverables []string
	Status       string
	Tasks        []Task
}

// Task is one phase or subtask of a plan.
type Task struct {
	ID            int64
	PlanID        int64
	Seq           int
	Title         string
	Detail        string
	Status        string
	ReviewVerdict string
	ReviewNotes   string
	CommitSHA     string
}

// PendingTask returns the first task not yet finished, or nil when the plan is
// complete. This is how the loop knows what to work on next after a restart,
// without keeping progress in memory.
func (p Plan) PendingTask() *Task {
	for i := range p.Tasks {
		switch p.Tasks[i].Status {
		case TaskDone, TaskSkipped:
			continue
		default:
			return &p.Tasks[i]
		}
	}
	return nil
}

// Progress reports how many tasks are finished.
func (p Plan) Progress() (done, total int) {
	for _, t := range p.Tasks {
		if t.Status == TaskDone || t.Status == TaskSkipped {
			done++
		}
	}
	return done, len(p.Tasks)
}

// createPlan stores a plan and its tasks, awaiting approval.
//
// Plan and tasks are written in one transaction: a plan with half its tasks
// would be presented for approval as if it were complete.
func (s *store) createPlan(ctx context.Context, epoch int, runID *int64, p Plan, raw string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("agent: begin plan create: %w", err)
	}
	defer tx.Rollback()

	var deliverablesJSON any
	if len(p.Deliverables) > 0 {
		encoded, err := json.Marshal(p.Deliverables)
		if err != nil {
			return 0, fmt.Errorf("agent: encode deliverables: %w", err)
		}
		deliverablesJSON = string(encoded)
	}

	const planQ = `
INSERT INTO goal_plans (epoch, run_id, title, goal, deliverables, raw, status)
VALUES (?, ?, ?, ?, ?, ?, 'awaiting_approval')
RETURNING id`

	var planID int64
	if err := tx.QueryRowContext(ctx, planQ,
		epoch, runID, p.Title, p.Goal, deliverablesJSON, nullableString(raw),
	).Scan(&planID); err != nil {
		return 0, fmt.Errorf("agent: create plan: %w", err)
	}

	const taskQ = `INSERT INTO plan_tasks (plan_id, seq, title, detail, status) VALUES (?, ?, ?, ?, 'pending')`
	for i, t := range p.Tasks {
		if _, err := tx.ExecContext(ctx, taskQ, planID, i+1, t.Title, nullableString(t.Detail)); err != nil {
			return 0, fmt.Errorf("agent: create plan task %d: %w", i+1, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("agent: commit plan: %w", err)
	}
	return planID, nil
}

// loadPlan reads a plan and its tasks.
func (s *store) loadPlan(ctx context.Context, id int64) (Plan, error) {
	const q = `
SELECT id, epoch, run_id, title, goal, COALESCE(deliverables,''), status
  FROM goal_plans
 WHERE id = ?`

	var (
		p                Plan
		runID            sql.NullInt64
		deliverablesJSON string
	)
	err := s.db.QueryRowContext(ctx, q, id).Scan(
		&p.ID, &p.Epoch, &runID, &p.Title, &p.Goal, &deliverablesJSON, &p.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return Plan{}, ErrNoPlan
	}
	if err != nil {
		return Plan{}, fmt.Errorf("agent: load plan %d: %w", id, err)
	}
	if runID.Valid {
		v := runID.Int64
		p.RunID = &v
	}
	if deliverablesJSON != "" {
		_ = json.Unmarshal([]byte(deliverablesJSON), &p.Deliverables)
	}

	tasks, err := s.loadTasks(ctx, id)
	if err != nil {
		return Plan{}, err
	}
	p.Tasks = tasks
	return p, nil
}

// activePlan returns the plan currently awaiting approval or in progress.
func (s *store) activePlan(ctx context.Context, epoch int) (Plan, error) {
	const q = `
SELECT id
  FROM goal_plans
 WHERE epoch = ?
   AND status IN ('awaiting_approval','approved','in_progress')
 ORDER BY id DESC
 LIMIT 1`

	var id int64
	err := s.db.QueryRowContext(ctx, q, epoch).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Plan{}, ErrNoPlan
	}
	if err != nil {
		return Plan{}, fmt.Errorf("agent: find active plan: %w", err)
	}
	return s.loadPlan(ctx, id)
}

func (s *store) loadTasks(ctx context.Context, planID int64) ([]Task, error) {
	const q = `
SELECT id, plan_id, seq, title, COALESCE(detail,''), status,
       COALESCE(review_verdict,''), COALESCE(review_notes,''), COALESCE(commit_sha,'')
  FROM plan_tasks
 WHERE plan_id = ?
 ORDER BY seq`

	rows, err := s.db.QueryContext(ctx, q, planID)
	if err != nil {
		return nil, fmt.Errorf("agent: load plan tasks: %w", err)
	}
	defer rows.Close()

	var out []Task
	for rows.Next() {
		var t Task
		if err := rows.Scan(&t.ID, &t.PlanID, &t.Seq, &t.Title, &t.Detail, &t.Status,
			&t.ReviewVerdict, &t.ReviewNotes, &t.CommitSHA); err != nil {
			return nil, fmt.Errorf("agent: scan plan task: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// setPlanStatus updates a plan's status, stamping approval time when approved.
func (s *store) setPlanStatus(ctx context.Context, id int64, status string) error {
	const q = `
UPDATE goal_plans
   SET status = ?,
       approved_at = CASE WHEN ? = 'approved' AND approved_at IS NULL
                          THEN strftime('%Y-%m-%dT%H:%M:%fZ','now')
                          ELSE approved_at END,
       updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
 WHERE id = ?`
	if _, err := s.db.ExecContext(ctx, q, status, status, id); err != nil {
		return fmt.Errorf("agent: set plan %d status %s: %w", id, status, err)
	}
	return nil
}

// setPlanRun links a plan to the run executing it.
func (s *store) setPlanRun(ctx context.Context, planID, runID int64) error {
	const q = `UPDATE goal_plans SET run_id = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ','now') WHERE id = ?`
	if _, err := s.db.ExecContext(ctx, q, runID, planID); err != nil {
		return fmt.Errorf("agent: link plan %d to run %d: %w", planID, runID, err)
	}
	return nil
}

// setTaskStatus updates one task's progress.
func (s *store) setTaskStatus(ctx context.Context, taskID int64, status string) error {
	const q = `
UPDATE plan_tasks
   SET status = ?,
       started_at = CASE WHEN ? = 'in_progress' AND started_at IS NULL
                         THEN strftime('%Y-%m-%dT%H:%M:%fZ','now')
                         ELSE started_at END,
       ended_at   = CASE WHEN ? IN ('done','skipped','failed')
                         THEN strftime('%Y-%m-%dT%H:%M:%fZ','now')
                         ELSE ended_at END
 WHERE id = ?`
	if _, err := s.db.ExecContext(ctx, q, status, status, status, taskID); err != nil {
		return fmt.Errorf("agent: set task %d status %s: %w", taskID, status, err)
	}
	return nil
}

// setTaskReview records the reviewer's verdict and the primary agent's handling.
func (s *store) setTaskReview(ctx context.Context, taskID int64, verdict, notes string) error {
	const q = `UPDATE plan_tasks SET review_verdict = ?, review_notes = ? WHERE id = ?`
	if _, err := s.db.ExecContext(ctx, q, nullableString(verdict), nullableString(notes), taskID); err != nil {
		return fmt.Errorf("agent: set task %d review: %w", taskID, err)
	}
	return nil
}

// setTaskCommit records the commit a task produced, so the branch is
// reconstructible even if the sandbox is reset.
func (s *store) setTaskCommit(ctx context.Context, taskID int64, sha string) error {
	const q = `UPDATE plan_tasks SET commit_sha = ? WHERE id = ?`
	if _, err := s.db.ExecContext(ctx, q, nullableString(sha), taskID); err != nil {
		return fmt.Errorf("agent: set task %d commit: %w", taskID, err)
	}
	return nil
}

// recordPush stores the outcome of a push and its optional PR.
func (s *store) recordPush(ctx context.Context, runID *int64, branch, headSHA string,
	prNumber int, prURL, prStatus, prError string) error {

	const q = `
INSERT INTO pushes (run_id, branch, head_sha, pr_number, pr_url, pr_status, pr_error)
VALUES (?, ?, ?, ?, ?, ?, ?)`

	var number any
	if prNumber > 0 {
		number = prNumber
	}

	_, err := s.db.ExecContext(ctx, q, runID, branch, headSHA, number,
		nullableString(prURL), nullableString(prStatus), nullableString(prError))
	if err != nil {
		return fmt.Errorf("agent: record push: %w", err)
	}
	return nil
}
