//go:build windows

package api

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isRefused recognizes Windows' AF_UNIX errors for a socket file without a listener.
func isRefused(err error) bool {
	return errors.Is(err, windows.WSAECONNREFUSED) || errors.Is(err, windows.ERROR_FILE_NOT_FOUND)
}
