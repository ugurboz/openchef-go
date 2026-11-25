package task

import (
	"errors"
	"fmt"
	"slices"
)

// ErrInvalidTransition means a state change is not allowed by the state machine.
var ErrInvalidTransition = errors.New("task: invalid state transition")

// TransitionError describes a rejected state change. It matches ErrInvalidTransition
// with errors.Is.
type TransitionError struct {
	Entity string // "task", "step", "attempt", "session" or "turn"
	From   string
	To     string
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("task: %s cannot move from %q to %q", e.Entity, e.From, e.To)
}

// Is reports whether target is ErrInvalidTransition.
func (e *TransitionError) Is(target error) bool { return target == ErrInvalidTransition }

// machine lists, for every known state, the states it may move to. A state with no
// entry is unknown; a state with an empty list is terminal.
type machine[S ~string] struct {
	entity string
	next   map[S][]S
}

func (m machine[S]) valid(s S) bool {
	_, ok := m.next[s]
	return ok
}

func (m machine[S]) terminal(s S) bool {
	next, ok := m.next[s]
	return ok && len(next) == 0
}

func (m machine[S]) check(from, to S) error {
	if slices.Contains(m.next[from], to) {
		return nil
	}
	return &TransitionError{Entity: m.entity, From: string(from), To: string(to)}
}

// Status is the state of a task (docs/architecture.md §7.1).
type Status string

// Task states.
const (
	StatusPlanning         Status = "planning"
	StatusAwaitingApproval Status = "awaiting_approval"
	StatusRunning          Status = "running"
	StatusReviewing        Status = "reviewing"
	StatusWaitingQuota     Status = "waiting_quota"
	StatusNeedsAttention   Status = "needs_attention"
	StatusDone             Status = "done"
	StatusFailed           Status = "failed"
	StatusCancelled        Status = "cancelled"
)

var taskMachine = machine[Status]{entity: "task", next: map[Status][]Status{
	StatusPlanning:         {StatusAwaitingApproval, StatusFailed, StatusCancelled},
	StatusAwaitingApproval: {StatusRunning, StatusCancelled},
	StatusRunning: {
		StatusReviewing, StatusDone, StatusWaitingQuota, StatusNeedsAttention,
		StatusFailed, StatusCancelled,
	},
	// A review that requests changes sends the task back to running (M5).
	StatusReviewing:      {StatusRunning, StatusDone, StatusNeedsAttention, StatusFailed, StatusCancelled},
	StatusWaitingQuota:   {StatusRunning, StatusCancelled},
	StatusNeedsAttention: {StatusRunning, StatusCancelled},
	StatusDone:           {},
	StatusFailed:         {},
	StatusCancelled:      {},
}}

// Valid reports whether s is a known task state.
func (s Status) Valid() bool { return taskMachine.valid(s) }

// Terminal reports whether s is final (done, failed or cancelled).
func (s Status) Terminal() bool { return taskMachine.terminal(s) }

// TerminalStatuses lists the final task states.
func TerminalStatuses() []Status {
	var out []Status
	for s, next := range taskMachine.next {
		if len(next) == 0 {
			out = append(out, s)
		}
	}
	slices.Sort(out)
	return out
}

// Active reports whether an engine loop drives a task in state s: its steps may start
// attempts, sessions and turns only then.
func (s Status) Active() bool { return s == StatusRunning || s == StatusReviewing }

// CheckTransition returns a *TransitionError unless a task may move from s to to.
func (s Status) CheckTransition(to Status) error { return taskMachine.check(s, to) }

// StepStatus is the state of one plan step.
type StepStatus string

// Step states.
const (
	StepPending  StepStatus = "pending"
	StepRunning  StepStatus = "running"
	StepVerified StepStatus = "verified"
	StepFailed   StepStatus = "failed"
)

