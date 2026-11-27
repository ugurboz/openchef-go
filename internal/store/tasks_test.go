package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/task"
)

// fixedClock makes stored timestamps deterministic and strictly increasing.
type fixedClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fixedClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Second)
	return c.t
}

func openDomain(t *testing.T) *DB {
	t.Helper()
	db, err := Open(context.Background(), dbPath(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	clock := &fixedClock{t: time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)}
	db.now = clock.now
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return db
}

func newRunTask(t *testing.T) (task.Task, []task.Step) {
	t.Helper()
	id, err := task.NewID()
	if err != nil {
		t.Fatal(err)
	}
	tk := task.Task{
		ID: id, Kind: task.KindRun, Status: task.StatusRunning, Prompt: "add rate limiting",
		RepoRoot: "/repo", Mode: task.ModeIsolated, Backend: task.BackendLocal,
		Importance: task.ImportanceNormal, Permissions: task.PermissionsAcceptEdits,
	}
	steps := []task.Step{{
		Index: 1, Title: "add rate limiting", Prompt: "add rate limiting",
		Files: []string{"api/**"}, Verify: "go test ./...", Importance: task.ImportanceNormal,
		Status: task.StepPending,
	}}
	return tk, steps
}

func createRun(t *testing.T, db *DB) task.Task {
	t.Helper()
	tk, steps := newRunTask(t)
	if err := db.CreateTask(context.Background(), &tk, steps); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	return tk
}

func TestCreateAndReadTask(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openDomain(t)
	tk, steps := newRunTask(t)
	tk.AgentPin = "opus"
	if err := db.CreateTask(ctx, &tk, steps); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if tk.CreatedAt.IsZero() || !tk.CreatedAt.Equal(tk.UpdatedAt) {
		t.Fatalf("timestamps not set: %v %v", tk.CreatedAt, tk.UpdatedAt)
	}
	got, err := db.Task(ctx, tk.ID)
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if got != tk {
		t.Fatalf("Task = %+v\nwant   %+v", got, tk)
	}
	gotSteps, err := db.Steps(ctx, tk.ID)
	if err != nil {
		t.Fatalf("Steps: %v", err)
	}
	if len(gotSteps) != 1 || !slices.Equal(gotSteps[0].Files, []string{"api/**"}) ||
		gotSteps[0].Verify != "go test ./..." || gotSteps[0].TaskID != tk.ID {
		t.Fatalf("Steps = %+v", gotSteps)
	}
}

func TestCreateTaskRejects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openDomain(t)
	cases := map[string]func(*task.Task, *[]task.Step){
		"invalid task":       func(tk *task.Task, _ *[]task.Step) { tk.Prompt = "" },
		"wrong initial":      func(tk *task.Task, _ *[]task.Step) { tk.Status = task.StatusPlanning },
		"run without step":   func(_ *task.Task, s *[]task.Step) { *s = nil },
		"run with two steps": func(_ *task.Task, s *[]task.Step) { *s = append(*s, (*s)[0]) },
		"bad step":           func(_ *task.Task, s *[]task.Step) { (*s)[0].Title = "" },
		"pipeline with steps": func(tk *task.Task, _ *[]task.Step) {
			tk.Kind, tk.Status = task.KindPipeline, task.StatusPlanning
		},
	}
	for name, mut := range cases {
		tk, steps := newRunTask(t)
		mut(&tk, &steps)
		if err := db.CreateTask(ctx, &tk, steps); !errors.Is(err, task.ErrInvalid) {
			t.Errorf("%s: CreateTask = %v; want task.ErrInvalid", name, err)
		}
	}
	all, err := db.Tasks(ctx, TaskFilter{})
	if err != nil || len(all) != 0 {
		t.Fatalf("rejected creates left rows: %d, %v", len(all), err)
	}
}

