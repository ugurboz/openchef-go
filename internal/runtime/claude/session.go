package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ugurboz/openchef/internal/procs"
	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/runtime/rawlog"
	"github.com/ugurboz/openchef/internal/runtime/turnq"
)

// Timeouts are variables so tests can shorten them.
var (
	initTimeout   = 15 * time.Second // init answers in ~0.2 s (m1-findings §9)
	interruptWait = 10 * time.Second // D-009: how long Interrupt waits for the owed result
	closeWait     = 3 * time.Second  // D-010: how long Close waits for an in-flight turn
	exitWait      = 2 * time.Second  // how long a crash report waits for the exit status
)

const (
	maxLine       = 64 << 20
	maxToolOutput = 4 << 10
	stderrTail    = 2 << 10
)

// proc is one Claude process ("generation"). A session replaces it for a new output
// schema (D-011), after killing a turn that would not stop (D-009, D-012) and after an
// unexpected exit; the replacement continues the conversation with --resume.
type proc struct {
	p      *procs.Process // nil in replay tests
	w      io.Writer      // the process's stdin
	stderr *tailBuffer
	wmu    sync.Mutex
	done   chan struct{} // closed when the reader finished (stdout closed)
	killed atomic.Bool   // stopped deliberately by OpenChef

	// Guarded by session.mu.
	written  int // user messages written to this process
	results  int // results received from this process
	controls map[string]chan controlResponseBody
	perms    map[string]context.CancelFunc
}

func newProc(w io.Writer, p *procs.Process) *proc {
	return &proc{
		p: p, w: w, stderr: &tailBuffer{}, done: make(chan struct{}),
		controls: map[string]chan controlResponseBody{}, perms: map[string]context.CancelFunc{},
	}
}

type session struct {
	rt        *Runtime
	spec      runtime.SessionSpec
	log       *rawlog.Log
	mcpConfig string
	tmpDir    string

	// life bounds work the session does on its own behalf (permission requests);
	// Close cancels it.
	life    context.Context
	endLife context.CancelFunc

	mu      sync.Mutex
	ref     runtime.SessionRef
	cur     *proc
	schema  json.RawMessage // schema the current process was started with
	respawn bool            // the next turn must start a new process
	// started is true once Claude has a transcript for the session id, i.e. after the
	// first result of any process or when resuming. Before that --resume fails ("No
	// conversation found"), so a replacement process reuses --session-id.
	started bool
	closed  bool
	// starting is true while Turn prepares a turn outside the lock (re-spawn), so a
	// concurrent Turn cannot start a second process.
	starting bool
	quota    *runtime.QuotaSignal
	turn     *turnState
	ctlSeq   int

	closeOnce sync.Once
	closeErr  error
}

func newSession(rt *Runtime, spec runtime.SessionSpec, log *rawlog.Log, ref runtime.SessionRef, started bool) *session {
	life, end := context.WithCancel(context.Background())
	return &session{rt: rt, spec: spec, log: log, ref: ref, started: started, life: life, endLife: end}
}

type turnState struct {
	q        *turnq.Queue
	ordinal  int   // result_index of the result that ends this turn
	gen      *proc // process the turn runs in
	finished chan struct{}
	stop     runtime.FailureKind // why OpenChef stopped it, if it did; guarded by session.mu
	tools    map[string]bool     // tool_use ids already reported (dedupe)
	skip     map[string]bool     // tool_use ids of plumbing (StructuredOutput)
}

func newTurnState(g *proc) *turnState {
	return &turnState{
		q: turnq.New(), ordinal: g.written, gen: g, finished: make(chan struct{}),
		tools: map[string]bool{}, skip: map[string]bool{},
	}
}

func (s *session) Ref() runtime.SessionRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ref
}

