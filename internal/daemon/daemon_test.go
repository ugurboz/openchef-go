package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/api"
	"github.com/ugurboz/openchef/internal/config"
	"github.com/ugurboz/openchef/internal/runtime/linerpc"
)

const helperEnv = "OPENCHEF_DAEMON_HELPER"

// TestMain runs a real daemon when helperEnv names a home directory, so StartDetached
// can be tested against an actual detached process.
func TestMain(m *testing.M) {
	if home := os.Getenv(helperEnv); home != "" {
		paths, err := config.PathsIn(home)
		if err == nil {
			err = Run(context.Background(), Options{Paths: paths, Version: "test"})
		}
		if err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// shortHome is a home directory with a socket path under the Unix limit (t.TempDir
// paths on macOS are too long).
func shortHome(t *testing.T) config.Paths {
	t.Helper()
	dir, err := os.MkdirTemp("", "oc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	p, err := config.PathsIn(filepath.Join(dir, "h"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type running struct {
	paths config.Paths
	done  chan error
	stop  context.CancelFunc
}

func start(t *testing.T, opts Options) *running {
	t.Helper()
	if opts.Paths.Home == "" {
		opts.Paths = shortHome(t)
	}
	if opts.Version == "" {
		opts.Version = "test"
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{paths: opts.Paths, done: make(chan error, 1), stop: cancel}
	go func() { r.done <- Run(ctx, opts) }()
	t.Cleanup(func() {
		cancel()
		<-r.done //nolint:errcheck // tests that care check Run's result themselves
	})
	waitUp(t, r)
	return r
}

func waitUp(t *testing.T, r *running) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, err := api.Dial(context.Background(), r.paths.Socket, "test")
		if err == nil {
			_ = c.Close()
			return
		}
		select {
		case err := <-r.done:
			t.Fatalf("daemon exited: %v", err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon not up: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func dial(t *testing.T, r *running) *api.Client {
	t.Helper()
	c, err := api.Dial(context.Background(), r.paths.Socket, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestLockIsExclusive(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "daemon.lock")
	l, err := Acquire(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Acquire(path); !errors.Is(err, ErrRunning) {
		t.Fatalf("second Acquire = %v; want ErrRunning", err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Errorf("second Release = %v", err)
	}
	again, err := Acquire(path)
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
	_ = again.Release()
}

func TestStatusAndSingleInstance(t *testing.T) {
	t.Parallel()
	r := start(t, Options{})
	c := dial(t, r)
	if c.Daemon.Version != api.Version || c.Daemon.DaemonVersion != "test" || c.Daemon.PID != os.Getpid() {
		t.Fatalf("handshake %+v", c.Daemon)
	}
	st, err := c.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Socket != r.paths.Socket || st.Connections != 1 || time.Since(st.StartedAt) > time.Minute {
		t.Fatalf("status %+v", st)
	}
	if err := Run(context.Background(), Options{Paths: r.paths}); !errors.Is(err, ErrRunning) {
		t.Fatalf("second daemon = %v; want ErrRunning", err)
	}
	// The refused second daemon must not have removed the first one's socket.
	if _, err := c.Status(context.Background()); err != nil {
		t.Fatalf("first daemon after refused second: %v", err)
	}
	if info, err := os.Stat(r.paths.Socket); err != nil || (info.Mode().Perm()&0o077 != 0 && !isWindows()) {
		t.Errorf("socket %v, %v; want owner-only", info, err)
	}
}

func TestStopRemovesSocketAndLock(t *testing.T) {
	t.Parallel()
	stopped := make(chan struct{})
	r := start(t, Options{OnStop: func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("OnStop without a deadline")
		}
		close(stopped)
		return nil
	}})
	c := dial(t, r)
	if err := c.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-r.done:
		if err != nil {
			t.Fatalf("Run = %v", err)
		}
		r.done <- nil // for Cleanup
	case <-time.After(5 * time.Second):
		t.Fatal("daemon did not stop")
	}
	<-stopped
	if _, err := os.Stat(r.paths.Socket); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("socket left behind: %v", err)
	}
	l, err := Acquire(r.paths.Lock)
	if err != nil {
		t.Fatalf("lock still held: %v", err)
	}
	_ = l.Release()
	if _, err := api.Dial(context.Background(), r.paths.Socket, "test"); !errors.Is(err, api.ErrNotRunning) {
		t.Errorf("Dial after stop = %v; want ErrNotRunning", err)
	}
}

func TestStaleSocketIsReplaced(t *testing.T) {
	t.Parallel()
	p := shortHome(t)
	if err := os.MkdirAll(p.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Socket, []byte("left by a crashed daemon"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := start(t, Options{Paths: p})
	if _, err := dial(t, r).Status(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestProtocolGuards(t *testing.T) {
	t.Parallel()
	r := start(t, Options{Methods: map[string]Method{
		"test/echo": func(_ context.Context, c *Conn, params json.RawMessage) (any, error) {
			if err := c.Notify("test/notified", params); err != nil {
				return nil, err
			}
			return params, nil
		},
	}})
	c := dial(t, r)
	var out map[string]int
	if err := c.Call(context.Background(), "test/echo", map[string]int{"n": 7}, &out); err != nil || out["n"] != 7 {
		t.Fatalf("echo = %v, %v", out, err)
	}
	select {
	case n := <-c.Notifications():
		if n.Method != "test/notified" {
			t.Errorf("notification %+v", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no notification")
	}
	var rpcErr *linerpc.Error
	if err := c.Call(context.Background(), "nope", nil, nil); !errors.As(err, &rpcErr) || rpcErr.Code != linerpc.CodeMethodNotFound {
		t.Errorf("unknown method = %v", err)
	}

	// A raw peer that skips initialize, and one with the wrong version.
	raw := rawPeer(t, r)
	if err := raw.Call(context.Background(), api.MethodStatus, nil, nil); !errors.As(err, &rpcErr) || rpcErr.Code != api.CodeNotInitialized {
		t.Errorf("status before initialize = %v", err)
	}
	err := raw.Call(context.Background(), api.MethodInitialize, api.InitializeParams{Version: "openchef.v0"}, nil)
	if !errors.As(err, &rpcErr) || rpcErr.Code != api.CodeVersionMismatch {
		t.Errorf("old version = %v", err)
	}
}

func TestDialWithoutDaemon(t *testing.T) {
	t.Parallel()
	p := shortHome(t)
	if _, err := api.Dial(context.Background(), p.Socket, "test"); !errors.Is(err, api.ErrNotRunning) {
		t.Fatalf("Dial = %v; want ErrNotRunning", err)
	}
}

func TestSocketPathTooLong(t *testing.T) {
	t.Parallel()
	p, err := config.PathsIn(filepath.Join(t.TempDir(), "a-directory-name-long-enough-to-push-the-socket-path-past-the-unix-limit-for-sure"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), Options{Paths: p}); err == nil {
		t.Fatal("overlong socket path accepted")
	}
}

// A detached daemon outlives its starter and stops on request.
func TestStartDetached(t *testing.T) {
	t.Parallel()
	p := shortHome(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	info, err := StartDetached(context.Background(), Detached{
		Exe: exe, Args: []string{"-test.run=^$"}, Env: append(os.Environ(), helperEnv+"="+p.Home),
		LogPath: filepath.Join(p.Logs, "daemon.log"), Socket: p.Socket, Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { // stop it even if an assertion below fails
		if c, err := api.Dial(context.Background(), p.Socket, "cleanup"); err == nil {
			_ = c.Stop(context.Background())
			_ = c.Close()
		}
	})
	if info.PID == os.Getpid() || info.PID <= 0 {
		t.Fatalf("pid %d", info.PID)
	}
	c, err := api.Dial(context.Background(), p.Socket, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Done():
	case <-time.After(15 * time.Second):
		t.Fatal("detached daemon did not stop")
	}
	_ = c.Close()
}
