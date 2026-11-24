package codex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/ugurboz/openchef/internal/procs"
	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/runtime/linerpc"
	"github.com/ugurboz/openchef/internal/runtime/rawlog"
)

const (
	callTimeout     = 60 * time.Second
	closeTimeout    = 10 * time.Second
	eventBuffer     = 256
	maxToolOutput   = 4 << 10
	askUserToolName = "ask_user"
)

type session struct {
	spec runtime.SessionSpec
	proc *procs.Process
	peer *linerpc.Peer
	log  *rawlog.Log

	// routeMu serializes notification handling between the router goroutine and Turn,
	// which replays notifications that arrived before the turn id was known.
	routeMu sync.Mutex

	mu      sync.Mutex
	ref     runtime.SessionRef
	efforts []runtime.Effort
	limits  rateLimitSnapshot
	turn    *turnState
	items   map[string][]string // fileChange item id -> paths
	closed  bool

	closeOnce sync.Once
	closeErr  error
}

func newSession(spec runtime.SessionSpec, proc *procs.Process, log *rawlog.Log) *session {
	return &session{spec: spec, proc: proc, log: log, items: map[string][]string{}}
}

// turnState tracks one turn. Events are queued without blocking and handed to the
// consumer by a dedicated goroutine, so a consumer that stops reading can never block
// the notification router, the approval handler, Interrupt or Close.
type turnState struct {
	mu         sync.Mutex
	cond       *sync.Cond
	queue      []runtime.Event
	id         string
	ch         chan runtime.Event
	started    time.Time
	wantSchema bool
	final      string
	usageBase  *tokenBreakdown
	usageLast  tokenBreakdown
	denials    int
	lastErr    *turnError
	done       bool
	// early holds turn-scoped notifications that arrived while the turn id was still
	// unknown. They are replayed once the id is known; those of other (stale) turns are
	// then discarded.
	early []linerpc.Notification
}

func newTurnState(wantSchema bool) *turnState {
	ts := &turnState{ch: make(chan runtime.Event, eventBuffer), started: time.Now(), wantSchema: wantSchema}
	ts.cond = sync.NewCond(&ts.mu)
	go ts.deliver()
	return ts
}

// emit queues a non-terminal event. It never blocks.
func (ts *turnState) emit(ev runtime.Event) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.done {
		return
	}
	ev.At = time.Now()
	ts.queue = append(ts.queue, ev)
	ts.cond.Signal()
}

// finish queues the terminal event once. It never blocks and reports whether this call
// ended the turn.
func (ts *turnState) finish(ev runtime.Event) bool {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.done {
		return false
	}
	ts.done = true
	ev.At = time.Now()
	ts.queue = append(ts.queue, ev)
	ts.cond.Signal()
	return true
}

// deliver moves queued events to the consumer's channel and closes it after the
// terminal event. The consumer must drain the channel until it is closed.
func (ts *turnState) deliver() {
	for {
		ts.mu.Lock()
		for len(ts.queue) == 0 {
			ts.cond.Wait()
		}
		ev := ts.queue[0]
		ts.queue = ts.queue[1:]
		ts.mu.Unlock()
		ts.ch <- ev
		if ev.Kind.Terminal() {
			close(ts.ch)
			return
		}
	}
}

func (ts *turnState) usage() runtime.Usage {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.usageBase == nil {
		return runtime.Usage{}
	}
	return ts.usageLast.minus(*ts.usageBase).usage()
}

// Ref implements runtime.Session.
func (s *session) Ref() runtime.SessionRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ref
}

// Turn implements runtime.Session.
func (s *session) Turn(ctx context.Context, in runtime.TurnInput) (<-chan runtime.Event, error) {
	s.mu.Lock()
	switch {
	case s.closed:
		s.mu.Unlock()
		return nil, runtime.ErrSessionClosed
	case s.turn != nil:
		s.mu.Unlock()
		return nil, runtime.ErrTurnInProgress
	}
	ts := newTurnState(len(in.OutputSchema) > 0)
	s.turn = ts
	thread := s.ref.ID
	params := map[string]any{
		"threadId": thread,
		"input":    []any{map[string]any{"type": "text", "text": in.Text}},
	}
	if e := s.spec.Effort.Nearest(s.efforts); e != runtime.EffortDefault {
		params["effort"] = string(e)
	}
	s.mu.Unlock()
	if ts.wantSchema {
		params["outputSchema"] = in.OutputSchema
	}

	var res turnStartResponse
	err := s.call(ctx, "turn/start", params, &res)
	if err == nil && res.Turn.ID == "" {
		err = &runtime.ProtocolError{Runtime: Name, Reason: "turn/start returned no turn id"}
	}
	if err != nil {
		// Terminate the delivery goroutine; nobody will read this channel.
		ts.finish(runtime.Event{Kind: runtime.EventTurnFailed, Failure: &runtime.Failure{Kind: runtime.FailProtocol, Message: err.Error()}})
		go func() {
			for range ts.ch { // drain: the channel is never handed to a caller
			}
		}()
		s.clearTurn(ts)
		return nil, err
	}
	s.setTurnID(ts, res.Turn.ID)

	go s.watchContext(ctx, ts)
	return ts.ch, nil
}