// spawn starts a process for the session. With fallback, a failed start is retried
// once with the other identity flag: whether Claude already has a transcript is only
// known for sure after a result, and a process that died mid-turn may or may not have
// written one. The caller must not hold s.mu.
func (s *session) spawn(ctx context.Context, schema json.RawMessage, fallback bool) error {
	s.mu.Lock()
	resume := s.started
	s.mu.Unlock()
	err := s.spawnOnce(ctx, resume, schema)
	if err == nil || !fallback || s.isClosed() {
		return err
	}
	s.log.Notef(rawlog.Note, "claude: start with resume=%t failed, retrying with resume=%t: %v", resume, !resume, err)
	if err2 := s.spawnOnce(ctx, !resume, schema); err2 != nil {
		return errors.Join(err, err2)
	}
	return nil
}

func (s *session) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *session) spawnOnce(ctx context.Context, resume bool, schema json.RawMessage) error {
	args, err := buildArgs(s.spec, launch{sessionID: s.Ref().ID, resume: resume, schema: schema, mcpConfig: s.mcpConfig})
	if err != nil {
		return err
	}
	stderr := &tailBuffer{}
	p, err := procs.Start(ctx, procs.Spec{
		Path:   s.rt.opts.Binary,
		Args:   append(append([]string(nil), s.rt.opts.PrefixArgs...), args...),
		Dir:    s.spec.Dir,
		Env:    s.spec.Env,
		Stderr: io.MultiWriter(s.log.LineWriter(rawlog.Stderr), stderr),
	})
	if err != nil {
		return fmt.Errorf("claude: start: %w", err)
	}
	g := newProc(p.Stdin, p)
	g.stderr = stderr
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		g.killed.Store(true)
		if err := p.Close(context.WithoutCancel(ctx)); err != nil {
			s.log.Notef(rawlog.Note, "claude: closing process started during Close: %v", err)
		}
		return runtime.ErrSessionClosed
	}
	s.cur, s.schema, s.respawn = g, schema, false
	s.mu.Unlock()
	go s.read(g) //nolint:contextcheck // the reader lives as long as the process, not the start context; permission work uses s.life

	ictx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()
	if _, err := s.control(ictx, g, "initialize", nil); err != nil {
		s.kill(ctx, g)
		if tail := g.stderr.String(); tail != "" {
			return fmt.Errorf("claude: initialize: %w (stderr: %s)", err, tail)
		}
		return fmt.Errorf("claude: initialize: %w", err)
	}
	return nil
}

// Turn implements runtime.Session.
func (s *session) Turn(ctx context.Context, in runtime.TurnInput) (<-chan runtime.Event, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, runtime.ErrSessionClosed
	}
	if s.turn != nil || s.starting {
		s.mu.Unlock()
		return nil, runtime.ErrTurnInProgress
	}
	old := s.cur
	needSpawn := s.respawn || old == nil || isDone(old.done) || !bytes.Equal(s.schema, in.OutputSchema)
	if !needSpawn && old.written > old.results {
		// A live process still owes a result (D-009); a new message would be
		// credited with it.
		s.mu.Unlock()
		return nil, runtime.ErrTurnInProgress
	}
	s.starting = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.starting = false
		s.mu.Unlock()
	}()

	if needSpawn {
		if old != nil && !isDone(old.done) {
			old.killed.Store(true)
			if err := old.p.Close(ctx); err != nil {
				s.log.Notef(rawlog.Note, "claude: closing previous process: %v", err)
			}
		}
		if err := s.spawn(ctx, in.OutputSchema, true); err != nil {
			return nil, err
		}
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, runtime.ErrSessionClosed
	}
	g := s.cur
	ts := newTurnState(g)
	g.written++
	s.turn = ts
	quota := s.quota
	s.mu.Unlock()
	if quota != nil {
		ts.q.Emit(runtime.Event{Kind: runtime.EventQuota, Quota: quota}) // latest known signal
	}

	if err := s.write(g, userMessage(in.Text)); err != nil {
		s.endTurn(ts, runtime.Event{Kind: runtime.EventTurnFailed, Failure: &runtime.Failure{Kind: runtime.FailCrashed, Message: err.Error()}})
		ts.q.Discard()
		return nil, fmt.Errorf("claude: send turn: %w", err)
	}
	go s.watch(ctx, ts)
	return ts.q.C(), nil
}

