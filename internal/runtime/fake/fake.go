// Package fake is a scriptable in-process runtime.Runtime for engine tests
// (docs/architecture.md §16, §18). Each Runtime plays back a queue of scripted turns
// shared by all of its sessions, so a test can say "the first agent hits its quota, the
// second one finishes" without any CLI installed.
package fake

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ugurboz/openchef/internal/procs"
	"github.com/ugurboz/openchef/internal/runtime"
)

// pidBase is above every real PID limit (Linux caps at 2^22), so the identities a fake
// reports through OnSpawn never belong to a live process.
const pidBase = 1 << 23

// Turn scripts one turn.
type Turn struct {
	// Events are emitted before the terminal event (text, tool calls, quota signals).
	Events []runtime.Event
	// Edit, if set, runs in the session directory before the turn ends, standing in
	// for the agent's file changes. An error fails the turn as crashed.
	Edit func(dir string) error
	// Fail, if set, ends the turn with this failure instead of completing.
	Fail *runtime.Failure
	// Hang blocks the turn until it is interrupted or its context ends.
	Hang bool
	// Text is the final answer of a completed turn.
	Text string
}

// CallKind names a recorded call.
type CallKind string

// Recorded calls.
const (
	CallOpen      CallKind = "open"
	CallResume    CallKind = "resume"
	CallTurn      CallKind = "turn"
	CallInterrupt CallKind = "interrupt"
	CallClose     CallKind = "close"
)

// Call is one recorded interaction, for assertions.
type Call struct {
	Kind    CallKind
	Session string
	// Text is the message of a turn.
	Text string
	// Spec is the session spec of an open or resume.
	Spec runtime.SessionSpec
}

// Runtime is a fake agent CLI.
type Runtime struct {
	name string

	mu       sync.Mutex
	script   []Turn
	calls    []Call
	sessions int
	tokens   uint64
	openErrs []error
	live     map[string]*session
}

// New returns a fake runtime named name that plays turns in order.
func New(name string, turns ...Turn) *Runtime {
	return &Runtime{name: name, script: turns, live: map[string]*session{}}
}

// Push appends turns to the script.
func (r *Runtime) Push(turns ...Turn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.script = append(r.script, turns...)
}

// FailNextOpen makes the next Open or Resume return err.
func (r *Runtime) FailNextOpen(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.openErrs = append(r.openErrs, err)
}

// Calls returns every recorded call in order.
func (r *Runtime) Calls() []Call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Call(nil), r.calls...)
}

// Turns returns the messages of every turn in order.
func (r *Runtime) Turns() []string {
	calls := r.Calls()
	var out []string
	for i := range calls {
		if calls[i].Kind == CallTurn {
			out = append(out, calls[i].Text)
		}
	}
	return out
}

// Remaining is the number of scripted turns not yet played.
func (r *Runtime) Remaining() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.script)
}

// Live is the number of sessions not yet closed.
func (r *Runtime) Live() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.live)
}

// Name implements runtime.Runtime.
func (r *Runtime) Name() string { return r.name }

// Open implements runtime.Runtime.
func (r *Runtime) Open(ctx context.Context, spec runtime.SessionSpec) (runtime.Session, error) {
	return r.start(ctx, spec, "", CallOpen)
}

// Resume implements runtime.Runtime.
func (r *Runtime) Resume(ctx context.Context, ref runtime.SessionRef, spec runtime.SessionSpec) (runtime.Session, error) {
	if ref.Runtime != r.name || ref.ID == "" {
		return nil, fmt.Errorf("fake: cannot resume %+v on %s", ref, r.name)
	}
	return r.start(ctx, spec, ref.ID, CallResume)
}

func (r *Runtime) start(ctx context.Context, spec runtime.SessionSpec, id string, kind CallKind) (runtime.Session, error) {
	if spec.Approver == nil {
		return nil, errors.New("fake: SessionSpec.Approver is required")
	}
	r.mu.Lock()
	r.sessions++
	r.tokens++
	n, token := r.sessions, r.tokens
	if id == "" {
		id = fmt.Sprintf("%s-session-%d", r.name, n)
	}
	r.calls = append(r.calls, Call{Kind: kind, Session: id, Spec: spec})
	var openErr error
	if len(r.openErrs) > 0 {
		openErr, r.openErrs = r.openErrs[0], r.openErrs[1:]
	}
	r.mu.Unlock()
	if openErr != nil {
		return nil, openErr
	}
	if err := spec.Spawned(ctx, procs.Identity{PID: pidBase + n, StartToken: token}); err != nil {
		return nil, err // Spawned already wraps runtime.ErrSpawnRejected
	}
	s := &session{rt: r, ref: runtime.SessionRef{Runtime: r.name, ID: id}, dir: spec.Dir}
	r.mu.Lock()
	r.live[id] = s
	r.mu.Unlock()
	return s, nil
}

