// Package task is OpenChef's domain model (docs/architecture.md §4, §7.1): tasks, their
// steps, the attempts agents make at them, the CLI sessions those attempts run in and the
// turns sent to each session, together with the state machines that govern them.
//
// The package has no dependencies on storage, runtimes or the engine; those layers
// import it.
package task

import (
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// MaxSteps is the largest number of steps a plan may have (§4: keeps plans reviewable).
const MaxSteps = 7

// ErrInvalid means a value does not satisfy the domain rules.
var ErrInvalid = errors.New("task: invalid")

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalid}, args...)...)
}

// ID identifies a task. It is short enough to type (`openchef status k3m9x2q7ab`) and
// safe to use in branch names and paths.
type ID string

const (
	idLen = 10
	// idAlphabet is Crockford's base32 in lower case: no i, l, o or u to misread.
	idAlphabet = "0123456789abcdefghjkmnpqrstvwxyz"
)

var idEncoding = base32.NewEncoding(idAlphabet).WithPadding(base32.NoPadding)

// NewID returns a random task ID (50 bits from crypto/rand).
func NewID() (ID, error) {
	var b [7]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("task: generate id: %w", err)
	}
	return ID(idEncoding.EncodeToString(b[:])[:idLen]), nil
}

// ParseID validates s as a task ID.
func ParseID(s string) (ID, error) {
	if len(s) != idLen {
		return "", invalidf("task id %q: want %d characters", s, idLen)
	}
	for _, r := range s {
		if !strings.ContainsRune(idAlphabet, r) {
			return "", invalidf("task id %q: unexpected character %q", s, r)
		}
	}
	return ID(s), nil
}

// Kind says how a task was started.
type Kind string

// Task kinds.
const (
	KindRun      Kind = "run"      // single step, no plan; starts running
	KindPipeline Kind = "pipeline" // plan → approve → implement → verify → review
)

// Valid reports whether k is a known kind.
func (k Kind) Valid() bool { return k == KindRun || k == KindPipeline }

// InitialStatus is the state a new task of this kind starts in.
func (k Kind) InitialStatus() Status {
	if k == KindRun {
		return StatusRunning
	}
	return StatusPlanning
}

// Mode is the workspace mode (§8.1).
type Mode string

// Workspace modes.
const (
	ModeIsolated Mode = "isolated"
	ModeTeam     Mode = "team"
)

// Valid reports whether m is a known mode.
func (m Mode) Valid() bool { return m == ModeIsolated || m == ModeTeam }

// Backend is where a task's workspace lives (§8.1, §21.3).
type Backend string

// Execution backends.
const (
	BackendLocal   Backend = "local"
	BackendSandbox Backend = "sandbox"
)

// Valid reports whether b is a known backend.
func (b Backend) Valid() bool { return b == BackendLocal || b == BackendSandbox }

// Importance describes how much a mistake in a step would hurt (§7.4).
type Importance string

// Importance levels, lowest first.
const (
	ImportanceLow      Importance = "low"
	ImportanceNormal   Importance = "normal"
	ImportanceHigh     Importance = "high"
	ImportanceCritical Importance = "critical"
)

var importanceOrder = []Importance{ImportanceLow, ImportanceNormal, ImportanceHigh, ImportanceCritical}

// Valid reports whether i is a known level.
func (i Importance) Valid() bool { return slices.Contains(importanceOrder, i) }

// AtLeast returns the higher of i and floor.
func (i Importance) AtLeast(floor Importance) Importance {
	if slices.Index(importanceOrder, floor) > slices.Index(importanceOrder, i) {
		return floor
	}
	return i
}

// PermissionMode is a permission mode in Claude Code's vocabulary (§9.1). The runtime
// layer has the same list (runtime.Modes); a test keeps them equal.
type PermissionMode string

// Permission modes.
const (
	PermissionsManual            PermissionMode = "manual"
	PermissionsAcceptEdits       PermissionMode = "acceptEdits"
	PermissionsPlan              PermissionMode = "plan"
	PermissionsAuto              PermissionMode = "auto"
	PermissionsDontAsk           PermissionMode = "dontAsk"
	PermissionsBypassPermissions PermissionMode = "bypassPermissions"
)

// PermissionModes lists every mode.
var PermissionModes = []PermissionMode{PermissionsManual, PermissionsAcceptEdits, PermissionsPlan,
	PermissionsAuto, PermissionsDontAsk, PermissionsBypassPermissions}

// Valid reports whether m is a known mode.
func (m PermissionMode) Valid() bool { return slices.Contains(PermissionModes, m) }

// TurnKind says why a message was sent to a session (§7.2, §5.3).
type TurnKind string

// Turn kinds.
const (
	TurnPrompt   TurnKind = "prompt"   // the step prompt, first message of an attempt
	TurnRetry    TurnKind = "retry"    // restated step + verification failure output
	TurnHandoff  TurnKind = "handoff"  // failover to another pool
	TurnRecovery TurnKind = "recovery" // the daemon restarted during the previous turn
	TurnNudge    TurnKind = "nudge"    // loop detected or idle; interrupted and redirected
)

// Valid reports whether k is a known turn kind.
func (k TurnKind) Valid() bool {
	switch k {
	case TurnPrompt, TurnRetry, TurnHandoff, TurnRecovery, TurnNudge:
		return true
	default:
		return false
	}
}

