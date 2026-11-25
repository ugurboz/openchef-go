package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"github.com/ugurboz/openchef/internal/task"
)

// Attempts, sessions and turns can only start while their task is active (running or
// reviewing). Together with TransitionTask stopping a task's work when it leaves those
// states, this keeps a cancel committed by the CLI from being overtaken by the engine:
// whatever the engine tries next is rejected before its side effect.

// StartAttempt records a new running attempt at a step and sets a.ID, a.Status and
// a.StartedAt. The task must be active, the step running, and the step may have only
// one running attempt at a time.
func (db *DB) StartAttempt(ctx context.Context, a *task.Attempt) error {
	key := fmt.Sprintf("%s/%d", a.TaskID, a.StepIndex)
	if a.Agent == "" || a.Runtime == "" || a.Pool == "" {
		return fmt.Errorf("store: attempt at %s: %w: agent, runtime and pool are required", key, task.ErrInvalid)
	}
	now := db.now()
	var id int64
	err := db.Write(ctx, func(tx *sql.Tx) error {
		if err := requireActive(ctx, tx, a.TaskID); err != nil {
			return err
		}
		var step string
		var running int
		err := tx.QueryRowContext(ctx, `SELECT status, (SELECT COUNT(*) FROM attempts
			WHERE task_id = ?1 AND step_idx = ?2 AND status = 'running')
			FROM steps WHERE task_id = ?1 AND idx = ?2`,
			string(a.TaskID), a.StepIndex).Scan(&step, &running)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: step %s", ErrNotFound, key)
		case err != nil:
			return fmt.Errorf("store: read step %s: %w", key, err)
		case step != string(task.StepRunning):
			return &ConflictError{Entity: "step", Key: key, Want: string(task.StepRunning), Got: step}
		case running > 0:
			return &ConflictError{Entity: "step", Key: key, Want: "no running attempt", Got: "attempt running"}
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO attempts
			(task_id, step_idx, agent, runtime, model, pool, status, started_at)
			VALUES (?, ?, ?, ?, ?, ?, 'running', ?)`,
			string(a.TaskID), a.StepIndex, a.Agent, a.Runtime, nullString(a.Model), a.Pool,
			formatTime(now))
		if err != nil {
			return fmt.Errorf("store: insert attempt at %s: %w", key, err)
		}
		if id, err = res.LastInsertId(); err != nil {
			return fmt.Errorf("store: attempt id at %s: %w", key, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	a.ID, a.Status, a.StartedAt = id, task.AttemptRunning, now
	return nil
}

// EndAttempt moves a running attempt to a terminal status with an outcome. An attempt
// succeeds only once its step is verified and no turn of it is in flight.
func (db *DB) EndAttempt(ctx context.Context, id int64, to task.AttemptStatus, outcome string) error {
	key := strconv.FormatInt(id, 10)
	if err := task.AttemptRunning.CheckTransition(to); err != nil {
		return fmt.Errorf("store: attempt %s: %w", key, err)
	}
	return db.Write(ctx, func(tx *sql.Tx) error {
		if to == task.AttemptSucceeded {
			if err := requireSucceeded(ctx, tx, id); err != nil {
				return err
			}
		}
		ok, err := execOne(ctx, tx,
			"UPDATE attempts SET status = ?, outcome = ?, ended_at = ? WHERE id = ? AND status = 'running'",
			string(to), nullString(outcome), formatTime(db.now()), id)
		if err != nil {
			return fmt.Errorf("store: attempt %s: %w", key, err)
		}
		if !ok {
			return casFailed(ctx, tx, "attempt", key, string(task.AttemptRunning),
				"SELECT status FROM attempts WHERE id = ?", id)
		}
		return nil
	})
}

func requireSucceeded(ctx context.Context, tx *sql.Tx, id int64) error {
	key := strconv.FormatInt(id, 10)
	var step string
	var inFlight int
	err := tx.QueryRowContext(ctx, `SELECT st.status, (SELECT COUNT(*) FROM turns t
		JOIN sessions s ON s.id = t.session_id WHERE s.attempt_id = a.id AND t.status = 'sent')
		FROM attempts a JOIN steps st ON st.task_id = a.task_id AND st.idx = a.step_idx
		WHERE a.id = ?`, id).Scan(&step, &inFlight)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: attempt %s", ErrNotFound, key)
	case err != nil:
		return fmt.Errorf("store: read attempt %s: %w", key, err)
	case step != string(task.StepVerified):
		return fmt.Errorf("store: attempt %s: %w: succeeded but its step is %s", key, task.ErrInvalid, step)
	case inFlight > 0:
		return fmt.Errorf("store: attempt %s: %w: succeeded with a turn in flight", key, task.ErrInvalid)
	}
	return nil
}

const attemptColumns = `id, task_id, step_idx, agent, runtime, model, pool, status, outcome,
	started_at, ended_at`

func scanAttempt(r rowScanner) (task.Attempt, error) {
	var (
		a                   task.Attempt
		taskID, status, st  string
		model, outcome, end sql.NullString
	)
	if err := r.Scan(&a.ID, &taskID, &a.StepIndex, &a.Agent, &a.Runtime, &model, &a.Pool,
		&status, &outcome, &st, &end); err != nil {
		return task.Attempt{}, err // callers wrap with context and map sql.ErrNoRows
	}
	a.TaskID, a.Status = task.ID(taskID), task.AttemptStatus(status)
	a.Model, a.Outcome = model.String, outcome.String
	var err error
	if a.StartedAt, err = parseTime(st); err != nil {
		return task.Attempt{}, err
	}
	if a.EndedAt, err = parseNullTime(end); err != nil {
		return task.Attempt{}, err
	}
	return a, nil
}

// Attempts returns a task's attempts, oldest first.
func (db *DB) Attempts(ctx context.Context, id task.ID) ([]task.Attempt, error) {
	return queryAll(ctx, db.reader, scanAttempt, "attempts of "+string(id),
		"SELECT "+attemptColumns+" FROM attempts WHERE task_id = ? ORDER BY id", string(id))
}

// RunningAttempts returns every running attempt across all tasks, oldest first. After a
// daemon restart these are the attempts recovery resumes (§5.3).
func (db *DB) RunningAttempts(ctx context.Context) ([]task.Attempt, error) {
	return queryAll(ctx, db.reader, scanAttempt, "running attempts",
		"SELECT "+attemptColumns+" FROM attempts WHERE status = 'running' ORDER BY id")
}

// RecordSession records a spawned agent process as a starting session and sets s.ID,
// s.NativeID, s.State and s.OpenedAt. It must be called before anything is written to
// the process (§5.1). The attempt must be running and its task active.
//
// NativeID may be set when it is known before the CLI confirms it (Claude takes the
// session id as an argument). To continue a conversation after a daemon restart, set
// ResumedFrom to the closed session being resumed; NativeID is then taken from it. A
// live session with the same native id is a *ConflictError.
func (db *DB) RecordSession(ctx context.Context, s *task.Session) error {
	attempt := strconv.FormatInt(s.AttemptID, 10)
	if s.Runtime == "" || s.PID <= 0 {
		return fmt.Errorf("store: session of attempt %s: %w: runtime and pid are required", attempt, task.ErrInvalid)
	}
	now := db.now()
	var id int64
	native := s.NativeID
	err := db.Write(ctx, func(tx *sql.Tx) error {
		var status, taskID string
		err := tx.QueryRowContext(ctx, "SELECT status, task_id FROM attempts WHERE id = ?", s.AttemptID).
			Scan(&status, &taskID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: attempt %s", ErrNotFound, attempt)
		case err != nil:
			return fmt.Errorf("store: read attempt %s: %w", attempt, err)
		case status != string(task.AttemptRunning):
			return &ConflictError{Entity: "attempt", Key: attempt, Want: string(task.AttemptRunning), Got: status}
		}
		if err := requireActive(ctx, tx, task.ID(taskID)); err != nil {
			return err
		}
		if s.ResumedFrom != 0 {
			if native, err = resumable(ctx, tx, s); err != nil {
				return err
			}
		}
		if native != "" {
			if err := requireNoLiveNative(ctx, tx, s.Runtime, native); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO sessions
			(attempt_id, resumed_from, runtime, native_id, pid, start_token, state, log_dir, opened_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			s.AttemptID, sql.NullInt64{Int64: s.ResumedFrom, Valid: s.ResumedFrom != 0}, s.Runtime,
			nullString(native), s.PID, formatToken(s.StartToken), string(task.SessionStarting),
			nullString(s.LogDir), formatTime(now))
		if err != nil {
			return fmt.Errorf("store: insert session of attempt %s: %w", attempt, err)
		}
		if id, err = res.LastInsertId(); err != nil {
			return fmt.Errorf("store: session id of attempt %s: %w", attempt, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	s.ID, s.NativeID, s.State, s.OpenedAt = id, native, task.SessionStarting, now
	return nil
}

// resumable checks that s may continue the session it names in ResumedFrom and returns
// that session's native id.
func resumable(ctx context.Context, tx *sql.Tx, s *task.Session) (string, error) {
	key := strconv.FormatInt(s.ResumedFrom, 10)
	var state, rt string
	var native sql.NullString
	err := tx.QueryRowContext(ctx, "SELECT state, runtime, native_id FROM sessions WHERE id = ?", s.ResumedFrom).
		Scan(&state, &rt, &native)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("%w: session %s", ErrNotFound, key)
	case err != nil:
		return "", fmt.Errorf("store: read session %s: %w", key, err)
	case state != string(task.SessionClosed):
		return "", &ConflictError{Entity: "session", Key: key, Want: string(task.SessionClosed), Got: state}
	case rt != s.Runtime:
		return "", fmt.Errorf("store: resume session %s: %w: runtime %s, not %s", key, task.ErrInvalid, rt, s.Runtime)
	case !native.Valid:
		return "", fmt.Errorf("store: resume session %s: %w: it never reported a native id", key, task.ErrInvalid)
	case s.NativeID != "" && s.NativeID != native.String:
		return "", fmt.Errorf("store: resume session %s: %w: native id %q, not %q",
			key, task.ErrInvalid, native.String, s.NativeID)
	}
	return native.String, nil
}

func requireNoLiveNative(ctx context.Context, tx *sql.Tx, runtime, native string) error {
	var live int64
	err := tx.QueryRowContext(ctx,
		"SELECT id FROM sessions WHERE runtime = ? AND native_id = ? AND state <> 'closed'",
		runtime, native).Scan(&live)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("store: check native id %s/%s: %w", runtime, native, err)
	}
	return &ConflictError{
		Entity: "native session", Key: runtime + "/" + native,
		Want: "no live session", Got: "live in session " + strconv.FormatInt(live, 10),
	}
}

// OpenSession records the CLI's native session id and marks the session open. If the
// native id was recorded earlier it must match.
func (db *DB) OpenSession(ctx context.Context, id int64, nativeID string) error {
	key := strconv.FormatInt(id, 10)
	if nativeID == "" {
		return fmt.Errorf("store: session %s: %w: empty native id", key, task.ErrInvalid)
	}
	return db.Write(ctx, func(tx *sql.Tx) error {
		var state, rt string
		var known sql.NullString
		err := tx.QueryRowContext(ctx, "SELECT state, runtime, native_id FROM sessions WHERE id = ?", id).
			Scan(&state, &rt, &known)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: session %s", ErrNotFound, key)
		case err != nil:
			return fmt.Errorf("store: read session %s: %w", key, err)
		case state != string(task.SessionStarting):
			return &ConflictError{Entity: "session", Key: key, Want: string(task.SessionStarting), Got: state}
		case known.Valid && known.String != nativeID:
			return fmt.Errorf("store: session %s: %w: CLI reported native id %q, recorded %q",
				key, task.ErrInvalid, nativeID, known.String)
		case !known.Valid:
			if err := requireNoLiveNative(ctx, tx, rt, nativeID); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE sessions SET state = 'open', native_id = ? WHERE id = ?", nativeID, id); err != nil {
			return fmt.Errorf("store: open session %s: %w", key, err)
		}
		return nil
	})
}

// SetSessionProcess records a new process for a live session: some CLIs replace their
// process within one conversation (runtime.SessionSpec.OnSpawn). It must be called
// before anything is written to the new process.
func (db *DB) SetSessionProcess(ctx context.Context, id int64, pid int, startToken uint64) error {
	key := strconv.FormatInt(id, 10)
	if pid <= 0 {
		return fmt.Errorf("store: session %s: %w: pid %d", key, task.ErrInvalid, pid)
	}
	return db.Write(ctx, func(tx *sql.Tx) error {
		ok, err := execOne(ctx, tx,
			"UPDATE sessions SET pid = ?, start_token = ? WHERE id = ? AND state <> 'closed'",
			pid, formatToken(startToken), id)
		if err != nil {
			return fmt.Errorf("store: session %s: %w", key, err)
		}
		if !ok {
			return casFailed(ctx, tx, "session", key, "not closed",
				"SELECT state FROM sessions WHERE id = ?", id)
		}
		return nil
	})
}

// CloseSession marks a session closed once its process is gone. Closing a closed
// session is a no-op, matching runtime.Session.Close.
func (db *DB) CloseSession(ctx context.Context, id int64) error {
	key := strconv.FormatInt(id, 10)
	return db.Write(ctx, func(tx *sql.Tx) error {
		ok, err := execOne(ctx, tx,
			"UPDATE sessions SET state = 'closed', closed_at = ? WHERE id = ? AND state <> 'closed'",
			formatTime(db.now()), id)
		if err != nil {
			return fmt.Errorf("store: close session %s: %w", key, err)
		}
		if ok {
			return nil
		}
		var exists int
		err = tx.QueryRowContext(ctx, "SELECT 1 FROM sessions WHERE id = ?", id).Scan(&exists)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: session %s", ErrNotFound, key)
		case err != nil:
			return fmt.Errorf("store: read session %s: %w", key, err)
		}
		return nil
	})
}

const sessionColumns = `s.id, s.attempt_id, s.resumed_from, s.runtime, s.native_id, s.pid,
	s.start_token, s.state, s.log_dir, s.opened_at, s.closed_at`

func scanSession(r rowScanner) (task.Session, error) {
	var (
		s                      task.Session
		resumedFrom            sql.NullInt64
		token, state, opened   string
		native, logDir, closed sql.NullString
	)
	if err := r.Scan(&s.ID, &s.AttemptID, &resumedFrom, &s.Runtime, &native, &s.PID, &token,
		&state, &logDir, &opened, &closed); err != nil {
		return task.Session{}, err // callers wrap with context and map sql.ErrNoRows
	}
	s.ResumedFrom, s.NativeID = resumedFrom.Int64, native.String
	s.State, s.LogDir = task.SessionState(state), logDir.String
	var err error
	if s.StartToken, err = parseToken(token); err != nil {
		return task.Session{}, err
	}
	if s.OpenedAt, err = parseTime(opened); err != nil {
		return task.Session{}, err
	}
	if s.ClosedAt, err = parseNullTime(closed); err != nil {
		return task.Session{}, err
	}
	return s, nil
}

// Sessions returns an attempt's sessions, oldest first.
func (db *DB) Sessions(ctx context.Context, attemptID int64) ([]task.Session, error) {
	return queryAll(ctx, db.reader, scanSession, "sessions of attempt "+strconv.FormatInt(attemptID, 10),
		"SELECT "+sessionColumns+" FROM sessions s WHERE s.attempt_id = ? ORDER BY s.id", attemptID)
}

// TaskSessions returns the sessions of every attempt of a task, oldest first; with live
// set, only those not yet closed (the processes to stop after a cancel).
func (db *DB) TaskSessions(ctx context.Context, id task.ID, live bool) ([]task.Session, error) {
	q := "SELECT " + sessionColumns + ` FROM sessions s
		JOIN attempts a ON a.id = s.attempt_id WHERE a.task_id = ?`
	if live {
		q += " AND s.state <> 'closed'"
	}
	return queryAll(ctx, db.reader, scanSession, "sessions of "+string(id), q+" ORDER BY s.id", string(id))
}

// LiveSessions returns every session not yet closed, across all tasks. After a daemon
// restart these are the processes that may still be running (§5.3). A session still
// starting never reported a native id and cannot be resumed; recovery kills its
// process, closes it and starts a new attempt.
func (db *DB) LiveSessions(ctx context.Context) ([]task.Session, error) {
	return queryAll(ctx, db.reader, scanSession, "live sessions",
		"SELECT "+sessionColumns+" FROM sessions s WHERE s.state <> 'closed' ORDER BY s.id")
}

// BeginTurn records a message as sent to a session, before it is written to the CLI,
// and sets t.ID, t.Seq, t.Status and t.StartedAt. The session must be open, its attempt
// running, its task active, and no other turn of it in flight.
func (db *DB) BeginTurn(ctx context.Context, t *task.Turn) error {
	key := strconv.FormatInt(t.SessionID, 10)
	if !t.Kind.Valid() || t.Message == "" {
		return fmt.Errorf("store: turn of session %s: %w: kind %q, message empty: %t",
			key, task.ErrInvalid, t.Kind, t.Message == "")
	}
	now := db.now()
	var id int64
	var seq int
	err := db.Write(ctx, func(tx *sql.Tx) error {
		var state, attempt, taskID string
		var inFlight int
		err := tx.QueryRowContext(ctx, `SELECT s.state, a.status, a.task_id,
			(SELECT COUNT(*) FROM turns WHERE session_id = s.id AND status = 'sent'),
			(SELECT COALESCE(MAX(seq), 0) + 1 FROM turns WHERE session_id = s.id)
			FROM sessions s JOIN attempts a ON a.id = s.attempt_id WHERE s.id = ?`,
			t.SessionID).Scan(&state, &attempt, &taskID, &inFlight, &seq)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: session %s", ErrNotFound, key)
		case err != nil:
			return fmt.Errorf("store: read session %s: %w", key, err)
		case state != string(task.SessionOpen):
			return &ConflictError{Entity: "session", Key: key, Want: string(task.SessionOpen), Got: state}
		case attempt != string(task.AttemptRunning):
			return &ConflictError{Entity: "attempt of session", Key: key, Want: string(task.AttemptRunning), Got: attempt}
		case inFlight > 0:
			return &ConflictError{Entity: "session", Key: key, Want: "no turn in flight", Got: "turn in flight"}
		}
		if err := requireActive(ctx, tx, task.ID(taskID)); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO turns
			(session_id, seq, kind, message, status, started_at) VALUES (?, ?, ?, ?, 'sent', ?)`,
			t.SessionID, seq, string(t.Kind), t.Message, formatTime(now))
		if err != nil {
			return fmt.Errorf("store: insert turn of session %s: %w", key, err)
		}
		if id, err = res.LastInsertId(); err != nil {
			return fmt.Errorf("store: turn id of session %s: %w", key, err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	t.ID, t.Seq, t.Status, t.StartedAt = id, seq, task.TurnSent, now
	return nil
}

// EndTurn moves a sent turn to a terminal status with an outcome.
func (db *DB) EndTurn(ctx context.Context, id int64, to task.TurnStatus, outcome string) error {
	key := strconv.FormatInt(id, 10)
	if err := task.TurnSent.CheckTransition(to); err != nil {
		return fmt.Errorf("store: turn %s: %w", key, err)
	}
	return db.Write(ctx, func(tx *sql.Tx) error {
		ok, err := execOne(ctx, tx,
			"UPDATE turns SET status = ?, outcome = ?, ended_at = ? WHERE id = ? AND status = 'sent'",
			string(to), nullString(outcome), formatTime(db.now()), id)
		if err != nil {
			return fmt.Errorf("store: turn %s: %w", key, err)
		}
		if !ok {
			return casFailed(ctx, tx, "turn", key, string(task.TurnSent),
				"SELECT status FROM turns WHERE id = ?", id)
		}
		return nil
	})
}

const turnColumns = `id, session_id, seq, kind, message, status, outcome, started_at, ended_at`

func scanTurn(r rowScanner) (task.Turn, error) {
	var (
		t                   task.Turn
		kind, status, start string
		outcome, end        sql.NullString
	)
	if err := r.Scan(&t.ID, &t.SessionID, &t.Seq, &kind, &t.Message, &status, &outcome,
		&start, &end); err != nil {
		return task.Turn{}, err // callers wrap with context and map sql.ErrNoRows
	}
	t.Kind, t.Status, t.Outcome = task.TurnKind(kind), task.TurnStatus(status), outcome.String
	var err error
	if t.StartedAt, err = parseTime(start); err != nil {
		return task.Turn{}, err
	}
	if t.EndedAt, err = parseNullTime(end); err != nil {
		return task.Turn{}, err
	}
	return t, nil
}

// Turns returns a session's turns in order.
func (db *DB) Turns(ctx context.Context, sessionID int64) ([]task.Turn, error) {
	return queryAll(ctx, db.reader, scanTurn, "turns of session "+strconv.FormatInt(sessionID, 10),
		"SELECT "+turnColumns+" FROM turns WHERE session_id = ? ORDER BY seq", sessionID)
}

// InFlightTurn returns a session's turn that was sent but never ended, the one recovery
// retries with a recovery note (§5.3). It fails with ErrNotFound if there is none.
func (db *DB) InFlightTurn(ctx context.Context, sessionID int64) (task.Turn, error) {
	key := strconv.FormatInt(sessionID, 10)
	t, err := scanTurn(db.reader.QueryRowContext(ctx,
		"SELECT "+turnColumns+" FROM turns WHERE session_id = ? AND status = 'sent'", sessionID))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return task.Turn{}, fmt.Errorf("%w: turn in flight in session %s", ErrNotFound, key)
	case err != nil:
		return task.Turn{}, fmt.Errorf("store: read turn in flight in session %s: %w", key, err)
	}
	return t, nil
}

func queryAll[T any](ctx context.Context, pool *sql.DB, scan func(rowScanner) (T, error), what, query string, args ...any) ([]T, error) {
	rows, err := pool.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list %s: %w", what, err)
	}
	defer func() { _ = rows.Close() }() // read-only cursor; rows.Err reports failures
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list %s: %w", what, err)
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list %s: %w", what, err)
	}
	return out, nil
}
