package claude

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/procs"
	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/runtime/runtimetest"
)

type recordingApprover struct {
	mu        sync.Mutex
	decision  runtime.Decision
	err       error
	block     bool // wait for ctx instead of deciding
	cancelled chan struct{}
	approvals []runtime.ApprovalRequest
}

func (a *recordingApprover) Approve(ctx context.Context, req runtime.ApprovalRequest) (runtime.Decision, error) {
	a.mu.Lock()
	a.approvals = append(a.approvals, req)
	block := a.block
	a.mu.Unlock()
	if block {
		<-ctx.Done()
		close(a.cancelled)
		return runtime.Decision{}, ctx.Err()
	}
	return a.decision, a.err
}

func (*recordingApprover) Answer(context.Context, runtime.Question) (string, error) {
	return "teal", nil
}

func (a *recordingApprover) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.approvals)
}

type harness struct {
	t       *testing.T
	rt      *Runtime
	spec    runtime.SessionSpec
	logPath string
	appr    *recordingApprover
}

func newHarness(t *testing.T, scenario string) *harness {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	state := filepath.Join(dir, "transcripts")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "fake.jsonl")
	appr := &recordingApprover{decision: runtime.Allow(), cancelled: make(chan struct{})}
	return &harness{
		t:       t,
		rt:      New(Options{Binary: exe}),
		logPath: logPath,
		appr:    appr,
		spec: runtime.SessionSpec{
			Dir:      dir,
			Model:    "haiku",
			Effort:   runtime.EffortLow,
			Mode:     runtime.ModeManual,
			Env:      []string{fakeEnv + "=" + scenario, fakeLogEnv + "=" + logPath, fakeStateEnv + "=" + state},
			Approver: appr,
			LogPath:  filepath.Join(dir, "raw.jsonl"),
		},
	}
}

func (h *harness) open() runtime.Session {
	h.t.Helper()
	s, err := h.rt.Open(context.Background(), h.spec)
	if err != nil {
		h.t.Fatalf("Open: %v", err)
	}
	h.t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

type fakeRecord struct {
	PID  int             `json:"pid"`
	Argv []string        `json:"argv"`
	In   json.RawMessage `json:"in"`
}

func (h *harness) records() []fakeRecord {
	h.t.Helper()
	raw, err := os.ReadFile(h.logPath)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []fakeRecord
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var r fakeRecord
		if json.Unmarshal([]byte(line), &r) == nil {
			out = append(out, r)
		}
	}
	return out
}

// launches returns the argv of every process the adapter started.
func (h *harness) launches() [][]string {
	var out [][]string
	for _, r := range h.records() {
		if r.Argv != nil {
			out = append(out, r.Argv)
		}
	}
	return out
}

// controlResponses returns every control_response the fake received on stdin.
func (h *harness) controlResponses() []json.RawMessage {
	var out []json.RawMessage
	for _, r := range h.records() {
		var m struct {
			Type string `json:"type"`
		}
		if r.In != nil && json.Unmarshal(r.In, &m) == nil && m.Type == "control_response" {
			out = append(out, r.In)
		}
	}
	return out
}

func drain(t *testing.T, ch <-chan runtime.Event) []runtime.Event {
	t.Helper()
	var evs []runtime.Event
	timeout := time.After(drainTimeout)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return evs
			}
			evs = append(evs, ev)
		case <-timeout:
			t.Fatalf("turn did not finish; events: %+v", evs)
		}
	}
}

// drainTimeout bounds how long tests wait for a turn; the live test raises it.
var drainTimeout = 15 * time.Second

func turn(t *testing.T, s runtime.Session, in runtime.TurnInput) (end runtime.Event, evs []runtime.Event) {
	t.Helper()
	ch, err := s.Turn(context.Background(), in)
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	evs = drain(t, ch)
	return terminal(t, evs), evs
}

