package agy

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
)

type recordingApprover struct {
	mu        sync.Mutex
	decision  runtime.Decision
	err       error
	block     bool
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

func (a *recordingApprover) list() []runtime.ApprovalRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]runtime.ApprovalRequest(nil), a.approvals...)
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
	state := filepath.Join(dir, "sessions")
	if err := os.Mkdir(state, 0o700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "fake.jsonl")
	appr := &recordingApprover{decision: runtime.Allow(), cancelled: make(chan struct{})}
	return &harness{
		t:       t,
		rt:      New(Options{Binary: exe, Args: []string{}, AllowTempDir: true, SettingsDir: filepath.Join(dir, "agy-settings")}),
		logPath: logPath,
		appr:    appr,
		spec: runtime.SessionSpec{
			Dir:      dir,
			Model:    "gemini-big",
			Effort:   runtime.EffortLow,
			Mode:     runtime.ModeManual,
			Env:      []string{fakeEnv + "=" + scenario, fakeLogEnv + "=" + logPath, fakeStateEnv + "=" + state},
			Approver: appr,
			LogPath:  filepath.Join(dir, "raw.jsonl"),
			AskUser:  &runtime.MCPServer{Name: "openchef", Command: "openchef", Args: []string{"mcp"}},
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
	Argv    []string        `json:"argv"`
	In      json.RawMessage `json:"in"`
	Outcome string          `json:"permission_outcome"`
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

func (h *harness) launches() int {
	n := 0
	for _, r := range h.records() {
		if r.Argv != nil {
			n++
		}
	}
	return n
}

// requests returns the params of every request of method the fake received.
func (h *harness) requests(method string) []json.RawMessage {
	var out []json.RawMessage
	for _, r := range h.records() {
		var m struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if r.In != nil && json.Unmarshal(r.In, &m) == nil && m.Method == method {
			out = append(out, m.Params)
		}
	}
	return out
}

// answers returns our replies to the fake's permission requests.
func (h *harness) answers() []string {
	var out []string
	for _, r := range h.records() {
		var m struct {
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
		}
		if r.In != nil && json.Unmarshal(r.In, &m) == nil && m.Method == "" && len(m.Result) > 0 {
			out = append(out, outcome(m.Result))
		}
	}
	return out
}

var drainTimeout = 15 * time.Second

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

func turn(t *testing.T, s runtime.Session, in runtime.TurnInput) (end runtime.Event, evs []runtime.Event) {
	t.Helper()
	ch, err := s.Turn(context.Background(), in)
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	evs = drain(t, ch)
	return terminal(t, evs), evs
}

// started waits for the turn's first streamed text.
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

func TestOpenTurnEventsAndConfig(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	s := h.open()
	if ref := s.Ref(); ref.Runtime != Name || !strings.HasPrefix(ref.ID, "sess-") {
		t.Fatalf("Ref %+v", ref)
	}
	end, evs := turn(t, s, runtime.TurnInput{Text: "ok"})
	mustComplete(t, end, "Hello") // the text after the last tool call, not "Let me look."
	r := end.Result
	if want := (runtime.Usage{InputTokens: 100, OutputTokens: 15, CacheReadTokens: 40}); r.Usage != want {
		t.Errorf("usage %+v", r.Usage)
	}
	if !slices.Equal(r.Models, []string{"gemini-big"}) {
		t.Errorf("models %v", r.Models)
	}
	kinds := make([]runtime.EventKind, 0, len(evs))
	for _, ev := range evs {
		kinds = append(kinds, ev.Kind)
	}
	want := []runtime.EventKind{runtime.EventText, runtime.EventToolCall, runtime.EventToolResult, runtime.EventText, runtime.EventText, runtime.EventTurnCompleted}
	if !slices.Equal(kinds, want) {
		t.Fatalf("events %v, want %v", kinds, want)
	}
	if tc := evs[1].ToolCall; tc.Name != "Bash" || tc.ID != "c1" || !strings.Contains(tc.Signature, "cat a.txt") {
		t.Errorf("tool call %+v", tc)
	}
	if tr := evs[2].ToolResult; tr.Output != "file body" || tr.IsError {
		t.Errorf("tool result %+v", tr)
	}
	// Flags, configuration and session parameters.
	argv := h.records()[0].Argv
	for _, f := range []string{"--sandbox", "--disable-telemetry", "--include-default-customizations=false", "--cli_dir_name"} {
		if !slices.Contains(argv, f) {
			t.Errorf("missing %s in %v", f, argv)
		}
	}
	cfg := h.requests("session/set_config_option")
	set := make([]string, 0, len(cfg))
	for _, p := range cfg {
		var c struct{ ConfigID, Value string }
		_ = json.Unmarshal(p, &c)
		set = append(set, c.ConfigID+"="+c.Value)
	}
	if !slices.Equal(set, []string{"model=gemini-big", "thought_level=low", "mode=default"}) {
		t.Errorf("config %v", set)
	}
	if sn := h.requests("session/new"); len(sn) != 1 || !strings.Contains(string(sn[0]), `"name":"openchef"`) || !strings.Contains(string(sn[0]), `"cwd"`) {
		t.Errorf("session/new %s", sn)
	}
	if init := h.requests("initialize"); len(init) != 1 || !strings.Contains(string(init[0]), `"terminal":false`) {
		t.Errorf("initialize %s", init)
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(h.logPath), "agy-settings", "settings.json"))
	if err != nil || !strings.Contains(string(raw), "proceed-in-sandbox") {
		t.Errorf("settings %s %v", raw, err)
	}
}

func TestEffortFollowsTheModelsLevels(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	h.spec.Model, h.spec.Effort = "gemini-small", runtime.EffortMax // small offers low|medium only
	h.open()
	var level string
	for _, p := range h.requests("session/set_config_option") {
		var c struct{ ConfigID, Value string }
		_ = json.Unmarshal(p, &c)
		if c.ConfigID == "thought_level" {
			level = c.Value
		}
	}
	if level != "medium" {
		t.Fatalf("thought_level %q, want the highest the model offers", level)
	}
	h2 := newHarness(t, "1")
	h2.spec.Model = "nonexistent"
	if _, err := h2.rt.Open(context.Background(), h2.spec); err == nil || !strings.Contains(err.Error(), "unknown model") {
		t.Fatalf("bad model: %v", err)
	}
}

func TestBarrierKeepsEveryChunk(t *testing.T) {
	t.Parallel()
	for range 5 {
		h := newHarness(t, "1")
		end, _ := turn(t, h.open(), runtime.TurnInput{Text: "burst"})
		if end.Kind != runtime.EventTurnCompleted || len(end.Result.FinalText) != 500 {
			t.Fatalf("final text has %d of 500 chunks", len(end.Result.FinalText))
		}
	}
}

func TestPermissionsRouted(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	h.appr.decision = runtime.Deny("no builds")
	s := h.open()
	end, _ := turn(t, s, runtime.TurnInput{Text: "perm"})
	mustComplete(t, end, "decided deny")
	if end.Result.Denials != 1 {
		t.Errorf("denials %d", end.Result.Denials)
	}
	a := h.appr.list()
	if len(a) != 1 || a[0].Kind != runtime.ApproveCommand || a[0].Command != "go build ./..." || a[0].Session != s.Ref() ||
		!strings.Contains(a[0].Description, "outside the agy sandbox") {
		t.Fatalf("approvals %+v", a)
	}
	h.appr.decision = runtime.Allow()
	end, _ = turn(t, s, runtime.TurnInput{Text: "edit"})
	mustComplete(t, end, "decided allow")
	if a := h.appr.list(); len(a) != 2 || a[1].Kind != runtime.ApproveFileChange || !slices.Equal(a[1].Paths, []string{"/w/a.go"}) {
		t.Fatalf("edit approval %+v", a)
	}
}

func TestApproverErrorDenies(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	h.appr.err = errors.New("evaluator down")
	end, _ := turn(t, h.open(), runtime.TurnInput{Text: "perm"})
	mustComplete(t, end, "decided deny")
}

func TestAskUserAllowedWithoutApprover(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	h.appr.decision = runtime.Deny("must not be asked")
	end, _ := turn(t, h.open(), runtime.TurnInput{Text: "askuser"})
	mustComplete(t, end, "decided allow")
	if len(h.appr.list()) != 0 {
		t.Fatal("ask_user went to the approver")
	}
}

func TestPlanModeRefusesWithoutAsking(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	h.spec.Mode = runtime.ModePlan
	s := h.open()
	for _, k := range []string{"perm", "edit"} {
		end, _ := turn(t, s, runtime.TurnInput{Text: k})
		mustComplete(t, end, "decided deny")
	}
	end, _ := turn(t, s, runtime.TurnInput{Text: "askuser"}) // questions still work
	mustComplete(t, end, "decided allow")
	if len(h.appr.list()) != 0 {
		t.Fatal("plan mode consulted the approver")
	}
}

func TestPermissionOutsideTurnIsDenied(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "permidle")
	h.open()
	deadline := time.Now().Add(10 * time.Second)
	for len(h.answers()) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if a := h.answers(); len(a) != 1 || a[0] != "deny" {
		t.Fatalf("answers %v", a)
	}
	if len(h.appr.list()) != 0 {
		t.Fatal("approver consulted outside a turn")
	}
}