func TestCreatePipelineWithoutSteps(t *testing.T) {
	t.Parallel()
	db := openDomain(t)
	tk, _ := newRunTask(t)
	tk.Kind, tk.Status = task.KindPipeline, task.StatusPlanning
	if err := db.CreateTask(context.Background(), &tk, nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
}

func TestCreateDuplicateID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openDomain(t)
	tk := createRun(t, db)
	dup, steps := newRunTask(t)
	dup.ID = tk.ID
	if err := db.CreateTask(ctx, &dup, steps); err == nil {
		t.Fatal("duplicate id accepted")
	}
	if _, err := db.Steps(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}
}

func TestTaskNotFound(t *testing.T) {
	t.Parallel()
	db := openDomain(t)
	if _, err := db.Task(context.Background(), "0000000000"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Task = %v; want ErrNotFound", err)
	}
	_, err := db.TransitionTask(context.Background(), "0000000000", task.StatusRunning, task.StatusDone, TaskChange{})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("TransitionTask = %v; want ErrNotFound", err)
	}
}

func TestTransitionTask(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openDomain(t)
	tk := createRun(t, db)
	resume := time.Date(2026, 11, 25, 15, 0, 0, 0, time.UTC)

	got, err := db.TransitionTask(ctx, tk.ID, task.StatusRunning, task.StatusWaitingQuota, TaskChange{ResumeAt: resume})
	if err != nil {
		t.Fatalf("→ waiting_quota: %v", err)
	}
	if got.Status != task.StatusWaitingQuota || !got.ResumeAt.Equal(resume) || !got.UpdatedAt.After(tk.UpdatedAt) {
		t.Fatalf("after waiting_quota: %+v", got)
	}
	got, err = db.TransitionTask(ctx, tk.ID, task.StatusWaitingQuota, task.StatusRunning, TaskChange{Reason: "ignored"})
	if err != nil {
		t.Fatalf("→ running: %v", err)
	}
	if !got.ResumeAt.IsZero() || got.Reason != "" {
		t.Fatalf("resume_at/reason not cleared: %+v", got)
	}
	got, err = db.TransitionTask(ctx, tk.ID, task.StatusRunning, task.StatusNeedsAttention, TaskChange{Reason: "retries exhausted"})
	if err != nil || got.Reason != "retries exhausted" {
		t.Fatalf("→ needs_attention: %+v, %v", got, err)
	}
}

func TestTransitionTaskRejects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openDomain(t)
	tk := createRun(t, db)

	if _, err := db.TransitionTask(ctx, tk.ID, task.StatusRunning, task.StatusPlanning, TaskChange{}); !errors.Is(err, task.ErrInvalidTransition) {
		t.Errorf("forbidden move = %v; want ErrInvalidTransition", err)
	}
	if _, err := db.TransitionTask(ctx, tk.ID, task.StatusRunning, task.StatusWaitingQuota, TaskChange{}); !errors.Is(err, task.ErrInvalid) {
		t.Errorf("waiting_quota without resume = %v; want ErrInvalid", err)
	}
	_, err := db.TransitionTask(ctx, tk.ID, task.StatusNeedsAttention, task.StatusRunning, TaskChange{})
	var ce *ConflictError
	if !errors.As(err, &ce) || !errors.Is(err, ErrConflict) || ce.Got != string(task.StatusRunning) {
		t.Errorf("stale from = %v; want ConflictError got=running", err)
	}
	if _, err := db.TransitionTask(ctx, tk.ID, task.StatusRunning, task.StatusFailed, TaskChange{}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.TransitionTask(ctx, tk.ID, task.StatusFailed, task.StatusRunning, TaskChange{}); !errors.Is(err, task.ErrInvalidTransition) {
		t.Errorf("terminal task moved: %v", err)
	}
}

