package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/ugurboz/openchef/internal/task"
)

const approvalColumns = "id, task_id, agent, kind, status, tool, summary, options, answer, decided_by, created_at, expires_at, decided_at"

// CreateApproval queues a pending request. ID, Status, CreatedAt and ExpiresAt are set
// on a (ExpiresAt from task.ApprovalTimeout unless already set). request is the
// normalized request kept for audit; it may be nil.
func (db *DB) CreateApproval(ctx context.Context, a *task.Approval, request json.RawMessage) error {
	if !a.Kind.Valid() {
		return fmt.Errorf("store: create approval: %w: kind %q", task.ErrInvalid, a.Kind)
	}
	if a.Summary == "" || a.Agent == "" {
		return fmt.Errorf("store: create approval: %w: summary and agent are required", task.ErrInvalid)
	}
	options := a.Options
	if options == nil {
		options = []string{}
	}
	optionsJSON, err := json.Marshal(options)
	if err != nil {
		return fmt.Errorf("store: encode approval options: %w", err)
	}
	now := db.now()
	expires := a.ExpiresAt
	if expires.IsZero() {
		expires = now.Add(task.ApprovalTimeout)
	}
	var id int64
	err = db.Write(ctx, func(tx *sql.Tx) error {
		if err := requireActive(ctx, tx, a.TaskID); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `INSERT INTO approvals
			(task_id, agent, kind, status, tool, summary, options, request, created_at, expires_at)
			VALUES (?, ?, ?, 'pending', ?, ?, ?, ?, ?, ?) RETURNING id`,
			string(a.TaskID), a.Agent, string(a.Kind), nullString(a.Tool), a.Summary, string(optionsJSON),
			nullString(string(request)), formatTime(now), formatTime(expires),
		).Scan(&id)
	})
	if err != nil {
		return fmt.Errorf("store: create approval for task %s: %w", a.TaskID, err)
	}
	a.ID, a.Status, a.CreatedAt, a.ExpiresAt = id, task.ApprovalPending, now, expires
	a.Answer, a.DecidedBy, a.DecidedAt = "", "", time.Time{}
	return nil
}

// DecideApproval settles a pending request (compare-and-set on pending). A request that
// was already settled returns a *ConflictError with its status, so a late answer from
// the CLI is reported instead of overwriting the first one.
func (db *DB) DecideApproval(ctx context.Context, id int64, d task.Decision) (task.Approval, error) {
	key := strconv.FormatInt(id, 10)
	var out task.Approval
	err := db.Write(ctx, func(tx *sql.Tx) error {
		a, err := scanApproval(tx.QueryRowContext(ctx, "SELECT "+approvalColumns+" FROM approvals WHERE id = ?", id))
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: approval %s", ErrNotFound, key)
		case err != nil:
			return fmt.Errorf("store: read approval %s: %w", key, err)
		}
		if a.Status != task.ApprovalPending {
			return &ConflictError{Entity: "approval", Key: key, Want: string(task.ApprovalPending), Got: string(a.Status)}
		}
		if err := d.Validate(a.Kind); err != nil {
			return fmt.Errorf("store: decide approval %s: %w", key, err)
		}
		now := db.now()
		if _, err := tx.ExecContext(ctx,
			"UPDATE approvals SET status = ?, answer = ?, decided_by = ?, decided_at = ? WHERE id = ? AND status = 'pending'",
			string(d.Status), nullString(d.Answer), string(d.DecidedBy), formatTime(now), id); err != nil {
			return fmt.Errorf("store: decide approval %s: %w", key, err)
		}
		a.Status, a.Answer, a.DecidedBy, a.DecidedAt = d.Status, d.Answer, d.DecidedBy, now
		out = a
		return nil
	})
	return out, err
}

// ExpirePending settles every pending request as expired by the daemon, with reason as
// the message to the agent. The daemon calls it on start: whoever was waiting for those
// answers died with the previous daemon. It returns the settled requests.
func (db *DB) ExpirePending(ctx context.Context, reason string) ([]task.Approval, error) {
	var out []task.Approval
	err := db.Write(ctx, func(tx *sql.Tx) error {
		now := formatTime(db.now())
		rows, err := tx.QueryContext(ctx, `UPDATE approvals SET status = 'expired', answer = ?, decided_by = 'daemon', decided_at = ?
			WHERE status = 'pending' RETURNING `+approvalColumns, nullString(reason), now)
		if err != nil {
			return fmt.Errorf("store: expire approvals: %w", err)
		}
		defer func() { _ = rows.Close() }() // rows.Err reports failures
		for rows.Next() {
			a, err := scanApproval(rows)
			if err != nil {
				return fmt.Errorf("store: expire approvals: %w", err)
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return out, err
}

// Approval returns one request.
func (db *DB) Approval(ctx context.Context, id int64) (task.Approval, error) {
	a, err := scanApproval(db.reader.QueryRowContext(ctx, "SELECT "+approvalColumns+" FROM approvals WHERE id = ?", id))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return task.Approval{}, fmt.Errorf("%w: approval %d", ErrNotFound, id)
	case err != nil:
		return task.Approval{}, fmt.Errorf("store: read approval %d: %w", id, err)
	}
	return a, nil
}

// ApprovalFilter selects requests for Approvals.
type ApprovalFilter struct {
	// Task limits the result to one task's requests.
	Task task.ID
	// Pending limits the result to requests still waiting.
	Pending bool
	// Limit caps the result to the newest requests; 0 means no limit.
	Limit int
}

// Approvals returns requests, oldest first (the order to answer them in).
func (db *DB) Approvals(ctx context.Context, f ApprovalFilter) ([]task.Approval, error) {
	q := "SELECT " + approvalColumns + " FROM approvals WHERE 1 = 1"
	var args []any
	if f.Task != "" {
		q += " AND task_id = ?"
		args = append(args, string(f.Task))
	}
	if f.Pending {
		q += " AND status = 'pending'"
	}
	q += " ORDER BY id DESC"
	if f.Limit > 0 {
		q += " LIMIT ?"
		args = append(args, f.Limit)
	}
	out, err := queryAll(ctx, db.reader, scanApproval, "approvals", q, args...)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func scanApproval(r rowScanner) (task.Approval, error) {
	var (
		a                            task.Approval
		taskID, kind, status         string
		options, created, expires    string
		tool, answer, decidedBy, dec sql.NullString
	)
	if err := r.Scan(&a.ID, &taskID, &a.Agent, &kind, &status, &tool, &a.Summary, &options,
		&answer, &decidedBy, &created, &expires, &dec); err != nil {
		return task.Approval{}, err // callers wrap with context and map sql.ErrNoRows
	}
	a.TaskID, a.Kind, a.Status = task.ID(taskID), task.ApprovalKind(kind), task.ApprovalStatus(status)
	a.Tool, a.Answer, a.DecidedBy = tool.String, answer.String, task.DecidedBy(decidedBy.String)
	if err := json.Unmarshal([]byte(options), &a.Options); err != nil {
		return task.Approval{}, fmt.Errorf("store: decode approval options: %w", err)
	}
	if len(a.Options) == 0 {
		a.Options = nil
	}
	var err error
	if a.CreatedAt, err = parseTime(created); err != nil {
		return task.Approval{}, err
	}
	if a.ExpiresAt, err = parseTime(expires); err != nil {
		return task.Approval{}, err
	}
	if a.DecidedAt, err = parseNullTime(dec); err != nil {
		return task.Approval{}, err
	}
	return a, nil
}
