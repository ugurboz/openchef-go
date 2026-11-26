package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/runtime/fake"
	"github.com/ugurboz/openchef/internal/store"
	"github.com/ugurboz/openchef/internal/task"
	"github.com/ugurboz/openchef/internal/verify"
)

const helperEnv = "OPENCHEF_ENGINE_HELPER"

// TestMain doubles as the verification command: with helperEnv=check:<file> the test
// binary passes when <file> exists in its working directory.
func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		name, arg, _ := strings.Cut(mode, ":")
		if name == "sleep" {
			time.Sleep(time.Hour)
			os.Exit(0)
		}
		if name == "check" {
			if _, err := os.Stat(arg); err != nil {
				fmt.Printf("missing %s\n", arg)
				os.Exit(1)
			}
			fmt.Printf("found %s\n", arg)
			os.Exit(0)
		}
		os.Exit(2)
	}
	os.Exit(m.Run())
}

type allowAll struct{}

func (allowAll) Approve(context.Context, runtime.ApprovalRequest) (runtime.Decision, error) {
	return runtime.Allow(), nil
}

func (allowAll) Answer(context.Context, runtime.Question) (string, error) { return "yes", nil }

// sink collects events.
type sink struct {
	mu  sync.Mutex
	evs []Event
}

func (s *sink) Publish(ev Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evs = append(s.evs, ev)
}

func (s *sink) kinds() []EventKind {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]EventKind, 0, len(s.evs))
	for i := range s.evs {
		if s.evs[i].Kind != EventAgent {
			out = append(out, s.evs[i].Kind)
		}
	}
	return out
}

func (s *sink) notices() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for i := range s.evs {
		if s.evs[i].Kind == EventNotice {
			out = append(out, s.evs[i].Message)
		}
	}
	return out
}

type harness struct {
	t      *testing.T
	db     *store.DB
	repo   string
	claude *fake.Runtime
	codex  *fake.Runtime
	sink   *sink
	cfg    Config
	verify string // verification command

	mu        sync.Mutex
	sleeps    []time.Duration
	exhausted []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	gitInit(t, repo)

	db, err := store.Open(ctx, filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(exe, " \"") {
		t.Skipf("test binary path %q needs quoting", exe)
	}
	h := &harness{
		t: t, db: db, repo: repo, sink: &sink{}, verify: exe,
		claude: fake.New("claude"), codex: fake.New("codex"),
	}
	h.cfg = Config{
		Store:    db,
		Runtimes: map[string]runtime.Runtime{"claude": h.claude, "codex": h.codex},
		Router: &Ordered{Agents: []Agent{
			{Name: "opus", Runtime: "claude", Model: "claude-opus-5-5", Pool: "claude-max", Effort: runtime.EffortMedium},
			{Name: "gpt", Runtime: "codex", Model: "gpt-5", Pool: "chatgpt-plus"},
		}},
		SpecFor: func(*task.Task, Agent) (runtime.SessionSpec, error) {
			return runtime.SessionSpec{Mode: runtime.ModeAcceptEdits, Approver: allowAll{}}, nil
		},
		VerifyEnv:    append(verify.MinimalEnv(os.Environ()), helperEnv+"=check:done.txt"),
		WorktreeRoot: filepath.Join(dir, "home", "worktrees"),
		SessionRoot:  filepath.Join(dir, "home", "sessions"),
		Sink:         h.sink,
		Sleep: func(_ context.Context, d time.Duration) error {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.sleeps = append(h.sleeps, d)
			return nil
		},
		PoolExhausted: func(pool string, _ time.Time) {
			h.mu.Lock()
			defer h.mu.Unlock()
			h.exhausted = append(h.exhausted, pool)
		},
	}
	return h
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "--quiet", "--initial-branch=main"},
		{"config", "user.name", "Test User"},
		{"config", "user.email", "test@example.com"},
	} {
		git(t, dir, args...)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hi\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", ".")
	git(t, dir, "commit", "--quiet", "-m", "initial")
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (h *harness) engine() *Engine {
	h.t.Helper()
	e, err := New(h.cfg)
	if err != nil {
		h.t.Fatal(err)
	}
	return e
}

// runTask creates a run task with one step and returns its id.
func (h *harness) runTask(verifyCmd string) task.ID {
	h.t.Helper()
	id, err := task.NewID()
	if err != nil {
		h.t.Fatal(err)
	}
	tk := task.Task{
		ID: id, Kind: task.KindRun, Status: task.StatusRunning, Prompt: "create done.txt",
		RepoRoot: h.repo, Mode: task.ModeIsolated, Backend: task.BackendLocal, Importance: task.ImportanceNormal, Permissions: task.PermissionsAcceptEdits,
	}
	steps := []task.Step{{
		Index: 1, Title: "create done.txt", Prompt: "Create a file named done.txt.",
		Verify: verifyCmd, Importance: task.ImportanceNormal, Status: task.StepPending,
	}}
	if err := h.db.CreateTask(context.Background(), &tk, steps); err != nil {
		h.t.Fatal(err)
	}
	return id
}

func (h *harness) task(id task.ID) task.Task {
	h.t.Helper()
	tk, err := h.db.Task(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return tk
}

func (h *harness) step(id task.ID) task.Step {
	h.t.Helper()
	steps, err := h.db.Steps(context.Background(), id)
	if err != nil || len(steps) != 1 {
		h.t.Fatalf("steps: %v, %v", steps, err)
	}
	return steps[0]
}

func (h *harness) attempts(id task.ID) []task.Attempt {
	h.t.Helper()
	a, err := h.db.Attempts(context.Background(), id)
	if err != nil {
		h.t.Fatal(err)
	}
	return a
}

// noLiveSessions asserts the engine closed every session, in the fakes and the store.
func (h *harness) noLiveSessions() {
	h.t.Helper()
	if n := h.claude.Live() + h.codex.Live(); n != 0 {
		h.t.Errorf("%d fake sessions still open", n)
	}
	live, err := h.db.LiveSessions(context.Background())
	if err != nil || len(live) != 0 {
		h.t.Errorf("live sessions in store: %+v, %v", live, err)
	}
}

// writeDone is a fake agent edit that satisfies the verification command.
func writeDone(dir string) error {
	return os.WriteFile(filepath.Join(dir, "done.txt"), []byte("done\n"), 0o600)
}

func writeFile(name, body string) func(string) error {
	return func(dir string) error {
		return os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600)
	}
}

func storeChange() store.StepChange { return store.StepChange{} }
