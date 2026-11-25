package agy

import (
	"context"
	"encoding/json"
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
	recs, err := rawlog.ReadFile(filepath.Join("..", "..", "..", "testdata", "agy", name))
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

type wire struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
}

// replayApprover allows everything except commands mentioning "denied" or "outside",
// like the spike did, and records what it saw.
type replayApprover struct {
	mu   sync.Mutex
	reqs []runtime.ApprovalRequest
}

func (a *replayApprover) Approve(_ context.Context, req runtime.ApprovalRequest) (runtime.Decision, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.reqs = append(a.reqs, req)
	if strings.Contains(req.Command, "denied") || strings.Contains(req.Command, "outside") {
		return runtime.Deny("spike policy"), nil
	}
	return runtime.Allow(), nil
}

func (*replayApprover) Answer(context.Context, runtime.Question) (string, error) { return "", nil }

type replayed struct {
	turns   [][]runtime.Event
	answers []json.RawMessage // our replies to session/request_permission
	reqs    []runtime.ApprovalRequest
}

// replay feeds a recorded session through the adapter: updates through onUpdate,
// permission requests through handle, prompts through drive with the recorded result.
// Records before session/new or session/load completes belong to the handshake.
func replay(t *testing.T, mode runtime.Mode, files ...string) replayed {
	t.Helper()
	appr := &replayApprover{}
	spec := runtime.SessionSpec{Approver: appr, Mode: mode, AskUser: &runtime.MCPServer{Name: "openchef"}}
	s := newSession(New(Options{}), spec, nil, "")
	var out replayed
	for _, f := range files {
		g := &proc{stderr: &tailBuffer{}, routed: make(chan struct{})}
		s.mu.Lock()
		s.cur = g
		s.mu.Unlock()
		sent := map[string]string{}
		var ts *turnState
		var results chan promptResult
		var ended chan runtime.Event
		for _, r := range fixture(t, f) {
			if r.Msg == nil {
				continue
			}
			var m wire
			if err := json.Unmarshal(r.Msg, &m); err != nil {
				t.Fatalf("%s: bad record: %v", f, err)
			}
			switch {
			case r.Dir == rawlog.Send && m.Method != "":
				sent[string(m.ID)] = m.Method
				switch m.Method {
				case "session/load":
					s.mu.Lock()
					s.loading = true
					s.mu.Unlock()
				case "session/prompt":
					if ts == nil {
						s.mu.Lock()
						ts = s.newTurnState(g)
						s.turn = ts
						s.mu.Unlock()
						results, ended = make(chan promptResult, 2), make(chan runtime.Event, 1)
						cur := ts
						go func() {
							ended <- s.drive(cur, "", nil, func(*turnState, string) (promptResult, error) { return <-results, nil })
						}()
					}
				}
			case r.Dir == rawlog.Recv && m.Method == "session/update":
				s.onUpdate(g, m.Params)
			case r.Dir == rawlog.Recv && m.Method == "session/request_permission":
				ans, err := s.handle(context.Background(), g, m.Method, m.Params)
				if err != nil {
					t.Fatalf("%s: handle: %v", f, err)
				}
				raw, _ := json.Marshal(ans)
				out.answers = append(out.answers, raw)
			case r.Dir == rawlog.Recv && m.Method == "" && len(m.ID) > 0:
				switch sent[string(m.ID)] {
				case "session/new":
					var res sessionResult
					_ = json.Unmarshal(m.Result, &res)
					s.mu.Lock()
					s.ref = runtime.SessionRef{Runtime: Name, ID: res.SessionID}
					s.mu.Unlock()
				case "session/load":
					s.mu.Lock()
					s.loading = false
					s.mu.Unlock()
				case "session/prompt":
					var res promptResult
					if err := json.Unmarshal(m.Result, &res); err != nil {
						t.Fatalf("%s: prompt result: %v", f, err)
					}
					results <- res
					var end runtime.Event
					select {
					case end = <-ended:
					case <-time.After(5 * time.Second):
						t.Fatalf("%s: turn did not end", f)
					}
					s.endTurn(ts, end)
					out.turns = append(out.turns, collect(t, ts))
					ts = nil
				}
			}
		}
		if ts != nil {
			t.Fatalf("%s: last turn never ended", f)
		}
	}
	appr.mu.Lock()
	out.reqs = append(out.reqs, appr.reqs...)
	appr.mu.Unlock()
	return out
}

func collect(t *testing.T, ts *turnState) []runtime.Event {
	t.Helper()
	var evs []runtime.Event
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ts.q.C():
			if !ok {
				return evs
			}
			evs = append(evs, ev)
		case <-timeout:
			t.Fatalf("events did not close: %+v", evs)
		}
	}
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

func streamed(evs []runtime.Event) string {
	var b strings.Builder
	for _, ev := range evs {
		if ev.Kind == runtime.EventText {
			b.WriteString(ev.Text)
		}
	}
	return b.String()
}

