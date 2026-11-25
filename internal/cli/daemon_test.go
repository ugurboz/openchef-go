package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/config"
	"github.com/ugurboz/openchef/internal/daemon"
	"github.com/ugurboz/openchef/internal/doctor"
)

func daemonDeps(t *testing.T) (Deps, config.Paths) {
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
	return Deps{Detector: doctor.New(), Paths: func() (config.Paths, error) { return p, nil }}, p
}

func runDaemonCmd(t *testing.T, deps Deps, args ...string) (string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := NewRootCommandWith(IO{Out: &out, Err: &errOut}, deps)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func TestDaemonStatusAndStop(t *testing.T) {
	t.Parallel()
	deps, paths := daemonDeps(t)

	out, err := runDaemonCmd(t, deps, "daemon", "status")
	var exit *ExitError
	if !errors.As(err, &exit) || exit.Code != 3 || !strings.Contains(out, "not running") {
		t.Fatalf("status without daemon: %q, %v", out, err)
	}
	if out, err := runDaemonCmd(t, deps, "daemon", "stop"); err != nil || !strings.Contains(out, "not running") {
		t.Fatalf("stop without daemon: %q, %v", out, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- daemon.Run(ctx, daemon.Options{Paths: paths, Version: "test"}) }()
	t.Cleanup(func() {
		cancel()
		<-done //nolint:errcheck // the test checks Run's result where it matters
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		out, err = runDaemonCmd(t, deps, "daemon", "status")
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon not up: %q, %v", out, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(out, "daemon running (pid") || !strings.Contains(out, paths.Socket) {
		t.Errorf("status: %q", out)
	}
	if out, err := runDaemonCmd(t, deps, "daemon", "status", "--json"); err != nil || !strings.Contains(out, `"daemonVersion": "test"`) {
		t.Errorf("status --json: %q, %v", out, err)
	}
	if out, err := runDaemonCmd(t, deps, "daemon", "stop"); err != nil || !strings.Contains(out, "daemon stopped") {
		t.Fatalf("stop: %q, %v", out, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon Run = %v", err)
		}
		done <- nil
	case <-time.After(10 * time.Second):
		t.Fatal("daemon still running after stop")
	}
}