// Exactly one of many concurrent writers wins a compare-and-set; the rest see a conflict.
func TestTransitionTaskConcurrent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openDomain(t)
	tk := createRun(t, db)

	var wins, conflicts atomic.Int32
	var wg sync.WaitGroup
	targets := []task.Status{task.StatusCancelled, task.StatusNeedsAttention, task.StatusFailed}
	for i := range 16 {
		wg.Go(func() {
			_, err := db.TransitionTask(ctx, tk.ID, task.StatusRunning, targets[i%len(targets)], TaskChange{})
			switch {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, ErrConflict):
				conflicts.Add(1)
			default:
				t.Errorf("unexpected: %v", err)
			}
		})
	}
	wg.Wait()
	if wins.Load() != 1 || conflicts.Load() != 15 {
		t.Fatalf("wins=%d conflicts=%d; want 1 and 15", wins.Load(), conflicts.Load())
	}
}

func TestTasksFilter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openDomain(t)
	first := createRun(t, db)
	second := createRun(t, db)
	third := createRun(t, db)
	if _, err := db.TransitionTask(ctx, second.ID, task.StatusRunning, task.StatusFailed, TaskChange{}); err != nil {
		t.Fatal(err)
	}
	ids := func(ts []task.Task) []task.ID {
		out := make([]task.ID, 0, len(ts))
		for i := range ts {
			out = append(out, ts[i].ID)
		}
		return out
	}
	all, err := db.Tasks(ctx, TaskFilter{})
	if err != nil || !slices.Equal(ids(all), []task.ID{third.ID, second.ID, first.ID}) {
		t.Fatalf("all = %v, %v", ids(all), err)
	}
	active, err := db.Tasks(ctx, TaskFilter{Active: true})
	if err != nil || !slices.Equal(ids(active), []task.ID{third.ID, first.ID}) {
		t.Fatalf("active = %v, %v", ids(active), err)
	}
	limited, err := db.Tasks(ctx, TaskFilter{Limit: 1})
	if err != nil || !slices.Equal(ids(limited), []task.ID{third.ID}) {
		t.Fatalf("limited = %v, %v", ids(limited), err)
	}
}

func TestSetWorkspace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openDomain(t)
	tk := createRun(t, db)
	if err := db.SetWorkspace(ctx, tk.ID, "", "/wt", "abc"); !errors.Is(err, task.ErrInvalid) {
		t.Errorf("empty branch = %v", err)
	}
	if err := db.SetWorkspace(ctx, tk.ID, "openchef/"+string(tk.ID), "/wt", "abc123"); err != nil {
		t.Fatal(err)
	}
	got, err := db.Task(ctx, tk.ID)
	if err != nil || got.Branch != "openchef/"+string(tk.ID) || got.Worktree != "/wt" || got.BaseCommit != "abc123" {
		t.Fatalf("workspace = %+v, %v", got, err)
	}
	if _, err := db.TransitionTask(ctx, tk.ID, task.StatusRunning, task.StatusCancelled, TaskChange{}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetWorkspace(ctx, tk.ID, "b", "/wt", "abc"); !errors.Is(err, ErrConflict) {
		t.Errorf("terminal task = %v; want ErrConflict", err)
	}
}

func TestTransitionStep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openDomain(t)
	tk := createRun(t, db)

	s, err := db.TransitionStep(ctx, tk.ID, 1, task.StepPending, task.StepRunning, StepChange{})
	if err != nil || s.Status != task.StepRunning {
		t.Fatalf("→ running: %+v, %v", s, err)
	}
	for range 2 {
		if s, err = db.TransitionStep(ctx, tk.ID, 1, task.StepRunning, task.StepRunning, StepChange{CountRetry: true}); err != nil {
			t.Fatalf("retry: %v", err)
		}
	}
	if s.Retries != 2 {
		t.Fatalf("retries = %d; want 2", s.Retries)
	}
	if _, err := db.TransitionStep(ctx, tk.ID, 1, task.StepRunning, task.StepRunning, StepChange{}); !errors.Is(err, task.ErrInvalidTransition) {
		t.Errorf("self move without retry = %v", err)
	}
	if _, err := db.TransitionStep(ctx, tk.ID, 1, task.StepRunning, task.StepVerified, StepChange{}); !errors.Is(err, task.ErrInvalid) {
		t.Errorf("verified without commit = %v", err)
	}
	if _, err := db.TransitionStep(ctx, tk.ID, 1, task.StepRunning, task.StepPending, StepChange{VerifiedCommit: "x"}); !errors.Is(err, task.ErrInvalid) {
		t.Errorf("commit on pending = %v", err)
	}
	if _, err := db.TransitionStep(ctx, tk.ID, 1, task.StepPending, task.StepRunning, StepChange{}); !errors.Is(err, ErrConflict) {
		t.Errorf("stale from = %v", err)
	}
	if _, err := db.TransitionStep(ctx, tk.ID, 2, task.StepPending, task.StepRunning, StepChange{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing step = %v", err)
	}
	s, err = db.TransitionStep(ctx, tk.ID, 1, task.StepRunning, task.StepVerified, StepChange{VerifiedCommit: "deadbeef"})
	if err != nil || s.VerifiedCommit != "deadbeef" || s.Retries != 2 {
		t.Fatalf("→ verified: %+v, %v", s, err)
	}
}