func (r *Runtime) next() (Turn, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.script) == 0 {
		return Turn{}, false
	}
	t := r.script[0]
	r.script = r.script[1:]
	return t, true
}

func (r *Runtime) record(c Call) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, c)
}

type session struct {
	rt  *Runtime
	ref runtime.SessionRef
	dir string

	mu        sync.Mutex
	closed    bool
	turning   bool
	interrupt chan struct{}
}

func (s *session) Ref() runtime.SessionRef { return s.ref }

func (s *session) Turn(ctx context.Context, in runtime.TurnInput) (<-chan runtime.Event, error) {
	s.mu.Lock()
	switch {
	case s.closed:
		s.mu.Unlock()
		return nil, runtime.ErrSessionClosed
	case s.turning:
		s.mu.Unlock()
		return nil, runtime.ErrTurnInProgress
	}
	s.turning = true
	s.interrupt = make(chan struct{})
	stop := s.interrupt
	s.mu.Unlock()
	s.rt.record(Call{Kind: CallTurn, Session: s.ref.ID, Text: in.Text})

	ch := make(chan runtime.Event, 16)
	go s.play(ctx, stop, ch)
	return ch, nil
}

func (s *session) play(ctx context.Context, stop <-chan struct{}, ch chan<- runtime.Event) {
	defer close(ch)
	defer func() {
		s.mu.Lock()
		s.turning = false
		s.mu.Unlock()
	}()
	start := time.Now()
	t, ok := s.rt.next()
	if !ok {
		ch <- failed(runtime.FailProtocol, "fake: script exhausted")
		return
	}
	for _, ev := range t.Events {
		if ev.At.IsZero() {
			ev.At = time.Now()
		}
		select {
		case ch <- ev:
		case <-stop:
			ch <- failed(runtime.FailInterrupted, "fake: interrupted")
			return
		case <-ctx.Done():
			ch <- ctxFailure(ctx)
			return
		}
	}
	if t.Hang {
		select {
		case <-stop:
			ch <- failed(runtime.FailInterrupted, "fake: interrupted")
		case <-ctx.Done():
			ch <- ctxFailure(ctx)
		}
		return
	}
	if t.Edit != nil {
		if err := t.Edit(s.dir); err != nil {
			ch <- failed(runtime.FailCrashed, "fake: edit: "+err.Error())
			return
		}
	}
	if t.Fail != nil {
		f := *t.Fail
		ch <- runtime.Event{Kind: runtime.EventTurnFailed, At: time.Now(), Failure: &f}
		return
	}
	ch <- runtime.Event{Kind: runtime.EventTurnCompleted, At: time.Now(), Result: &runtime.TurnResult{
		FinalText: t.Text, Duration: time.Since(start),
	}}
}

func failed(kind runtime.FailureKind, msg string) runtime.Event {
	return runtime.Event{Kind: runtime.EventTurnFailed, At: time.Now(), Failure: &runtime.Failure{Kind: kind, Message: msg}}
}

func ctxFailure(ctx context.Context) runtime.Event {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return failed(runtime.FailTimeout, "fake: turn deadline")
	}
	return failed(runtime.FailInterrupted, "fake: context cancelled")
}

func (s *session) Interrupt(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return runtime.ErrSessionClosed
	}
	s.rt.record(Call{Kind: CallInterrupt, Session: s.ref.ID})
	s.stopTurnLocked()
	return nil
}

// stopTurnLocked ends the turn in progress, if any. The caller holds s.mu.
func (s *session) stopTurnLocked() {
	if s.turning && s.interrupt != nil {
		select {
		case <-s.interrupt:
		default:
			close(s.interrupt)
		}
	}
}

func (s *session) Close(context.Context) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.stopTurnLocked()
	s.mu.Unlock()
	s.rt.record(Call{Kind: CallClose, Session: s.ref.ID})
	s.rt.mu.Lock()
	delete(s.rt.live, s.ref.ID)
	s.rt.mu.Unlock()
	return nil
}

// Completed is a convenience Turn that completes with text.
func Completed(text string) Turn { return Turn{Text: text} }

// Failing is a convenience Turn that fails with kind.
func Failing(kind runtime.FailureKind, msg string) Turn {
	return Turn{Fail: &runtime.Failure{Kind: kind, Message: msg}}
}

// ToolCall returns a tool-call event with the given signature.
func ToolCall(name, signature string) runtime.Event {
	return runtime.Event{Kind: runtime.EventToolCall, ToolCall: &runtime.ToolCall{Name: name, Signature: signature}}
}
