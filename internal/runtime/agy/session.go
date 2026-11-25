package agy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ugurboz/openchef/internal/procs"
	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/runtime/linerpc"
	"github.com/ugurboz/openchef/internal/runtime/rawlog"
	"github.com/ugurboz/openchef/internal/runtime/turnq"
	"github.com/ugurboz/openchef/internal/schema"
)

// Timeouts are variables so tests can shorten them.
var (
	callTimeout    = 90 * time.Second // initialize, session/new|load, config (session/new starts a language server)
	barrierTimeout = 10 * time.Second
	interruptWait  = 10 * time.Second // D-016: how long Interrupt waits for "cancelled"
	closeWait      = 3 * time.Second  // in-flight turn on Close, as for Claude (D-010)
	exitWait       = 2 * time.Second  // how long a crash report waits for the exit status
)

const (
	maxToolOutput = 4 << 10
	stderrTail    = 2 << 10
)

// proc is one `agy acp` process. A session replaces it after a crash or a kill and
// continues with session/load.
type proc struct {
	p      *procs.Process
	peer   *linerpc.Peer
	stderr *tailBuffer
	routed chan struct{} // closed when every notification of this process was handled
	killed atomic.Bool
}

type session struct {
	rt   *Runtime
	spec runtime.SessionSpec
	log  *rawlog.Log

	life    context.Context // bounds calls made on the session's own behalf
	endLife context.CancelFunc

	mu       sync.Mutex
	ref      runtime.SessionRef
	cur      *proc
	respawn  bool
	loading  bool // session/load is replaying history: drop updates (D-016)
	closed   bool
	starting bool
	turn     *turnState
	model    string // model reported by agy after configuration

	closeOnce sync.Once
	closeErr  error
}

func newSession(rt *Runtime, spec runtime.SessionSpec, log *rawlog.Log, id string) *session {
	life, end := context.WithCancel(context.Background())
	s := &session{rt: rt, spec: spec, log: log, life: life, endLife: end}
	if id != "" {
		s.ref = runtime.SessionRef{Runtime: Name, ID: id}
	}
	return s
}

type turnState struct {
	q        *turnq.Queue
	gen      *proc
	finished chan struct{}
	started  time.Time

	// Guarded by session.mu.
	stop    runtime.FailureKind
	text    strings.Builder // agent text since the last tool call: the final answer
	tools   map[string]bool
	results map[string]bool
	denials int

	perms       context.Context // cancelled on interrupt: pending approvals answer "cancelled"
	cancelPerms context.CancelFunc
}

func (s *session) newTurnState(g *proc) *turnState {
	perms, cancel := context.WithCancel(s.life)
	return &turnState{
		q: turnq.New(), gen: g, finished: make(chan struct{}), started: time.Now(),
		tools: map[string]bool{}, results: map[string]bool{}, perms: perms, cancelPerms: cancel,
	}
}

// Ref implements runtime.Session.
func (s *session) Ref() runtime.SessionRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ref
}

// start launches a process, then creates the session (or loads it) and configures it.
func (s *session) start(ctx context.Context) error {
	args := append(append([]string(nil), s.rt.opts.Args...),
		"--sandbox", "--disable-telemetry", "--include-default-customizations=false",
		"--cli_dir_name", s.rt.opts.CLIDirName)
	stderr := &tailBuffer{}
	p, err := procs.Start(ctx, procs.Spec{
		Path: s.rt.opts.Binary, Args: args, Dir: s.spec.Dir, Env: s.spec.Env,
		Stderr: io.MultiWriter(s.log.LineWriter(rawlog.Stderr), stderr),
	})
	if err != nil {
		return fmt.Errorf("agy: start: %w", err)
	}
	g := &proc{p: p, stderr: stderr, routed: make(chan struct{})}
	g.peer = linerpc.New(p.Stdout, p.Stdin, linerpc.Options{
		Log: s.log,
		Handler: func(hctx context.Context, method string, params json.RawMessage) (any, error) {
			return s.handle(hctx, g, method, params)
		},
		OnProtocolError: func(_ []byte, err error) {
			s.log.Notef(rawlog.Note, "agy: protocol error: %v", err)
		},
	})
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		g.killed.Store(true)
		if err := p.Close(context.WithoutCancel(ctx)); err != nil {
			s.log.Notef(rawlog.Note, "agy: closing process started during Close: %v", err)
		}
		return runtime.ErrSessionClosed
	}
	s.cur, s.respawn = g, false
	id := s.ref.ID
	s.mu.Unlock()
	go s.route(g)

	if err := s.handshake(ctx, g, id); err != nil {
		s.kill(ctx, g)
		if tail := g.stderr.String(); tail != "" {
			return fmt.Errorf("%w (stderr: %s)", err, tail)
		}
		return err
	}
	return nil
}

