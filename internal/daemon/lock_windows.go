//go:build windows

package daemon

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// The lock covers one byte far past the PID so other processes can still read it.
const lockOffset = 1 << 20

func lockFile(f *os.File) error {
	ol := &windows.Overlapped{Offset: lockOffset}
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return ErrRunning
	}
	if err != nil {
		return fmt.Errorf("daemon: lock: %w", err)
	}
	return nil
}

func unlockFile(f *os.File) error {
	ol := &windows.Overlapped{Offset: lockOffset}
	if err := windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, ol); err != nil {
		return fmt.Errorf("daemon: unlock: %w", err)
	}
	return nil
}
