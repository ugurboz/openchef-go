package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/ugurboz/openchef/internal/api"
)

// Detached describes a daemon process to start in the background.
type Detached struct {
	Exe  string
	Args []string
	// Env is the daemon's environment; nil means this process's environment.
	Env []string
	// LogPath receives the daemon's output (appended).
	LogPath string
	// Socket is where the daemon will answer.
	Socket string
	// Timeout bounds the wait for the daemon to answer.
	Timeout time.Duration
}

// StartDetached launches d as a background daemon: in its own session (Unix) or detached
// process group (Windows), with stdin closed and output appended to LogPath, so it
// outlives the terminal that started it. It then waits until the daemon answers on
// Socket, or until the process exits or Timeout passes.
func StartDetached(ctx context.Context, d Detached) (api.InitializeResult, error) {
	exe, args, logPath, socket, timeout := d.Exe, d.Args, d.LogPath, d.Socket, d.Timeout
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return api.InitializeResult{}, fmt.Errorf("daemon: create log directory: %w", err)
	}
	//nolint:gosec // G304: the path is the configured daemon log.
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return api.InitializeResult{}, fmt.Errorf("daemon: open log: %w", err)
	}
	defer func() { _ = logFile.Close() }() // the child keeps its own descriptor

	// The daemon must outlive ctx (and this process), so ctx never kills it.
	//nolint:gosec // G204: exe is this program (os.Executable).
	cmd := exec.CommandContext(context.WithoutCancel(ctx), exe, args...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.Env = d.Env
	if cmd.Env == nil {
		cmd.Env = os.Environ()
	}
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return api.InitializeResult{}, fmt.Errorf("daemon: start: %w", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	deadline := time.Now().Add(timeout)
	for {
		dctx, cancel := context.WithTimeout(ctx, time.Second)
		c, err := api.Dial(dctx, socket, "openchef-start")
		cancel()
		if err == nil {
			info := c.Daemon
			return info, c.Close()
		}
		if !errors.Is(err, api.ErrNotRunning) {
			return api.InitializeResult{}, err
		}
		select {
		case werr := <-exited:
			if werr == nil {
				werr = errors.New("exit status 0")
			}
			return api.InitializeResult{}, fmt.Errorf("daemon: exited during start (%w); see %s", werr, logPath)
		case <-ctx.Done():
			return api.InitializeResult{}, fmt.Errorf("daemon: start: %w", ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return api.InitializeResult{}, fmt.Errorf("daemon: not answering after %s; see %s", timeout, logPath)
		}
	}
}
