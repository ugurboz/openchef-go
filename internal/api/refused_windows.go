//go:build windows

package api

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isRefused recognizes Windows' AF_UNIX errors for a socket without a listener: a stale
// socket file gives WSAECONNREFUSED, a missing directory WSAENETDOWN.
func isRefused(err error) bool {
	return errors.Is(err, windows.WSAECONNREFUSED) || errors.Is(err, windows.WSAENETDOWN) ||
		errors.Is(err, windows.ERROR_FILE_NOT_FOUND)
}
