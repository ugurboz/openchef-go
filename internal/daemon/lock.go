package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// ErrRunning means another daemon holds the single-instance lock.
var ErrRunning = errors.New("daemon: another OpenChef daemon is running")

// Lock is the single-instance lock (~/.openchef/daemon.lock, §5.3 step 1). It is an OS
// file lock, so it is released by the kernel when the daemon dies, however it dies.
type Lock struct {
	f *os.File
}

// Acquire takes the lock at path without waiting, or fails with ErrRunning.
func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("daemon: create %s: %w", filepath.Dir(path), err)
	}
	//nolint:gosec // G304: the path is the configured lock file.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("daemon: open lock: %w", err)
	}
	if err := lockFile(f); err != nil {
		_ = f.Close() // the lock error is the one to report
		return nil, err
	}
	// The PID is informational (diagnostics); the lock itself is the authority.
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0) // best effort
	}
	return &Lock{f: f}, nil
}

// Release drops the lock. It is idempotent.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	return errors.Join(unlockFile(f), f.Close())
}