// watch interrupts the turn when the caller's context ends.
func (s *session) watch(ctx context.Context, ts *turnState) {
	select {
	case <-ts.finished:
		return
	case <-ctx.Done():
	}
	kind := runtime.FailInterrupted
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		kind = runtime.FailTimeout
	}
	s.setStop(ts, kind)
	if err := s.interrupt(context.WithoutCancel(ctx), ts, interruptWait); err != nil {
		s.log.Notef(rawlog.Note, "claude: interrupt after context end: %v", err)
	}
}

// setStop records why OpenChef stops a turn; the first reason wins.
func (s *session) setStop(ts *turnState, kind runtime.FailureKind) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ts.stop == "" {
		ts.stop = kind
	}
}

// Interrupt implements runtime.Session. It returns once the turn has ended (D-009).
func (s *session) Interrupt(ctx context.Context) error {
	s.mu.Lock()
	ts := s.turn
	s.mu.Unlock()
	if ts == nil {
		return nil
	}
	s.setStop(ts, runtime.FailInterrupted)
	return s.interrupt(ctx, ts, interruptWait)
}

// interrupt asks Claude to stop and waits up to wait in total for the owed result. If
// it does not come, or the message was still queued (D-012), the process is killed
// and the next turn starts a new one with --resume.
func (s *session) interrupt(ctx context.Context, ts *turnState, wait time.Duration) error {
	deadline := time.Now().Add(wait)
	s.cancelPermissions(ts.gen)
	cctx, cancel := context.WithDeadline(ctx, deadline)
	resp, err := s.control(cctx, ts.gen, "interrupt", nil)
	cancel()
	if err == nil {
		if n := stillQueued(resp.Response); n > 0 {
			s.kill(ctx, ts.gen)
			s.endTurn(ts, runtime.Event{Kind: runtime.EventTurnFailed, Failure: &runtime.Failure{
				Kind: s.stopKind(ts), Message: "turn interrupted before Claude started it; process replaced",
			}})
			return nil
		}
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-ts.finished:
		return nil
	case <-timer.C:
	case <-ctx.Done():
	}
	s.kill(ctx, ts.gen)
	s.endTurn(ts, runtime.Event{Kind: runtime.EventTurnFailed, Failure: &runtime.Failure{
		Kind: s.stopKind(ts), Message: "turn did not stop after interrupt; process killed",
	}})
	if err != nil && !errors.Is(err, runtime.ErrSessionClosed) {
		return fmt.Errorf("claude: interrupt: %w", err)
	}
	return nil
}

func (s *session) stopKind(ts *turnState) runtime.FailureKind {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ts.stop != "" {
		return ts.stop
	}
	return runtime.FailInterrupted
}

// kill stops a process generation immediately; the session starts a new one on the
// next turn.
func (s *session) kill(ctx context.Context, g *proc) {
	g.killed.Store(true)
	s.mu.Lock()
	if s.cur == g {
		s.respawn = true
	}
	s.mu.Unlock()
	if g.p == nil {
		return
	}
	if err := g.p.Terminate(context.WithoutCancel(ctx), 0); err != nil {
		s.log.Notef(rawlog.Note, "claude: kill: %v", err)
	}
}

// Close implements runtime.Session (D-010).
func (s *session) Close(ctx context.Context) error {
	s.closeOnce.Do(func() {
		ctx := context.WithoutCancel(ctx)
		s.mu.Lock()
		s.closed = true
		ts, g := s.turn, s.cur
		s.mu.Unlock()
		var errs []error
		switch {
		case ts != nil:
			// In flight: after EOF Claude would finish the turn and keep writing to the
			// worktree; give it closeWait to stop, then kill without further grace.
			s.setStop(ts, runtime.FailInterrupted)
			if err := s.interrupt(ctx, ts, closeWait); err != nil {
				errs = append(errs, err)
			}
			s.kill(ctx, ts.gen)
		case g != nil && g.p != nil:
			g.killed.Store(true)
			if err := g.p.Close(ctx); err != nil {
				errs = append(errs, err)
			}
		}
		if g != nil {
			select {
			case <-g.done:
			case <-time.After(closeWait):
				errs = append(errs, errors.New("claude: process output did not close"))
			}
		}
		if ts != nil {
			s.endTurn(ts, runtime.Event{Kind: runtime.EventTurnFailed, Failure: &runtime.Failure{Kind: runtime.FailInterrupted, Message: "session closed"}})
		}
		s.endLife()
		errs = append(errs, s.log.Close())
		s.closeErr = errors.Join(errs...)
		s.cleanup()
	})
	return s.closeErr
}

