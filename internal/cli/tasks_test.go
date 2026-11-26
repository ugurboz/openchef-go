package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/config"
	"github.com/ugurboz/openchef/internal/daemon"
	"github.com/ugurboz/openchef/internal/doctor"
	"github.com/ugurboz/openchef/internal/engine"
	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/runtime/fake"
	"github.com/ugurboz/openchef/internal/service"
	"github.com/ugurboz/openchef/internal/store"
	"github.com/ugurboz/openchef/internal/task"
)

// queued sends every request and question to the daemon's approval queue, like the
// real approver does for everything but file changes.
type queued struct {
	svc   *service.Service
	task  task.ID
	agent string
}

func (q queued) Approve(ctx context.Context, req runtime.ApprovalRequest) (runtime.Decision, error) {
	a, err := q.svc.Ask(ctx, service.Request{Task: q.task, Agent: q.agent, Kind: task.ApprovalKind(req.Kind), Tool: req.Tool, Summary: req.Command})
	if err != nil {
		return runtime.Decision{}, err
	}
	if a.Status == task.ApprovalAllowed {
		return runtime.Allow(), nil
	}
	return runtime.Deny("denied: " + a.Answer), nil
}

func (q queued) Answer(ctx context.Context, qu runtime.Question) (string, error) {
	a, err := q.svc.Ask(ctx, service.Request{Task: q.task, Agent: q.agent, Kind: task.ApprovalQuestion, Summary: qu.Text, Options: qu.Options})
	if err != nil {
		return "", err
	}
	return a.Answer, nil
}

// cliEnv is a repository, a home directory and a daemon that starts on demand, with a
// fake agent instead of real CLIs.
type cliEnv struct {
	t      *testing.T
	repo   string
	paths  config.Paths
	claude *fake.Runtime
	deps   Deps

	once  sync.Once
	stop  func()
	quota *service.Quota
}

