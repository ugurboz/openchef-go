package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ugurboz/openchef/internal/task"
)

// openSession records and opens a session for a running attempt.
func openSession(t *testing.T, db *DB, a task.Attempt) task.Session {
	const native = "n1"
	t.Helper()
	ctx := context.Background()
	s := task.Session{AttemptID: a.ID, Runtime: a.Runtime, PID: 4242, StartToken: 7}
	if err := db.RecordSession(ctx, &s); err != nil {
		t.Fatalf("RecordSession: %v", err)
	}
	if err := db.OpenSession(ctx, s.ID, native); err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	s.NativeID, s.State = native, task.SessionOpen
	return s
}

func TestCancelStopsWork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openDomain(t)
	tk, a := startAttempt(t, db)
	s := openSession(t, db, a)
	turn := task.Turn{SessionID: s.ID, Kind: task.TurnPrompt, Message: "go"}
	if err := db.BeginTurn(ctx, &turn); err != nil {
		t.Fatal(err)
	}

	if _, err := db.TransitionTask(ctx, tk.ID, task.StatusRunning, task.StatusCancelled, TaskChange{Reason: "user"}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	attempts, err := db.Attempts(ctx, tk.ID)
	if err != nil || attempts[0].Status != task.AttemptCancelled || attempts[0].Outcome != "task cancelled" {
		t.Fatalf("attempt after cancel = %+v, %v", attempts, err)
	}
	if _, err := db.InFlightTurn(ctx, s.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("turn still in flight: %v", err)
	}
	turns, err := db.Turns(ctx, s.ID)
	if err != nil || turns[0].Status != task.TurnInterrupted {
		t.Fatalf("turn after cancel = %+v, %v", turns, err)
	}
	steps, err := db.Steps(ctx, tk.ID)
	if err != nil || steps[0].Status != task.StepPending {
		t.Fatalf("step after cancel = %+v, %v", steps, err)
	}
	live, err := db.TaskSessions(ctx, tk.ID, true)
	if err != nil || len(live) != 1 || live[0].ID != s.ID {
		t.Fatalf("live sessions to stop = %+v, %v", live, err)
	}

	// Nothing the engine tries afterwards is authorized.
	if err := db.BeginTurn(ctx, &task.Turn{SessionID: s.ID, Kind: task.TurnRetry, Message: "again"}); !errors.Is(err, ErrConflict) {
		t.Errorf("BeginTurn after cancel = %v; want ErrConflict", err)
	}
	if err := db.RecordSession(ctx, &task.Session{AttemptID: a.ID, Runtime: "claude", PID: 9}); !errors.Is(err, ErrConflict) {
		t.Errorf("RecordSession after cancel = %v; want ErrConflict", err)
	}
	next := task.Attempt{TaskID: tk.ID, StepIndex: 1, Agent: "x", Runtime: "codex", Pool: "p"}
	if err := db.StartAttempt(ctx, &next); !errors.Is(err, ErrConflict) {
		t.Errorf("StartAttempt after cancel = %v; want ErrConflict", err)
	}
	if err := db.CloseSession(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if live, err := db.TaskSessions(ctx, tk.ID, true); err != nil || len(live) != 0 {
		t.Fatalf("live after close = %+v, %v", live, err)
	}
}

// A cancel committed by the CLI is never overtaken by the engine: no turn starts after
// the cancel returns, and nothing is left in flight.
func TestCancelRacesEngine(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for range 20 {
		db := openDomain(t)
		tk, a := startAttempt(t, db)
		s := openSession(t, db, a)

		var cancelled atomic.Bool
		var wg sync.WaitGroup
		wg.Go(func() {
			for {
				before := cancelled.Load()
				turn := task.Turn{SessionID: s.ID, Kind: task.TurnPrompt, Message: "go"}
				err := db.BeginTurn(ctx, &turn)
				if err != nil {
					if !errors.Is(err, ErrConflict) {
						t.Errorf("BeginTurn: %v", err)
					}
					return
				}
				if before {
					t.Error("turn began after the cancel was committed")
					return
				}
				if err := db.EndTurn(ctx, turn.ID, task.TurnCompleted, "completed"); err != nil &&
					!errors.Is(err, ErrConflict) {
					t.Errorf("EndTurn: %v", err)
					return
				}
			}
		})
		wg.Go(func() {
			if _, err := db.TransitionTask(ctx, tk.ID, task.StatusRunning, task.StatusCancelled, TaskChange{}); err != nil {
				t.Errorf("cancel: %v", err)
			}
			cancelled.Store(true)
		})
		wg.Wait()
		if _, err := db.InFlightTurn(ctx, s.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("turn left in flight: %v", err)
		}
	}
}

func TestDoneRequiresFinishedWork(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openDomain(t)
	tk, a := startAttempt(t, db)

	if _, err := db.TransitionTask(ctx, tk.ID, task.StatusRunning, task.StatusDone, TaskChange{}); !errors.Is(err, task.ErrInvalid) {
		t.Fatalf("done with running step = %v; want ErrInvalid", err)
	}
	if got, err := db.Task(ctx, tk.ID); err != nil || got.Status != task.StatusRunning {
		t.Fatalf("rejected done changed the task: %+v, %v", got, err)
	}
	s := openSession(t, db, a)
	turn := task.Turn{SessionID: s.ID, Kind: task.TurnPrompt, Message: "go"}
	if err := db.BeginTurn(ctx, &turn); err != nil {
		t.Fatal(err)
	}
	if err := db.EndAttempt(ctx, a.ID, task.AttemptSucceeded, ""); !errors.Is(err, task.ErrInvalid) {
		t.Errorf("succeeded with unverified step = %v", err)
	}
	if _, err := db.TransitionStep(ctx, tk.ID, 1, task.StepRunning, task.StepVerified, StepChange{VerifiedCommit: "c1"}); err != nil {
		t.Fatal(err)
	}
	if err := db.EndAttempt(ctx, a.ID, task.AttemptSucceeded, ""); !errors.Is(err, task.ErrInvalid) {
		t.Errorf("succeeded with turn in flight = %v", err)
	}
	if err := db.EndTurn(ctx, turn.ID, task.TurnCompleted, "completed"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.TransitionTask(ctx, tk.ID, task.StatusRunning, task.StatusDone, TaskChange{}); !errors.Is(err, task.ErrInvalid) {
		t.Errorf("done with running attempt = %v", err)
	}
	if err := db.EndAttempt(ctx, a.ID, task.AttemptSucceeded, "verified"); err != nil {
		t.Fatal(err)
	}
	if err := db.CloseSession(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.TransitionTask(ctx, tk.ID, task.StatusRunning, task.StatusDone, TaskChange{}); err != nil {
		t.Fatalf("done: %v", err)
	}
}

// The recovery flow of §5.3: the old process is killed and its session closed, the
// interrupted turn is found, and a new session continues the same conversation.
func TestResumeAfterRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openDomain(t)
	tk, a := startAttempt(t, db)
	old := openSession(t, db, a)
	turn := task.Turn{SessionID: old.ID, Kind: task.TurnPrompt, Message: "go"}
	if err := db.BeginTurn(ctx, &turn); err != nil {
		t.Fatal(err)
	}

	running, err := db.RunningAttempts(ctx)
	if err != nil || len(running) != 1 || running[0].ID != a.ID {
		t.Fatalf("RunningAttempts = %+v, %v", running, err)
	}
	early := task.Session{AttemptID: a.ID, Runtime: "claude", PID: 5, ResumedFrom: old.ID}
	if err := db.RecordSession(ctx, &early); !errors.Is(err, ErrConflict) {
		t.Errorf("resume of a live session = %v; want ErrConflict", err)
	}
	inFlight, err := db.InFlightTurn(ctx, old.ID)
	if err != nil || inFlight.ID != turn.ID || inFlight.Message != "go" {
		t.Fatalf("InFlightTurn = %+v, %v", inFlight, err)
	}
	if err := db.EndTurn(ctx, turn.ID, task.TurnInterrupted, "daemon restart"); err != nil {
		t.Fatal(err)
	}
	if err := db.CloseSession(ctx, old.ID); err != nil {
		t.Fatal(err)
	}

	wrong := task.Session{AttemptID: a.ID, Runtime: "codex", PID: 5, ResumedFrom: old.ID}
	if err := db.RecordSession(ctx, &wrong); !errors.Is(err, task.ErrInvalid) {
		t.Errorf("resume on another runtime = %v; want ErrInvalid", err)
	}
	resumed := task.Session{AttemptID: a.ID, Runtime: "claude", PID: 6, StartToken: 8, ResumedFrom: old.ID}
	if err := db.RecordSession(ctx, &resumed); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if resumed.NativeID != "n1" {
		t.Fatalf("resumed native id = %q", resumed.NativeID)
	}
	if err := db.OpenSession(ctx, resumed.ID, "n2"); !errors.Is(err, task.ErrInvalid) {
		t.Errorf("open with another native id = %v; want ErrInvalid", err)
	}
	if err := db.OpenSession(ctx, resumed.ID, "n1"); err != nil {
		t.Fatalf("open resumed: %v", err)
	}
	retry := task.Turn{SessionID: resumed.ID, Kind: task.TurnRecovery, Message: "The orchestrator restarted…\n\ngo"}
	if err := db.BeginTurn(ctx, &retry); err != nil || retry.Seq != 1 {
		t.Fatalf("recovery turn: %+v, %v", retry, err)
	}
	all, err := db.TaskSessions(ctx, tk.ID, false)
	if err != nil || len(all) != 2 || all[1].ResumedFrom != old.ID || all[0].ResumedFrom != 0 {
		t.Fatalf("TaskSessions = %+v, %v", all, err)
	}
}

// A session that never reported its native id cannot be resumed.
func TestResumeNeedsNativeID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openDomain(t)
	_, a := startAttempt(t, db)
	s := task.Session{AttemptID: a.ID, Runtime: "claude", PID: 5}
	if err := db.RecordSession(ctx, &s); err != nil {
		t.Fatal(err)
	}
	if err := db.CloseSession(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	r := task.Session{AttemptID: a.ID, Runtime: "claude", PID: 6, ResumedFrom: s.ID}
	if err := db.RecordSession(ctx, &r); !errors.Is(err, task.ErrInvalid) {
		t.Fatalf("resume without native id = %v; want ErrInvalid", err)
	}
}

func TestSetSessionProcess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openDomain(t)
	_, a := startAttempt(t, db)
	s := openSession(t, db, a)
	if err := db.SetSessionProcess(ctx, s.ID, 0, 1); !errors.Is(err, task.ErrInvalid) {
		t.Errorf("pid 0 = %v", err)
	}
	if err := db.SetSessionProcess(ctx, s.ID, 777, 99); err != nil {
		t.Fatal(err)
	}
	live, err := db.LiveSessions(ctx)
	if err != nil || live[0].PID != 777 || live[0].StartToken != 99 {
		t.Fatalf("after respawn = %+v, %v", live, err)
	}
	if err := db.CloseSession(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSessionProcess(ctx, s.ID, 778, 100); !errors.Is(err, ErrConflict) {
		t.Errorf("closed session = %v; want ErrConflict", err)
	}
}

func TestSetPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openDomain(t)
	tk, _ := newRunTask(t)
	tk.Kind, tk.Status = task.KindPipeline, task.StatusPlanning
	if err := db.CreateTask(ctx, &tk, nil); err != nil {
		t.Fatal(err)
	}
	if err := db.SetPlan(ctx, tk.ID, []task.Step{planStep(1), planStep(2)}); err != nil {
		t.Fatalf("SetPlan: %v", err)
	}
	if _, err := db.TransitionTask(ctx, tk.ID, task.StatusPlanning, task.StatusAwaitingApproval, TaskChange{}); err != nil {
		t.Fatal(err)
	}
	edited := []task.Step{planStep(1)}
	edited[0].Title = "edited"
	if err := db.SetPlan(ctx, tk.ID, edited); err != nil {
		t.Fatalf("edit during approval: %v", err)
	}
	steps, err := db.Steps(ctx, tk.ID)
	if err != nil || len(steps) != 1 || steps[0].Title != "edited" {
		t.Fatalf("Steps = %+v, %v", steps, err)
	}
	if _, err := db.TransitionTask(ctx, tk.ID, task.StatusAwaitingApproval, task.StatusRunning, TaskChange{}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetPlan(ctx, tk.ID, []task.Step{planStep(1)}); !errors.Is(err, ErrConflict) {
		t.Errorf("SetPlan while running = %v; want ErrConflict", err)
	}
	used := planStep(1)
	used.Retries = 1
	if err := db.SetPlan(ctx, tk.ID, []task.Step{used}); !errors.Is(err, task.ErrInvalid) {
		t.Errorf("non-fresh step = %v; want ErrInvalid", err)
	}
	run := createRun(t, db)
	if err := db.SetPlan(ctx, run.ID, []task.Step{planStep(1)}); !errors.Is(err, task.ErrInvalid) {
		t.Errorf("plan for a run task = %v; want ErrInvalid", err)
	}
}

func TestRetryOnlyWhileRunning(t *testing.T) {
	t.Parallel()
	db := openDomain(t)
	tk := createRun(t, db)
	_, err := db.TransitionStep(context.Background(), tk.ID, 1, task.StepPending, task.StepRunning, StepChange{CountRetry: true})
	if !errors.Is(err, task.ErrInvalid) {
		t.Fatalf("retry on start = %v; want ErrInvalid", err)
	}
}

func TestDeleteTaskCascades(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openDomain(t)
	tk, a := startAttempt(t, db)
	s := openSession(t, db, a)
	if err := db.BeginTurn(ctx, &task.Turn{SessionID: s.ID, Kind: task.TurnPrompt, Message: "go"}); err != nil {
		t.Fatal(err)
	}
	if err := db.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, "DELETE FROM tasks WHERE id = ?", string(tk.ID))
		return err
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for _, table := range []string{"steps", "attempts", "sessions", "turns"} {
		var n int
		if err := db.Reader().QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s: %d rows left, %v", table, n, err)
		}
	}
}

// The SQL terminal list and the migration's partial index must match the domain.
func TestTerminalListMatchesDomain(t *testing.T) {
	t.Parallel()
	if terminalSQL != "('cancelled', 'done', 'failed')" {
		t.Fatalf("terminalSQL = %s", terminalSQL)
	}
	body, err := migrationFiles.ReadFile("migrations/0002_tasks.sql")
	if err != nil {
		t.Fatal(err)
	}
	var index string
	for line := range strings.SplitSeq(string(body), "\n") {
		if strings.HasPrefix(line, "CREATE INDEX tasks_active") {
			index = line
		}
	}
	for _, s := range []task.Status{
		task.StatusPlanning, task.StatusAwaitingApproval, task.StatusRunning, task.StatusReviewing,
		task.StatusWaitingQuota, task.StatusNeedsAttention, task.StatusDone, task.StatusFailed, task.StatusCancelled,
	} {
		if strings.Contains(index, "'"+string(s)+"'") != s.Terminal() {
			t.Errorf("tasks_active index and %s disagree: %s", s, index)
		}
	}
}

func planStep(i int) task.Step {
	return task.Step{Index: i, Title: "t", Prompt: "p", Importance: task.ImportanceNormal, Status: task.StepPending}
}
