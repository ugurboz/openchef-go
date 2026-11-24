package procs

import (
	"bufio"
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
)

// The test binary doubles as the child process. OPENCHEF_PROCS_HELPER selects a mode.
const helperEnv = "OPENCHEF_PROCS_HELPER"

func TestMain(m *testing.M) {
	mode := os.Getenv(helperEnv)
	if mode == "" {
		os.Exit(m.Run())
	}
	os.Exit(runHelper(mode))
}

func runHelper(mode string) int {
	switch mode {
	case "echo":
		if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
			return 2
		}
		return 0
	case "until-eof":
		_, _ = io.Copy(io.Discard, os.Stdin) // any outcome ends the helper
		return 0
	case "sleep":
		time.Sleep(10 * time.Minute)
		return 0
	case "ignore-term":
		ignoreTerm()
		fmt.Println("ready")
		time.Sleep(10 * time.Minute)
		return 0
	case "spawn", "spawn-and-exit":
		child, err := startHelperChild()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 3
		}
		token, err := StartToken(child.Process.Pid)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 4
		}
		fmt.Printf("%d %d\n", child.Process.Pid, token)
		if mode == "spawn-and-exit" {
			return 0
		}
		time.Sleep(10 * time.Minute)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
		return 1
	}
}

// startHelperChild starts a grandchild in the same kill domain (no new group/job).
func startHelperChild() (*exec.Cmd, error) {
	cmd := exec.CommandContext(context.Background(), os.Args[0])
	cmd.Env = helperEnviron("sleep")
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start grandchild: %w", err)
	}
	return cmd, nil
}

func helperEnviron(mode string) []string {
	env := []string{helperEnv + "=" + mode}
	for _, k := range []string{"SYSTEMROOT", "TMPDIR", "TEMP", "TMP"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func startHelper(t *testing.T, mode string) *Process {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	p, err := Start(context.Background(), Spec{Path: exe, Env: helperEnviron(mode)})
	if err != nil {
		t.Fatalf("Start(%s): %v", mode, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := p.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return p
}

func readLine(t *testing.T, r io.Reader) string {
	t.Helper()
	type result struct {
		line string
		err  error
	}
	ch := make(chan result, 1)
	go func() {
		line, err := bufio.NewReader(r).ReadString('\n')
		ch <- result{strings.TrimSpace(line), err}
	}()
	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatalf("read line: %v", res.err)
		}
		return res.line
	case <-time.After(10 * time.Second):
		t.Fatal("timed out reading from child stdout")
		return ""
	}
}

func parseIdentity(t *testing.T, line string) Identity {
	t.Helper()
	parts := strings.Fields(line)
	if len(parts) != 2 {
		t.Fatalf("unexpected helper output %q", line)
	}
	pid, err := strconv.Atoi(parts[0])
	if err != nil {
		t.Fatalf("parse pid: %v", err)
	}
	token, err := strconv.ParseUint(parts[1], 10, 64)
	if err != nil {
		t.Fatalf("parse token: %v", err)
	}
	return Identity{PID: pid, StartToken: token}
}

func waitDone(t *testing.T, p *Process, d time.Duration) {
	t.Helper()
	select {
	case <-p.Done():
	case <-time.After(d):
		t.Fatalf("process %d did not finish within %v", p.Identity().PID, d)
	}
}

func eventually(t *testing.T, d time.Duration, what string, cond func() (bool, error)) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		ok, err := cond()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: condition not met within %v", what, d)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func notAlive(id Identity) func() (bool, error) {
	return func() (bool, error) {
		alive, err := Alive(id)
		return !alive, err
	}
}

func TestEchoOverPipes(t *testing.T) {
	t.Parallel()
	p := startHelper(t, "echo")
	if _, err := io.WriteString(p.Stdin, "hello\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := readLine(t, p.Stdout); got != "hello" {
		t.Fatalf("echo = %q, want %q", got, "hello")
	}
	if err := p.Stdin.Close(); err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	waitDone(t, p, 10*time.Second)
	if err := p.ExitErr(); err != nil {
		t.Fatalf("exit: %v", err)
	}
}

func TestTerminateGracefulOnStdinEOF(t *testing.T) {
	t.Parallel()
	p := startHelper(t, "until-eof")
	start := time.Now()
	if err := p.Terminate(context.Background(), 10*time.Second); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("graceful stop took %v; stdin EOF should end the process quickly", elapsed)
	}
	if err := p.ExitErr(); err != nil {
		t.Fatalf("expected clean exit after EOF, got %v", err)
	}
}

func TestTerminateEscalatesWhenStdinIsIgnored(t *testing.T) {
	t.Parallel()
	p := startHelper(t, "sleep")
	if err := p.Terminate(context.Background(), 200*time.Millisecond); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if alive, err := Alive(p.Identity()); err != nil || alive {
		t.Fatalf("process still alive after Terminate (alive=%v, err=%v)", alive, err)
	}
	var exitErr *exec.ExitError
	if !errors.As(p.ExitErr(), &exitErr) {
		t.Fatalf("expected a non-zero exit after forced stop, got %v", p.ExitErr())
	}
}

