package codex

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
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
	answer    string
	approvals []runtime.ApprovalRequest
	questions []runtime.Question
}

func (a *recordingApprover) Approve(_ context.Context, req runtime.ApprovalRequest) (runtime.Decision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.approvals = append(a.approvals, req)
	return a.decision, a.err
}

func (a *recordingApprover) Answer(_ context.Context, q runtime.Question) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.questions = append(a.questions, q)
	return a.answer, nil
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
	logPath := filepath.Join(dir, "fake-requests.jsonl")
	appr := &recordingApprover{decision: runtime.Allow(), answer: "teal"}
	return &harness{
		t:       t,
		rt:      New(Options{Binary: exe, Args: []string{}}),
		logPath: logPath,
		appr:    appr,
		spec: runtime.SessionSpec{
			Dir:      dir,
			Model:    "gpt-test",
			Effort:   runtime.EffortMax,
			Mode:     runtime.ModeManual,
			Env:      []string{fakeEnv + "=" + scenario, fakeLogEnv + "=" + logPath},
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

// requests returns the params of every request of the given method the fake received.
func (h *harness) requests(method string) []json.RawMessage {
	h.t.Helper()
	raw, err := os.ReadFile(h.logPath)
	if err != nil {
		h.t.Fatal(err)
	}
	var out []json.RawMessage
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var m wire
		if json.Unmarshal([]byte(line), &m) == nil && m.Method == method {
			out = append(out, m.Params)
		}
	}
	return out
}

// drainTimeout bounds how long tests wait for a turn; live tests raise it.
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

func TestOpenTurnAndParams(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "ok")
	s := h.open()
	if ref := s.Ref(); ref.Runtime != Name || ref.ID != "thr-1" {
		t.Fatalf("Ref = %+v", ref)
	}
	end, evs := turn(t, s, runtime.TurnInput{Text: "hi"})
	if end.Kind != runtime.EventTurnCompleted || end.Result.FinalText != "Hello" {
		t.Fatalf("end = %+v", end)
	}
	if got := end.Result.Usage; got.InputTokens != 60 || got.CacheReadTokens != 40 || got.OutputTokens != 10 {
		t.Fatalf("usage must be the turn delta with cache split out: %+v", got)
	}
	if end.Result.Duration != 42*time.Millisecond {
		t.Errorf("duration = %v", end.Result.Duration)
	}
	var text string
	var quota *runtime.QuotaSignal
	for _, ev := range evs {
		switch ev.Kind {
		case runtime.EventText:
			text += ev.Text
		case runtime.EventQuota:
			quota = ev.Quota
		default:
		}
	}
	if text != "Hello" {
		t.Errorf("streamed text = %q", text)
	}
	if quota == nil || len(quota.Windows) != 2 || quota.Windows[0].Name != "five_hour" || quota.Windows[0].Utilization != 0.12 || quota.Windows[1].Name != "weekly" {
		t.Errorf("quota must merge the sparse update into the snapshot: %+v", quota)
	}

	var start struct {
		ApprovalPolicy string `json:"approvalPolicy"`
		Sandbox        string `json:"sandbox"`
		Model          string `json:"model"`
		Cwd            string `json:"cwd"`
	}
	_ = json.Unmarshal(h.requests("thread/start")[0], &start)
	if start.ApprovalPolicy != "untrusted" || start.Sandbox != "workspace-write" || start.Model != "gpt-test" || start.Cwd != h.spec.Dir {
		t.Errorf("thread/start params = %+v", start)
	}
	var ts struct {
		Effort string `json:"effort"`
	}
	_ = json.Unmarshal(h.requests("turn/start")[0], &ts)
	if ts.Effort != "high" {
		t.Errorf("effort max must map to the highest supported normalized level (high), got %q", ts.Effort)
	}
	if _, err := os.Stat(h.spec.LogPath); err != nil {
		t.Errorf("raw log not written: %v", err)
	}
}