func (s *session) handshake(ctx context.Context, g *proc, id string) error {
	cctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	if err := g.peer.Call(cctx, "initialize", map[string]any{
		"protocolVersion": 1,
		// OpenChef offers no client file system or terminal: agy uses its own tools,
		// which run in its sandbox and ask OpenChef before leaving it.
		"clientCapabilities": map[string]any{"fs": map[string]any{"readTextFile": false, "writeTextFile": false}, "terminal": false},
		"clientInfo":         map[string]any{"name": "openchef", "title": "OpenChef", "version": s.rt.opts.ClientVersion},
	}, nil); err != nil {
		return fmt.Errorf("agy: initialize: %w", err)
	}
	var opts []configOption
	if id == "" {
		var res sessionResult
		if err := g.peer.Call(cctx, "session/new", map[string]any{"cwd": s.spec.Dir, "mcpServers": mcpServers(s.spec.AskUser)}, &res); err != nil {
			return fmt.Errorf("agy: session/new: %w", err)
		}
		if res.SessionID == "" {
			return &runtime.ProtocolError{Runtime: Name, Reason: "session/new returned no session id"}
		}
		s.mu.Lock()
		s.ref = runtime.SessionRef{Runtime: Name, ID: res.SessionID}
		s.mu.Unlock()
		id, opts = res.SessionID, res.ConfigOptions
	} else {
		var err error
		if opts, err = s.load(cctx, g, id); err != nil {
			return err
		}
	}
	return s.configure(cctx, g, id, opts)
}

// load resumes a session; the history agy replays meanwhile is dropped (D-016).
func (s *session) load(ctx context.Context, g *proc, id string) ([]configOption, error) {
	s.mu.Lock()
	s.loading = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.loading = false
		s.mu.Unlock()
	}()
	var res sessionResult
	if err := g.peer.Call(ctx, "session/load", map[string]any{"sessionId": id, "cwd": s.spec.Dir, "mcpServers": mcpServers(s.spec.AskUser)}, &res); err != nil {
		return nil, fmt.Errorf("agy: session/load: %w", err)
	}
	if err := g.peer.Barrier(ctx); err != nil {
		return nil, fmt.Errorf("agy: session/load: %w", err)
	}
	return res.ConfigOptions, nil
}

// configure sets model, thinking level and ACP mode "default" (D-015). The thinking
// levels on offer depend on the model, so they are read after the model is set.
func (s *session) configure(ctx context.Context, g *proc, id string, opts []configOption) error {
	set := func(key, value string) ([]configOption, error) {
		var res configResult
		if err := g.peer.Call(ctx, "session/set_config_option", map[string]any{"sessionId": id, "configId": key, "value": value}, &res); err != nil {
			return nil, fmt.Errorf("agy: set %s=%s: %w", key, value, err)
		}
		return res.ConfigOptions, nil
	}
	if s.spec.Model != "" {
		got, err := set("model", s.spec.Model)
		if err != nil {
			return err
		}
		if len(got) > 0 {
			opts = got
		}
	}
	if level := thoughtLevel(s.spec.Effort, opts); level != "" {
		got, err := set("thought_level", level)
		if err != nil {
			return err
		}
		if len(got) > 0 {
			opts = got
		}
	}
	got, err := set("mode", "default")
	if err != nil {
		return err
	}
	if len(got) > 0 {
		opts = got
	}
	if m, ok := findOption(opts, "model"); ok {
		s.mu.Lock()
		s.model = m.CurrentValue
		s.mu.Unlock()
	}
	return nil
}

// Turn implements runtime.Session.
func (s *session) Turn(ctx context.Context, in runtime.TurnInput) (<-chan runtime.Event, error) {
	var sch *schema.Schema
	if len(in.OutputSchema) > 0 {
		c, err := schema.Compile(in.OutputSchema)
		if err != nil {
			return nil, fmt.Errorf("agy: output schema: %w", err)
		}
		sch = c
	}
	s.mu.Lock()
	switch {
	case s.closed:
		s.mu.Unlock()
		return nil, runtime.ErrSessionClosed
	case s.turn != nil || s.starting:
		s.mu.Unlock()
		return nil, runtime.ErrTurnInProgress
	}
	needStart := s.respawn || s.cur == nil || isDone(s.cur.peer.Done())
	old := s.cur
	s.starting = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.starting = false
		s.mu.Unlock()
	}()

	if needStart {
		if old != nil {
			old.killed.Store(true)
			if err := old.p.Close(ctx); err != nil {
				s.log.Notef(rawlog.Note, "agy: closing previous process: %v", err)
			}
		}
		if err := s.start(ctx); err != nil {
			return nil, err
		}
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, runtime.ErrSessionClosed
	}
	ts := s.newTurnState(s.cur)
	s.turn = ts
	s.mu.Unlock()

	text := in.Text
	if sch != nil {
		text += schema.Instruction(in.OutputSchema)
	}
	go s.run(ts, text, sch) // outlives Turn's context: watch turns its end into an interrupt
	go s.watch(ctx, ts)
	return ts.q.C(), nil
}

