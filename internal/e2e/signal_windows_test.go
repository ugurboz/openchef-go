//go:build windows

package e2e

// Windows has no SIGTERM; the job object kills the agent with its daemon.
func ignoreTerm() {}
