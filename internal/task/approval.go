package task

import (
	"time"
)

// ApprovalKind is what a queued request asks the user for (§9.2): a permission for one
// of the runtime's approval kinds, or an answer to an agent's question.
type ApprovalKind string

// Approval kinds. The permission kinds mirror runtime.ApprovalKind.
const (
	ApprovalCommand    ApprovalKind = "command"
	ApprovalFileChange ApprovalKind = "file_change"
	ApprovalTool       ApprovalKind = "tool"
	ApprovalModeChange ApprovalKind = "mode_change"
	ApprovalQuestion   ApprovalKind = "question"
)

// Valid reports whether k is a known kind.
func (k ApprovalKind) Valid() bool {
	switch k {
	case ApprovalCommand, ApprovalFileChange, ApprovalTool, ApprovalModeChange, ApprovalQuestion:
		return true
	}
	return false
}

// ApprovalStatus is where a queued request stands.
type ApprovalStatus string

// Approval states. Pending is the only non-final state.
const (
	ApprovalPending  ApprovalStatus = "pending"
	ApprovalAllowed  ApprovalStatus = "allowed"
	ApprovalDenied   ApprovalStatus = "denied"
	ApprovalAnswered ApprovalStatus = "answered"
	// ApprovalExpired: nobody answered in time, or the daemon that asked is gone. A
	// permission is denied; a question gets the "no one is available" answer.
	ApprovalExpired ApprovalStatus = "expired"
	// ApprovalWithdrawn: the agent stopped waiting (its turn ended, the task was
	// cancelled), so the answer no longer matters.
	ApprovalWithdrawn ApprovalStatus = "withdrawn"
)

// Valid reports whether s is a known status.
func (s ApprovalStatus) Valid() bool {
	switch s {
	case ApprovalPending, ApprovalAllowed, ApprovalDenied, ApprovalAnswered, ApprovalExpired, ApprovalWithdrawn:
		return true
	}
	return false
}

// DecidedBy says who settled a request.
type DecidedBy string

// Deciders.
const (
	DecidedByUser    DecidedBy = "user"
	DecidedByTimeout DecidedBy = "timeout"
	DecidedByDaemon  DecidedBy = "daemon" // restart, cancel or a turn that ended
)

// ApprovalTimeout is how long a request waits before it expires (§9.2).
const ApprovalTimeout = 15 * time.Minute

// Approval is one queued permission request or question.
type Approval struct {
	ID     int64          `json:"id"`
	TaskID ID             `json:"task"`
	Agent  string         `json:"agent"`
	Kind   ApprovalKind   `json:"kind"`
	Status ApprovalStatus `json:"status"`
	// Tool is the tool in Claude Code vocabulary (Bash, Edit, WebFetch, mcp__…).
	Tool string `json:"tool,omitempty"`
	// Summary is the one line a person decides on: the command, the paths, the
	// question.
	Summary string `json:"summary"`
	// Options are a question's suggested answers.
	Options []string `json:"options,omitempty"`
	// Answer is the user's text (question) or message to the agent (deny).
	Answer    string    `json:"answer,omitempty"`
	DecidedBy DecidedBy `json:"decided_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	DecidedAt time.Time `json:"decided_at,omitzero"`
}

// Decision is how a pending request is settled.
type Decision struct {
	Status    ApprovalStatus
	Answer    string
	DecidedBy DecidedBy
}

// Validate checks that d settles a request of kind k.
func (d Decision) Validate(k ApprovalKind) error {
	switch d.Status {
	case ApprovalAllowed, ApprovalDenied:
		if k == ApprovalQuestion {
			return invalidf("a question is answered, not %s", d.Status)
		}
	case ApprovalAnswered:
		if k != ApprovalQuestion {
			return invalidf("only a question can be answered; approve or deny a %s request", k)
		}
		if d.Answer == "" {
			return invalidf("empty answer")
		}
	case ApprovalExpired, ApprovalWithdrawn:
	default:
		return invalidf("approval status %q is not a decision", d.Status)
	}
	switch d.DecidedBy {
	case DecidedByUser, DecidedByTimeout, DecidedByDaemon:
	default:
		return invalidf("unknown decider %q", d.DecidedBy)
	}
	return nil
}