// run drives one turn and ends it.
func (s *session) run(ts *turnState, text string, sch *schema.Schema) {
	s.endTurn(ts, s.drive(ts, text, sch, s.prompt))
}

// drive sends the prompt and, for a schema, one repair prompt if the answer does not
// validate (agy has no native structured output, m1-findings §4). It returns the
// terminal event. send is s.prompt; replay tests substitute recorded results.
func (s *session) drive(ts *turnState, text string, sch *schema.Schema, send func(*turnState, string) (promptResult, error)) runtime.Event {
	res, err := send(ts, text)
	usage := res.usage()
	if ev, failed := s.failure(ts, res, err, usage); failed {
		return ev
	}
	final := s.finalText(ts)
	var structured json.RawMessage
	if sch != nil {
		doc, verr := validate(sch, final)
		if verr != nil {
			s.resetText(ts)
			res, err = send(ts, schema.Repair(verr))
			usage = addUsage(usage, res.usage())
			if ev, failed := s.failure(ts, res, err, usage); failed {
				return ev
			}
			final = s.finalText(ts)
			if doc, verr = validate(sch, final); verr != nil {
				return runtime.Event{Kind: runtime.EventTurnFailed, Failure: &runtime.Failure{
					Kind: runtime.FailProtocol, Message: "answer does not satisfy the output schema after one repair: " + verr.Error(), Usage: usage,
				}}
			}
		}
		structured = doc
	}
	s.mu.Lock()
	denials, model := ts.denials, s.model
	s.mu.Unlock()
	r := &runtime.TurnResult{
		FinalText: final, Structured: structured, Usage: usage, Denials: denials,
		Duration: time.Since(ts.started),
	}
	if model != "" {
		r.Models = []string{model}
	}
	return runtime.Event{Kind: runtime.EventTurnCompleted, Result: r}
}

func validate(sch *schema.Schema, text string) (json.RawMessage, error) {
	doc, err := schema.Extract(text)
	if err != nil {
		return nil, err
	}
	if err := sch.Validate(doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// prompt sends one session/prompt and returns once every update that preceded the
// response has been routed (linerpc.Barrier).
func (s *session) prompt(ts *turnState, text string) (promptResult, error) {
	var res promptResult
	err := ts.gen.peer.Call(s.life, "session/prompt", map[string]any{
		"sessionId": s.Ref().ID,
		"prompt":    []any{map[string]any{"type": "text", "text": text}},
	}, &res)
	bctx, cancel := context.WithTimeout(s.life, barrierTimeout)
	defer cancel()
	if berr := ts.gen.peer.Barrier(bctx); berr != nil && err == nil {
		s.log.Notef(rawlog.Note, "agy: %v", berr)
	}
	if err != nil {
		return res, fmt.Errorf("agy: session/prompt: %w", err)
	}
	return res, nil
}

// failure turns an error or a non-success stop reason into the terminal event.
func (s *session) failure(ts *turnState, res promptResult, err error, usage runtime.Usage) (runtime.Event, bool) {
	stop := s.stopKind(ts)
	if err != nil {
		kind := classifyError(err)
		msg := err.Error()
		if ts.gen.killed.Load() && stop != "" {
			kind = stop
		} else if kind == runtime.FailCrashed {
			// stdout can end before stderr is fully copied; once the process is reaped
			// (cmd.Wait), the copy is complete.
			select {
			case <-ts.gen.p.Done():
			case <-time.After(exitWait):
			}
			if tail := ts.gen.stderr.String(); tail != "" {
				msg += ": " + tail
			}
		}
		return runtime.Event{Kind: runtime.EventTurnFailed, Failure: &runtime.Failure{Kind: kind, Message: msg, Usage: usage}}, true
	}
	if kind, failed := stopFailure(res.StopReason, stop); failed {
		return runtime.Event{Kind: runtime.EventTurnFailed, Failure: &runtime.Failure{
			Kind: kind, Message: "agy stopped: " + res.StopReason, Usage: usage,
		}}, true
	}
	return runtime.Event{}, false
}

func (s *session) finalText(ts *turnState) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.TrimSpace(ts.text.String())
}

func (s *session) resetText(ts *turnState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts.text.Reset()
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
		s.log.Notef(rawlog.Note, "agy: interrupt after context end: %v", err)
	}
}