func TestStructuredOutput(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "ok")
	s := h.open()
	end, _ := turn(t, s, runtime.TurnInput{Text: "plan", OutputSchema: json.RawMessage(`{"type":"object"}`)})
	if string(end.Result.Structured) != `{"ok":true}` {
		t.Fatalf("structured = %s", end.Result.Structured)
	}
	var ts struct {
		OutputSchema json.RawMessage `json:"outputSchema"`
	}
	_ = json.Unmarshal(h.requests("turn/start")[0], &ts)
	if string(ts.OutputSchema) != `{"type":"object"}` {
		t.Errorf("outputSchema not forwarded: %s", ts.OutputSchema)
	}
}

func TestPlanModeUsesReadOnlySandbox(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "ok")
	h.spec.Mode = runtime.ModePlan
	h.open()
	var start struct {
		Sandbox string `json:"sandbox"`
	}
	_ = json.Unmarshal(h.requests("thread/start")[0], &start)
	if start.Sandbox != "read-only" {
		t.Fatalf("sandbox = %q", start.Sandbox)
	}
}

func TestApprovalIsRoutedAndDenialCounted(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "approve")
	h.appr.decision = runtime.Deny("not in the plan")
	s := h.open()
	end, _ := turn(t, s, runtime.TurnInput{Text: "clean"})
	if end.Result.FinalText != "decision:decline" || end.Result.Denials != 1 {
		t.Fatalf("end = %+v", end.Result)
	}
	if len(h.appr.approvals) != 1 {
		t.Fatalf("approvals = %+v", h.appr.approvals)
	}
	req := h.appr.approvals[0]
	if req.Kind != runtime.ApproveCommand || req.Tool != "Bash" || req.Command != "rm -rf build" || req.Description != "cleanup" || req.Session.ID != "thr-1" {
		t.Fatalf("request = %+v", req)
	}
}

func TestApprovalForSessionAndApproverError(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "approve")
	h.appr.decision = runtime.Decision{Allow: true, ForSession: true}
	end, _ := turn(t, h.open(), runtime.TurnInput{Text: "x"})
	if end.Result.FinalText != "decision:acceptForSession" {
		t.Fatalf("got %q", end.Result.FinalText)
	}

	h2 := newHarness(t, "approve")
	h2.appr.decision = runtime.Allow()
	h2.appr.err = errors.New("approval service down")
	end, _ = turn(t, h2.open(), runtime.TurnInput{Text: "x"})
	if end.Result.FinalText != "decision:decline" {
		t.Fatalf("an approver error must deny, got %q", end.Result.FinalText)
	}
}

func TestAskUserIsAutoAcceptedAndAnswered(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "ask")
	h.spec.AskUser = &runtime.MCPServer{Name: "openchef", Command: "/bin/openchef", Args: []string{"mcp"}, Env: []string{"A=1"}}
	s := h.open()
	end, _ := turn(t, s, runtime.TurnInput{Text: "ask"})
	if end.Result.FinalText != "accept:teal" {
		t.Fatalf("got %q", end.Result.FinalText)
	}
	if len(h.appr.approvals) != 0 {
		t.Errorf("OpenChef's own ask_user must not reach the approver: %+v", h.appr.approvals)
	}
	if len(h.appr.questions) != 1 || h.appr.questions[0].Text != "Which colour?" {
		t.Errorf("questions = %+v", h.appr.questions)
	}
	var start struct {
		Config struct {
			MCPServers map[string]struct {
				Command string            `json:"command"`
				Args    []string          `json:"args"`
				Env     map[string]string `json:"env"`
			} `json:"mcp_servers"`
		} `json:"config"`
	}
	_ = json.Unmarshal(h.requests("thread/start")[0], &start)
	srv := start.Config.MCPServers["openchef"]
	if srv.Command != "/bin/openchef" || len(srv.Args) != 1 || srv.Env["A"] != "1" {
		t.Errorf("mcp server config = %+v", start.Config)
	}
}

