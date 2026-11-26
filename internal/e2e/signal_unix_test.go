//go:build unix

package e2e

import (
	"os/signal"
	"syscall"
)

func ignoreTerm() { signal.Ignore(syscall.SIGTERM, syscall.SIGHUP) }
