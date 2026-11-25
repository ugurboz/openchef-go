package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ugurboz/openchef/internal/api"
	"github.com/ugurboz/openchef/internal/buildinfo"
	"github.com/ugurboz/openchef/internal/daemon"
)

const (
	startTimeout = 10 * time.Second
	stopTimeout  = daemon.StopTimeout + 5*time.Second
	clientName   = "openchef-cli"
)

func newDaemonCommand(deps Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Start, stop or inspect the background daemon",
		Long: "The daemon runs every agent session. It starts automatically when a task " +
			"needs it; these commands control it directly.",
	}
	cmd.AddCommand(newDaemonRunCommand(deps), newDaemonStartCommand(deps), newDaemonStopCommand(deps), newDaemonStatusCommand(deps))
	return cmd
}

// daemon run: the daemon itself, in the foreground (what `daemon start` launches).
func newDaemonRunCommand(deps Deps) *cobra.Command {
	return &cobra.Command{
		Use:    "run",
		Short:  "Run the daemon in the foreground",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := deps.Paths()
			if err != nil {
				return err
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			log := slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), nil))
			return daemon.Run(ctx, daemon.Options{Paths: paths, Version: buildinfo.Get().Version, Log: log})
		},
	}
}

func newDaemonStartCommand(deps Deps) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the daemon in the background",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := deps.Paths()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if c, err := api.Dial(ctx, paths.Socket, clientName); err == nil {
				info := c.Daemon
				if err := c.Close(); err != nil {
					return err
				}
				return report(cmd.OutOrStdout(), asJSON, info, fmt.Sprintf("daemon already running (pid %d)", info.PID))
			} else if !errors.Is(err, api.ErrNotRunning) {
				return err
			}
			exe, err := os.Executable()
			if err != nil {
				return fmt.Errorf("locate openchef executable: %w", err)
			}
			logPath := filepath.Join(paths.Logs, "daemon.log")
			info, err := daemon.StartDetached(ctx, daemon.Detached{
				Exe: exe, Args: []string{"daemon", "run"}, LogPath: logPath, Socket: paths.Socket, Timeout: startTimeout,
			})
			if err != nil {
				return err
			}
			return report(cmd.OutOrStdout(), asJSON, info, fmt.Sprintf("daemon started (pid %d)", info.PID))
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result as JSON")
	return cmd
}

func newDaemonStopCommand(deps Deps) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the daemon; running tasks resume on next start",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := deps.Paths()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), stopTimeout)
			defer cancel()
			c, err := api.Dial(ctx, paths.Socket, clientName)
			if errors.Is(err, api.ErrNotRunning) {
				return report(cmd.OutOrStdout(), asJSON, map[string]bool{"running": false}, "daemon not running")
			}
			if err != nil {
				return err
			}
			pid := c.Daemon.PID
			if err := c.Stop(ctx); err != nil {
				return errors.Join(err, c.Close())
			}
			select {
			case <-c.Done():
			case <-ctx.Done():
				return errors.Join(fmt.Errorf("daemon (pid %d) did not stop within %s", pid, stopTimeout), c.Close())
			}
			if err := c.Close(); err != nil {
				return err
			}
			return report(cmd.OutOrStdout(), asJSON, map[string]any{"running": false, "pid": pid},
				fmt.Sprintf("daemon stopped (pid %d)", pid))
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result as JSON")
	return cmd
}

// errNotRunning makes `daemon status` exit 3 when no daemon runs (as `systemctl
// status` does for an inactive unit) after printing a normal message.
var errNotRunning = &ExitError{Code: 3}

func newDaemonStatusCommand(deps Deps) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show whether the daemon is running",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			paths, err := deps.Paths()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Second)
			defer cancel()
			c, err := api.Dial(ctx, paths.Socket, clientName)
			if errors.Is(err, api.ErrNotRunning) {
				if rerr := report(cmd.OutOrStdout(), asJSON, map[string]bool{"running": false}, "daemon not running"); rerr != nil {
					return rerr
				}
				return errNotRunning
			}
			if err != nil {
				return err
			}
			st, err := c.Status(ctx)
			if cerr := c.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return err
			}
			text := fmt.Sprintf("daemon running (pid %d, version %s, up %s)\nsocket %s",
				st.PID, st.DaemonVersion, st.Uptime(time.Now()).Round(time.Second), st.Socket)
			return report(cmd.OutOrStdout(), asJSON, st, text)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the result as JSON")
	return cmd
}

func report(w io.Writer, asJSON bool, v any, text string) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if err := enc.Encode(v); err != nil {
			return fmt.Errorf("encode result: %w", err)
		}
		return nil
	}
	if _, err := fmt.Fprintln(w, text); err != nil {
		return fmt.Errorf("write result: %w", err)
	}
	return nil
}
