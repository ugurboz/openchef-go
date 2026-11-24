// Package procs starts and stops agent processes so that none of them can outlive its
// owner (docs/architecture.md §5).
//
// Guarantees:
//
//   - Every process gets its own kill domain: a process group on Unix, a Job Object with
//     KILL_ON_JOB_CLOSE on Windows. Grandchildren spawned by the agent belong to it.
//   - When the process exits for any reason, everything left in its kill domain is killed
//     before the process is reaped, so leftovers (dev servers, watchers) cannot leak and
//     a recycled PID can never be hit by mistake.
//   - On Linux the kernel sends SIGTERM to the process if the daemon dies (Pdeathsig);
//     on Windows the kernel kills the whole job when the daemon's handle closes. On macOS
//     the process sees EOF on its stdin pipe, and the daemon sweeps recorded identities
//     at startup with KillIfAlive.
//
// Stdin and Stdout are plain OS pipes owned by the caller: Stdin is closed by Terminate,
// Stdout must be drained and closed by the caller (Close does both).
package procs

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Default timings for Terminate when callers have no better value.
const (
	DefaultGrace = 5 * time.Second
	killTimeout  = 5 * time.Second
)

// Errors returned by this package.
var (
	// ErrUnsupported is returned on platforms without a supervision implementation.
	ErrUnsupported = errors.New("procs: platform not supported")
	// ErrKillTimeout means the process did not exit even after a forced kill.
	ErrKillTimeout = errors.New("procs: process did not exit after kill")
)

// Identity identifies one process instance across daemon restarts. PIDs are recycled,
// so a PID alone is never trusted: StartToken is an opaque, platform-specific value
// derived from the process start time that only matches the same instance.
type Identity struct {
	PID        int    `json:"pid"`
	StartToken uint64 `json:"startToken"`
}

// Spec describes a process to start.
type Spec struct {
	Path string
	Args []string // arguments after the program name
	Dir  string
	// Env is the complete environment of the child. It is never inherited implicitly:
	// callers build a filtered environment (architecture §13). nil means empty.
	Env []string
	// Stderr receives the child's stderr. nil discards it.
	Stderr io.Writer
}

// Process is a running, supervised child process.
type Process struct {
	// Stdin writes to the child's standard input. Closing it signals EOF.
	Stdin io.WriteCloser
	// Stdout reads the child's standard output. The caller drains and closes it.
	Stdout io.ReadCloser

	cmd  *exec.Cmd
	id   Identity
	plat *platformState

	done    chan struct{}
	exitErr error

	stdinOnce sync.Once
	stdinErr  error
}

// Start launches spec under supervision. The context only bounds the start itself; use
// Terminate or Close to stop the process.
func Start(ctx context.Context, spec Spec) (*Process, error) {
	if spec.Path == "" {
		return nil, errors.New("procs: empty Path")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("procs: start %s: %w", spec.Path, err)
	}

	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("procs: stdin pipe: %w", err)
	}
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		closeAll(stdinR, stdinW)
		return nil, fmt.Errorf("procs: stdout pipe: %w", err)
	}

	// The context bounds only the start: WithoutCancel keeps exec from killing the process
	// when ctx ends. Stopping is Terminate's job.
	//nolint:gosec // G204: launching agent CLIs chosen by configuration is this package's purpose.
	cmd := exec.CommandContext(context.WithoutCancel(ctx), spec.Path, spec.Args...)
	cmd.Dir = spec.Dir
	cmd.Env = spec.Env
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	cmd.Stdin = stdinR
	cmd.Stdout = stdoutW
	cmd.Stderr = spec.Stderr
	if cmd.Stderr == nil {
		cmd.Stderr = io.Discard
	}

	plat, err := prepare(cmd)
	if err != nil {
		closeAll(stdinR, stdinW, stdoutR, stdoutW)
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		plat.release()
		closeAll(stdinR, stdinW, stdoutR, stdoutW)
		return nil, fmt.Errorf("procs: start %s: %w", spec.Path, err)
	}
	// The child holds its own copies; the parent keeps only its ends.
	closeAll(stdinR, stdoutW)

	pid := cmd.Process.Pid
	if err := plat.attach(pid); err != nil {
		killAfterFailedStart(cmd, plat)
		closeAll(stdinW, stdoutR)
		return nil, err
	}
	token, err := StartToken(pid)
	if err != nil {
		killAfterFailedStart(cmd, plat)
		closeAll(stdinW, stdoutR)
		return nil, fmt.Errorf("procs: read start token of pid %d: %w", pid, err)
	}

	p := &Process{
		Stdin:  stdinW,
		Stdout: stdoutR,
		cmd:    cmd,
		id:     Identity{PID: pid, StartToken: token},
		plat:   plat,
		done:   make(chan struct{}),
	}
	go p.supervise()
	return p, nil
}

