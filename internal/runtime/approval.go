package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Approver decides permission requests and answers agent questions. Adapters call it
// synchronously while the agent waits; implementations may block (e.g. on a human) and
// must honour ctx.
type Approver interface {
	Approve(ctx context.Context, req ApprovalRequest) (Decision, error)
	Answer(ctx context.Context, q Question) (string, error)
}

// ApprovalKind is what the agent wants to do.
type ApprovalKind string

// Approval kinds.
const (
	ApproveCommand    ApprovalKind = "command"     // run a shell command
	ApproveFileChange ApprovalKind = "file_change" // write/edit/delete files
	ApproveTool       ApprovalKind = "tool"        // any other tool (MCP, web, ...)
	ApproveModeChange ApprovalKind = "mode_change" // e.g. Claude's ExitPlanMode
)

// ApprovalRequest is a normalized permission request.
type ApprovalRequest struct {
	Session SessionRef   `json:"session"`
	Kind    ApprovalKind `json:"kind"`
	// Tool is the tool name in Claude Code vocabulary where one exists (Bash, Edit,
	// Write, Read, WebFetch, mcp__server__tool) so permission rules apply uniformly.
	Tool string `json:"tool"`
	// Command is the shell command for ApproveCommand.
	Command string `json:"command,omitempty"`
	// Paths are the files concerned, absolute where the CLI reports them so.
	Paths       []string        `json:"paths,omitempty"`
	Description string          `json:"description,omitempty"`
	Input       json.RawMessage `json:"input,omitempty"`
	// Raw is the CLI's original request, for audit.
	Raw json.RawMessage `json:"raw,omitempty"`
}

// Decision answers an ApprovalRequest.
type Decision struct {
	Allow bool `json:"allow"`
	// ForSession asks the CLI to remember an allow for the rest of the session where it
	// supports that.
	ForSession bool `json:"forSession,omitempty"`
	// Message is shown to the agent on deny.
	Message string `json:"message,omitempty"`
}

// Allow and Deny are convenience constructors.
func Allow() Decision { return Decision{Allow: true} }

// Deny refuses with a message for the agent.
func Deny(message string) Decision { return Decision{Message: message} }

// Question is the agent asking the user something.
type Question struct {
	Session SessionRef `json:"session"`
	Text    string     `json:"text"`
	Options []string   `json:"options,omitempty"`
}

// ErrTurnInProgress is returned by Session.Turn when a turn is already running.
var ErrTurnInProgress = errors.New("runtime: a turn is already in progress")

// ErrSessionClosed is returned by Session methods after Close or after the process died.
var ErrSessionClosed = errors.New("runtime: session is closed")

// ErrSpawnRejected means SessionSpec.OnSpawn refused a new process (for example because
// the task was cancelled); the process was killed. Adapters do not retry the start.
var ErrSpawnRejected = errors.New("runtime: process rejected by OnSpawn")

// ProtocolError means the CLI sent something the adapter does not understand where it
// must. It carries the offending line for diagnosis.
type ProtocolError struct {
	Runtime string
	Reason  string
	Line    []byte
}

func (e *ProtocolError) Error() string {
	line := string(e.Line)
	if len(line) > 200 {
		line = line[:200] + "…"
	}
	return fmt.Sprintf("runtime %s: protocol error: %s (line: %s)", e.Runtime, e.Reason, line)
}
