//go:build darwin || linux

package procs

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// platformState is empty on Unix: the process group ID equals the child's PID.
type platformState struct{}

func prepare(cmd *exec.Cmd) (*platformState, error) {
	attr := &syscall.SysProcAttr{Setpgid: true}
	setParentDeathSignal(attr)
	cmd.SysProcAttr = attr
	return &platformState{}, nil
}

func (*platformState) attach(int) error         { return nil }
func (*platformState) release()                 {}
func (*platformState) supportsTermSignal() bool { return true }

func (*platformState) signalTerm(pid int) error {
	return signalGroup(pid, unix.SIGTERM)
}

func (*platformState) kill(pid int) error {
	return signalGroup(pid, unix.SIGKILL)
}

// signalGroup signals the whole process group led by pid. A missing group is not an
// error: everything in it is already gone.
func signalGroup(pid int, sig unix.Signal) error {
	err := unix.Kill(-pid, sig)
	if err == nil || errors.Is(err, unix.ESRCH) {
		return nil
	}
	// macOS reports EPERM instead of ESRCH when the only remaining member is the
	// exited-but-unreaped leader. That is "nothing left to signal", not a failure.
	if errors.Is(err, unix.EPERM) {
		if _, running, perr := probe(pid); perr == nil && !running {
			return nil
		}
	}
	return fmt.Errorf("procs: signal %v to process group %d: %w", sig, pid, err)
}

func killOrphan(pid int) error {
	if err := signalGroup(pid, unix.SIGKILL); err != nil {
		return err
	}
	// The process may have left its group; make sure the instance itself is gone.
	if err := unix.Kill(pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("procs: kill pid %d: %w", pid, err)
	}
	return nil
}