// abort releases what begin acquired when the session never became usable.
func (s *session) abort() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.endLife()
	_ = s.log.Close() // nothing to report it to: the caller gets the start error
	s.cleanup()
}

func (s *session) cleanup() {
	if s.tmpDir != "" {
		_ = os.RemoveAll(s.tmpDir) // best effort: a temp dir with one config file
	}
}

// endTurn clears the turn before queuing its terminal event: a consumer that starts the
// next turn as soon as the channel closes must not get ErrTurnInProgress.
func (s *session) endTurn(ts *turnState, ev runtime.Event) {
	s.mu.Lock()
	if s.turn == ts {
		s.turn = nil
	}
	s.mu.Unlock()
	if !ts.q.Finish(ev) {
		return
	}
	close(ts.finished)
}

func (s *session) write(g *proc, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	g.wmu.Lock()
	defer g.wmu.Unlock()
	s.log.Line(rawlog.Send, raw)
	if _, err := g.w.Write(append(raw, '\n')); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// control sends a control_request and waits for its control_response.
func (s *session) control(ctx context.Context, g *proc, subtype string, extra map[string]any) (controlResponseBody, error) {
	s.mu.Lock()
	s.ctlSeq++
	id := subtype + "-" + strconv.Itoa(s.ctlSeq)
	ch := make(chan controlResponseBody, 1)
	g.controls[id] = ch
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(g.controls, id)
		s.mu.Unlock()
	}()
	req := map[string]any{"subtype": subtype}
	for k, v := range extra {
		req[k] = v
	}
	if err := s.write(g, map[string]any{"type": "control_request", "request_id": id, "request": req}); err != nil {
		return controlResponseBody{}, err
	}
	select {
	case resp := <-ch:
		if resp.Subtype == "error" {
			return resp, controlError(subtype, resp.Error)
		}
		return resp, nil
	case <-g.done:
		return controlResponseBody{}, fmt.Errorf("%s: %w", subtype, runtime.ErrSessionClosed)
	case <-ctx.Done():
		return controlResponseBody{}, fmt.Errorf("%s: %w", subtype, ctx.Err())
	}
}

// read consumes one process generation's stdout.
func (s *session) read(g *proc) {
	defer s.processGone(g)
	if g.p == nil {
		return
	}
	br := bufio.NewReaderSize(g.p.Stdout, 64<<10)
	for {
		line, err := readLine(br)
		if len(line) > 0 {
			s.log.Line(rawlog.Recv, line)
			s.dispatch(g, line)
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.log.Notef(rawlog.Note, "claude: read: %v", err)
			}
			return
		}
	}
}

func readLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > maxLine {
			return nil, fmt.Errorf("claude: message exceeds %d bytes", maxLine)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		buf = bytes.TrimRight(buf, "\r\n")
		if err != nil {
			return buf, fmt.Errorf("read: %w", err)
		}
		return buf, nil
	}
}

