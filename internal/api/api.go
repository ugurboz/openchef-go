// Package api is the daemon's JSON-RPC 2.0 interface (docs/architecture.md §12.3):
// method names, parameter and result types, and a client. Messages are newline-delimited
// JSON over a local socket. The API is versioned (Version) and internal in v1: it is
// documented but not promised stable.
package api

import (
	"errors"
	"fmt"
	"time"

	"github.com/ugurboz/openchef/internal/runtime/linerpc"
)

// Version is the protocol version a client and the daemon must agree on.
const Version = "openchef.v1"

// Methods.
const (
	// MethodInitialize must be the first call on a connection.
	MethodInitialize = "initialize"
	MethodStatus     = "daemon/status"
	MethodStop       = "daemon/stop"
)

// Error codes beyond the JSON-RPC standard ones.
const (
	CodeNotInitialized  = -32002
	CodeVersionMismatch = -32003
	CodeInvalidParams   = -32602
)

// ErrVersionMismatch means client and daemon speak different protocol versions,
// typically after an upgrade with the old daemon still running.
var ErrVersionMismatch = errors.New("api: client and daemon versions differ; restart the daemon (openchef daemon stop)")

// InitializeParams opens a connection.
type InitializeParams struct {
	Version string `json:"version"`
	// Client names the caller, e.g. "openchef-cli 0.3.0".
	Client string `json:"client"`
}

// InitializeResult describes the daemon.
type InitializeResult struct {
	Version       string `json:"version"`
	DaemonVersion string `json:"daemonVersion"`
	PID           int    `json:"pid"`
}

// Status is the result of daemon/status.
type Status struct {
	PID           int       `json:"pid"`
	DaemonVersion string    `json:"daemonVersion"`
	StartedAt     time.Time `json:"startedAt"`
	Socket        string    `json:"socket"`
	Home          string    `json:"home"`
	// Connections is the number of connected clients, this one included.
	Connections int `json:"connections"`
}

// Uptime is how long the daemon has been running at now.
func (s *Status) Uptime(now time.Time) time.Duration { return now.Sub(s.StartedAt) }

// RPCError converts err into a JSON-RPC error with code.
func RPCError(code int, format string, args ...any) *linerpc.Error {
	return &linerpc.Error{Code: code, Message: fmt.Sprintf(format, args...)}
}