// started waits until the turn streams text, i.e. Claude picked up the message, and
// returns the events seen so far.
func started(t *testing.T, ch <-chan runtime.Event) []runtime.Event {
	t.Helper()
	var evs []runtime.Event
	timeout := time.After(drainTimeout)
	for {
		select {
		case ev := <-ch:
			evs = append(evs, ev)
			if ev.Kind == runtime.EventText {
				return evs
			}
		case <-timeout:
			t.Fatalf("turn never started; events: %+v", evs)
		}
	}
}

func mustComplete(t *testing.T, end runtime.Event, text string) {
	t.Helper()
	if end.Kind != runtime.EventTurnCompleted || end.Result.FinalText != text {
		t.Fatalf("want completed %q, got %s %+v %+v", text, end.Kind, end.Result, end.Failure)
	}
}

func mustFail(t *testing.T, end runtime.Event, kind runtime.FailureKind) {
	t.Helper()
	if end.Kind != runtime.EventTurnFailed || end.Failure.Kind != kind {
		t.Fatalf("want %s, got %s %+v %+v", kind, end.Kind, end.Result, end.Failure)
	}
}

func TestOpenTurnEventsAndArgs(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	s := h.open()
	ref := s.Ref()
	if ref.Runtime != Name || len(ref.ID) != 36 {
		t.Fatalf("Ref = %+v", ref)
	}
	end, evs := turn(t, s, runtime.TurnInput{Text: "ok"})
	mustComplete(t, end, "Hello")
	r := end.Result
	if want := (runtime.Usage{InputTokens: 100, OutputTokens: 10, CacheReadTokens: 40, CacheWriteTokens: 5, CostUSD: 0.001}); r.Usage != want {
		t.Errorf("usage %+v", r.Usage)
	}
	if !slices.Equal(r.Models, []string{"claude-haiku-5-5"}) || r.Duration != 42*time.Millisecond {
		t.Errorf("result %+v", r)
	}
	kinds := make([]runtime.EventKind, 0, len(evs))
	for _, ev := range evs {
		kinds = append(kinds, ev.Kind)
	}
	want := []runtime.EventKind{runtime.EventQuota, runtime.EventText, runtime.EventText, runtime.EventToolCall, runtime.EventToolResult, runtime.EventTurnCompleted}
	if !slices.Equal(kinds, want) {
		t.Fatalf("events %v, want %v (duplicate or subagent tool calls must be dropped)", kinds, want)
	}
	if tc := evs[3].ToolCall; tc.Name != "Read" || tc.ID != "t1" || !strings.HasPrefix(tc.Signature, "Read:") {
		t.Errorf("tool call %+v", tc)
	}
	if tr := evs[4].ToolResult; tr.ID != "t1" || tr.Output != "file body" {
		t.Errorf("tool result %+v", tr)
	}
	argv := h.launches()[0]
	if v, _ := flag(argv, "--session-id"); v != ref.ID {
		t.Errorf("--session-id %q, want %q", v, ref.ID)
	}
	if !slices.Contains(argv, "--restricted") {
		t.Error("--restricted missing")
	}
	// The cached quota signal opens the next turn, before anything else.
	_, evs = turn(t, s, runtime.TurnInput{Text: "ok"})
	if evs[0].Kind != runtime.EventQuota {
		t.Errorf("second turn starts with %s", evs[0].Kind)
	}
	if n := len(h.launches()); n != 1 {
		t.Errorf("%d processes for two plain turns", n)
	}
}

