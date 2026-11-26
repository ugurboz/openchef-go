package service

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/engine"
	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/runtime/fake"
	"github.com/ugurboz/openchef/internal/store"
	"github.com/ugurboz/openchef/internal/task"
)

type allowAll struct{}

func (allowAll) Approve(context.Context, runtime.ApprovalRequest) (runtime.Decision, error) {
	return runtime.Allow(), nil
}
func (allowAll) Answer(context.Context, runtime.Question) (string, error) { return "", nil }

type env struct {
	t      *testing.T
	dir    string
	repo   string
	db     *store.DB
	claude *fake.Runtime
	codex  *fake.Runtime
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "--quiet", "--initial-branch=main"},
		{"config", "user.name", "Test"}, {"config", "user.email", "t@example.com"},
		{"commit", "--quiet", "--allow-empty", "-m", "initial"},
	} {
		cmd := exec.CommandContext(context.Background(), "git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	db, err := store.Open(context.Background(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &env{t: t, dir: dir, repo: repo, db: db, claude: fake.New("claude"), codex: fake.New("codex")}
}

// service builds a service over the env's store and fakes (a new one per "daemon").
func (e *env) service() *Service {
	e.t.Helper()
	return e.serviceWith(func(*Deps) {})
}

func (e *env) serviceWith(tweak func(*Deps)) *Service {
	e.t.Helper()
	d := Deps{
		Store:    e.db,
		EventDir: filepath.Join(e.dir, "sessions"),
		NewEngine: func(sink engine.Sink) (*engine.Engine, error) {
			return engine.New(engine.Config{
				Store:    e.db,
				Runtimes: map[string]runtime.Runtime{"claude": e.claude, "codex": e.codex},
				Router: &engine.Ordered{Agents: []engine.Agent{
					{Name: "opus", Runtime: "claude", Pool: "claude-max"},
					{Name: "gpt", Runtime: "codex", Pool: "chatgpt-plus"},
				}},
				SpecFor: func(*task.Task, engine.Agent) (runtime.SessionSpec, error) {
					return runtime.SessionSpec{Approver: allowAll{}}, nil
				},
				WorktreeRoot: filepath.Join(e.dir, "worktrees"),
				SessionRoot:  filepath.Join(e.dir, "sessions"),
				Sink:         sink,
				Sleep:        func(context.Context, time.Duration) error { return nil },
			})
		},
	}
	tweak(&d)
	s, err := New(d)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

func stop(t *testing.T, s *Service) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// waitStatus waits until the task reaches status.
func (e *env) waitStatus(id task.ID, status task.Status) task.Task {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		t, err := e.db.Task(context.Background(), id)
		if err != nil {
			e.t.Fatal(err)
		}
		if t.Status == status {
			return t
		}
		if time.Now().After(deadline) {
			e.t.Fatalf("task %s is %s (%s); want %s", id, t.Status, t.Reason, status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func writeFile(dir string) error {
	return os.WriteFile(filepath.Join(dir, "out.txt"), []byte("x\n"), 0o600)
}

func TestRunAndWatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	s := e.service()
	defer stop(t, s)
	e.claude.Push(fake.Turn{Edit: writeFile, Text: "done"})

	prompt := "Add a file named out.txt\n\nwith one line; this second paragraph is not the title."
	tk, err := s.Run(ctx, RunRequest{Prompt: prompt, Dir: e.repo})
	if err != nil {
		t.Fatal(err)
	}
	past, sub, err := s.Watch(ctx, tk.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Unwatch(tk.ID, sub)
	seen := append([]engine.Event(nil), past...)
	finished := func() bool {
		for _, ev := range seen {
			if ev.Kind == engine.EventTaskStatus && ev.Status == string(task.StatusDone) {
				return true
			}
		}
		return false
	}
	for !finished() {
		select {
		case ev, ok := <-sub.C:
			if !ok {
				t.Fatalf("subscription ended: %v", sub.Err())
			}
			seen = append(seen, ev)
		case <-time.After(10 * time.Second):
			t.Fatalf("no done event; saw %d events", len(seen))
		}
	}
	if seen[0].Kind != engine.EventTaskStatus || seen[0].Message != "created" {
		t.Errorf("first event %+v", seen[0])
	}
	for i := 1; i < len(seen); i++ {
		if seen[i].TaskID != tk.ID {
			t.Fatalf("event of another task: %+v", seen[i])
		}
	}

	d, err := s.Get(ctx, tk.ID)
	if err != nil || d.Task.Status != task.StatusDone || d.Steps[0].Title != "Add a file named out.txt" || len(d.Attempts) != 1 {
		t.Fatalf("detail %+v, %v", d, err)
	}
	// A late viewer replays the same history from the log.
	waitFor(t, func() bool { d, _ := s.Get(ctx, tk.ID); return !d.Driving })
	replayed, late, err := s.Watch(ctx, tk.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	s.Unwatch(tk.ID, late)
	if len(replayed) != len(seen) {
		t.Errorf("replay has %d events; live saw %d", len(replayed), len(seen))
	}
}

func TestRunRejectsBadRequests(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	s := e.service()
	defer stop(t, s)
	if _, err := s.Run(context.Background(), RunRequest{Prompt: "  ", Dir: e.repo}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("empty prompt = %v", err)
	}
	if _, err := s.Run(context.Background(), RunRequest{Prompt: "x", Dir: t.TempDir()}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("not a repository = %v", err)
	}
}

func TestCancelStopsTheRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	s := e.service()
	defer stop(t, s)
	e.claude.Push(fake.Turn{Hang: true})
	tk, err := s.Run(ctx, RunRequest{Prompt: "hang", Dir: e.repo})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(e.claude.Turns()) == 1 })
	got, err := s.Cancel(ctx, tk.ID)
	if err != nil || got.Status != task.StatusCancelled {
		t.Fatalf("Cancel = %+v, %v", got, err)
	}
	waitFor(t, func() bool { d, _ := s.Get(ctx, tk.ID); return !d.Driving })
	if e.claude.Live() != 0 {
		t.Error("agent session still open after cancel")
	}
	if _, err := s.Cancel(ctx, tk.ID); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("second cancel = %v", err)
	}
}

func TestRetryAfterAttention(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	s := e.service()
	defer stop(t, s)
	e.claude.Push(fake.Failing(runtime.FailAuth, "token expired"))
	tk, err := s.Run(ctx, RunRequest{Prompt: "p", Dir: e.repo})
	if err != nil {
		t.Fatal(err)
	}
	e.waitStatus(tk.ID, task.StatusNeedsAttention)
	waitFor(t, func() bool { d, _ := s.Get(ctx, tk.ID); return !d.Driving })
	e.claude.Push(fake.Turn{Edit: writeFile})
	if _, err := s.Retry(ctx, tk.ID); err != nil {
		t.Fatal(err)
	}
	e.waitStatus(tk.ID, task.StatusDone)
	if _, err := s.Retry(ctx, tk.ID); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("retry of a done task = %v", err)
	}
}

// A task parked in waiting_quota wakes by itself when the quota resets.
func TestWaitingQuotaWakesUp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	s := e.service()
	defer stop(t, s)
	reset := time.Now().Add(300 * time.Millisecond)
	e.claude.Push(fake.Turn{Fail: &runtime.Failure{Kind: runtime.FailQuotaExhausted, ResetsAt: reset}})
	e.codex.Push(fake.Turn{Fail: &runtime.Failure{Kind: runtime.FailQuotaExhausted, ResetsAt: reset}})
	e.claude.Push(fake.Turn{Edit: writeFile})
	tk, err := s.Run(ctx, RunRequest{Prompt: "p", Dir: e.repo})
	if err != nil {
		t.Fatal(err)
	}
	// The wait can be shorter than one poll on a loaded machine, so the event log, not
	// the current status, proves the task waited.
	e.waitStatus(tk.ID, task.StatusDone)
	past, sub, err := s.Watch(ctx, tk.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	s.Unwatch(tk.ID, sub)
	var statuses []string
	for _, ev := range past {
		if ev.Kind == engine.EventTaskStatus {
			statuses = append(statuses, ev.Status)
		}
	}
	if !slices.Contains(statuses, string(task.StatusWaitingQuota)) {
		t.Fatalf("task statuses %v: never waited for quota", statuses)
	}
}

// A daemon restart: the new service kills leftovers and resumes the running task.
func TestStartResumesAfterRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	first := e.service()
	e.claude.Push(fake.Turn{Hang: true})
	tk, err := first.Run(ctx, RunRequest{Prompt: "p", Dir: e.repo})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(e.claude.Turns()) == 1 })
	stop(t, first)
	if got := e.waitStatus(tk.ID, task.StatusRunning); got.Status != task.StatusRunning {
		t.Fatal("shutdown changed the task")
	}

	e.claude.Push(fake.Turn{Edit: writeFile})
	second := e.service()
	defer stop(t, second)
	if err := second.Start(ctx); err != nil {
		t.Fatal(err)
	}
	e.waitStatus(tk.ID, task.StatusDone)
	turns := e.claude.Turns()
	if !strings.HasPrefix(turns[len(turns)-1], "The orchestrator restarted") {
		t.Errorf("last turn %q", turns[len(turns)-1])
	}
}

func TestTitle(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("ş", 100)
	if got := title(long); len([]rune(got)) != 72 || !strings.HasSuffix(got, "…") {
		t.Errorf("title(long) = %q", got)
	}
	if got := title("  short  \nmore"); got != "short" {
		t.Errorf("title = %q", got)
	}
}