// Identity returns the persistable identity of the process.
func (p *Process) Identity() Identity { return p.id }

// Done is closed after the process exited, its kill domain was cleaned up and it was
// reaped.
func (p *Process) Done() <-chan struct{} { return p.done }

// ExitErr returns the result of the process after Done is closed: nil for exit status 0,
// an *exec.ExitError otherwise.
func (p *Process) ExitErr() error {
	<-p.done
	return p.exitErr
}

// Terminate stops the process and everything in its kill domain. It escalates in three
// stages, each bounded by grace: close stdin (agent CLIs exit on EOF), send a termination
// signal to the group (Unix only), force-kill. It returns once the process is reaped.
func (p *Process) Terminate(ctx context.Context, grace time.Duration) error {
	if p.exited() {
		return nil
	}
	if err := p.closeStdin(); err != nil {
		return err
	}
	if p.waitFor(ctx, grace) {
		return nil
	}
	if p.plat.supportsTermSignal() {
		if err := p.plat.signalTerm(p.id.PID); err != nil {
			return err
		}
		if p.waitFor(ctx, grace) {
			return nil
		}
	}
	if err := p.plat.kill(p.id.PID); err != nil {
		return err
	}
	if p.waitFor(context.WithoutCancel(ctx), killTimeout) {
		return nil
	}
	return fmt.Errorf("pid %d: %w", p.id.PID, ErrKillTimeout)
}

// Close terminates the process with DefaultGrace and releases the caller-side pipes.
// It is idempotent and safe to defer.
func (p *Process) Close(ctx context.Context) error {
	termErr := p.Terminate(ctx, DefaultGrace)
	stdoutErr := p.Stdout.Close()
	if errors.Is(stdoutErr, os.ErrClosed) {
		stdoutErr = nil
	}
	if stdoutErr != nil {
		stdoutErr = fmt.Errorf("procs: close stdout: %w", stdoutErr)
	}
	return errors.Join(termErr, stdoutErr)
}

func (p *Process) closeStdin() error {
	p.stdinOnce.Do(func() {
		err := p.Stdin.Close()
		if err != nil && !errors.Is(err, os.ErrClosed) {
			p.stdinErr = fmt.Errorf("procs: close stdin: %w", err)
		}
	})
	return p.stdinErr
}

func (p *Process) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// waitFor reports whether the process finished within d. A cancelled ctx ends the
// wait early (returning false) so Terminate escalates immediately.
func (p *Process) waitFor(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-p.done:
		return true
	case <-t.C:
		return false
	case <-ctx.Done():
		return p.exited()
	}
}

// supervise waits for the process to exit without reaping it, kills whatever is left in
// its kill domain while the PID is still reserved, and then reaps it.
func (p *Process) supervise() {
	defer close(p.done)
	if waitExitNoReap(p.id.PID) {
		// Errors are not actionable here: the domain may already be empty.
		_ = p.plat.kill(p.id.PID) // best-effort cleanup of leftovers
	}
	p.exitErr = p.cmd.Wait()
	// On Windows the job outlives the process handle, so leftovers are killed here too.
	p.plat.release()
}

func killAfterFailedStart(cmd *exec.Cmd, plat *platformState) {
	_ = plat.kill(cmd.Process.Pid) // best effort; Kill below is the fallback
	_ = cmd.Process.Kill()         // process may already be gone
	_ = cmd.Wait()                 // only reaping; the start error is reported
	plat.release()
}

func closeAll(files ...*os.File) {
	for _, f := range files {
		_ = f.Close() // cleanup on an error path; the original error is returned
	}
}

// Alive reports whether the exact process instance id is still running (not a zombie).
func Alive(id Identity) (bool, error) {
	if id.PID <= 0 {
		return false, nil
	}
	token, running, err := probe(id.PID)
	if err != nil {
		return false, err
	}
	return running && token == id.StartToken, nil
}

// KillIfAlive force-kills the kill domain of id if that exact instance is still running.
// The daemon uses it at startup to remove processes left behind by a crashed instance.
// It reports whether anything was killed.
func KillIfAlive(id Identity) (bool, error) {
	alive, err := Alive(id)
	if err != nil || !alive {
		return false, err
	}
	if err := killOrphan(id.PID); err != nil {
		return false, err
	}
	return true, nil
}