func TestQuotaExhaustion(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "quota")
	end, evs := turn(t, h.open(), runtime.TurnInput{Text: "x"})
	if end.Kind != runtime.EventTurnFailed || end.Failure.Kind != runtime.FailQuotaExhausted {
		t.Fatalf("end = %+v", end)
	}
	if end.Failure.ResetsAt.IsZero() || time.Until(end.Failure.ResetsAt) < time.Hour {
		t.Errorf("ResetsAt should come from the exhausted window: %v", end.Failure.ResetsAt)
	}
	if !strings.Contains(end.Failure.Message, "usageLimitExceeded") {
		t.Errorf("message = %q", end.Failure.Message)
	}
	var exhausted bool
	for _, ev := range evs {
		if ev.Kind == runtime.EventQuota && ev.Quota.Exhausted {
			exhausted = true
		}
	}
	if !exhausted {
		t.Error("an exhausted quota signal should be emitted")
	}
}

func TestCrashDuringTurn(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "crash")
	s := h.open()
	end, _ := turn(t, s, runtime.TurnInput{Text: "x"})
	if end.Kind != runtime.EventTurnFailed || end.Failure.Kind != runtime.FailCrashed {
		t.Fatalf("end = %+v", end)
	}
	if _, err := s.Turn(context.Background(), runtime.TurnInput{Text: "again"}); !errors.Is(err, runtime.ErrSessionClosed) {
		t.Fatalf("Turn after crash: %v", err)
	}
}

func TestContextTimeoutInterruptsTurn(t *testing.T) {
	t.Parallel()
	reached := 0
	for range 10 { // the race between our timeout and Codex's "interrupted" status
		if contextTimeoutOnce(t) {
			reached++
		}
	}
	if reached == 0 {
		t.Fatal("the deadline never fell inside a running turn")
	}
}

// contextTimeoutOnce reports whether the deadline fell inside the turn. On a loaded
// machine it can expire during turn/start; Turn then fails with the context error,
// which is correct but does not exercise the race.
func contextTimeoutOnce(t *testing.T) bool {
	t.Helper()
	h := newHarness(t, "hang")
	s := h.open()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	ch, err := s.Turn(ctx, runtime.TurnInput{Text: "x"})
	if errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	end := terminal(t, drain(t, ch))
	if end.Kind != runtime.EventTurnFailed || end.Failure.Kind != runtime.FailTimeout {
		t.Fatalf("end = %+v", end)
	}
	// The interrupt reaches Codex, and the session is usable again.
	deadline := time.Now().Add(5 * time.Second)
	for len(h.requests("turn/interrupt")) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if len(h.requests("turn/interrupt")) == 0 {
		t.Fatal("turn/interrupt was not sent")
	}
	return true
}

func TestSecondTurnWhileRunning(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "hang")
	s := h.open()
	ch, err := s.Turn(context.Background(), runtime.TurnInput{Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Turn(context.Background(), runtime.TurnInput{Text: "y"}); !errors.Is(err, runtime.ErrTurnInProgress) {
		t.Fatalf("got %v, want ErrTurnInProgress", err)
	}
	if err := s.Interrupt(context.Background()); err != nil {
		t.Fatal(err)
	}
	if end := terminal(t, drain(t, ch)); end.Failure == nil || end.Failure.Kind != runtime.FailInterrupted {
		t.Fatalf("end = %+v", end)
	}
	// After the interrupted turn completes, a new turn may start.
	if _, err := s.Turn(context.Background(), runtime.TurnInput{Text: "z"}); err != nil {
		t.Fatalf("turn after interrupt: %v", err)
	}
}

func TestResume(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "ok")
	s, err := h.rt.Resume(context.Background(), runtime.SessionRef{Runtime: Name, ID: "thr-old"}, h.spec)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	if s.Ref().ID != "thr-old" {
		t.Fatalf("Ref = %+v", s.Ref())
	}
	var p struct {
		ThreadID     string `json:"threadId"`
		ExcludeTurns bool   `json:"excludeTurns"`
	}
	_ = json.Unmarshal(h.requests("thread/resume")[0], &p)
	if p.ThreadID != "thr-old" || !p.ExcludeTurns {
		t.Fatalf("thread/resume params = %+v", p)
	}
	if end, _ := turn(t, s, runtime.TurnInput{Text: "x"}); end.Kind != runtime.EventTurnCompleted {
		t.Fatalf("turn after resume: %+v", end)
	}
	if _, err := h.rt.Resume(context.Background(), runtime.SessionRef{Runtime: "claude", ID: "x"}, h.spec); err == nil {
		t.Fatal("resuming a foreign ref must fail")
	}
}

