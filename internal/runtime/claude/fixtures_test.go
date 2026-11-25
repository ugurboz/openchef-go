package claude

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/runtime/rawlog"
)

func fixture(t *testing.T, name string) []rawlog.Record {
	t.Helper()
	recs, err := rawlog.ReadFile(filepath.Join("..", "..", "..", "testdata", "claude", name))
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

// signalApprover records requests and signals each one, so replay can wait for the
// asynchronous permission goroutine before feeding the next line.
type signalApprover struct {
	mu   sync.Mutex
	reqs []runtime.ApprovalRequest
	seen chan struct{}
}

func (a *signalApprover) Approve(_ context.Context, req runtime.ApprovalRequest) (runtime.Decision, error) {
	a.mu.Lock()
	a.reqs = append(a.reqs, req)
	a.mu.Unlock()
	a.seen <- struct{}{}
	return runtime.Allow(), nil
}

func (*signalApprover) Answer(context.Context, runtime.Question) (string, error) { return "", nil }

type replay struct {
	turns     [][]runtime.Event
	approvals []runtime.ApprovalRequest
	results   []resultMessage
}

// replayFixture feeds a recorded session through the adapter's dispatcher, starting a
// turn at every recorded user message, and returns the normalized events per turn.
func replayFixture(t *testing.T, name string) replay {
	t.Helper()
	appr := &signalApprover{seen: make(chan struct{}, 16)}
	s := newSession(New(Options{}), runtime.SessionSpec{Approver: appr}, nil, runtime.SessionRef{Runtime: Name, ID: "fixture"}, false)
	g := newProc(io.Discard, nil)
	s.cur = g
	var out replay
	var ts *turnState
	collect := func() {
		var evs []runtime.Event
		timeout := time.After(5 * time.Second)
		for {
			select {
			case ev, ok := <-ts.q.C():
				if !ok {
					out.turns = append(out.turns, evs)
					ts = nil
					return
				}
				evs = append(evs, ev)
			case <-timeout:
				t.Fatalf("%s: turn %d did not terminate; events: %+v", name, len(out.turns), evs)
			}
		}
	}
	for _, r := range fixture(t, name) {
		if r.Msg == nil {
			continue
		}
		var env envelope
		if err := json.Unmarshal(r.Msg, &env); err != nil {
			t.Fatalf("%s: bad record: %v", name, err)
		}
		switch {
		case r.Dir == rawlog.Send && env.Type == "user":
			if ts != nil {
				t.Fatalf("%s: user message while a turn is open", name)
			}
			s.mu.Lock()
			ts = newTurnState(g)
			g.written++
			s.turn = ts
			s.mu.Unlock()
		case r.Dir == rawlog.Recv:
			s.dispatch(g, r.Msg)
			if env.Type == "control_request" && strings.Contains(string(env.Request), `"can_use_tool"`) {
				select {
				case <-appr.seen:
				case <-time.After(5 * time.Second):
					t.Fatalf("%s: permission request never reached the approver", name)
				}
			}
			if env.Type == "result" {
				var res resultMessage
				_ = json.Unmarshal(r.Msg, &res)
				out.results = append(out.results, res)
				if ts == nil {
					t.Fatalf("%s: result without a turn", name)
				}
				collect()
			}
		}
	}
	if ts != nil {
		t.Fatalf("%s: last turn never ended", name)
	}
	appr.mu.Lock()
	out.approvals = append(out.approvals, appr.reqs...)
	appr.mu.Unlock()
	return out
}

func terminal(t *testing.T, evs []runtime.Event) runtime.Event {
	t.Helper()
	if len(evs) == 0 || !evs[len(evs)-1].Kind.Terminal() {
		t.Fatalf("turn has no terminal event: %+v", evs)
	}
	for _, ev := range evs[:len(evs)-1] {
		if ev.Kind.Terminal() {
			t.Fatalf("terminal event before the end: %+v", evs)
		}
	}
	return evs[len(evs)-1]
}

// TestReplayAllFixtures checks the contract on every recorded session: one terminal
// event per user message, usage from the result only, final text from the result only,
// tool results matching tool calls, no StructuredOutput plumbing in the event stream.
func TestReplayAllFixtures(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "testdata", "claude", "*.jsonl"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	for _, f := range files {
		name := filepath.Base(f)
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rp := replayFixture(t, name)
			if len(rp.turns) == 0 || len(rp.turns) != len(rp.results) {
				t.Fatalf("%d turns for %d results", len(rp.turns), len(rp.results))
			}
			for i, evs := range rp.turns {
				end := terminal(t, evs)
				want := rp.results[i].usage()
				var got runtime.Usage
				if end.Result != nil {
					got = end.Result.Usage
					if end.Result.FinalText != rp.results[i].Result {
						t.Errorf("turn %d: final text is not the result's", i)
					}
				} else {
					got = end.Failure.Usage
				}
				if got != want {
					t.Errorf("turn %d: usage %+v, want result.usage %+v", i, got, want)
				}
				calls := map[string]bool{}
				for _, ev := range evs {
					switch ev.Kind {
					case runtime.EventToolCall:
						if ev.ToolCall.Name == structuredOutputTool {
							t.Errorf("turn %d: StructuredOutput leaked as a tool call", i)
						}
						if calls[ev.ToolCall.ID] {
							t.Errorf("turn %d: tool call %s reported twice", i, ev.ToolCall.ID)
						}
						calls[ev.ToolCall.ID] = true
					case runtime.EventToolResult:
						if !calls[ev.ToolResult.ID] {
							t.Errorf("turn %d: tool result %s without a call", i, ev.ToolResult.ID)
						}
					case runtime.EventText, runtime.EventQuota, runtime.EventTurnCompleted, runtime.EventTurnFailed:
					}
				}
			}
		})
	}
}

