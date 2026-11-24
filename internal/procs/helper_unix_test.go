//go:build darwin || linux

package procs

import (
	"os/signal"
	"syscall"
)

func ignoreTerm() { signal.Ignore(syscall.SIGTERM) }

func termSignalSupported() bool { return true }