func (s *session) setStop(ts *turnState, kind runtime.FailureKind) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ts.stop == "" {
		ts.stop = kind
	}
}

func (s *session) stopKind(ts *turnState) runtime.FailureKind {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ts.stop
}

// Interrupt implements runtime.Session. It returns once the turn has ended.
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

// interrupt sends session/cancel, answers pending approvals "cancelled" and waits for
// the prompt to return; after wait the process is killed and the next turn reloads.
func (s *session) interrupt(ctx context.Context, ts *turnState, wait time.Duration) error {
	ts.cancelPerms()
	err := ts.gen.peer.Notify("session/cancel", map[string]any{"sessionId": s.Ref().ID})
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-ts.finished:
		return nil
	case <-timer.C:
	case <-ctx.Done():
	}
	s.kill(ctx, ts.gen) // the pending prompt fails with ErrClosed and run ends the turn
	select {
	case <-ts.finished:
	case <-time.After(closeWait):
		s.endTurn(ts, runtime.Event{Kind: runtime.EventTurnFailed, Failure: &runtime.Failure{
			Kind: s.stopKindOr(ts), Message: "turn did not stop after cancel; process killed",
		}})
	}
	if err != nil && !errors.Is(err, linerpc.ErrClosed) {
		return fmt.Errorf("agy: cancel: %w", err)
	}
	return nil
}

func (s *session) stopKindOr(ts *turnState) runtime.FailureKind {
	if k := s.stopKind(ts); k != "" {
		return k
	}
	return runtime.FailInterrupted
}

func (s *session) kill(ctx context.Context, g *proc) {
	g.killed.Store(true)
	s.mu.Lock()
	if s.cur == g {
		s.respawn = true
	}
	s.mu.Unlock()
	if err := g.p.Terminate(context.WithoutCancel(ctx), 0); err != nil {
		s.log.Notef(rawlog.Note, "agy: kill: %v", err)
	}
}

// Close implements runtime.Session.
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
			s.setStop(ts, runtime.FailInterrupted)
			if err := s.interrupt(ctx, ts, closeWait); err != nil {
				errs = append(errs, err)
			}
			s.kill(ctx, ts.gen)
		case g != nil:
			g.killed.Store(true)
			if err := g.p.Close(ctx); err != nil { // agy exits on EOF within ~50 ms (m1-findings §2)
				errs = append(errs, err)
			}
		}
		if g != nil {
			select {
			case <-g.routed:
			case <-time.After(closeWait):
				errs = append(errs, errors.New("agy: notifications did not drain"))
			}
		}
		if ts != nil {
			s.endTurn(ts, runtime.Event{Kind: runtime.EventTurnFailed, Failure: &runtime.Failure{Kind: runtime.FailInterrupted, Message: "session closed"}})
		}
		s.endLife()
		errs = append(errs, s.log.Close())
		s.closeErr = errors.Join(errs...)
	})
	return s.closeErr
}

// abort releases a session that never became usable.
func (s *session) abort(ctx context.Context) {
	s.mu.Lock()
	s.closed = true
	g := s.cur
	s.mu.Unlock()
	if g != nil {
		g.killed.Store(true)
		_ = g.p.Close(context.WithoutCancel(ctx)) // already failing; the start error is returned
	}
	s.endLife()
	_ = s.log.Close() // nothing to report it to: the caller gets the start error
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
	ts.cancelPerms()
	close(ts.finished)
}

func isDone(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// route handles one process's notifications in order, then marks it gone.
func (s *session) route(g *proc) {
	defer func() {
		close(g.routed)
		s.mu.Lock()
		if s.cur == g && !s.closed {
			s.respawn = true // an unexpected exit is recovered on the next turn
		}
		s.mu.Unlock()
	}()
	for n := range g.peer.Notifications() {
		if n.Release() {
			continue
		}
		if n.Method == "session/update" {
			s.onUpdate(g, n.Params)
		}
	}
}

// active returns the running turn if it belongs to process g.
func (s *session) active(g *proc) *turnState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turn != nil && s.turn.gen == g {
		return s.turn
	}
	return nil
}

