package e2e

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/api"
	"github.com/ugurboz/openchef/internal/config"
	"github.com/ugurboz/openchef/internal/daemon"
	"github.com/ugurboz/openchef/internal/engine"
	"github.com/ugurboz/openchef/internal/procs"
	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/service"
	"github.com/ugurboz/openchef/internal/store"
	"github.com/ugurboz/openchef/internal/task"
)

const (
	stubbornEnv   = "OPENCHEF_E2E_STUBBORN"
	grandchildEnv = "OPENCHEF_E2E_GRANDCHILD"
	agentEnv      = "OPENCHEF_E2E_AGENT"
	daemonEnv     = "OPENCHEF_E2E_DAEMON" // home directory
	flagEnv       = "OPENCHEF_E2E_FLAG"   // turns complete once this file exists
)

type allow struct{}

func (allow) Approve(context.Context, runtime.ApprovalRequest) (runtime.Decision, error) {
	return runtime.Allow(), nil
}
func (allow) Answer(context.Context, runtime.Question) (string, error) { return "", nil }

// TestMain turns the test binary into the agent child or the daemon process.
func TestMain(m *testing.M) {
	switch {
	case os.Getenv(grandchildEnv) != "":
		time.Sleep(time.Hour)
		os.Exit(0)
	case os.Getenv(agentEnv) != "":
		os.Exit(agentMain())
	case os.Getenv(daemonEnv) != "":
		if err := daemonMain(os.Getenv(daemonEnv), os.Getenv(flagEnv)); err != nil {
			_, _ = os.Stderr.WriteString("daemon: " + err.Error() + "\n")
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// daemonMain is a real daemon wired like app.Daemon, with the process-backed fake agent.
func daemonMain(home, flag string) error {
	paths, err := config.PathsIn(home)
	if err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	rt := &procRuntime{exe: exe, flag: flag}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	return daemon.Run(context.Background(), daemon.Options{Paths: paths, Version: "e2e", Log: log,
		Init: func(ctx context.Context) (daemon.Services, error) {
			db, err := store.Open(ctx, paths.State)
			if err != nil {
				return daemon.Services{}, err
			}
			svc, err := service.New(service.Deps{Store: db, EventDir: paths.Sessions, Log: log,
				NewEngine: func(sink engine.Sink) (*engine.Engine, error) {
					return engine.New(engine.Config{
						Store: db, Runtimes: map[string]runtime.Runtime{"claude": rt}, Sink: sink,
						Router: &engine.Ordered{Agents: []engine.Agent{{Name: "opus", Runtime: "claude", Pool: "claude-max"}}},
						SpecFor: func(*task.Task, engine.Agent) (runtime.SessionSpec, error) {
							return runtime.SessionSpec{Approver: allow{}, Env: os.Environ()}, nil
						},
						WorktreeRoot: paths.Worktrees, SessionRoot: paths.Sessions,
					})
				}})
			if err != nil {
				return daemon.Services{}, err
			}
			if err := svc.Start(ctx); err != nil {
				return daemon.Services{}, err
			}
			return daemon.Services{Methods: daemon.TaskMethods(svc), OnStop: func(ctx context.Context) error {
				if err := svc.Stop(ctx); err != nil {
					return err // as app.Daemon: never close the store under runs still stopping
				}
				return db.Close()
			}}, nil
		}})
}

func startDaemon(t *testing.T, paths config.Paths, flag string, stubborn bool) api.InitializeResult {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	info, err := daemon.StartDetached(context.Background(), daemon.Detached{
		Exe: exe, Args: []string{"-test.run=^$"},
		Env:     daemonEnvFor(paths, flag, stubborn),
		LogPath: filepath.Join(paths.Logs, "daemon.log"), Socket: paths.Socket, Timeout: 20 * time.Second,
	})
	if err != nil {
		log, _ := os.ReadFile(filepath.Join(paths.Logs, "daemon.log"))
		t.Fatalf("start daemon: %v\n%s", err, log)
	}
	return info
}

func daemonEnvFor(paths config.Paths, flag string, stubborn bool) []string {
	env := append(os.Environ(), daemonEnv+"="+paths.Home, flagEnv+"="+flag)
	if stubborn {
		env = append(env, stubbornEnv+"=1")
	}
	return env
}

func dial(t *testing.T, paths config.Paths) *api.Client {
	t.Helper()
	c, err := api.Dial(context.Background(), paths.Socket, "e2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func waitUntil(t *testing.T, what string, limit time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// agentProcesses reads the identities of every recorded agent process from the store.
func agentProcesses(t *testing.T, paths config.Paths, id string) []procs.Identity {
	t.Helper()
	db, err := store.Open(context.Background(), paths.State)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	sessions, err := db.TaskSessions(context.Background(), task.ID(id), false)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]procs.Identity, 0, len(sessions))
	for i := range sessions {
		out = append(out, procs.Identity{PID: sessions[i].PID, StartToken: sessions[i].StartToken})
	}
	return out
}

// TestKillTheDaemon is M3's exit criterion (§19): a task survives its daemon being
// killed mid-turn, no agent process outlives the dead daemon, and the restarted daemon
// finishes the task in the same conversation.
func TestKillTheDaemon(t *testing.T) { killTheDaemon(t, false) }

// TestKillTheDaemonStubbornAgent: the agent ignores SIGTERM and stdin EOF and has a
// grandchild, so it survives its daemon; the restarted daemon's sweep (§5.3 step 2)
// must kill both before resuming.
func TestKillTheDaemonStubbornAgent(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("the job object kills the agent tree with its daemon on Windows")
	}
	killTheDaemon(t, true)
}

func killTheDaemon(t *testing.T, stubborn bool) {
	if testing.Short() {
		t.Skip("starts real daemon processes")
	}
	dir, err := os.MkdirTemp("", "oce2e")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	paths, err := config.PathsIn(filepath.Join(dir, "h"))
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(dir, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "--quiet", "--initial-branch=main"},
		{"config", "user.name", "Test"}, {"config", "user.email", "t@example.com"},
		{"commit", "--quiet", "--allow-empty", "-m", "initial"},
	} {
		if out, err := exec.CommandContext(context.Background(), "git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	flag := filepath.Join(dir, "go.flag")
	ctx := context.Background()

	first := startDaemon(t, paths, flag, stubborn)
	t.Cleanup(func() { // whatever happens, leave no daemon behind
		if c, err := api.Dial(ctx, paths.Socket, "cleanup"); err == nil {
			_ = c.Stop(ctx)
			_ = c.Close()
		}
	})
	c := dial(t, paths)
	tk, err := c.Run(ctx, api.RunParams{Prompt: "write done.txt", Dir: repo})
	if err != nil {
		t.Fatal(err)
	}
	// The turn is in flight once a turn row exists for the agent's session.
	var agents []procs.Identity
	waitUntil(t, "the agent process", 20*time.Second, func() bool {
		agents = agentProcesses(t, paths, tk.ID)
		if len(agents) == 0 {
			return false
		}
		alive, _ := procs.Alive(agents[0])
		return alive
	})
	time.Sleep(200 * time.Millisecond) // let the turn start

	// SIGKILL (TerminateProcess on Windows): no graceful shutdown of any kind.
	p, err := os.FindProcess(first.PID)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Kill(); err != nil {
		t.Fatalf("kill daemon: %v", err)
	}
	waitUntil(t, "the daemon to die", 10*time.Second, func() bool {
		_, err := api.Dial(ctx, paths.Socket, "probe")
		return errors.Is(err, api.ErrNotRunning)
	})
	var grandchild procs.Identity
	if stubborn {
		// It survives its daemon, which is the point of this variant.
		d, err := api.Dial(ctx, paths.Socket, "probe")
		if err == nil {
			_ = d.Close()
			t.Fatal("daemon still answering")
		}
		grandchild = grandchildOf(t, paths, tk.ID)
		time.Sleep(time.Second)
		for _, id := range []procs.Identity{agents[0], grandchild} {
			if alive, _ := procs.Alive(id); !alive {
				t.Fatalf("stubborn process %d died with the daemon; the test proves nothing", id.PID)
			}
		}
	} else {
		// No leaked processes: the agent dies with its daemon (Pdeathsig on Linux,
		// stdin EOF on macOS, job object on Windows), before any new daemon could
		// sweep it.
		waitUntil(t, "the orphaned agent to exit", 10*time.Second, func() bool {
			alive, err := procs.Alive(agents[0])
			return err == nil && !alive
		})
	}

	if err := os.WriteFile(flag, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	second := startDaemon(t, paths, flag, stubborn)
	if second.PID == first.PID {
		t.Fatal("same daemon pid")
	}
	if stubborn {
		waitUntil(t, "the sweep to kill the leftover agent and its child", 10*time.Second, func() bool {
			a, _ := procs.Alive(agents[0])
			g, _ := procs.Alive(grandchild)
			return !a && !g
		})
		log, _ := os.ReadFile(filepath.Join(paths.Logs, "daemon.log"))
		if !strings.Contains(string(log), "killed processes left by a previous daemon") {
			t.Errorf("the second daemon did not report its sweep:\n%s", log)
		}
	}
	c2 := dial(t, paths)
	var d api.TaskDetail
	waitUntil(t, "the task to finish after the restart", 30*time.Second, func() bool {
		d, err = c2.Get(ctx, tk.ID)
		return err == nil && api.Finished(d.Task.Status)
	})
	if d.Task.Status != string(task.StatusDone) {
		t.Fatalf("task %s: %s", d.Task.Status, d.Task.Reason)
	}
	if len(d.Attempts) != 1 || d.Steps[0].Commit == "" {
		t.Fatalf("detail %+v; the same attempt must finish", d)
	}
	done, err := exec.CommandContext(ctx, "git", "-C", d.Task.Worktree, "show", d.Steps[0].Commit+":done.txt").Output()
	if err != nil || !strings.HasPrefix(string(done), "The orchestrator restarted during your last turn") {
		t.Fatalf("done.txt %q, %v; the resumed turn must carry the recovery note", done, err)
	}
	for _, id := range agentProcesses(t, paths, tk.ID) {
		if alive, err := procs.Alive(id); err != nil || alive {
			t.Errorf("agent process %d still alive after the task finished", id.PID)
		}
	}
	if err := c2.Stop(ctx); err != nil {
		t.Fatal(err)
	}
}

// grandchildOf reads the identity of the stubborn agent's child from the file the
// agent writes next to its workspace.
func grandchildOf(t *testing.T, paths config.Paths, id string) procs.Identity {
	t.Helper()
	db, err := store.Open(context.Background(), paths.State)
	if err != nil {
		t.Fatal(err)
	}
	tk, err := db.Task(context.Background(), task.ID(id))
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(filepath.Dir(tk.Worktree), "grandchild-"+filepath.Base(tk.Worktree)+".pid")
	var g procs.Identity
	waitUntil(t, "the grandchild pid file", 10*time.Second, func() bool {
		raw, err := os.ReadFile(file)
		if err != nil {
			return false
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
		if err != nil {
			return false
		}
		tok, err := procs.StartToken(pid)
		if err != nil {
			return false
		}
		g = procs.Identity{PID: pid, StartToken: tok}
		return true
	})
	return g
}
