package procs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// setParentDeathSignal asks the kernel to SIGTERM the child when the daemon dies.
// Pdeathsig is tied to the creating OS thread; the Go runtime does not retire threads
// while the program runs unless a goroutine exits while locked to its thread, which this
// codebase never does.
func setParentDeathSignal(attr *syscall.SysProcAttr) {
	attr.Pdeathsig = syscall.SIGTERM
}

// waitExitNoReap blocks until pid has exited, leaving it as a zombie so its PID (and
// process group ID) stay reserved until cmd.Wait reaps it. It reports whether the exit
// was observed.
func waitExitNoReap(pid int) bool {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if err == nil {
			return true
		}
		if !errors.Is(err, unix.EINTR) {
			return false
		}
	}
}

// StartToken returns the process start time in clock ticks since boot (field 22 of
// /proc/<pid>/stat). Unlike wall-clock times it is immune to clock adjustments.
func StartToken(pid int) (uint64, error) {
	token, _, err := readStat(pid)
	return token, err
}

func probe(pid int) (token uint64, running bool, err error) {
	token, state, err := readStat(pid)
	// ENOENT: no such PID. ESRCH: the process vanished between open and read of its
	// stat file. Both mean "not running".
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, unix.ESRCH) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return token, state != 'Z' && state != 'X', nil
}

func readStat(pid int) (token uint64, state byte, err error) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, fmt.Errorf("procs: read stat of pid %d: %w", pid, err)
	}
	return parseStat(string(raw))
}

// parseStat extracts state and starttime from a /proc/<pid>/stat line. The command name
// (field 2) may contain spaces and parentheses, so fields are counted after the last ')'.
func parseStat(line string) (token uint64, state byte, err error) {
	end := strings.LastIndexByte(line, ')')
	if end < 0 || end+2 > len(line) {
		return 0, 0, fmt.Errorf("procs: malformed stat line %q", line)
	}
	fields := strings.Fields(line[end+1:])
	// fields[0] is field 3 (state); starttime is field 22, i.e. fields[19].
	const startTimeIndex = 19
	if len(fields) <= startTimeIndex || len(fields[0]) != 1 {
		return 0, 0, fmt.Errorf("procs: short stat line %q", line)
	}
	token, err = strconv.ParseUint(fields[startTimeIndex], 10, 64)
	if err != nil {
		return 0, 0, fmt.Errorf("procs: parse starttime: %w", err)
	}
	return token, fields[0][0], nil
}