func TestReplayManualApprovals(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]int{"stdio-manual.jsonl": 3, "m16-restricted-manual.jsonl": 3, "m16-restricted-acceptedits.jsonl": 2} {
		rp := replayFixture(t, name)
		if len(rp.approvals) != want {
			t.Fatalf("%s: %d approvals, want %d", name, len(rp.approvals), want)
		}
		for _, a := range rp.approvals {
			if a.Session.ID != "fixture" || len(a.Raw) == 0 {
				t.Errorf("%s: %+v", name, a)
			}
			if a.Tool == "Bash" && (a.Kind != runtime.ApproveCommand || a.Command == "") {
				t.Errorf("%s: bash request %+v", name, a)
			}
			if a.Tool == "Write" && (a.Kind != runtime.ApproveFileChange || len(a.Paths) == 0) {
				t.Errorf("%s: write request %+v", name, a)
			}
		}
	}
	// acceptEdits: writes to tool config and outside the directory still reach the host
	// under --restricted (m1-findings §9).
	rp := replayFixture(t, "m16-restricted-acceptedits.jsonl")
	var paths []string
	for _, a := range rp.approvals {
		paths = append(paths, a.Paths...)
	}
	if joined := strings.Join(paths, " "); !strings.Contains(joined, ".claude") {
		t.Errorf("settings write did not reach the approver: %v", paths)
	}
}

func TestReplayStructuredOutput(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"json-schema.jsonl", "m17-planner.jsonl"} {
		end := terminal(t, replayFixture(t, name).turns[0])
		if end.Kind != runtime.EventTurnCompleted || !json.Valid(end.Result.Structured) || len(end.Result.Structured) < 3 {
			t.Fatalf("%s: %+v", name, end.Result)
		}
	}
	// The planner shape keeps the requested model (D-008); Claude's plan mode did not.
	if m := terminal(t, replayFixture(t, "m17-planner.jsonl").turns[0]).Result.Models; len(m) != 1 || m[0] != "claude-haiku-5-5" {
		t.Errorf("planner models %v", m)
	}
	if m := terminal(t, replayFixture(t, "m16-plan-model.jsonl").turns[0]).Result.Models; len(m) != 1 || m[0] != "claude-sonnet-5-5" {
		t.Errorf("plan-mode models %v", m)
	}
}

func TestReplayInterruptThenNextTurn(t *testing.T) {
	t.Parallel()
	rp := replayFixture(t, "m16-interrupt.jsonl")
	if len(rp.turns) != 2 {
		t.Fatalf("%d turns", len(rp.turns))
	}
	first, second := terminal(t, rp.turns[0]), terminal(t, rp.turns[1])
	if first.Kind != runtime.EventTurnFailed || first.Failure.Kind != runtime.FailInterrupted {
		t.Errorf("first: %+v", first.Failure)
	}
	if second.Kind != runtime.EventTurnCompleted || second.Result.FinalText != "SECOND" {
		t.Errorf("second: %+v", second.Result)
	}
	if got := streamed(rp.turns[1]); got != "SECOND" {
		t.Errorf("streamed text %q", got)
	}
}

func streamed(evs []runtime.Event) string {
	var text strings.Builder
	for _, ev := range evs {
		if ev.Kind == runtime.EventText {
			text.WriteString(ev.Text)
		}
	}
	return text.String()
}

func TestReplayTwoTurnsAndBudget(t *testing.T) {
	t.Parallel()
	rp := replayFixture(t, "m17-auto.jsonl")
	a, b := terminal(t, rp.turns[0]), terminal(t, rp.turns[1])
	if a.Result.Denials != 1 || b.Result.Denials != 0 || b.Result.FinalText != "SECOND" {
		t.Errorf("auto: %+v / %+v", a.Result, b.Result)
	}
	for i, evs := range replayFixture(t, "m16-budget.jsonl").turns {
		if end := terminal(t, evs); end.Failure == nil || end.Failure.Kind != runtime.FailUnknown {
			t.Errorf("budget turn %d: %+v", i, end)
		}
	}
	quota := 0
	for _, ev := range rp.turns[0] {
		if ev.Kind == runtime.EventQuota {
			quota++
			if ev.Quota.Exhausted || len(ev.Quota.Windows) != 2 {
				t.Errorf("quota %+v", ev.Quota)
			}
		}
	}
	if quota == 0 {
		t.Error("rate_limit_event did not become a quota event")
	}
}

// TestReplayStreamedTextIsNotTheFinalText: deltas are streamed, but the final text
// comes from the result alone, never from both (§6.1).
func TestReplayStreamedTextIsNotTheFinalText(t *testing.T) {
	t.Parallel()
	evs := replayFixture(t, "eof-midturn.jsonl").turns[0]
	end := terminal(t, evs)
	text := streamed(evs)
	if text == "" || text != end.Result.FinalText {
		t.Fatalf("streamed %d bytes, final %d bytes", len(text), len(end.Result.FinalText))
	}
}