func TestStructuredOutputRespawnsAndBack(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	s := h.open()
	schema := json.RawMessage(`{"type":"object","properties":{"ok":{"type":"boolean"}}}`)
	end, evs := turn(t, s, runtime.TurnInput{Text: "ok", OutputSchema: schema})
	mustComplete(t, end, `{"ok":true}`)
	if string(end.Result.Structured) != `{"ok":true}` {
		t.Fatalf("structured %s", end.Result.Structured)
	}
	for _, ev := range evs {
		if ev.Kind == runtime.EventToolCall && ev.ToolCall.Name == structuredOutputTool {
			t.Fatal("StructuredOutput leaked")
		}
		if ev.Kind == runtime.EventToolResult && ev.ToolResult.ID == "so1" {
			t.Fatal("StructuredOutput result leaked")
		}
	}
	end, _ = turn(t, s, runtime.TurnInput{Text: "ok"})
	mustComplete(t, end, "Hello")
	l := h.launches()
	if len(l) != 3 {
		t.Fatalf("%d processes, want 3 (open, schema, back without)", len(l))
	}
	// No transcript existed before the first result, so the schema process must reuse
	// --session-id; afterwards --resume (live check, 2026-11-25).
	if _, ok := flag(l[1], "--session-id"); !ok {
		t.Errorf("schema process: %v", l[1])
	}
	if v, _ := flag(l[1], "--json-schema"); v != string(schema) {
		t.Errorf("--json-schema %q", v)
	}
	if _, ok := flag(l[2], "--resume"); !ok {
		t.Errorf("third process must resume: %v", l[2])
	}
	if _, ok := flag(l[2], "--json-schema"); ok {
		t.Error("schema kept after a turn without one")
	}
}

func TestApprovalRoutedAndDenialCounted(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	h.appr.decision = runtime.Deny("no rm")
	end, _ := turn(t, h.open(), runtime.TurnInput{Text: "perm"})
	mustComplete(t, end, "decided deny")
	if end.Result.Denials != 1 {
		t.Errorf("denials %d", end.Result.Denials)
	}
	a := h.appr.approvals
	if len(a) != 1 || a[0].Kind != runtime.ApproveCommand || a[0].Command != "rm -rf build" || a[0].Session.Runtime != Name ||
		!slices.Contains(a[0].Paths, "/work/x.txt") {
		t.Fatalf("approvals %+v", a)
	}
	resp := h.controlResponses()
	if len(resp) != 1 || !strings.Contains(string(resp[0]), `"behavior":"deny"`) || !strings.Contains(string(resp[0]), "no rm") {
		t.Fatalf("responses %s", resp)
	}
}

func TestApproverErrorDenies(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	h.appr.decision, h.appr.err = runtime.Allow(), errors.New("evaluator down")
	end, _ := turn(t, h.open(), runtime.TurnInput{Text: "perm"})
	mustComplete(t, end, "decided deny")
	if resp := h.controlResponses(); len(resp) != 1 || !strings.Contains(string(resp[0]), "evaluator down") {
		t.Fatalf("responses %s", resp)
	}
}

func TestAskUserAllowedWithoutApprover(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	h.spec.AskUser = &runtime.MCPServer{Name: "openchef", Command: "openchef", Args: []string{"mcp"}}
	h.appr.decision = runtime.Deny("must not be asked")
	s := h.open()
	end, _ := turn(t, s, runtime.TurnInput{Text: "askuser"})
	mustComplete(t, end, "decided allow")
	if h.appr.count() != 0 {
		t.Fatal("ask_user went to the approver")
	}
	cfg, _ := flag(h.launches()[0], "--mcp-config")
	raw, err := os.ReadFile(cfg)
	if err != nil || !strings.Contains(string(raw), `"openchef"`) {
		t.Fatalf("mcp config %q: %v %s", cfg, err, raw)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg); !os.IsNotExist(err) {
		t.Errorf("mcp config left behind: %v", err)
	}
}

func TestPermissionOutsideTurnIsDenied(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "permidle")
	h.open()
	deadline := time.Now().Add(10 * time.Second)
	for len(h.controlResponses()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	resp := h.controlResponses()
	if len(resp) != 1 || !strings.Contains(string(resp[0]), `"behavior":"deny"`) {
		t.Fatalf("responses %s", resp)
	}
	if h.appr.count() != 0 {
		t.Fatal("approver consulted outside a turn")
	}
}