// Task is one user request.
type Task struct {
	ID     ID
	Kind   Kind
	Status Status
	// Prompt is the user's request as typed.
	Prompt string
	// RepoRoot is the absolute path of the user's repository.
	RepoRoot string
	// BaseCommit is the commit the task's branch was created from.
	BaseCommit string
	Mode       Mode
	Backend    Backend
	// Importance is the user's floor for every step (§7.4).
	Importance Importance
	// AgentPin, if set, disables routing for this task (§7.3).
	AgentPin string
	// Permissions is the task's permission mode (§9.1).
	Permissions PermissionMode
	// Branch and Worktree are set once the workspace exists.
	Branch   string
	Worktree string
	// ResumeAt is when a waiting_quota task is rescheduled; zero otherwise.
	ResumeAt time.Time
	// Reason explains needs_attention, failed or cancelled.
	Reason    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Validate checks the fields a new task must have.
func (t *Task) Validate() error {
	if _, err := ParseID(string(t.ID)); err != nil {
		return err
	}
	switch {
	case !t.Kind.Valid():
		return invalidf("task %s: kind %q", t.ID, t.Kind)
	case !t.Status.Valid():
		return invalidf("task %s: status %q", t.ID, t.Status)
	case strings.TrimSpace(t.Prompt) == "":
		return invalidf("task %s: empty prompt", t.ID)
	case t.RepoRoot == "":
		return invalidf("task %s: empty repository root", t.ID)
	case !t.Mode.Valid():
		return invalidf("task %s: mode %q", t.ID, t.Mode)
	case !t.Backend.Valid():
		return invalidf("task %s: backend %q", t.ID, t.Backend)
	case !t.Importance.Valid():
		return invalidf("task %s: importance %q", t.ID, t.Importance)
	case !t.Permissions.Valid():
		return invalidf("task %s: permission mode %q", t.ID, t.Permissions)
	}
	return nil
}

// Step is one unit of a plan. Index starts at 1.
type Step struct {
	TaskID ID
	Index  int
	Title  string
	Prompt string
	// Files is the optional scope of paths or globs the step may touch.
	Files []string
	// Verify is the verification command; empty means the step is accepted on completion.
	Verify     string
	Importance Importance
	Status     StepStatus
	// Retries counts verification failures fed back to the agent.
	Retries int
	// VerifiedCommit is the workspace commit that recorded this step.
	VerifiedCommit string
	UpdatedAt      time.Time
}

// ValidatePlan checks a task's steps: 1..MaxSteps steps, numbered 1..n in order.
func ValidatePlan(steps []Step) error {
	if len(steps) == 0 || len(steps) > MaxSteps {
		return invalidf("plan has %d steps; want 1..%d", len(steps), MaxSteps)
	}
	for i := range steps {
		s := &steps[i]
		switch {
		case s.Index != i+1:
			return invalidf("step %d has index %d", i+1, s.Index)
		case strings.TrimSpace(s.Title) == "":
			return invalidf("step %d: empty title", s.Index)
		case strings.TrimSpace(s.Prompt) == "":
			return invalidf("step %d: empty prompt", s.Index)
		case !s.Importance.Valid():
			return invalidf("step %d: importance %q", s.Index, s.Importance)
		case !s.Status.Valid():
			return invalidf("step %d: status %q", s.Index, s.Status)
		case s.Retries < 0:
			return invalidf("step %d: negative retries", s.Index)
		}
	}
	return nil
}

// Attempt is one agent working on one step. A failover starts a new attempt.
type Attempt struct {
	ID        int64
	TaskID    ID
	StepIndex int
	// Agent, Runtime and Pool are the routed (runtime, model, pool) triple (§4).
	Agent   string
	Runtime string
	Model   string
	Pool    string
	Status  AttemptStatus
	// Outcome is the reason the attempt ended (e.g. "quota_exhausted", "verify_failed").
	Outcome   string
	StartedAt time.Time
	EndedAt   time.Time
}

// Session is one agent CLI process and the conversation it hosts.
type Session struct {
	ID        int64
	AttemptID int64
	// ResumedFrom is the closed session whose conversation this one continues after a
	// daemon restart (§5.3); 0 for a new conversation.
	ResumedFrom int64
	Runtime     string
	// NativeID is the CLI's own id (Claude session id, Codex thread id, ACP session id);
	// empty until the CLI reports it.
	NativeID string
	// PID and StartToken identify the process exactly (see package procs) so a later
	// daemon can kill it without hitting a reused PID.
	PID        int
	StartToken uint64
	State      SessionState
	// LogDir holds raw.jsonl and events.jsonl for this session (§11).
	LogDir   string
	OpenedAt time.Time
	ClosedAt time.Time
}

// Turn is one message sent to a session and everything until the CLI ends the turn.
type Turn struct {
	ID        int64
	SessionID int64
	// Seq numbers the turns of one session from 1.
	Seq     int
	Kind    TurnKind
	Message string
	Status  TurnStatus
	// Outcome is the normalized result (e.g. "completed", "rate_limited", "crashed").
	Outcome   string
	StartedAt time.Time
	EndedAt   time.Time
}