func TestInterruptCancelsPendingApproval(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	h.appr.block = true
	s := h.open()
	ch, err := s.Turn(context.Background(), runtime.TurnInput{Text: "hangperm"})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(h.appr.list()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if err := s.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	mustFail(t, terminal(t, drain(t, ch)), runtime.FailInterrupted)
	select {
	case <-h.appr.cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("approver context not cancelled")
	}
	found := false
	for _, r := range h.records() {
		if r.Outcome != "" {
			found = r.Outcome == "cancelled"
		}
	}
	if !found {
		t.Fatal(`pending request was not answered "cancelled"`)
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
		t.Fatal(err)
	}
	end := terminal(t, append(first, drain(t, ch)...))
	mustFail(t, end, runtime.FailInterrupted)
	if end.Failure.Usage.InputTokens != 100 {
		t.Errorf("usage of the cancelled turn lost: %+v", end.Failure.Usage)
	}
	end, _ = turn(t, s, runtime.TurnInput{Text: "ok"})
	mustComplete(t, end, "Hello")
	if n := h.launches(); n != 1 {
		t.Errorf("%d processes", n)
	}
	cancels := 0
	for _, r := range h.records() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if r.In != nil && json.Unmarshal(r.In, &m) == nil && m.Method == "session/cancel" {
			cancels++
			if len(m.ID) > 0 {
				t.Error("session/cancel must be a notification")
			}
		}
	}
	if cancels != 1 {
		t.Errorf("%d session/cancel messages", cancels)
	}
}

