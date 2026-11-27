package app

import (
	"context"
	"log/slog"
	"os/exec"
	goruntime "runtime"
	"time"
)

// desktopNotifier shows a desktop notification, best effort (§9.2: when no viewer is
// attached). Title and body go to the helper as arguments, never through a shell or a
// script string, so request text cannot inject commands.
func desktopNotifier(log *slog.Logger) func(title, body string) {
	var name string
	var args func(title, body string) []string
	switch goruntime.GOOS {
	case "darwin":
		name = "osascript"
		args = func(title, body string) []string {
			return []string{"-e", "on run argv", "-e", "display notification (item 2 of argv) with title (item 1 of argv)",
				"-e", "end run", title, body}
		}
	case "linux", "freebsd", "openbsd", "netbsd":
		name = "notify-send"
		args = func(title, body string) []string { return []string{"--app-name=OpenChef", "--", title, body} }
	default:
		return nil // Windows: no dependency-free notifier; `openchef approvals` lists requests
	}
	path, err := exec.LookPath(name)
	if err != nil {
		log.Info("desktop notifications unavailable", "helper", name)
		return nil
	}
	return func(title, body string) {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			//nolint:gosec // G204: fixed helper; title and body are separate arguments.
			if out, err := exec.CommandContext(ctx, path, args(title, firstRunes(body, 300))...).CombinedOutput(); err != nil {
				log.Debug("desktop notification failed", "err", err, "output", string(out))
			}
		}()
	}
}

func firstRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
