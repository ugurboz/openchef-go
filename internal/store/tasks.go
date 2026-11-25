package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ugurboz/openchef/internal/task"
)

// Every state change below is a compare-and-set on the state the caller believes the row
// is in, validated against the package task state machine and committed before the
// caller performs the side effect it authorizes (write-ahead rule, §11).

// CreateTask inserts a new task and its steps in one transaction. A run task has exactly
// one step; a pipeline task has none until its plan is stored. CreatedAt and UpdatedAt
// are set on t.
func (db *DB) CreateTask(ctx context.Context, t *task.Task, steps []task.Step) error {
	if err := t.Validate(); err != nil {
		return fmt.Errorf("store: create task: %w", err)
	}
	if t.Status != t.Kind.InitialStatus() {
		return fmt.Errorf("store: create task %s: %w: %s task must start %s, not %s",
			t.ID, task.ErrInvalid, t.Kind, t.Kind.InitialStatus(), t.Status)
	}
	switch {
	case t.Kind == task.KindRun && len(steps) != 1:
		return fmt.Errorf("store: create task %s: %w: run task needs exactly one step, got %d",
			t.ID, task.ErrInvalid, len(steps))
	case t.Kind == task.KindPipeline && len(steps) != 0:
		return fmt.Errorf("store: create task %s: %w: pipeline steps come from its plan",
			t.ID, task.ErrInvalid)
	}
	if len(steps) > 0 {
		if err := task.ValidatePlan(steps); err != nil {
			return fmt.Errorf("store: create task %s: %w", t.ID, err)
		}
	}

	now := db.now()
	err := db.Write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO tasks
			(id, kind, status, prompt, repo_root, base_commit, mode, backend, importance,
			 agent_pin, branch, worktree, resume_at, reason, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			string(t.ID), string(t.Kind), string(t.Status), t.Prompt, t.RepoRoot,
			nullString(t.BaseCommit), string(t.Mode), string(t.Backend), string(t.Importance),
			nullString(t.AgentPin), nullString(t.Branch), nullString(t.Worktree),
			nullTime(t.ResumeAt), nullString(t.Reason), formatTime(now), formatTime(now),
		); err != nil {
			return fmt.Errorf("store: insert task %s: %w", t.ID, err)
		}
		for i := range steps {
			if err := insertStep(ctx, tx, t.ID, &steps[i], now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	t.CreatedAt, t.UpdatedAt = now, now
	for i := range steps {
		steps[i].TaskID, steps[i].UpdatedAt = t.ID, now
	}
	return nil
}

func insertStep(ctx context.Context, tx *sql.Tx, id task.ID, s *task.Step, now time.Time) error {
	files := s.Files
	if files == nil {
		files = []string{}
	}
	filesJSON, err := json.Marshal(files)
	if err != nil {
		return fmt.Errorf("store: encode step %d files: %w", s.Index, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO steps
		(task_id, idx, title, prompt, files, verify, importance, status, retries,
		 verified_commit, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(id), s.Index, s.Title, s.Prompt, string(filesJSON), nullString(s.Verify),
		string(s.Importance), string(s.Status), s.Retries, nullString(s.VerifiedCommit),
		formatTime(now),
	); err != nil {
		return fmt.Errorf("store: insert step %s/%d: %w", id, s.Index, err)
	}
	return nil
}

const taskColumns = `id, kind, status, prompt, repo_root, base_commit, mode, backend,
	importance, agent_pin, branch, worktree, resume_at, reason, created_at, updated_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTask(r rowScanner) (task.Task, error) {
	var (
		t                                              task.Task
		id, kind, status, mode, backend, importance    string
		baseCommit, agentPin, branch, worktree, reason sql.NullString
		resumeAt                                       sql.NullString
		created, updated                               string
	)
	if err := r.Scan(&id, &kind, &status, &t.Prompt, &t.RepoRoot, &baseCommit, &mode,
		&backend, &importance, &agentPin, &branch, &worktree, &resumeAt, &reason,
		&created, &updated); err != nil {
		return task.Task{}, err // callers wrap with context and map sql.ErrNoRows
	}
	t.ID, t.Kind, t.Status = task.ID(id), task.Kind(kind), task.Status(status)
	t.Mode, t.Backend, t.Importance = task.Mode(mode), task.Backend(backend), task.Importance(importance)
	t.BaseCommit, t.AgentPin, t.Branch = baseCommit.String, agentPin.String, branch.String
	t.Worktree, t.Reason = worktree.String, reason.String
	var err error
	if t.ResumeAt, err = parseNullTime(resumeAt); err != nil {
		return task.Task{}, err
	}
	if t.CreatedAt, err = parseTime(created); err != nil {
		return task.Task{}, err
	}
	if t.UpdatedAt, err = parseTime(updated); err != nil {
		return task.Task{}, err
	}
	return t, nil
}

// Task returns one task.
func (db *DB) Task(ctx context.Context, id task.ID) (task.Task, error) {
	t, err := scanTask(db.reader.QueryRowContext(ctx,
		"SELECT "+taskColumns+" FROM tasks WHERE id = ?", string(id)))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return task.Task{}, fmt.Errorf("%w: task %s", ErrNotFound, id)
	case err != nil:
		return task.Task{}, fmt.Errorf("store: read task %s: %w", id, err)
	}
	return t, nil
}

// TaskFilter selects tasks for Tasks.
type TaskFilter struct {
	// Active limits the result to tasks that are not done, failed or cancelled.
	Active bool
	// Limit caps the number of tasks returned; 0 means no limit.
	Limit int
}

// Tasks returns tasks, newest first.
func (db *DB) Tasks(ctx context.Context, f TaskFilter) ([]task.Task, error) {
	var q strings.Builder
	q.WriteString("SELECT " + taskColumns + " FROM tasks")
	if f.Active {
		q.WriteString(" WHERE status NOT IN " + terminalSQL)
	}
	q.WriteString(" ORDER BY created_at DESC, id")
	var args []any
	if f.Limit > 0 {
		q.WriteString(" LIMIT ?")
		args = append(args, f.Limit)
	}
	return queryAll(ctx, db.reader, scanTask, "tasks", q.String(), args...)
}

// TaskChange carries the fields a task transition may set alongside its new status.
type TaskChange struct {
	// Reason explains needs_attention, failed or cancelled; it is cleared on any other
	// transition.
	Reason string
	// ResumeAt is required when moving to waiting_quota and cleared on any other
	// transition.
	ResumeAt time.Time
}

// TransitionTask moves a task from one status to another and returns the updated task.
// It fails with *task.TransitionError if the state machine forbids the move and with
// *ConflictError if the task is no longer in from.
//
// Moving to done requires every step verified and no attempt running. Leaving the
// active states (running, reviewing) stops the task's work in the same transaction, so
// nothing the engine does afterwards is authorized: running attempts end (cancelled on
// cancel, interrupted otherwise), turns in flight end interrupted and running steps go
// back to pending. Sessions stay recorded until their processes are gone; the caller
// stops them (TaskSessions) and then calls CloseSession.
func (db *DB) TransitionTask(ctx context.Context, id task.ID, from, to task.Status, c TaskChange) (task.Task, error) {
	if err := from.CheckTransition(to); err != nil {
		return task.Task{}, fmt.Errorf("store: task %s: %w", id, err)
	}
	if to == task.StatusWaitingQuota && c.ResumeAt.IsZero() {
		return task.Task{}, fmt.Errorf("store: task %s: %w: waiting_quota needs a resume time", id, task.ErrInvalid)
	}
	if to != task.StatusWaitingQuota {
		c.ResumeAt = time.Time{}
	}
	switch to {
	case task.StatusNeedsAttention, task.StatusFailed, task.StatusCancelled:
	default:
		c.Reason = ""
	}

	var out task.Task
	err := db.Write(ctx, func(tx *sql.Tx) error {
		now := formatTime(db.now())
		ok, err := execOne(ctx, tx,
			"UPDATE tasks SET status = ?, reason = ?, resume_at = ?, updated_at = ? WHERE id = ? AND status = ?",
			string(to), nullString(c.Reason), nullTime(c.ResumeAt), now, string(id), string(from))
		if err != nil {
			return fmt.Errorf("store: task %s: %w", id, err)
		}
		if !ok {
			return casFailed(ctx, tx, "task", string(id), string(from),
				"SELECT status FROM tasks WHERE id = ?", string(id))
		}
		if to == task.StatusDone {
			if err := requireFinished(ctx, tx, id); err != nil {
				return err
			}
		}
		if !to.Active() {
			if err := stopWork(ctx, tx, id, to, now); err != nil {
				return err
			}
		}
		out, err = scanTask(tx.QueryRowContext(ctx, "SELECT "+taskColumns+" FROM tasks WHERE id = ?", string(id)))
		if err != nil {
			return fmt.Errorf("store: reread task %s: %w", id, err)
		}
		return nil
	})
	return out, err
}

// requireFinished rejects done while a step is unverified or an attempt still runs.
func requireFinished(ctx context.Context, tx *sql.Tx, id task.ID) error {
	var steps, unverified, running int
	err := tx.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM steps WHERE task_id = ?1),
		(SELECT COUNT(*) FROM steps WHERE task_id = ?1 AND status <> 'verified'),
		(SELECT COUNT(*) FROM attempts WHERE task_id = ?1 AND status = 'running')`,
		string(id)).Scan(&steps, &unverified, &running)
	switch {
	case err != nil:
		return fmt.Errorf("store: check task %s steps: %w", id, err)
	case steps == 0 || unverified > 0:
		return fmt.Errorf("store: task %s: %w: done needs every step verified (%d of %d are not)",
			id, task.ErrInvalid, unverified, steps)
	case running > 0:
		return fmt.Errorf("store: task %s: %w: done while an attempt is running", id, task.ErrInvalid)
	}
	return nil
}

// stopWork ends a task's in-flight work when it leaves the active states.
func stopWork(ctx context.Context, tx *sql.Tx, id task.ID, to task.Status, now string) error {
	end := task.AttemptInterrupted
	if to == task.StatusCancelled {
		end = task.AttemptCancelled
	}
	outcome := "task " + string(to)
	stmts := []struct {
		what  string
		query string
		args  []any
	}{
		{"turns", `UPDATE turns SET status = 'interrupted', outcome = ?, ended_at = ?
			WHERE status = 'sent' AND session_id IN (SELECT s.id FROM sessions s
			JOIN attempts a ON a.id = s.attempt_id WHERE a.task_id = ?)`,
			[]any{outcome, now, string(id)}},
		{"attempts", `UPDATE attempts SET status = ?, outcome = ?, ended_at = ?
			WHERE task_id = ? AND status = 'running'`,
			[]any{string(end), outcome, now, string(id)}},
		{"steps", `UPDATE steps SET status = 'pending', updated_at = ?
			WHERE task_id = ? AND status = 'running'`,
			[]any{now, string(id)}},
	}
	for _, st := range stmts {
		if _, err := tx.ExecContext(ctx, st.query, st.args...); err != nil {
			return fmt.Errorf("store: task %s: stop %s: %w", id, st.what, err)
		}
	}
	return nil
}

// SetPlan stores a pipeline task's plan, replacing any earlier version (plan approval
// may edit it). The task must be planning or awaiting approval and every step pending.
func (db *DB) SetPlan(ctx context.Context, id task.ID, steps []task.Step) error {
	if err := task.ValidatePlan(steps); err != nil {
		return fmt.Errorf("store: plan of %s: %w", id, err)
	}
	for i := range steps {
		if steps[i].Status != task.StepPending || steps[i].Retries != 0 || steps[i].VerifiedCommit != "" {
			return fmt.Errorf("store: plan of %s: %w: step %d is not a fresh pending step",
				id, task.ErrInvalid, steps[i].Index)
		}
	}
	now := db.now()
	err := db.Write(ctx, func(tx *sql.Tx) error {
		var kind, status string
		err := tx.QueryRowContext(ctx, "SELECT kind, status FROM tasks WHERE id = ?", string(id)).Scan(&kind, &status)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: task %s", ErrNotFound, id)
		case err != nil:
			return fmt.Errorf("store: read task %s: %w", id, err)
		case kind != string(task.KindPipeline):
			return fmt.Errorf("store: plan of %s: %w: %s task has no plan", id, task.ErrInvalid, kind)
		case status != string(task.StatusPlanning) && status != string(task.StatusAwaitingApproval):
			return &ConflictError{Entity: "task", Key: string(id), Want: "planning or awaiting_approval", Got: status}
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM steps WHERE task_id = ?", string(id)); err != nil {
			return fmt.Errorf("store: replace plan of %s: %w", id, err)
		}
		for i := range steps {
			if err := insertStep(ctx, tx, id, &steps[i], now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for i := range steps {
		steps[i].TaskID, steps[i].UpdatedAt = id, now
	}
	return nil
}

// SetWorkspace records the branch, worktree and base commit of a task's workspace. It is
// written before the worktree is created so a crash in between leaves a record of the
// path to clean up. The task must not be terminal.
func (db *DB) SetWorkspace(ctx context.Context, id task.ID, branch, worktree, baseCommit string) error {
	if branch == "" || worktree == "" || baseCommit == "" {
		return fmt.Errorf("store: task %s: %w: workspace needs branch, worktree and base commit", id, task.ErrInvalid)
	}
	return db.Write(ctx, func(tx *sql.Tx) error {
		ok, err := execOne(ctx, tx, `UPDATE tasks SET branch = ?, worktree = ?, base_commit = ?,
			updated_at = ? WHERE id = ? AND status NOT IN `+terminalSQL,
			branch, worktree, baseCommit, formatTime(db.now()), string(id))
		if err != nil {
			return fmt.Errorf("store: task %s: %w", id, err)
		}
		if !ok {
			return casFailed(ctx, tx, "task", string(id), "not terminal",
				"SELECT status FROM tasks WHERE id = ?", string(id))
		}
		return nil
	})
}

const stepColumns = `task_id, idx, title, prompt, files, verify, importance, status,
	retries, verified_commit, updated_at`

func scanStep(r rowScanner) (task.Step, error) {
	var (
		s                       task.Step
		id, files, imp, st, upd string
		verify, commit          sql.NullString
	)
	if err := r.Scan(&id, &s.Index, &s.Title, &s.Prompt, &files, &verify, &imp, &st,
		&s.Retries, &commit, &upd); err != nil {
		return task.Step{}, err // callers wrap with context and map sql.ErrNoRows
	}
	s.TaskID, s.Importance, s.Status = task.ID(id), task.Importance(imp), task.StepStatus(st)
	s.Verify, s.VerifiedCommit = verify.String, commit.String
	if err := json.Unmarshal([]byte(files), &s.Files); err != nil {
		return task.Step{}, fmt.Errorf("store: decode step %s/%d files: %w", id, s.Index, err)
	}
	var err error
	if s.UpdatedAt, err = parseTime(upd); err != nil {
		return task.Step{}, err
	}
	return s, nil
}

// Steps returns a task's steps in order.
func (db *DB) Steps(ctx context.Context, id task.ID) ([]task.Step, error) {
	return queryAll(ctx, db.reader, scanStep, "steps of "+string(id),
		"SELECT "+stepColumns+" FROM steps WHERE task_id = ? ORDER BY idx", string(id))
}

// StepChange carries the fields a step transition may set alongside its new status.
type StepChange struct {
	// VerifiedCommit is required when moving to verified.
	VerifiedCommit string
	// CountRetry increments the step's retry counter (a verification failure was fed
	// back to the agent).
	CountRetry bool
}

// TransitionStep moves a step from one status to another and returns the updated step.
// The task must be active (running or reviewing): steps only change while the engine
// drives the task. Leaving the active states resets running steps (TransitionTask).
func (db *DB) TransitionStep(ctx context.Context, id task.ID, idx int, from, to task.StepStatus, c StepChange) (task.Step, error) {
	key := fmt.Sprintf("%s/%d", id, idx)
	if from != to {
		if err := from.CheckTransition(to); err != nil {
			return task.Step{}, fmt.Errorf("store: step %s: %w", key, err)
		}
	} else if !c.CountRetry {
		return task.Step{}, fmt.Errorf("store: step %s: %w", key,
			&task.TransitionError{Entity: "step", From: string(from), To: string(to)})
	}
	if c.CountRetry && (from != task.StepRunning || to != task.StepRunning) {
		return task.Step{}, fmt.Errorf("store: step %s: %w: a retry keeps the step running", key, task.ErrInvalid)
	}
	if to == task.StepVerified && c.VerifiedCommit == "" {
		return task.Step{}, fmt.Errorf("store: step %s: %w: verified step needs its commit", key, task.ErrInvalid)
	}
	if to != task.StepVerified && c.VerifiedCommit != "" {
		return task.Step{}, fmt.Errorf("store: step %s: %w: only a verified step has a commit", key, task.ErrInvalid)
	}
	retry := 0
	if c.CountRetry {
		retry = 1
	}

	var out task.Step
	err := db.Write(ctx, func(tx *sql.Tx) error {
		if err := requireActive(ctx, tx, id); err != nil {
			return err
		}
		ok, err := execOne(ctx, tx, `UPDATE steps SET status = ?, retries = retries + ?,
			verified_commit = COALESCE(?, verified_commit), updated_at = ?
			WHERE task_id = ? AND idx = ? AND status = ?`,
			string(to), retry, nullString(c.VerifiedCommit), formatTime(db.now()),
			string(id), idx, string(from))
		if err != nil {
			return fmt.Errorf("store: step %s: %w", key, err)
		}
		if !ok {
			return casFailed(ctx, tx, "step", key, string(from),
				"SELECT status FROM steps WHERE task_id = ? AND idx = ?", string(id), idx)
		}
		out, err = scanStep(tx.QueryRowContext(ctx,
			"SELECT "+stepColumns+" FROM steps WHERE task_id = ? AND idx = ?", string(id), idx))
		if err != nil {
			return fmt.Errorf("store: reread step %s: %w", key, err)
		}
		return nil
	})
	return out, err
}

// requireActive fails with *ConflictError unless the task is running or reviewing.
func requireActive(ctx context.Context, q queryer, id task.ID) error {
	var got string
	err := q.QueryRowContext(ctx, "SELECT status FROM tasks WHERE id = ?", string(id)).Scan(&got)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: task %s", ErrNotFound, id)
	case err != nil:
		return fmt.Errorf("store: read task %s: %w", id, err)
	case !task.Status(got).Active():
		return &ConflictError{Entity: "task", Key: string(id), Want: "running or reviewing", Got: got}
	}
	return nil
}

// terminalSQL is the SQL list of terminal task states, e.g. ('cancelled', 'done', 'failed').
var terminalSQL = func() string {
	parts := make([]string, 0, 3)
	for _, s := range task.TerminalStatuses() {
		parts = append(parts, "'"+string(s)+"'")
	}
	return "(" + strings.Join(parts, ", ") + ")"
}()