func TestPermissionCancelledByClaude(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	h.appr.block = true
	end, _ := turn(t, h.open(), runtime.TurnInput{Text: "cancelperm"})
	mustComplete(t, end, "cancelled")
	select {
	case <-h.appr.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("control_cancel_request did not cancel the approver")
	}
}

func TestInterruptKeepsProcess(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	s := h.open()
	ch, err := s.Turn(context.Background(), runtime.TurnInput{Text: "hang"})
	if err != nil {
		t.Fatal(err)
	}
	first := started(t, ch)
	if err := s.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	mustFail(t, terminal(t, append(first, drain(t, ch)...)), runtime.FailInterrupted)
	end, _ := turn(t, s, runtime.TurnInput{Text: "ok"})
	mustComplete(t, end, "Hello")
	if n := len(h.launches()); n != 1 {
		t.Errorf("%d processes; a clean interrupt must keep the process", n)
	}
	if err := s.Interrupt(context.Background()); err != nil {
		t.Errorf("Interrupt while idle: %v", err)
	}
}

func TestStubbornInterruptKillsAndResumes(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	rec := &runtimetest.SpawnRecorder{FakeLog: h.logPath}
	h.spec.OnSpawn = rec.OnSpawn
	s := h.open()
	end, _ := turn(t, s, runtime.TurnInput{Text: "remember:KESTREL"})
	mustComplete(t, end, "Hello")
	ch, err := s.Turn(context.Background(), runtime.TurnInput{Text: "stubborn"})
	if err != nil {
		t.Fatal(err)
	}
	first := started(t, ch)
	start := time.Now()
	if err := s.Interrupt(context.Background()); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if d := time.Since(start); d > interruptWait+2*time.Second {
		t.Errorf("Interrupt took %v", d)
	}
	mustFail(t, terminal(t, append(first, drain(t, ch)...)), runtime.FailInterrupted)
	end, _ = turn(t, s, runtime.TurnInput{Text: "recall"})
	mustComplete(t, end, "recalled KESTREL")
	l := h.launches()
	if len(l) != 2 {
		t.Fatalf("%d processes", len(l))
	}
	if v, _ := flag(l[1], "--resume"); v != s.Ref().ID {
		t.Errorf("replacement process: %v", l[1])
	}
	// The replacement process is reported too, so the engine can update its record.
	if ids := rec.IDs(); len(ids) != 2 || ids[0].PID == ids[1].PID {
		t.Errorf("OnSpawn calls = %+v; want two distinct processes", ids)
	}
	if err := rec.Err(); err != nil {
		t.Error(err)
	}
}