// watchContext interrupts the turn when the caller's context ends.
func (s *session) watchContext(ctx context.Context, ts *turnState) {
	select {
	case <-ctx.Done():
	case <-s.peer.Done():
		return
	}
	ts.mu.Lock()
	done := ts.done
	ts.mu.Unlock()
	if done {
		return
	}
	ictx, cancel := context.WithTimeout(context.WithoutCancel(ctx), callTimeout)
	defer cancel()
	if err := s.Interrupt(ictx); err != nil {
		s.log.Notef(rawlog.Note, "codex: interrupt after context end failed: %v", err)
	}
	kind := runtime.FailInterrupted
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		kind = runtime.FailTimeout
	}
	s.endTurn(ts, runtime.Event{Kind: runtime.EventTurnFailed, Failure: &runtime.Failure{
		Kind: kind, Message: "turn context ended: " + ctx.Err().Error(), Usage: ts.usage(),
	}})
}

// Interrupt implements runtime.Session.
func (s *session) Interrupt(ctx context.Context) error {
	s.mu.Lock()
	ts, thread := s.turn, s.ref.ID
	s.mu.Unlock()
	if ts == nil {
		return nil
	}
	ts.mu.Lock()
	id := ts.id
	ts.mu.Unlock()
	return s.call(ctx, "turn/interrupt", map[string]any{"threadId": thread, "turnId": id}, nil)
}

// Close implements runtime.Session.
func (s *session) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		s.mu.Lock()
		s.closed = true
		ts := s.turn
		s.mu.Unlock()
		if ts != nil {
			s.endTurn(ts, runtime.Event{Kind: runtime.EventTurnFailed, Failure: &runtime.Failure{
				Kind: runtime.FailInterrupted, Message: "session closed", Usage: ts.usage(),
			}})
		}
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
		defer cancel()
		procErr := s.proc.Close(cctx)
		select {
		case <-s.peer.Done():
		case <-cctx.Done():
		}
		s.closeErr = errors.Join(procErr, s.log.Close())
	})
	return s.closeErr
}

// fail closes a session that could not be set up and returns err.
func (s *session) fail(ctx context.Context, err error) error {
	return errors.Join(err, s.Close(ctx))
}

func (s *session) call(ctx context.Context, method string, params, out any) error {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if err := s.peer.Call(cctx, method, params, out); err != nil {
		if errors.Is(err, linerpc.ErrClosed) {
			return fmt.Errorf("codex: %w: %w", runtime.ErrSessionClosed, err)
		}
		return fmt.Errorf("codex: %w", err)
	}
	return nil
}

func (s *session) clearTurn(ts *turnState) {
	s.mu.Lock()
	if s.turn == ts {
		s.turn = nil
	}
	s.mu.Unlock()
}

func (s *session) endTurn(ts *turnState, ev runtime.Event) {
	if ts.finish(ev) {
		s.clearTurn(ts)
	}
}

func (s *session) onProtocolError(line []byte, err error) {
	s.log.Notef(rawlog.Note, "codex: protocol error: %v", err)
	_ = line
}

// setTurnID records the turn id (first source wins: turn/start response or turn/started)
// and replays notifications that were held back while it was unknown.
func (s *session) setTurnID(ts *turnState, id string) {
	s.routeMu.Lock()
	defer s.routeMu.Unlock()
	s.adoptTurnID(ts, id)
}

// adoptTurnID must be called with routeMu held.
func (s *session) adoptTurnID(ts *turnState, id string) {
	ts.mu.Lock()
	if ts.id != "" {
		ts.mu.Unlock()
		return
	}
	ts.id = id
	early := ts.early
	ts.early = nil
	ts.mu.Unlock()
	for _, n := range early {
		s.onNotification(n)
	}
}