func TestTerminateKillsGrandchildren(t *testing.T) {
	t.Parallel()
	p := startHelper(t, "spawn")
	child := parseIdentity(t, readLine(t, p.Stdout))
	if alive, err := Alive(child); err != nil || !alive {
		t.Fatalf("grandchild should be running (alive=%v, err=%v)", alive, err)
	}
	if err := p.Terminate(context.Background(), 200*time.Millisecond); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	eventually(t, 10*time.Second, "grandchild killed", notAlive(child))
}

func TestLeaderExitKillsLeftovers(t *testing.T) {
	t.Parallel()
	p := startHelper(t, "spawn-and-exit")
	child := parseIdentity(t, readLine(t, p.Stdout))
	waitDone(t, p, 10*time.Second)
	if err := p.ExitErr(); err != nil {
		t.Fatalf("leader exit: %v", err)
	}
	eventually(t, 10*time.Second, "leftover grandchild killed after leader exit", notAlive(child))
}

func TestAlive(t *testing.T) {
	t.Parallel()

	self := os.Getpid()
	token, err := StartToken(self)
	if err != nil {
		t.Fatalf("StartToken(self): %v", err)
	}
	again, err := StartToken(self)
	if err != nil || again != token {
		t.Fatalf("StartToken not stable: %d vs %d (err=%v)", token, again, err)
	}

	cases := []struct {
		name string
		id   Identity
		want bool
	}{
		{"self", Identity{PID: self, StartToken: token}, true},
		{"wrong start token", Identity{PID: self, StartToken: token + 1}, false},
		{"zero pid", Identity{}, false},
		{"negative pid", Identity{PID: -1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Alive(tc.id)
			if err != nil {
				t.Fatalf("Alive: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Alive(%+v) = %v, want %v", tc.id, got, tc.want)
			}
		})
	}
}

func TestAliveAfterExit(t *testing.T) {
	t.Parallel()
	p := startHelper(t, "until-eof")
	id := p.Identity()
	if alive, err := Alive(id); err != nil || !alive {
		t.Fatalf("running process not alive (alive=%v, err=%v)", alive, err)
	}
	if err := p.Terminate(context.Background(), 5*time.Second); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if alive, err := Alive(id); err != nil || alive {
		t.Fatalf("exited process reported alive (alive=%v, err=%v)", alive, err)
	}
}

func TestKillIfAlive(t *testing.T) {
	t.Parallel()
	p := startHelper(t, "spawn")
	child := parseIdentity(t, readLine(t, p.Stdout))

	killed, err := KillIfAlive(p.Identity())
	if err != nil || !killed {
		t.Fatalf("KillIfAlive = %v, %v; want true, nil", killed, err)
	}
	waitDone(t, p, 10*time.Second)
	eventually(t, 10*time.Second, "orphan sweep reaches grandchild", notAlive(child))

	killed, err = KillIfAlive(p.Identity())
	if err != nil || killed {
		t.Fatalf("second KillIfAlive = %v, %v; want false, nil", killed, err)
	}
}

func TestIgnoredTermSignalStillStops(t *testing.T) {
	t.Parallel()
	if !termSignalSupported() {
		t.Skip("no termination signal stage on this platform")
	}
	p := startHelper(t, "ignore-term")
	if got := readLine(t, p.Stdout); got != "ready" {
		t.Fatalf("helper said %q", got)
	}
	if err := p.Terminate(context.Background(), 200*time.Millisecond); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if alive, err := Alive(p.Identity()); err != nil || alive {
		t.Fatalf("process survived escalation (alive=%v, err=%v)", alive, err)
	}
}

func TestCancelledContextSkipsGrace(t *testing.T) {
	t.Parallel()
	p := startHelper(t, "sleep")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := p.Terminate(ctx, time.Minute); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 15*time.Second {
		t.Fatalf("cancelled Terminate waited %v; grace periods should be skipped", elapsed)
	}
}

func TestStartErrors(t *testing.T) {
	t.Parallel()

	if _, err := Start(context.Background(), Spec{}); err == nil {
		t.Error("empty Path: expected error")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	if _, err := Start(ctx, Spec{Path: exe}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context: got %v, want context.Canceled", err)
	}

	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := Start(context.Background(), Spec{Path: missing}); err == nil {
		t.Error("missing binary: expected error")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	t.Parallel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	p, err := Start(context.Background(), Spec{Path: exe, Env: helperEnviron("until-eof")})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for i := range 3 {
		if err := p.Close(context.Background()); err != nil {
			t.Fatalf("Close #%d: %v", i+1, err)
		}
	}
}