func TestInterruptOfQueuedMessageReplacesProcess(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	s := h.open()
	ch, err := s.Turn(context.Background(), runtime.TurnInput{Text: "queued"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // let the fake take the message
	start := time.Now()
	if err := s.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("a still-queued message must not wait for a result (%v)", d)
	}
	mustFail(t, terminal(t, drain(t, ch)), runtime.FailInterrupted)
	end, _ := turn(t, s, runtime.TurnInput{Text: "ok"})
	mustComplete(t, end, "Hello")
	if n := len(h.launches()); n != 2 {
		t.Errorf("%d processes, want a replacement", n)
	}
}

func TestContextTimeoutReportsTimeout(t *testing.T) {
	t.Parallel()
	for range 5 { // our deadline races with Claude's abort result
		h := newHarness(t, "1")
		s := h.open()
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		ch, err := s.Turn(ctx, runtime.TurnInput{Text: "hang"})
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		end := terminal(t, drain(t, ch))
		cancel()
		mustFail(t, end, runtime.FailTimeout)
	}
}

func TestCrashThenResume(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	s := h.open()
	end, _ := turn(t, s, runtime.TurnInput{Text: "remember:OSPREY"})
	mustComplete(t, end, "Hello")
	end, _ = turn(t, s, runtime.TurnInput{Text: "crash"})
	mustFail(t, end, runtime.FailCrashed)
	if !strings.Contains(end.Failure.Message, "boom") {
		t.Errorf("stderr not in the failure: %q", end.Failure.Message)
	}
	end, _ = turn(t, s, runtime.TurnInput{Text: "recall"})
	mustComplete(t, end, "recalled OSPREY")
}

func TestCrashOnFirstTurnRecoversWithFallback(t *testing.T) {
	t.Parallel()
	// The fake writes the transcript before crashing, but no result arrived, so the
	// adapter cannot know: --session-id fails ("already in use") and the fallback
	// --resume must take over.
	h := newHarness(t, "1")
	s := h.open()
	end, _ := turn(t, s, runtime.TurnInput{Text: "crash"})
	mustFail(t, end, runtime.FailCrashed)
	end, _ = turn(t, s, runtime.TurnInput{Text: "ok"})
	mustComplete(t, end, "Hello")
	l := h.launches()
	if len(l) != 3 {
		t.Fatalf("%d processes, want open + failed session-id + resume", len(l))
	}
	if _, ok := flag(l[2], "--resume"); !ok {
		t.Errorf("fallback did not resume: %v", l[2])
	}
}

func TestStaleResultIgnored(t *testing.T) {
	t.Parallel()
	for range 10 {
		h := newHarness(t, "1")
		end, _ := turn(t, h.open(), runtime.TurnInput{Text: "stale"})
		mustComplete(t, end, "Hello")
	}
}

func TestQuotaExhausted(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	end, evs := turn(t, h.open(), runtime.TurnInput{Text: "exhausted"})
	mustFail(t, end, runtime.FailQuotaExhausted)
	if end.Failure.ResetsAt.IsZero() {
		t.Error("ResetsAt missing")
	}
	if evs[0].Kind != runtime.EventQuota || !evs[0].Quota.Exhausted {
		t.Errorf("quota event %+v", evs[0])
	}
}

func TestSecondTurnWhileRunning(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	s := h.open()
	ch, err := s.Turn(context.Background(), runtime.TurnInput{Text: "hang"})
	if err != nil {
		t.Fatal(err)
	}
	started(t, ch)
	if _, err := s.Turn(context.Background(), runtime.TurnInput{Text: "ok"}); !errors.Is(err, runtime.ErrTurnInProgress) {
		t.Fatalf("second Turn: %v", err)
	}
	if err := s.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	drain(t, ch)
}

func TestConcurrentTurnsStartOneProcess(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	s := h.open()
	schema := json.RawMessage(`{"type":"object"}`) // forces a re-spawn inside Turn
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	chans := make(chan (<-chan runtime.Event), 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, err := s.Turn(context.Background(), runtime.TurnInput{Text: "ok", OutputSchema: schema})
			if err != nil {
				errs <- err
				return
			}
			chans <- ch
		}()
	}
	wg.Wait()
	close(errs)
	close(chans)
	started := 0
	for ch := range chans {
		started++
		mustComplete(t, terminal(t, drain(t, ch)), `{"ok":true}`)
	}
	for err := range errs {
		if !errors.Is(err, runtime.ErrTurnInProgress) {
			t.Errorf("unexpected error: %v", err)
		}
	}
	if started != 1 {
		t.Fatalf("%d turns started concurrently", started)
	}
	if n := len(h.launches()); n != 2 {
		t.Fatalf("%d processes, want open + one re-spawn", n)
	}
}

func TestResumeAcrossSessions(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	s := h.open()
	end, _ := turn(t, s, runtime.TurnInput{Text: "remember:HERON"})
	mustComplete(t, end, "Hello")
	ref := s.Ref()
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	s2, err := h.rt.Resume(context.Background(), ref, h.spec)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	defer func() { _ = s2.Close(context.Background()) }()
	if s2.Ref() != ref {
		t.Fatalf("Ref %+v", s2.Ref())
	}
	end, _ = turn(t, s2, runtime.TurnInput{Text: "recall"})
	mustComplete(t, end, "recalled HERON")
	if v, _ := flag(h.launches()[1], "--resume"); v != ref.ID {
		t.Errorf("resume argv %v", h.launches()[1])
	}
}