// route consumes notifications for the whole life of the session.
func (s *session) route() {
	for n := range s.peer.Notifications() {
		s.routeMu.Lock()
		s.onNotification(n)
		s.routeMu.Unlock()
	}
	// The app server is gone.
	s.mu.Lock()
	s.closed = true
	ts := s.turn
	s.mu.Unlock()
	if ts != nil {
		msg := "codex app-server exited during the turn"
		if err := s.peer.Err(); err != nil {
			msg += ": " + err.Error()
		}
		s.endTurn(ts, runtime.Event{Kind: runtime.EventTurnFailed, Failure: &runtime.Failure{
			Kind: runtime.FailCrashed, Message: msg, Usage: ts.usage(),
		}})
	}
}

// current returns the active turn if notification n (scoped to threadID/turnID)
// belongs to it. While the turn id is unknown, turn-scoped notifications are held back
// in ts.early instead of being guessed at: after a resume Codex can still send
// notifications for the previous turn. Must be called with routeMu held.
func (s *session) current(n linerpc.Notification, threadID, turnID string) *turnState {
	s.mu.Lock()
	ts, thread := s.turn, s.ref.ID
	s.mu.Unlock()
	if ts == nil || threadID != thread {
		return nil
	}
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if ts.id == "" {
		if turnID != "" {
			ts.early = append(ts.early, n)
		}
		return nil
	}
	if turnID != "" && turnID != ts.id {
		return nil
	}
	return ts
}

func (s *session) onNotification(n linerpc.Notification) {
	switch n.Method {
	case "account/rateLimits/updated":
		var p rateLimitsUpdated
		if json.Unmarshal(n.Params, &p) != nil {
			return
		}
		sig := s.mergeLimits(p.RateLimits)
		s.mu.Lock()
		ts := s.turn
		s.mu.Unlock()
		if ts != nil && sig != nil {
			ts.emit(runtime.Event{Kind: runtime.EventQuota, Quota: sig})
		}
	case "item/agentMessage/delta":
		var p deltaNotification
		if json.Unmarshal(n.Params, &p) == nil && p.Delta != "" {
			if ts := s.current(n, p.ThreadID, p.TurnID); ts != nil {
				ts.emit(runtime.Event{Kind: runtime.EventText, Text: p.Delta})
			}
		}
	case "item/started":
		var p itemNotification
		if json.Unmarshal(n.Params, &p) != nil {
			return
		}
		if p.Item.Type == "fileChange" {
			s.rememberPaths(p.Item)
		}
		if ts := s.current(n, p.ThreadID, p.TurnID); ts != nil {
			if call := toolCall(p.Item); call != nil {
				ts.emit(runtime.Event{Kind: runtime.EventToolCall, ToolCall: call})
			}
		}
	case "item/completed":
		var p itemNotification
		if json.Unmarshal(n.Params, &p) != nil {
			return
		}
		ts := s.current(n, p.ThreadID, p.TurnID)
		if ts == nil {
			return
		}
		if p.Item.Type == "agentMessage" {
			if p.Item.Phase == "" || p.Item.Phase == "final_answer" {
				ts.mu.Lock()
				ts.final = p.Item.Text
				ts.mu.Unlock()
			}
			return
		}
		if res := toolResult(p.Item); res != nil {
			ts.emit(runtime.Event{Kind: runtime.EventToolResult, ToolResult: res})
		}
	case "thread/tokenUsage/updated":
		var p tokenUsageNotification
		if json.Unmarshal(n.Params, &p) != nil {
			return
		}
		if ts := s.current(n, p.ThreadID, p.TurnID); ts != nil {
			ts.mu.Lock()
			if ts.usageBase == nil {
				base := p.TokenUsage.Total.minus(p.TokenUsage.Last)
				ts.usageBase = &base
			}
			ts.usageLast = p.TokenUsage.Total
			ts.mu.Unlock()
		}
	case "error":
		var p errorNotification
		if json.Unmarshal(n.Params, &p) != nil {
			return
		}
		if ts := s.current(n, p.ThreadID, p.TurnID); ts != nil && !p.WillRetry {
			ts.mu.Lock()
			e := p.Error
			ts.lastErr = &e
			ts.mu.Unlock()
		}
	case "turn/started":
		var p struct {
			ThreadID string `json:"threadId"`
			Turn     struct {
				ID string `json:"id"`
			} `json:"turn"`
		}
		if json.Unmarshal(n.Params, &p) != nil || p.Turn.ID == "" {
			return
		}
		s.mu.Lock()
		ts, thread := s.turn, s.ref.ID
		s.mu.Unlock()
		if ts != nil && p.ThreadID == thread {
			s.adoptTurnID(ts, p.Turn.ID) // routeMu is held by the caller
		}
	case "turn/completed":
		var p turnCompletedNotification
		if json.Unmarshal(n.Params, &p) != nil {
			return
		}
		if ts := s.current(n, p.ThreadID, p.Turn.ID); ts != nil {
			s.endTurn(ts, s.completion(ts, p))
		}
	}
}