func TestStubbornCancelKillsAndReloads(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
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
		t.Fatal(err)
	}
	if d := time.Since(start); d > interruptWait+2*time.Second {
		t.Errorf("Interrupt took %v", d)
	}
	mustFail(t, terminal(t, append(first, drain(t, ch)...)), runtime.FailInterrupted)
	end, evs := turn(t, s, runtime.TurnInput{Text: "recall"})
	mustComplete(t, end, "recalled KESTREL")
	for _, ev := range evs {
		if ev.Kind == runtime.EventText && strings.Contains(ev.Text, "OLD") {
			t.Fatal("replayed history leaked into the turn")
		}
	}
	if n := h.launches(); n != 2 {
		t.Errorf("%d processes", n)
	}
	if l := h.requests("session/load"); len(l) != 1 || !strings.Contains(string(l[0]), s.Ref().ID) {
		t.Errorf("session/load %s", l)
	}
}

func TestContextTimeoutReportsTimeout(t *testing.T) {
	t.Parallel()
	for range 5 {
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

func TestCrashThenReload(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	s := h.open()
	end, _ := turn(t, s, runtime.TurnInput{Text: "remember:OSPREY"})
	mustComplete(t, end, "Hello")
	end, _ = turn(t, s, runtime.TurnInput{Text: "crash"})
	mustFail(t, end, runtime.FailCrashed)
	if !strings.Contains(end.Failure.Message, "boom") {
		t.Errorf("stderr missing: %q", end.Failure.Message)
	}
	end, _ = turn(t, s, runtime.TurnInput{Text: "recall"})
	mustComplete(t, end, "recalled OSPREY")
}

func TestQuotaAndRefusal(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	s := h.open()
	end, _ := turn(t, s, runtime.TurnInput{Text: "quota"})
	mustFail(t, end, runtime.FailQuotaExhausted)
	end, _ = turn(t, s, runtime.TurnInput{Text: "refuse"})
	mustFail(t, end, runtime.FailUnknown)
	end, _ = turn(t, s, runtime.TurnInput{Text: "ok"}) // the session survives both
	mustComplete(t, end, "Hello")
}

func TestStructuredOutput(t *testing.T) {
	t.Parallel()
	sch := json.RawMessage(`{"type":"object","properties":{"steps":{"type":"array","items":{"type":"string"}}},"required":["steps"],"additionalProperties":false}`)
	h := newHarness(t, "1")
	s := h.open()
	end, _ := turn(t, s, runtime.TurnInput{Text: "json", OutputSchema: sch})
	if end.Kind != runtime.EventTurnCompleted || string(end.Result.Structured) != `{"steps":["a","b"]}` {
		t.Fatalf("fenced json: %+v %+v", end.Result, end.Failure)
	}
	if p := h.requests("session/prompt"); !strings.Contains(string(p[0]), "JSON Schema") {
		t.Errorf("prompt lacks the schema instruction: %s", p[0])
	}
	// One repair turn, usage of both prompts counted.
	end, _ = turn(t, s, runtime.TurnInput{Text: "badjson", OutputSchema: sch})
	if end.Kind != runtime.EventTurnCompleted || string(end.Result.Structured) != `{"steps":["fixed"]}` || end.Result.Usage.InputTokens != 200 {
		t.Fatalf("repaired: %+v %+v", end.Result, end.Failure)
	}
	end, _ = turn(t, s, runtime.TurnInput{Text: "neverjson", OutputSchema: sch})
	mustFail(t, end, runtime.FailProtocol)
	if _, err := s.Turn(context.Background(), runtime.TurnInput{Text: "x", OutputSchema: json.RawMessage(`{"type":"bogus"}`)}); err == nil {
		t.Fatal("invalid schema accepted")
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
	end, _ = turn(t, s2, runtime.TurnInput{Text: "recall"})
	mustComplete(t, end, "recalled HERON")
	if _, err := h.rt.Resume(context.Background(), runtime.SessionRef{Runtime: Name, ID: "missing"}, h.spec); err == nil || !strings.Contains(err.Error(), "session not found") {
		t.Fatalf("unknown session: %v", err)
	}
	if _, err := h.rt.Resume(context.Background(), runtime.SessionRef{Runtime: "claude", ID: "x"}, h.spec); err == nil {
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
		t.Fatalf("type %T", s)
	}
	id := sess.cur.p.Identity()
	for range 3 {
		if err := s.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	if alive, err := procs.Alive(id); err != nil || alive {
		t.Fatalf("agy alive after Close (alive=%v err=%v)", alive, err)
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
		t.Fatalf("type %T", s)
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
	if d := time.Since(start); d > closeWait+3*time.Second {
		t.Errorf("Close took %v", d)
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
	time.Sleep(300 * time.Millisecond)
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

func TestConcurrentTurnsStartOneProcess(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	s := h.open()
	// "hang" keeps the first turn open until the interrupt, so every other call overlaps it.
	var wg sync.WaitGroup
	chans := make(chan (<-chan runtime.Event), 8)
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, err := s.Turn(context.Background(), runtime.TurnInput{Text: "hang"})
			if err != nil {
				errs <- err
				return
			}
			chans <- ch
		}()
	}
	wg.Wait()
	close(chans)
	close(errs)
	for err := range errs {
		if !errors.Is(err, runtime.ErrTurnInProgress) {
			t.Errorf("unexpected: %v", err)
		}
	}
	if len(chans) != 1 {
		t.Fatalf("%d concurrent turns started", len(chans))
	}
	ch := <-chans
	started(t, ch)
	if err := s.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	mustFail(t, terminal(t, drain(t, ch)), runtime.FailInterrupted)
	if n := h.launches(); n != 1 {
		t.Errorf("%d processes", n)
	}
}

func TestOpenValidation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	for name, mut := range map[string]func(*runtime.SessionSpec){
		"no approver": func(s *runtime.SessionSpec) { s.Approver = nil },
		"bad effort":  func(s *runtime.SessionSpec) { s.Effort = "turbo" },
		"bad mode":    func(s *runtime.SessionSpec) { s.Mode = "yolo" },
		"no dir":      func(s *runtime.SessionSpec) { s.Dir = "" },
	} {
		spec := h.spec
		mut(&spec)
		if _, err := h.rt.Open(context.Background(), spec); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	strict := New(Options{Binary: h.rt.opts.Binary, Args: []string{}, SettingsDir: h.rt.opts.SettingsDir})
	if _, err := strict.Open(context.Background(), h.spec); err == nil || !strings.Contains(err.Error(), "temp tree") {
		t.Errorf("temp workspace: %v", err)
	}
	bad := New(Options{Binary: filepath.Join(t.TempDir(), "no-agy"), AllowTempDir: true, SettingsDir: h.rt.opts.SettingsDir})
	if _, err := bad.Open(context.Background(), h.spec); err == nil {
		t.Error("missing binary accepted")
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