func newCLIEnv(t *testing.T) *cliEnv {
	t.Helper()
	deps, paths := daemonDeps(t)
	e := &cliEnv{t: t, paths: paths, claude: fake.New("claude"), repo: filepath.Join(t.TempDir(), "repo")}
	if err := os.MkdirAll(e.repo, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "--quiet", "--initial-branch=main"},
		{"config", "user.name", "Test"}, {"config", "user.email", "t@example.com"},
		{"commit", "--quiet", "--allow-empty", "-m", "initial"},
	} {
		if out, err := exec.CommandContext(context.Background(), "git", append([]string{"-C", e.repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	deps.StartDaemon = func(context.Context, config.Paths) error {
		e.once.Do(e.startDaemon)
		return nil
	}
	e.deps = deps
	t.Cleanup(func() {
		if e.stop != nil {
			e.stop()
		}
	})
	return e
}

func (e *cliEnv) startDaemon() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	opts := daemon.Options{Paths: e.paths, Version: "test", Init: func(ctx context.Context) (daemon.Services, error) {
		db, err := store.Open(ctx, e.paths.State)
		if err != nil {
			return daemon.Services{}, err
		}
		var svc *service.Service
		svc, err = service.New(service.Deps{
			Store: db, EventDir: e.paths.Sessions,
			NewEngine: func(sink engine.Sink) (*engine.Engine, error) {
				return engine.New(engine.Config{
					Store: db, Runtimes: map[string]runtime.Runtime{"claude": e.claude}, Sink: sink,
					Router: &engine.Ordered{Agents: []engine.Agent{{Name: "opus", Runtime: "claude", Pool: "claude-max"}}},
					SpecFor: func(t *task.Task, a engine.Agent) (runtime.SessionSpec, error) {
						return runtime.SessionSpec{Approver: queued{svc: svc, task: t.ID, agent: a.Name}}, nil
					},
					WorktreeRoot: e.paths.Worktrees, SessionRoot: e.paths.Sessions,
				})
			},
		})
		if err != nil {
			return daemon.Services{}, err
		}
		quotas, err := service.NewQuota(service.QuotaDeps{Store: db})
		if err != nil {
			return daemon.Services{}, err
		}
		if err := quotas.Start(ctx); err != nil {
			return daemon.Services{}, err
		}
		methods := daemon.ServiceMethods(svc)
		maps.Copy(methods, daemon.QuotaMethods(quotas))
		e.quota = quotas
		return daemon.Services{Methods: methods, OnStop: func(ctx context.Context) error {
			err := svc.Stop(ctx)
			quotas.Stop()
			return errors.Join(err, db.Close())
		}}, nil
	}}
	go func() { done <- daemon.Run(ctx, opts) }()
	e.stop = func() { cancel(); <-done } //nolint:errcheck // a test teardown; Run's result is not under test here
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(e.paths.Socket); err == nil {
			return
		}
		if time.Now().After(deadline) {
			e.t.Error("daemon did not start")
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// cli runs openchef with args against the env's daemon.
func (e *cliEnv) cli(args ...string) (string, error) {
	e.t.Helper()
	var out, errOut bytes.Buffer
	cmd := NewRootCommandWith(IO{Out: &out, Err: &errOut}, e.deps)
	cmd.SetArgs(args)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err := cmd.ExecuteContext(ctx)
	return out.String(), err
}

func taskIDFrom(t *testing.T, out string) string {
	t.Helper()
	f := strings.Fields(out)
	if len(f) < 2 || f[0] != "task" {
		t.Fatalf("no task id in %q", out)
	}
	return f[1]
}

// chdir enters the repository: `run` uses the working directory. Tests that call it
// cannot run in parallel.
func chdir(t *testing.T, dir string) {
	t.Helper()
	t.Chdir(dir)
}

func TestRunStreamsLiveAndFinishes(t *testing.T) {
	e := newCLIEnv(t)
	chdir(t, e.repo)
	e.claude.Push(fake.Turn{
		Events: []runtime.Event{
			{Kind: runtime.EventText, Text: "Adding the file.\n"},
			{Kind: runtime.EventToolCall, ToolCall: &runtime.ToolCall{Name: "Bash", Input: json.RawMessage(`{"command":"go test ./..."}`)}},
		},
		Edit: func(dir string) error { return os.WriteFile(filepath.Join(dir, "out.txt"), []byte("x"), 0o600) },
	})
	out, err := e.cli("run", "add", "out.txt")
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	for _, want := range []string{"task ", "▸ Step 1: add out.txt", "  agent opus", "  Adding the file.", "● Bash  go test ./...", "✓ step 1 done", "✔ done"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	id := taskIDFrom(t, out)

	list, err := e.cli("status", "--all")
	if err != nil || !strings.Contains(list, id) || !strings.Contains(list, "done") {
		t.Errorf("status --all: %v\n%s", err, list)
	}
	detail, err := e.cli("status", id)
	if err != nil || !strings.Contains(detail, "step 1  verified") || !strings.Contains(detail, "attempt") {
		t.Errorf("status <task>: %v\n%s", err, detail)
	}
	logs, err := e.cli("logs", id)
	if err != nil || !strings.Contains(logs, "✓ step 1 done") || !strings.Contains(logs, "● Bash  go test ./...") {
		t.Errorf("logs: %v\n%s", err, logs)
	}
	jsonLogs, err := e.cli("logs", id, "--json")
	if err != nil || !strings.Contains(jsonLogs, `"kind":"step_started"`) {
		t.Errorf("logs --json: %v\n%s", err, jsonLogs)
	}
}

func TestRunFailureExitsNonZero(t *testing.T) {
	e := newCLIEnv(t)
	chdir(t, e.repo)
	e.claude.Push(fake.Failing(runtime.FailAuth, "token expired"))
	out, err := e.cli("run", "-q", "anything")
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != 1 || !strings.Contains(out, "! needs attention") || strings.Contains(out, "▸ Step") {
		t.Fatalf("quiet failing run: %v\n%s", err, out)
	}
}

func TestDetachThenCancel(t *testing.T) {
	e := newCLIEnv(t)
	chdir(t, e.repo)
	e.claude.Push(fake.Turn{Hang: true})
	out, err := e.cli("run", "--detach", "hang")
	if err != nil || !strings.Contains(out, "started; follow it with: openchef logs") {
		t.Fatalf("detach: %v\n%s", err, out)
	}
	id := strings.Fields(out)[1]
	deadline := time.Now().Add(5 * time.Second)
	for len(e.claude.Turns()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if out, err := e.cli("cancel", id); err != nil || !strings.Contains(out, "cancelled") {
		t.Fatalf("cancel: %v\n%s", err, out)
	}
	if out, err := e.cli("cancel", id); err == nil || !strings.Contains(err.Error(), "already cancelled") {
		t.Errorf("second cancel: %v\n%s", err, out)
	}
	if out, err := e.cli("status", "nonsense!"); err == nil {
		t.Errorf("bad id accepted: %s", out)
	}
}

func TestCommandsWithoutDaemon(t *testing.T) {
	deps, _ := daemonDeps(t)
	deps.Detector = doctor.New()
	var out bytes.Buffer
	cmd := NewRootCommandWith(IO{Out: &out, Err: &out}, deps)
	cmd.SetArgs([]string{"status"})
	if err := cmd.ExecuteContext(context.Background()); err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("status without daemon = %v", err)
	}
}