func (s *session) completion(ts *turnState, p turnCompletedNotification) runtime.Event {
	usage := ts.usage()
	ts.mu.Lock()
	final, lastErr, denials, want := ts.final, ts.lastErr, ts.denials, ts.wantSchema
	duration := time.Since(ts.started)
	ts.mu.Unlock()
	if p.Turn.DurationMs != nil {
		duration = time.Duration(*p.Turn.DurationMs) * time.Millisecond
	}

	switch p.Turn.Status {
	case "completed":
		res := &runtime.TurnResult{FinalText: final, Usage: usage, Denials: denials, Duration: duration}
		if want && json.Valid([]byte(final)) {
			res.Structured = json.RawMessage(final)
		}
		return runtime.Event{Kind: runtime.EventTurnCompleted, Result: res}
	case "interrupted":
		return runtime.Event{Kind: runtime.EventTurnFailed, Failure: &runtime.Failure{
			Kind: runtime.FailInterrupted, Message: "turn interrupted", Usage: usage,
		}}
	default:
		te := p.Turn.Error
		if te == nil {
			te = lastErr
		}
		if te == nil {
			te = &turnError{Message: "turn ended with status " + p.Turn.Status}
		}
		f := te.failure(failureKind(te.CodexErrorInfo))
		f.Usage = usage
		if f.Kind == runtime.FailQuotaExhausted || f.Kind == runtime.FailRateLimited {
			f.ResetsAt = s.earliestReset()
		}
		return runtime.Event{Kind: runtime.EventTurnFailed, Failure: f}
	}
}

func toolCall(it threadItem) *runtime.ToolCall {
	switch it.Type {
	case "commandExecution":
		input, _ := json.Marshal(map[string]string{"command": it.Command})
		return &runtime.ToolCall{ID: it.ID, Name: "Bash", Input: input, Signature: "Bash:" + it.Command}
	case "fileChange":
		paths := make([]string, 0, len(it.Changes))
		for _, c := range it.Changes {
			paths = append(paths, c.Path)
		}
		input, _ := json.Marshal(map[string]any{"paths": paths})
		return &runtime.ToolCall{ID: it.ID, Name: "Edit", Input: input, Signature: "Edit:" + strings.Join(paths, ",")}
	case "mcpToolCall":
		name := "mcp__" + it.Server + "__" + it.Tool
		return &runtime.ToolCall{ID: it.ID, Name: name, Input: it.Arguments, Signature: name + ":" + string(it.Arguments)}
	default:
		return nil
	}
}

func toolResult(it threadItem) *runtime.ToolResult {
	switch it.Type {
	case "commandExecution":
		failed := it.Status != "completed" || (it.ExitCode != nil && *it.ExitCode != 0)
		return &runtime.ToolResult{ID: it.ID, IsError: failed, Output: truncate(it.AggregatedOutput)}
	case "fileChange", "mcpToolCall":
		failed := it.Status != "completed" || (len(it.Error) > 0 && string(it.Error) != "null")
		return &runtime.ToolResult{ID: it.ID, IsError: failed}
	default:
		return nil
	}
}

func truncate(s string) string {
	if len(s) <= maxToolOutput {
		return s
	}
	half := maxToolOutput / 2
	return s[:half] + "\n…\n" + s[len(s)-half:]
}

func (s *session) rememberPaths(it threadItem) {
	paths := make([]string, 0, len(it.Changes))
	for _, c := range it.Changes {
		paths = append(paths, c.Path)
	}
	s.mu.Lock()
	s.items[it.ID] = paths
	s.mu.Unlock()
}

func (s *session) setLimits(snap rateLimitSnapshot) {
	s.mu.Lock()
	s.limits = snap
	s.mu.Unlock()
}

func (s *session) mergeLimits(update rateLimitSnapshot) *runtime.QuotaSignal {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.limits = s.limits.merge(update)
	return s.limits.signal()
}

func (s *session) earliestReset() time.Time {
	s.mu.Lock()
	sig := s.limits.signal()
	s.mu.Unlock()
	var best time.Time
	if sig == nil {
		return best
	}
	for _, w := range sig.Windows {
		if !w.ResetsAt.IsZero() && w.Utilization >= 1 && (best.IsZero() || w.ResetsAt.Before(best)) {
			best = w.ResetsAt
		}
	}
	return best
}