func TestResumeUnknownSessionFails(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	_, err := h.rt.Resume(context.Background(), runtime.SessionRef{Runtime: Name, ID: "6f1c1c3e-0000-4000-8000-000000000000"}, h.spec)
	if err == nil || !strings.Contains(err.Error(), "No conversation found") {
		t.Fatalf("err = %v", err)
	}
	if _, err := h.rt.Resume(context.Background(), runtime.SessionRef{Runtime: "codex", ID: "x"}, h.spec); err == nil {
		t.Fatal("foreign ref accepted")
	}
}

func TestCloseIdleKillsProcessAndIsIdempotent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	s, err := h.rt.Open(context.Background(), h.spec)
	if err != nil {
		t.Fatal(err)
	}
	sess, ok := s.(*session)
	if !ok {
		t.Fatalf("unexpected session type %T", s)
	}
	id := sess.cur.p.Identity()
	for range 3 {
		if err := s.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	if alive, err := procs.Alive(id); err != nil || alive {
		t.Fatalf("claude still alive after Close (alive=%v err=%v)", alive, err)
	}
	if _, err := s.Turn(context.Background(), runtime.TurnInput{Text: "ok"}); !errors.Is(err, runtime.ErrSessionClosed) {
		t.Fatalf("Turn after Close: %v", err)
	}
}

func TestCloseInFlightIsFast(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	s, err := h.rt.Open(context.Background(), h.spec)
	if err != nil {
		t.Fatal(err)
	}
	sess, ok := s.(*session)
	if !ok {
		t.Fatalf("unexpected session type %T", s)
	}
	id := sess.cur.p.Identity()
	ch, err := s.Turn(context.Background(), runtime.TurnInput{Text: "stubborn"})
	if err != nil {
		t.Fatal(err)
	}
	first := started(t, ch)
	start := time.Now()
	if err := s.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if d := time.Since(start); d > closeWait+2*time.Second {
		t.Errorf("Close took %v with a turn in flight (D-010)", d)
	}
	mustFail(t, terminal(t, append(first, drain(t, ch)...)), runtime.FailInterrupted)
	if alive, _ := procs.Alive(id); alive {
		t.Fatal("process survived Close")
	}
}

func TestCloseWithUnreadEventsDoesNotBlock(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	s, err := h.rt.Open(context.Background(), h.spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Turn(context.Background(), runtime.TurnInput{Text: "flood"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // let the flood fill every buffer
	done := make(chan error, 1)
	go func() { done <- s.Close(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Close: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Close blocked while events were unread")
	}
}

func TestOpenValidation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	for name, mut := range map[string]func(*runtime.SessionSpec){
		"no approver": func(s *runtime.SessionSpec) { s.Approver = nil },
		"bad effort":  func(s *runtime.SessionSpec) { s.Effort = "turbo" },
		"bad mode":    func(s *runtime.SessionSpec) { s.Mode = "yolo" },
	} {
		spec := h.spec
		mut(&spec)
		if _, err := h.rt.Open(context.Background(), spec); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	bad := New(Options{Binary: filepath.Join(t.TempDir(), "no-claude")})
	if _, err := bad.Open(context.Background(), h.spec); err == nil {
		t.Error("missing binary must fail")
	}
	if New(Options{}).opts.Binary != "claude" || New(Options{}).Name() != Name {
		t.Error("defaults")
	}
}

// Regression: the channel closing must mean the session accepts the next turn.
func TestBackToBackTurns(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	s := h.open()
	for i := range 30 {
		ch, err := s.Turn(context.Background(), runtime.TurnInput{Text: "ok"})
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		drain(t, ch)
	}
}