var stepMachine = machine[StepStatus]{entity: "step", next: map[StepStatus][]StepStatus{
	// running → pending: the attempt ended without a verdict (failover, daemon restart).
	StepPending:  {StepRunning},
	StepRunning:  {StepVerified, StepFailed, StepPending},
	StepVerified: {},
	// failed → pending: the user retries the task.
	StepFailed: {StepPending},
}}

// Valid reports whether s is a known step state.
func (s StepStatus) Valid() bool { return stepMachine.valid(s) }

// CheckTransition returns a *TransitionError unless a step may move from s to to.
func (s StepStatus) CheckTransition(to StepStatus) error { return stepMachine.check(s, to) }

// AttemptStatus is the state of one agent's attempt at one step.
type AttemptStatus string

// Attempt states.
const (
	AttemptRunning     AttemptStatus = "running"
	AttemptSucceeded   AttemptStatus = "succeeded"
	AttemptFailedOver  AttemptStatus = "failed_over"
	AttemptFailed      AttemptStatus = "failed"
	AttemptInterrupted AttemptStatus = "interrupted"
	AttemptCancelled   AttemptStatus = "cancelled"
)

var attemptMachine = machine[AttemptStatus]{entity: "attempt", next: map[AttemptStatus][]AttemptStatus{
	AttemptRunning: {
		AttemptSucceeded, AttemptFailedOver, AttemptFailed, AttemptInterrupted, AttemptCancelled,
	},
	AttemptSucceeded:   {},
	AttemptFailedOver:  {},
	AttemptFailed:      {},
	AttemptInterrupted: {},
	AttemptCancelled:   {},
}}

// Valid reports whether s is a known attempt state.
func (s AttemptStatus) Valid() bool { return attemptMachine.valid(s) }

// Terminal reports whether the attempt has ended.
func (s AttemptStatus) Terminal() bool { return attemptMachine.terminal(s) }

// CheckTransition returns a *TransitionError unless an attempt may move from s to to.
func (s AttemptStatus) CheckTransition(to AttemptStatus) error {
	return attemptMachine.check(s, to)
}

// SessionState is the lifecycle of one agent CLI process and its conversation.
type SessionState string

// Session states. A session is recorded as starting (process identity known, native id
// not yet) before the first message is sent (§5.1), opened once the CLI reports its id,
// and closed when its process is gone.
const (
	SessionStarting SessionState = "starting"
	SessionOpen     SessionState = "open"
	SessionClosed   SessionState = "closed"
)

var sessionMachine = machine[SessionState]{entity: "session", next: map[SessionState][]SessionState{
	SessionStarting: {SessionOpen, SessionClosed},
	SessionOpen:     {SessionClosed},
	SessionClosed:   {},
}}

// Valid reports whether s is a known session state.
func (s SessionState) Valid() bool { return sessionMachine.valid(s) }

// CheckTransition returns a *TransitionError unless a session may move from s to to.
func (s SessionState) CheckTransition(to SessionState) error {
	return sessionMachine.check(s, to)
}

// TurnStatus is the state of one message sent to a session.
type TurnStatus string

// Turn states. A turn is recorded as sent before the message is written to the CLI.
const (
	TurnSent        TurnStatus = "sent"
	TurnCompleted   TurnStatus = "completed"
	TurnFailed      TurnStatus = "failed"
	TurnInterrupted TurnStatus = "interrupted"
)

var turnMachine = machine[TurnStatus]{entity: "turn", next: map[TurnStatus][]TurnStatus{
	TurnSent:        {TurnCompleted, TurnFailed, TurnInterrupted},
	TurnCompleted:   {},
	TurnFailed:      {},
	TurnInterrupted: {},
}}

// Valid reports whether s is a known turn state.
func (s TurnStatus) Valid() bool { return turnMachine.valid(s) }

// CheckTransition returns a *TransitionError unless a turn may move from s to to.
func (s TurnStatus) CheckTransition(to TurnStatus) error { return turnMachine.check(s, to) }
