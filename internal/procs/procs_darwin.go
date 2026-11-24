package procs

import (
	"errors"
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// Values from <sys/proc.h>; x/sys/unix does not export them.
const (
	sZomb  = 5          // p_stat: exited, waiting to be reaped
	pWExit = 0x00002000 // p_flag: working on exiting
)

// setParentDeathSignal is a no-op: macOS has no parent-death signal. Children see EOF on
// stdin when the daemon dies, and the daemon sweeps recorded identities at startup.
func setParentDeathSignal(*syscall.SysProcAttr) {}

// waitExitNoReap blocks until pid has exited using kqueue NOTE_EXIT and reports whether
// the exit was observed. The process stays a zombie (PID reserved) until cmd.Wait reaps
// it. It returns false only if kqueue itself fails; the caller then skips the pre-reap
// cleanup instead of signalling a process that may still be running.
func waitExitNoReap(pid int) bool {
	kq, err := unix.Kqueue()
	if err != nil {
		return false
	}
	defer func() { _ = unix.Close(kq) }() // closing a private kqueue fd

	var change unix.Kevent_t
	unix.SetKevent(&change, pid, unix.EVFILT_PROC, unix.EV_ADD|unix.EV_ONESHOT)
	change.Fflags = unix.NOTE_EXIT

	events := make([]unix.Kevent_t, 1)
	changes := []unix.Kevent_t{change}
	for {
		_, err := unix.Kevent(kq, changes, events, nil)
		switch {
		case err == nil:
			return true
		case errors.Is(err, unix.EINTR):
			changes = nil // registration already applied; keep waiting
		case errors.Is(err, unix.ESRCH):
			// Only this package reaps the child, so "no such process" at registration
			// means it already exited and is an unreaped zombie.
			return true
		default:
			return false
		}
	}
}

// StartToken returns the process start time in microseconds since the Unix epoch, as
// recorded by the kernel (kinfo_proc.p_starttime).
func StartToken(pid int) (uint64, error) {
	token, _, found, err := kinfo(pid)
	if err != nil {
		return 0, err
	}
	if !found {
		return 0, fmt.Errorf("procs: pid %d: %w", pid, unix.ESRCH)
	}
	return token, nil
}

func probe(pid int) (token uint64, running bool, err error) {
	token, live, found, err := kinfo(pid)
	if err != nil || !found {
		return 0, false, err
	}
	return token, live, nil
}

// kinfo reads the kernel's record of pid. live is false for zombies and for processes
// already inside exit(): XNU skips both when signalling a process group.
func kinfo(pid int) (token uint64, live, found bool, err error) {
	// The slice variant is used on purpose: for an unknown PID the kernel returns zero
	// bytes, which SysctlKinfoProc reports as EIO (indistinguishable from a real I/O
	// error) while SysctlKinfoProcSlice returns an empty slice.
	kps, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		return 0, false, false, fmt.Errorf("procs: sysctl kern.proc.pid %d: %w", pid, err)
	}
	if len(kps) == 0 || int(kps[0].Proc.P_pid) != pid {
		return 0, false, false, nil
	}
	kp := kps[0]
	tv := kp.Proc.P_starttime
	if tv.Sec < 0 || tv.Usec < 0 {
		return 0, false, false, fmt.Errorf("procs: pid %d: invalid start time %+v", pid, tv)
	}
	live = kp.Proc.P_stat != sZomb && kp.Proc.P_flag&pWExit == 0
	return uint64(tv.Sec)*1_000_000 + uint64(tv.Usec), live, true, nil
}
