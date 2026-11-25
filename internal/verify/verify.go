// Package verify runs a step's verification command in its workspace (docs/architecture.md
// §7.2): through the platform shell, in its own kill domain (package procs), with a
// minimal environment, a timeout that stops the whole process tree, and output bounded
// to its head and tail before it is fed back to an agent.
package verify

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	goruntime "runtime"
	"strings"
	"time"

	"github.com/ugurboz/openchef/internal/procs"
)

// Defaults used when Spec leaves a limit at zero.
const (
	DefaultTimeout   = 10 * time.Minute
	DefaultMaxOutput = 16 << 10 // bytes kept for the agent: 8 KiB head + 8 KiB tail
	// stopGrace is how long the command gets after SIGTERM before it is killed.
	stopGrace = 2 * time.Second
	// drainWait bounds waiting for output after the command's tree is gone; a process
	// that escaped the kill domain could otherwise hold the pipe open forever.
	drainWait = 2 * time.Second
)

// ErrInvalidSpec means the Spec cannot be run.
var ErrInvalidSpec = errors.New("verify: invalid spec")

// Spec describes one verification run.
type Spec struct {
	// Command is a shell command line, e.g. "go test ./...".
	Command string
	// Dir is the workspace to run in.
	Dir string
	// Env is the complete environment (see MinimalEnv); it is never inherited.
	Env []string
	// Timeout bounds the run; 0 means DefaultTimeout.
	Timeout time.Duration
	// MaxOutput is the number of output bytes kept; 0 means DefaultMaxOutput.
	MaxOutput int
}

// Result is the outcome of a run that started. A failing command is a Result with
// Passed false, not an error.
type Result struct {
	Passed bool
	// ExitCode is the command's exit status; -1 if it was stopped by a signal or timeout.
	ExitCode int
	TimedOut bool
	// Output is stdout and stderr interleaved as written, bounded to MaxOutput bytes.
	Output    string
	Truncated bool
	Duration  time.Duration
}

// Summary is a one-line description for logs and the live view.
func (r *Result) Summary() string {
	switch {
	case r.Passed:
		return fmt.Sprintf("passed in %s", r.Duration.Round(time.Millisecond))
	case r.TimedOut:
		return fmt.Sprintf("timed out after %s", r.Duration.Round(time.Millisecond))
	default:
		return fmt.Sprintf("failed with exit code %d in %s", r.ExitCode, r.Duration.Round(time.Millisecond))
	}
}

// Run executes spec. It returns an error only if the command could not be run at all
// (bad spec, shell missing, the process tree could not be stopped) or ctx was cancelled
// by the caller; in that last case the tree is stopped before Run returns.
func Run(ctx context.Context, spec Spec) (Result, error) {
	if strings.TrimSpace(spec.Command) == "" {
		return Result{}, fmt.Errorf("%w: empty command", ErrInvalidSpec)
	}
	if spec.Dir == "" {
		return Result{}, fmt.Errorf("%w: empty directory", ErrInvalidSpec)
	}
	if info, err := os.Stat(spec.Dir); err != nil || !info.IsDir() {
		return Result{}, fmt.Errorf("%w: directory %q: %w", ErrInvalidSpec, spec.Dir, statErr(err))
	}
	timeout := spec.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	limit := spec.MaxOutput
	if limit <= 0 {
		limit = DefaultMaxOutput
	}

	shell, args := shellCommand(spec.Command, spec.Env)
	out := newHeadTail(limit)
	start := time.Now()
	p, err := procs.Start(ctx, procs.Spec{Path: shell, Args: args, Dir: spec.Dir, Env: spec.Env, Stderr: out})
	if err != nil {
		return Result{}, fmt.Errorf("verify: start %q: %w", spec.Command, err)
	}
	// Verification takes no input; EOF now also keeps stdin-reading tools from hanging.
	if err := p.Stdin.Close(); err != nil {
		return Result{}, errors.Join(fmt.Errorf("verify: close stdin: %w", err), p.Close(context.WithoutCancel(ctx)))
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_, _ = io.Copy(out, p.Stdout) // ends at EOF or when Stdout is closed below
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var timedOut, cancelled bool
	select {
	case <-p.Done():
	case <-timer.C:
		timedOut = true
	case <-ctx.Done():
		cancelled = true
	}
	var stopErr error
	if timedOut || cancelled {
		stopErr = p.Terminate(context.WithoutCancel(ctx), stopGrace)
	}
	select {
	case <-drained:
	case <-time.After(drainWait):
	}
	closeErr := p.Stdout.Close()
	if errors.Is(closeErr, os.ErrClosed) {
		closeErr = nil
	}
	<-drained
	if stopErr != nil || closeErr != nil {
		return Result{}, errors.Join(stopErr, closeErr)
	}

	res := Result{Duration: time.Since(start), TimedOut: timedOut}
	res.Output, res.Truncated = out.result()
	if cancelled {
		return res, fmt.Errorf("verify: %q: %w", spec.Command, ctx.Err())
	}
	res.ExitCode = exitCode(p.ExitErr(), timedOut)
	res.Passed = !timedOut && res.ExitCode == 0
	return res, nil
}

func statErr(err error) error {
	if err == nil {
		return errors.New("not a directory")
	}
	return err
}

func exitCode(err error, timedOut bool) int {
	if timedOut {
		return -1
	}
	if err == nil {
		return 0
	}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		return ee.ExitCode() // -1 when killed by a signal
	}
	return -1
}

// shellCommand returns the shell and arguments that run command: /bin/sh -c on Unix,
// cmd.exe /d /s /c on Windows (located through ComSpec in env, then the system root).
func shellCommand(command string, env []string) (shell string, args []string) {
	if goruntime.GOOS != "windows" {
		return "/bin/sh", []string{"-c", command}
	}
	shell = lookupEnv(env, "ComSpec")
	if shell == "" {
		root := lookupEnv(env, "SystemRoot")
		if root == "" {
			root = `C:\Windows`
		}
		shell = root + `\System32\cmd.exe`
	}
	return shell, []string{"/d", "/s", "/c", command}
}

func lookupEnv(env []string, key string) string {
	for i := len(env) - 1; i >= 0; i-- {
		k, v, ok := strings.Cut(env[i], "=")
		if ok && strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}