func (s *session) onUpdate(g *proc, raw json.RawMessage) {
	var p updateParams
	if err := json.Unmarshal(raw, &p); err != nil {
		s.log.Notef(rawlog.Note, "agy: undecodable update: %v", err)
		return
	}
	s.mu.Lock()
	loading, id := s.loading, s.ref.ID
	s.mu.Unlock()
	ts := s.active(g)
	if loading || ts == nil || (p.SessionID != "" && p.SessionID != id) {
		return // replayed history, an update outside any turn, or another session
	}
	u := &p.Update
	switch u.Kind {
	case "agent_message_chunk":
		if t := u.chunkText(); t != "" {
			s.mu.Lock()
			ts.text.WriteString(t)
			s.mu.Unlock()
			ts.q.Emit(runtime.Event{Kind: runtime.EventText, Text: t})
		}
	case "tool_call":
		s.onToolCall(ts, u)
	case "tool_call_update":
		s.onToolCall(ts, u)
		if u.Status == "completed" || u.Status == "failed" {
			s.mu.Lock()
			dup := ts.results[u.ToolCallID]
			ts.results[u.ToolCallID] = true
			s.mu.Unlock()
			if !dup {
				r := u.result()
				r.Output = truncate(r.Output)
				ts.q.Emit(runtime.Event{Kind: runtime.EventToolResult, ToolResult: r})
			}
		}
	}
}

// onToolCall reports a tool call once; agent text before it is not the final answer.
func (s *session) onToolCall(ts *turnState, u *update) {
	if u.ToolCallID == "" {
		return
	}
	s.mu.Lock()
	dup := ts.tools[u.ToolCallID]
	ts.tools[u.ToolCallID] = true
	if !dup {
		ts.text.Reset()
	}
	s.mu.Unlock()
	if dup {
		return
	}
	name := toolName(u.Name, u.ToolKind, askUserName(s.spec.AskUser))
	ts.q.Emit(runtime.Event{Kind: runtime.EventToolCall, ToolCall: &runtime.ToolCall{
		ID: u.ToolCallID, Name: name, Input: u.RawInput, Signature: signature(name, u.RawInput),
	}})
}

func askUserName(m *runtime.MCPServer) string {
	if m == nil {
		return ""
	}
	return m.Name
}

// handle answers requests agy sends to OpenChef.
func (s *session) handle(ctx context.Context, g *proc, method string, params json.RawMessage) (any, error) {
	if method != "session/request_permission" {
		return nil, &linerpc.Error{Code: linerpc.CodeMethodNotFound, Message: "OpenChef does not provide " + method}
	}
	var p permissionParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &linerpc.Error{Code: -32602, Message: "invalid params: " + err.Error()}
	}
	d, cancel := s.decide(ctx, g, &p, params)
	if cancel {
		return cancelled(), nil
	}
	if d.Allow {
		if id, ok := p.optionID("allow_once"); ok {
			return selected(id), nil
		}
		s.log.Notef(rawlog.Note, "agy: no allow_once option; cancelling the request")
		return cancelled(), nil
	}
	if ts := s.active(g); ts != nil {
		s.mu.Lock()
		ts.denials++
		s.mu.Unlock()
	}
	if id, ok := p.optionID("reject_once"); ok {
		return selected(id), nil
	}
	return cancelled(), nil
}

// decide returns the decision for a permission request, or cancel=true when the turn
// was interrupted while it waited.
func (s *session) decide(ctx context.Context, g *proc, p *permissionParams, raw json.RawMessage) (d runtime.Decision, cancel bool) {
	ts := s.active(g)
	if ts == nil {
		return runtime.Deny("no OpenChef turn is active"), false
	}
	req := p.approvalRequest(raw, askUserName(s.spec.AskUser))
	if s.spec.AskUser != nil && req.Tool == "mcp__"+s.spec.AskUser.Name+"__ask_user" {
		return runtime.Allow(), false // OpenChef's own question tool
	}
	if s.spec.Mode == runtime.ModePlan {
		// D-015: plan is read-only; only sandboxed reads run, every request is refused.
		return runtime.Deny("plan mode is read-only: OpenChef refuses edits and commands outside the sandbox"), false
	}
	req.Session = s.Ref()
	actx, stop := context.WithCancel(ctx) // ctx ends when the peer closes
	defer stop()
	interrupted := ts.perms.Done()
	go func() {
		select {
		case <-interrupted: // the turn was interrupted
			stop()
		case <-actx.Done():
		}
	}()
	d, err := s.spec.Approver.Approve(actx, req)
	if actx.Err() != nil && ts.perms.Err() != nil {
		return runtime.Decision{}, true
	}
	if err != nil {
		return runtime.Deny("OpenChef could not decide this request: " + err.Error()), false
	}
	return d, false
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
