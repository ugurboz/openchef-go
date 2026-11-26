package engine

import (
	"time"

	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/task"
	"github.com/ugurboz/openchef/internal/verify"
)

// EventKind names an engine event.
type EventKind string

// Engine events, in the order a step produces them.
const (
	EventTaskStatus     EventKind = "task_status"     // Status, Message (reason)
	EventStepStarted    EventKind = "step_started"    // Step, Message (title)
	EventAttemptStarted EventKind = "attempt_started" // Attempt, Agent
	EventAgent          EventKind = "agent"           // Runtime: a normalized runtime event
	EventVerifyStarted  EventKind = "verify_started"  // Message (command)
	EventVerifyFinished EventKind = "verify_finished" // Verify
	EventAttemptEnded   EventKind = "attempt_ended"   // Status (attempt status), Message (outcome)
	EventStepFinished   EventKind = "step_finished"   // Status (step status), Message (commit)
	EventNotice         EventKind = "notice"          // Message: retries, backoff, failover, nudges

	// Published by the service's approval queue (§9.2), not by the engine.
	EventApprovalRequested EventKind = "approval_requested" // Agent, Approval
	EventApprovalDecided   EventKind = "approval_decided"   // Agent, Approval
)

// Event is one observable thing the engine did, streamed live to `run`, `logs -f` and
// `status` (D-014). Exactly the fields listed for its Kind are set.
type Event struct {
	TaskID  task.ID   `json:"task"`
	At      time.Time `json:"at"`
	Kind    EventKind `json:"kind"`
	Step    int       `json:"step,omitempty"`
	Attempt int64     `json:"attempt,omitempty"`
	Agent   string    `json:"agent,omitempty"`
	Status  string    `json:"status,omitempty"`
	Message string    `json:"message,omitempty"`

	Runtime  *runtime.Event `json:"runtime,omitempty"`
	Verify   *verify.Result `json:"verify,omitempty"`
	Approval *task.Approval `json:"approval,omitempty"`
}

// Sink receives engine events. Publish must not block: the engine calls it from the
// step loop, and a slow viewer must never slow an agent down. Implementations buffer
// and drop or fan out as they see fit. Publish may be called from several goroutines.
type Sink interface {
	Publish(Event)
}

// SinkFunc adapts a function to Sink.
type SinkFunc func(Event)

// Publish implements Sink.
func (f SinkFunc) Publish(ev Event) { f(ev) }

type discard struct{}

func (discard) Publish(Event) {}