func TestStepNeedsRunningTask(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openDomain(t)
	tk := createRun(t, db)
	if _, err := db.TransitionTask(ctx, tk.ID, task.StatusRunning, task.StatusNeedsAttention, TaskChange{Reason: "x"}); err != nil {
		t.Fatal(err)
	}
	_, err := db.TransitionStep(ctx, tk.ID, 1, task.StepPending, task.StepRunning, StepChange{})
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Entity != "task" {
		t.Fatalf("step move on paused task = %v; want task conflict", err)
	}
}

// startAttempt drives a fresh run task to the point where an agent session can open.
func startAttempt(t *testing.T, db *DB) (task.Task, task.Attempt) {
	t.Helper()
	ctx := context.Background()
	tk := createRun(t, db)
	if _, err := db.TransitionStep(ctx, tk.ID, 1, task.StepPending, task.StepRunning, StepChange{}); err != nil {
		t.Fatal(err)
	}
	a := task.Attempt{TaskID: tk.ID, StepIndex: 1, Agent: "opus", Runtime: "claude", Model: "claude-opus-5-5", Pool: "claude-max"}
	if err := db.StartAttempt(ctx, &a); err != nil {
		t.Fatalf("StartAttempt: %v", err)
	}
	return tk, a
}

func TestAttempts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openDomain(t)
	tk, a := startAttempt(t, db)
	if a.ID == 0 || a.Status != task.AttemptRunning || a.StartedAt.IsZero() {
		t.Fatalf("attempt = %+v", a)
	}
	second := a
	if err := db.StartAttempt(ctx, &second); !errors.Is(err, ErrConflict) {
		t.Errorf("second running attempt = %v; want ErrConflict", err)
	}
	if err := db.EndAttempt(ctx, a.ID, task.AttemptRunning, ""); !errors.Is(err, task.ErrInvalidTransition) {
		t.Errorf("end as running = %v", err)
	}
	if err := db.EndAttempt(ctx, a.ID, task.AttemptFailedOver, "quota_exhausted"); err != nil {
		t.Fatal(err)
	}
	if err := db.EndAttempt(ctx, a.ID, task.AttemptFailed, ""); !errors.Is(err, ErrConflict) {
		t.Errorf("end twice = %v; want ErrConflict", err)
	}
	next := task.Attempt{TaskID: tk.ID, StepIndex: 1, Agent: "gpt", Runtime: "codex", Pool: "chatgpt-plus"}
	if err := db.StartAttempt(ctx, &next); err != nil {
		t.Fatalf("failover attempt: %v", err)
	}
	all, err := db.Attempts(ctx, tk.ID)
	if err != nil || len(all) != 2 {
		t.Fatalf("Attempts = %v, %v", all, err)
	}
	if all[0].Status != task.AttemptFailedOver || all[0].Outcome != "quota_exhausted" || all[0].EndedAt.IsZero() ||
		all[0].Model != "claude-opus-5-5" || all[1].Model != "" {
		t.Fatalf("Attempts = %+v", all)
	}
}

