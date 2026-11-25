package verify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/procs"
)

const helperEnv = "OPENCHEF_VERIFY_HELPER"

// TestMain turns the test binary into the command under verification when helperEnv
// is set, so the tests need no shell utilities and behave the same on every OS.
func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		os.Exit(helper(mode))
	}
	os.Exit(m.Run())
}

func helper(mode string) int {
	name, arg, _ := strings.Cut(mode, ":")
	switch name {
	case "exit":
		n, _ := strconv.Atoi(arg)
		fmt.Println("exiting with", n)
		return n
	case "spam":
		n, _ := strconv.Atoi(arg)
		fmt.Fprint(os.Stderr, "BEGIN\n")
		line := strings.Repeat("x", 99) + "\n"
		for range n / 100 {
			fmt.Print(line)
		}
		fmt.Fprint(os.Stderr, "END\n")
		return 1
	case "sleep":
		time.Sleep(time.Hour)
		return 0
	case "tree":
		exe, _ := os.Executable()
		cmd := exec.CommandContext(context.Background(), exe)
		cmd.Env = append(os.Environ(), helperEnv+"=sleep")
		if err := cmd.Start(); err != nil {
			return 3
		}
		_ = os.WriteFile(arg, []byte(strconv.Itoa(cmd.Process.Pid)), 0o600)
		time.Sleep(time.Hour)
		return 0
	case "env":
		for _, kv := range os.Environ() {
			fmt.Println(kv)
		}
		return 0
	case "stdin":
		_, _ = io.Copy(io.Discard, os.Stdin)
		return 0
	}
	return 2
}

// spec builds a Spec that runs the test binary in the given helper mode.
func spec(t *testing.T, mode string) Spec {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(exe, " \"") {
		t.Skipf("test binary path %q needs shell quoting", exe)
	}
	env := append(MinimalEnv(os.Environ()), helperEnv+"="+mode)
	return Spec{Command: exe, Dir: t.TempDir(), Env: env, Timeout: 30 * time.Second}
}

func TestPassAndFail(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	res, err := Run(ctx, spec(t, "exit:0"))
	if err != nil || !res.Passed || res.ExitCode != 0 || !strings.Contains(res.Output, "exiting with 0") {
		t.Fatalf("pass: %+v, %v", res, err)
	}
	res, err = Run(ctx, spec(t, "exit:3"))
	if err != nil || res.Passed || res.ExitCode != 3 || res.TimedOut {
		t.Fatalf("fail: %+v, %v", res, err)
	}
	if got := res.Summary(); !strings.Contains(got, "exit code 3") {
		t.Errorf("Summary = %q", got)
	}
}

func TestShellSyntax(t *testing.T) {
	t.Parallel()
	s := spec(t, "exit:0")
	s.Command += " && " + s.Command
	res, err := Run(context.Background(), s)
	if err != nil || !res.Passed || strings.Count(res.Output, "exiting with 0") != 2 {
		t.Fatalf("chained command: %+v, %v", res, err)
	}
}