// processGone runs when a generation's output ends.
func (s *session) processGone(g *proc) {
	if g.p != nil {
		if err := g.p.Stdout.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			s.log.Notef(rawlog.Note, "claude: close stdout: %v", err)
		}
	}
	close(g.done)
	s.cancelPermissions(g)
	s.mu.Lock()
	ts := s.turn
	if ts != nil && ts.gen != g {
		ts = nil
	}
	if s.cur == g && !s.closed {
		s.respawn = true // an unexpected exit is recovered on the next turn
	}
	s.mu.Unlock()
	if ts == nil {
		return
	}
	kind := runtime.FailCrashed
	msg := "claude exited during the turn"
	if g.killed.Load() {
		kind = s.stopKind(ts)
		msg = "claude process stopped by OpenChef"
	} else {
		if g.p != nil {
			// stdout can close before stderr is fully copied; once the process is
			// reaped (cmd.Wait), the copy is complete.
			select {
			case <-g.p.Done():
			case <-time.After(exitWait):
			}
		}
		if tail := g.stderr.String(); tail != "" {
			msg += ": " + tail
		}
	}
	s.endTurn(ts, runtime.Event{Kind: runtime.EventTurnFailed, Failure: &runtime.Failure{Kind: kind, Message: msg}})
}

func isDone(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// active returns the running turn if it belongs to generation g.
func (s *session) active(g *proc) *turnState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turn != nil && s.turn.gen == g {
		return s.turn
	}
	return nil
}

func (s *session) dispatch(g *proc, line []byte) {
	var env envelope
	if err := json.Unmarshal(line, &env); err != nil {
		s.log.Notef(rawlog.Note, "claude: protocol error: %v", err)
		return
	}
	switch env.Type {
	case "control_response":
		var body controlResponseBody
		if json.Unmarshal(env.Response, &body) != nil {
			return
		}
		s.mu.Lock()
		ch := g.controls[body.RequestID]
		s.mu.Unlock()
		if ch != nil {
			select {
			case ch <- body:
			default:
			}
		}
	case "control_request":
		var req canUseToolRequest
		if err := json.Unmarshal(env.Request, &req); err != nil {
			s.log.Notef(rawlog.Note, "claude: undecodable control request: %v", err)
		}
		if req.Subtype == "can_use_tool" {
			// Register the cancel func here, in stream order: a control_cancel_request
			// read right after must find it even before the goroutine runs.
			ctx, cancel := context.WithCancel(s.life)
			s.mu.Lock()
			g.perms[env.RequestID] = cancel
			s.mu.Unlock()
			go s.permission(ctx, cancel, g, env.RequestID, req, env.Request)
			return
		}
		if err := s.write(g, errorResponse(env.RequestID, "OpenChef does not support "+req.Subtype)); err != nil {
			s.log.Notef(rawlog.Note, "claude: reply to %s: %v", req.Subtype, err)
		}
	case "control_cancel_request":
		s.mu.Lock()
		cancel := g.perms[env.RequestID]
		s.mu.Unlock()
		if cancel != nil {
			cancel()
		}
	case "stream_event":
		ts := s.active(g)
		if ts == nil || env.fromSubagent() {
			return
		}
		var ev streamEvent
		if json.Unmarshal(env.Event, &ev) == nil && ev.Type == "content_block_delta" && ev.Delta.Type == "text_delta" && ev.Delta.Text != "" {
			ts.q.Emit(runtime.Event{Kind: runtime.EventText, Text: ev.Delta.Text})
		}
	case "assistant":
		ts := s.active(g)
		if ts == nil || env.fromSubagent() {
			return
		}
		s.onAssistant(ts, env.Message)
	case "user":
		ts := s.active(g)
		if ts == nil || env.fromSubagent() {
			return
		}
		s.onUser(ts, env.Message)
	case "rate_limit_event":
		sig := quotaSignal(env.RateLimitInfo)
		if sig == nil {
			return
		}
		s.mu.Lock()
		s.quota = sig
		ts := s.turn
		s.mu.Unlock()
		if ts != nil && ts.gen == g {
			ts.q.Emit(runtime.Event{Kind: runtime.EventQuota, Quota: sig})
		}
	case "result":
		s.onResult(g, line)
	}
}