func TestStartAttemptRejects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openDomain(t)
	tk := createRun(t, db)
	a := task.Attempt{TaskID: tk.ID, StepIndex: 1, Agent: "opus", Runtime: "claude", Pool: "claude-max"}
	if err := db.StartAttempt(ctx, &a); !errors.Is(err, ErrConflict) {
		t.Errorf("pending step = %v; want ErrConflict", err)
	}
	a.StepIndex = 5
	if err := db.StartAttempt(ctx, &a); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing step = %v; want ErrNotFound", err)
	}
	a.Pool = ""
	if err := db.StartAttempt(ctx, &a); !errors.Is(err, task.ErrInvalid) {
		t.Errorf("no pool = %v; want ErrInvalid", err)
	}
}

func TestSessionsAndTurns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openDomain(t)
	_, a := startAttempt(t, db)

	s := task.Session{AttemptID: a.ID, Runtime: "claude", PID: 4242, StartToken: math.MaxUint64, LogDir: "/logs/1"}
	if err := db.RecordSession(ctx, &s); err != nil {
		t.Fatalf("RecordSession: %v", err)
	}
	turn := task.Turn{SessionID: s.ID, Kind: task.TurnPrompt, Message: "do it"}
	if err := db.BeginTurn(ctx, &turn); !errors.Is(err, ErrConflict) {
		t.Errorf("turn on starting session = %v; want ErrConflict", err)
	}
	if err := db.OpenSession(ctx, s.ID, ""); !errors.Is(err, task.ErrInvalid) {
		t.Errorf("empty native id = %v", err)
	}
	if err := db.OpenSession(ctx, s.ID, "sess-1"); err != nil {
		t.Fatal(err)
	}
	if err := db.OpenSession(ctx, s.ID, "sess-1"); !errors.Is(err, ErrConflict) {
		t.Errorf("open twice = %v", err)
	}

	if err := db.BeginTurn(ctx, &turn); err != nil || turn.Seq != 1 || turn.Status != task.TurnSent {
		t.Fatalf("BeginTurn: %+v, %v", turn, err)
	}
	again := task.Turn{SessionID: s.ID, Kind: task.TurnRetry, Message: "retry"}
	if err := db.BeginTurn(ctx, &again); !errors.Is(err, ErrConflict) {
		t.Errorf("second turn in flight = %v; want ErrConflict", err)
	}
	if err := db.EndTurn(ctx, turn.ID, task.TurnCompleted, "completed"); err != nil {
		t.Fatal(err)
	}
	if err := db.EndTurn(ctx, turn.ID, task.TurnFailed, ""); !errors.Is(err, ErrConflict) {
		t.Errorf("end twice = %v", err)
	}
	if err := db.BeginTurn(ctx, &again); err != nil || again.Seq != 2 {
		t.Fatalf("second turn: %+v, %v", again, err)
	}
	if err := db.BeginTurn(ctx, &task.Turn{SessionID: s.ID, Kind: "bogus", Message: "x"}); !errors.Is(err, task.ErrInvalid) {
		t.Errorf("bad kind = %v", err)
	}

	live, err := db.LiveSessions(ctx)
	if err != nil || len(live) != 1 || live[0].StartToken != math.MaxUint64 || live[0].NativeID != "sess-1" ||
		live[0].State != task.SessionOpen || live[0].LogDir != "/logs/1" {
		t.Fatalf("LiveSessions = %+v, %v", live, err)
	}
	turns, err := db.Turns(ctx, s.ID)
	if err != nil || len(turns) != 2 || turns[0].Outcome != "completed" || turns[1].Status != task.TurnSent ||
		turns[1].Kind != task.TurnRetry {
		t.Fatalf("Turns = %+v, %v", turns, err)
	}

	if err := db.CloseSession(ctx, s.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.CloseSession(ctx, s.ID); err != nil {
		t.Errorf("close twice = %v; want nil", err)
	}
	if err := db.CloseSession(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("close missing = %v", err)
	}
	if live, err := db.LiveSessions(ctx); err != nil || len(live) != 0 {
		t.Fatalf("LiveSessions after close = %+v, %v", live, err)
	}
	sessions, err := db.Sessions(ctx, a.ID)
	if err != nil || len(sessions) != 1 || sessions[0].ClosedAt.IsZero() {
		t.Fatalf("Sessions = %+v, %v", sessions, err)
	}
}