func TestReplayContractOnAllFixtures(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob(filepath.Join("..", "..", "..", "testdata", "agy", "*.jsonl"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	for _, f := range files {
		name := filepath.Base(f)
		if name == "eof-midturn.jsonl" {
			continue // the turn never ends: stdin was closed mid-turn (m1-findings §2)
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rp := replay(t, runtime.ModeManual, name)
			if len(rp.turns) == 0 {
				t.Fatal("no turns")
			}
			for i, evs := range rp.turns {
				terminal(t, evs)
				calls := map[string]bool{}
				for _, ev := range evs {
					switch ev.Kind {
					case runtime.EventToolCall:
						if calls[ev.ToolCall.ID] {
							t.Errorf("turn %d: tool call %s reported twice", i, ev.ToolCall.ID)
						}
						calls[ev.ToolCall.ID] = true
					case runtime.EventToolResult:
						if !calls[ev.ToolResult.ID] {
							t.Errorf("turn %d: result %s without a call", i, ev.ToolResult.ID)
						}
					case runtime.EventText, runtime.EventQuota, runtime.EventTurnCompleted, runtime.EventTurnFailed:
					}
				}
			}
		})
	}
}

func TestReplayLoadDropsHistory(t *testing.T) {
	t.Parallel()
	rp := replay(t, runtime.ModeManual, "m18-load.jsonl", "m18-load-resumed.jsonl")
	if len(rp.turns) != 2 {
		t.Fatalf("%d turns", len(rp.turns))
	}
	first, second := terminal(t, rp.turns[0]), terminal(t, rp.turns[1])
	if first.Result.FinalText != "READY" {
		t.Errorf("first: %q", first.Result.FinalText)
	}
	// The replayed history ("READY", the user's message) must not leak into the turn.
	if second.Result.FinalText != "KESTREL" || streamed(rp.turns[1]) != "KESTREL" {
		t.Errorf("resumed turn: final %q, streamed %q", second.Result.FinalText, streamed(rp.turns[1]))
	}
	if u := second.Result.Usage; u.CacheReadTokens != 17962 || u.InputTokens != 2271 {
		t.Errorf("usage %+v", u)
	}
}

func TestReplayCancel(t *testing.T) {
	t.Parallel()
	rp := replay(t, runtime.ModeManual, "m18-cancel.jsonl")
	a, b := terminal(t, rp.turns[0]), terminal(t, rp.turns[1])
	if a.Kind != runtime.EventTurnFailed || a.Failure.Kind != runtime.FailInterrupted || a.Failure.Usage.OutputTokens == 0 {
		t.Errorf("cancelled turn: %+v", a.Failure)
	}
	if b.Result == nil || b.Result.FinalText != "SECOND" {
		t.Errorf("next turn: %+v", b)
	}
}

func TestReplayPermissions(t *testing.T) {
	t.Parallel()
	rp := replay(t, runtime.ModeManual, "m18-bypass.jsonl")
	if len(rp.reqs) != 2 || rp.reqs[0].Command != "echo a > cmd.txt" || !strings.Contains(rp.reqs[0].Description, "outside the agy sandbox") {
		t.Fatalf("requests %+v", rp.reqs)
	}
	if len(rp.answers) != 2 || !strings.Contains(string(rp.answers[0]), `"optionId":"allow"`) || !strings.Contains(string(rp.answers[1]), `"optionId":"deny"`) {
		t.Fatalf("answers %s", rp.answers)
	}
	end := terminal(t, rp.turns[0])
	if end.Result.Denials != 1 {
		t.Errorf("denials %d", end.Result.Denials)
	}
	// Sandboxed failures are tool errors, not turn failures.
	var errs int
	for _, ev := range rp.turns[0] {
		if ev.Kind == runtime.EventToolResult && ev.ToolResult.IsError {
			errs++
		}
	}
	if errs < 2 {
		t.Errorf("%d failed tool results, want the sandbox failure and the denial", errs)
	}
}

func TestReplayAskUserIsAllowedWithoutApprover(t *testing.T) {
	t.Parallel()
	rp := replay(t, runtime.ModeManual, "m18-mcp.jsonl")
	if len(rp.reqs) != 0 {
		t.Fatalf("ask_user reached the approver: %+v", rp.reqs)
	}
	if len(rp.answers) != 1 || !strings.Contains(string(rp.answers[0]), `"optionId":"allow"`) {
		t.Fatalf("answers %s", rp.answers)
	}
	if end := terminal(t, rp.turns[0]); end.Result.FinalText != "teal" {
		t.Errorf("final %q", end.Result.FinalText)
	}
}

func TestReplayPlanModeRefusesEveryRequest(t *testing.T) {
	t.Parallel()
	rp := replay(t, runtime.ModePlan, "m18-review-sandbox.jsonl")
	if len(rp.reqs) != 0 {
		t.Fatalf("plan mode consulted the approver: %+v", rp.reqs)
	}
	for _, a := range rp.answers {
		if !strings.Contains(string(a), `"optionId":"deny"`) {
			t.Errorf("plan mode allowed: %s", a)
		}
	}
	if len(rp.answers) == 0 {
		t.Fatal("no requests in the fixture")
	}
}

func TestReplayFinalTextIsTheLastMessage(t *testing.T) {
	t.Parallel()
	rp := replay(t, runtime.ModeManual, "m18-auto.jsonl")
	end := terminal(t, rp.turns[0])
	if !strings.HasPrefix(end.Result.FinalText, "DONE") {
		t.Errorf("final text %q", end.Result.FinalText)
	}
	if got := streamed(rp.turns[0]); !strings.HasSuffix(got, end.Result.FinalText) {
		t.Errorf("final text is not the tail of the stream")
	}
}