func (s *session) onAssistant(ts *turnState, raw json.RawMessage) {
	var msg chatMessage
	if json.Unmarshal(raw, &msg) != nil {
		return
	}
	for i := range msg.Content {
		b := &msg.Content[i]
		if b.Type != "tool_use" || b.ID == "" {
			continue
		}
		s.mu.Lock()
		dup := ts.tools[b.ID] || ts.skip[b.ID]
		if b.Name == structuredOutputTool {
			ts.skip[b.ID] = true
		} else {
			ts.tools[b.ID] = true
		}
		s.mu.Unlock()
		if dup || b.Name == structuredOutputTool {
			continue
		}
		ts.q.Emit(runtime.Event{Kind: runtime.EventToolCall, ToolCall: &runtime.ToolCall{
			ID: b.ID, Name: b.Name, Input: b.Input, Signature: signature(b.Name, b.Input),
		}})
	}
}

func (s *session) onUser(ts *turnState, raw json.RawMessage) {
	var msg chatMessage
	if json.Unmarshal(raw, &msg) != nil {
		return
	}
	for i := range msg.Content {
		b := &msg.Content[i]
		if b.Type != "tool_result" {
			continue
		}
		s.mu.Lock()
		skip := ts.skip[b.ToolUseID]
		s.mu.Unlock()
		if skip {
			continue
		}
		ts.q.Emit(runtime.Event{Kind: runtime.EventToolResult, ToolResult: &runtime.ToolResult{
			ID: b.ToolUseID, IsError: b.IsError, Output: truncate(toolResultText(b.Content)),
		}})
	}
}

// onResult ends the turn whose ordinal matches the result (D-009); any other result is
// stale (e.g. the late result of an interrupted turn) and is ignored.
func (s *session) onResult(g *proc, line []byte) {
	var r resultMessage
	if err := json.Unmarshal(line, &r); err != nil {
		s.log.Notef(rawlog.Note, "claude: undecodable result: %v", err)
		return
	}
	s.mu.Lock()
	idx := g.results
	if r.ResultIndex != nil {
		idx = *r.ResultIndex
	}
	g.results++
	s.started = true // a result means Claude has a transcript for this session id
	ts := s.turn
	quota := s.quota
	if ts == nil || ts.gen != g || ts.ordinal != idx {
		s.mu.Unlock()
		s.log.Notef(rawlog.Note, "claude: ignoring result %d (no matching turn)", idx)
		return
	}
	stop := ts.stop
	s.mu.Unlock()
	s.endTurn(ts, r.outcome(stop, quota))
}

// permission answers a can_use_tool request. Each request runs in its own goroutine
// with a context cancelled on interrupt, Close or control_cancel_request; requests
// outside an OpenChef turn are denied.
func (s *session) permission(ctx context.Context, cancel context.CancelFunc, g *proc, requestID string, req canUseToolRequest, raw json.RawMessage) {
	defer func() {
		cancel()
		s.mu.Lock()
		delete(g.perms, requestID)
		s.mu.Unlock()
	}()

	var d runtime.Decision
	switch {
	case s.active(g) == nil:
		d = runtime.Deny("no OpenChef turn is active")
	case s.spec.AskUser != nil && req.ToolName == askUserTool(s.spec.AskUser):
		d = runtime.Allow()
	default:
		ar := req.approvalRequest(raw)
		ar.Session = s.Ref()
		var err error
		d, err = s.spec.Approver.Approve(ctx, ar)
		if err != nil {
			d = runtime.Deny("OpenChef could not decide this request: " + err.Error())
		}
	}
	// ForSession is intentionally not forwarded: OpenChef never asks Claude to persist
	// permission changes; the approver keeps its own session memory.
	if err := s.write(g, permissionResponse(requestID, req.Input, d)); err != nil {
		s.log.Notef(rawlog.Note, "claude: permission reply: %v", err)
	}
}

func (s *session) cancelPermissions(g *proc) {
	s.mu.Lock()
	cancels := make([]context.CancelFunc, 0, len(g.perms))
	for _, c := range g.perms {
		cancels = append(cancels, c)
	}
	s.mu.Unlock()
	for _, c := range cancels {
		c()
	}
}

func truncate(s string) string {
	if len(s) <= maxToolOutput {
		return s
	}
	half := maxToolOutput / 2
	return s[:half] + "\n…\n" + s[len(s)-half:]
}

// tailBuffer keeps the last stderrTail bytes written to it, for error messages.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > stderrTail {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-stderrTail:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}