func TestRecordSessionRejects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openDomain(t)
	_, a := startAttempt(t, db)
	if err := db.RecordSession(ctx, &task.Session{AttemptID: a.ID, Runtime: "claude"}); !errors.Is(err, task.ErrInvalid) {
		t.Errorf("no pid = %v", err)
	}
	if err := db.RecordSession(ctx, &task.Session{AttemptID: 999, Runtime: "claude", PID: 1}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing attempt = %v", err)
	}
	if err := db.EndAttempt(ctx, a.ID, task.AttemptInterrupted, "daemon restart"); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordSession(ctx, &task.Session{AttemptID: a.ID, Runtime: "claude", PID: 1}); !errors.Is(err, ErrConflict) {
		t.Errorf("ended attempt = %v; want ErrConflict", err)
	}
}

// One CLI conversation can't have two live sessions: their turns would interleave.
func TestNativeIDUnique(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openDomain(t)
	_, a := startAttempt(t, db)
	for i, want := range []bool{true, false} {
		s := task.Session{AttemptID: a.ID, Runtime: "claude", PID: 100 + i}
		if err := db.RecordSession(ctx, &s); err != nil {
			t.Fatal(err)
		}
		err := db.OpenSession(ctx, s.ID, "same")
		if (err == nil) != want || (!want && !errors.Is(err, ErrConflict)) {
			t.Errorf("open %d: err = %v; want ok = %v", i, err, want)
		}
	}
	dup := task.Session{AttemptID: a.ID, Runtime: "claude", PID: 200, NativeID: "same"}
	if err := db.RecordSession(ctx, &dup); !errors.Is(err, ErrConflict) {
		t.Errorf("record live native id = %v; want ErrConflict", err)
	}
	other := task.Session{AttemptID: a.ID, Runtime: "codex", PID: 201, NativeID: "same"}
	if err := db.RecordSession(ctx, &other); err != nil {
		t.Errorf("same id on another runtime = %v", err)
	}
}

// Timestamps sort as text in time order, which the created_at index relies on.
func TestTimeFormatSorts(t *testing.T) {
	t.Parallel()
	a := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	b := a.Add(time.Nanosecond * 1500)
	c := a.Add(time.Second)
	got := []string{formatTime(c), formatTime(a), formatTime(b)}
	slices.Sort(got)
	if !slices.Equal(got, []string{formatTime(a), formatTime(b), formatTime(c)}) {
		t.Fatalf("sorted = %v", got)
	}
	back, err := parseTime(formatTime(b.In(time.FixedZone("x", 3600))))
	if err != nil || !back.Equal(b) {
		t.Fatalf("round trip = %v, %v", back, err)
	}
}

// Tasks created before migration 0004 ran under the M3 policy (D-020): acceptEdits.
func TestPermissionModeMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := dbPath(t)
	early := map[string]string{}
	for _, name := range []string{"0001_meta.sql", "0002_tasks.sql", "0003_approvals.sql"} {
		body, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		early[name] = string(body)
	}
	old, err := open(ctx, path, testMigrations(early))
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Write(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO tasks (id, kind, status, prompt, repo_root, mode, backend, importance, created_at, updated_at)
			VALUES ('0123456789', 'run', 'done', 'p', '/r', 'isolated', 'local', 'normal', ?, ?)`, formatTime(time.Now()), formatTime(time.Now()))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	got, err := db.Task(ctx, "0123456789")
	if err != nil || got.Permissions != task.PermissionsAcceptEdits {
		t.Fatalf("migrated task %+v, %v", got, err)
	}
}