// Regression: a consumer that stops reading must not be able to wedge Close.
func TestCloseWithUnreadEventsDoesNotBlock(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "flood")
	s, err := h.rt.Open(context.Background(), h.spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Turn(context.Background(), runtime.TurnInput{Text: "x"}); err != nil {
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

func TestCloseKillsProcessAndIsIdempotent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "ok")
	s, err := h.rt.Open(context.Background(), h.spec)
	if err != nil {
		t.Fatal(err)
	}
	sess, ok := s.(*session)
	if !ok {
		t.Fatalf("unexpected session type %T", s)
	}
	id := sess.proc.Identity()
	for range 3 {
		if err := s.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}
	if alive, err := procs.Alive(id); err != nil || alive {
		t.Fatalf("app-server still alive after Close (alive=%v err=%v)", alive, err)
	}
	if _, err := s.Turn(context.Background(), runtime.TurnInput{Text: "x"}); !errors.Is(err, runtime.ErrSessionClosed) {
		t.Fatalf("Turn after Close: %v", err)
	}
}

func TestOpenValidation(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "ok")
	spec := h.spec
	spec.Approver = nil
	if _, err := h.rt.Open(context.Background(), spec); err == nil {
		t.Error("missing approver must fail")
	}
	spec = h.spec
	spec.Effort = "turbo"
	if _, err := h.rt.Open(context.Background(), spec); err == nil {
		t.Error("invalid effort must fail")
	}
	bad := New(Options{Binary: filepath.Join(t.TempDir(), "no-codex")})
	if _, err := bad.Open(context.Background(), h.spec); err == nil {
		t.Error("missing binary must fail")
	}
}

// Regression (found live): notifications of a previous turn that arrive before the new
// turn's id is known must not be attributed to the new turn.
func TestStaleNotificationsAreIgnored(t *testing.T) {
	t.Parallel()
	for range 20 { // the original bug was a race; repeat to make it likely to show
		h := newHarness(t, "stale")
		end, _ := turn(t, h.open(), runtime.TurnInput{Text: "x"})
		if end.Kind != runtime.EventTurnCompleted || end.Result.FinalText != "Hello" {
			t.Fatalf("stale completion leaked into the new turn: %+v %+v", end.Result, end.Failure)
		}
		if u := end.Result.Usage; u.InputTokens != 60 || u.OutputTokens != 10 {
			t.Fatalf("stale usage leaked into the new turn: %+v", u)
		}
	}
}

func TestImmediateFailureBeforeTurnStarted(t *testing.T) {
	t.Parallel()
	for range 20 {
		h := newHarness(t, "fastfail")
		end, _ := turn(t, h.open(), runtime.TurnInput{Text: "x"})
		if end.Kind != runtime.EventTurnFailed || end.Failure.Kind != runtime.FailAuth {
			t.Fatalf("end = %+v", end)
		}
	}
}

// Regression: the channel closing must mean the session accepts the next turn.
func TestBackToBackTurns(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "ok")
	s := h.open()
	for i := range 30 {
		ch, err := s.Turn(context.Background(), runtime.TurnInput{Text: "x"})
		if err != nil {
			t.Fatalf("turn %d: %v", i, err)
		}
		drain(t, ch)
	}
}