func TestOutputKeepsHeadAndTail(t *testing.T) {
	t.Parallel()
	s := spec(t, "spam:200000")
	s.MaxOutput = 1000
	res, err := Run(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || !strings.HasPrefix(res.Output, "BEGIN\n") || !strings.HasSuffix(res.Output, "END\n") ||
		!strings.Contains(res.Output, "bytes omitted") || len(res.Output) > 1100 {
		t.Fatalf("truncated output (%d bytes): %q", len(res.Output), res.Output)
	}
}

func TestStdinIsClosed(t *testing.T) {
	t.Parallel()
	s := spec(t, "stdin")
	s.Timeout = 10 * time.Second
	res, err := Run(context.Background(), s)
	if err != nil || !res.Passed {
		t.Fatalf("stdin reader: %+v, %v", res, err)
	}
}

// A timeout stops the command and everything it started.
func TestTimeoutKillsTree(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	s := spec(t, "tree:"+pidFile)
	s.Timeout = 3 * time.Second

	child := make(chan procs.Identity, 1)
	go func() {
		for range 300 {
			raw, err := os.ReadFile(pidFile)
			if pid, convErr := strconv.Atoi(string(raw)); err == nil && convErr == nil {
				if tok, err := procs.StartToken(pid); err == nil {
					child <- procs.Identity{PID: pid, StartToken: tok}
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		close(child)
	}()
	start := time.Now()
	res, err := Run(context.Background(), s)
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || res.Passed || res.ExitCode != -1 {
		t.Fatalf("result = %+v", res)
	}
	if d := time.Since(start); d > s.Timeout+stopGrace+drainWait+2*time.Second {
		t.Errorf("Run took %v", d)
	}
	id, ok := <-child
	if !ok {
		t.Fatal("grandchild never started")
	}
	if alive, err := procs.Alive(id); err != nil || alive {
		t.Fatalf("grandchild %d alive = %t, %v", id.PID, alive, err)
	}
}

func TestCancelStopsCommand(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	_, err := Run(ctx, spec(t, "sleep"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v; want DeadlineExceeded", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("Run took %v", d)
	}
}

func TestEnvIsNotInherited(t *testing.T) {
	t.Parallel()
	s := spec(t, "env")
	s.Env = append(s.Env, "OPENCHEF_VERIFY_EXTRA=1")
	res, err := Run(context.Background(), s)
	if err != nil || !res.Passed {
		t.Fatalf("%+v, %v", res, err)
	}
	if !strings.Contains(res.Output, "OPENCHEF_VERIFY_EXTRA=1") {
		t.Errorf("explicit variable missing: %s", res.Output)
	}
	for line := range strings.SplitSeq(res.Output, "\n") {
		k, _, _ := strings.Cut(strings.TrimSpace(line), "=")
		if strings.Contains(strings.ToUpper(k), "API_KEY") || strings.Contains(strings.ToUpper(k), "TOKEN") {
			t.Errorf("secret-looking variable leaked: %s", k)
		}
	}
}

func TestInvalidSpec(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for name, s := range map[string]Spec{
		"command": {Dir: t.TempDir()},
		"dir":     {Command: "true"},
		"missing": {Command: "true", Dir: filepath.Join(t.TempDir(), "nope")},
	} {
		if _, err := Run(ctx, s); !errors.Is(err, ErrInvalidSpec) {
			t.Errorf("%s: %v; want ErrInvalidSpec", name, err)
		}
	}
}

func TestMinimalEnv(t *testing.T) {
	t.Parallel()
	from := []string{
		"PATH=/bin", "HOME=/h", "ANTHROPIC_API_KEY=sk", "GITHUB_TOKEN=t", "LC_ALL=C",
		"SystemRoot=C:\\Windows", "GOFLAGS=-mod=mod", "noequals", "=C:=C:\\", "PATH=/usr/bin",
	}
	got := MinimalEnv(from, "goflags")
	want := []string{"PATH=/bin", "HOME=/h", "LC_ALL=C", "SystemRoot=C:\\Windows", "GOFLAGS=-mod=mod", "PATH=/usr/bin"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("MinimalEnv = %q\nwant         %q", got, want)
	}
}

func TestHeadTail(t *testing.T) {
	t.Parallel()
	cases := []struct {
		limit  int
		writes []string
		want   string
		trunc  bool
	}{
		{10, []string{"hello"}, "hello", false},
		{10, []string{"0123456789"}, "0123456789", false},
		{10, []string{"01234", "56789", "abcde"}, "01234\n\n[… 5 bytes omitted …]\n\nabcde", true},
		{10, []string{"0123456789abcdefghij"}, "01234\n\n[… 10 bytes omitted …]\n\nfghij", true},
		{10, []string{"01234", "5", "6", "7", "8", "9", "a", "b"}, "01234\n\n[… 2 bytes omitted …]\n\n789ab", true},
	}
	for i, c := range cases {
		h := newHeadTail(c.limit)
		for _, w := range c.writes {
			if n, err := h.Write([]byte(w)); n != len(w) || err != nil {
				t.Fatalf("case %d: Write = %d, %v", i, n, err)
			}
		}
		got, trunc := h.result()
		if got != c.want || trunc != c.trunc {
			t.Errorf("case %d: %q, %t; want %q, %t", i, got, trunc, c.want, c.trunc)
		}
	}
	h := newHeadTail(4)
	_, _ = h.Write([]byte("éééé")) // 8 bytes; cuts land inside characters
	if got, _ := h.result(); !strings.Contains(got, "é") || strings.ContainsRune(got, '\x00') {
		t.Errorf("utf-8 seams: %q", got)
	}
}
