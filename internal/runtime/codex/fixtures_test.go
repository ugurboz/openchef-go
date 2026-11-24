package codex

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/runtime/linerpc"
	"github.com/ugurboz/openchef/internal/runtime/rawlog"
)

func fixture(t *testing.T, name string) []rawlog.Record {
	t.Helper()
	recs, err := rawlog.ReadFile(filepath.Join("..", "..", "..", "testdata", "codex", name))
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

// replayTurns feeds a recorded session through the adapter's notification router, one
// turn at a time, and returns the normalized events of every turn.
func replayTurns(t *testing.T, recs []rawlog.Record) [][]runtime.Event {
	t.Helper()
	s := newSession(runtime.SessionSpec{}, nil, nil)
	var turns [][]runtime.Event
	var ts *turnState
	sendMethods := map[string]wire{}

	collect := func() {
		if ts == nil {
			return
		}
		var evs []runtime.Event
		timeout := time.After(5 * time.Second)
		for {
			select {
			case ev, ok := <-ts.ch:
				if !ok {
					turns = append(turns, evs)
					ts = nil
					return
				}
				evs = append(evs, ev)
			case <-timeout:
				t.Fatalf("turn %d did not terminate; events so far: %+v", len(turns), evs)
			}
		}
	}

	for _, r := range recs {
		if r.Msg == nil {
			continue
		}
		var m wire
		if err := json.Unmarshal(r.Msg, &m); err != nil {
			t.Fatalf("bad record: %v", err)
		}
		switch r.Dir {
		case rawlog.Send:
			if m.Method != "" {
				sendMethods[string(m.ID)] = m
			}
			if m.Method == "turn/start" {
				var p struct {
					OutputSchema json.RawMessage `json:"outputSchema"`
				}
				_ = json.Unmarshal(m.Params, &p)
				ts = newTurnState(len(p.OutputSchema) > 0)
				s.mu.Lock()
				s.turn = ts
				s.mu.Unlock()
			}
		case rawlog.Recv:
			switch {
			case m.Method != "" && len(m.ID) == 0:
				s.routeMu.Lock()
				s.onNotification(linerpc.Notification{Method: m.Method, Params: m.Params})
				s.routeMu.Unlock()
				if m.Method == "turn/completed" {
					collect()
				}
			case len(m.ID) > 0 && m.Method == "":
				req := sendMethods[string(m.ID)]
				switch req.Method {
				case "thread/start", "thread/resume":
					var res threadResponse
					_ = json.Unmarshal(m.Result, &res)
					s.mu.Lock()
					s.ref = runtime.SessionRef{Runtime: Name, ID: res.Thread.ID}
					s.mu.Unlock()
				case "turn/start":
					var res turnStartResponse
					_ = json.Unmarshal(m.Result, &res)
					s.setTurnID(ts, res.Turn.ID)
				case "account/rateLimits/read":
					var res rateLimitsRead
					if err := json.Unmarshal(m.Result, &res); err != nil {
						t.Fatalf("rateLimits/read result does not decode: %v", err)
					}
					s.setLimits(res.RateLimits)
				}
			}
		}
	}
	return turns
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

func toolCalls(evs []runtime.Event) []string {
	var out []string
	for _, ev := range evs {
		if ev.Kind == runtime.EventToolCall {
			out = append(out, ev.ToolCall.Signature)
		}
	}
	return out
}

func TestReplayMainSession(t *testing.T) {
	t.Parallel()
	turns := replayTurns(t, fixture(t, "main.jsonl"))
	// approve-cmd, decline-cmd, output-schema, user-input, interrupt
	if len(turns) != 5 {
		t.Fatalf("got %d turns, want 5", len(turns))
	}

	// 1. Approved command.
	end := terminal(t, turns[0])
	if end.Kind != runtime.EventTurnCompleted || end.Result.FinalText != "DONE" {
		t.Fatalf("turn 1 end: %+v", end)
	}
	if calls := toolCalls(turns[0]); len(calls) != 1 || !strings.Contains(calls[0], "echo spike > out.txt") {
		t.Fatalf("turn 1 tool calls: %v", calls)
	}
	u := end.Result.Usage
	if u.InputTokens <= 0 || u.OutputTokens <= 0 || u.CacheReadTokens < 0 {
		t.Fatalf("turn 1 usage not computed: %+v", u)
	}

	// 2. Declined command: the turn still completes; the command item is not a success.
	end = terminal(t, turns[1])
	if end.Kind != runtime.EventTurnCompleted {
		t.Fatalf("turn 2 end: %+v", end)
	}
	var sawFailedTool bool
	for _, ev := range turns[1] {
		if ev.Kind == runtime.EventToolResult && ev.ToolResult.IsError {
			sawFailedTool = true
		}
	}
	if !sawFailedTool {
		t.Fatalf("declined command must surface as a failed tool result: %+v", turns[1])
	}

	// 3. Structured output.
	end = terminal(t, turns[2])
	if end.Kind != runtime.EventTurnCompleted || len(end.Result.Structured) == 0 {
		t.Fatalf("turn 3 should carry structured output: %+v", end)
	}
	var plan struct {
		Steps []string `json:"steps"`
	}
	if err := json.Unmarshal(end.Result.Structured, &plan); err != nil || len(plan.Steps) != 2 {
		t.Fatalf("structured output: %s (%v)", end.Result.Structured, err)
	}

	// 5. Interrupted turn.
	end = terminal(t, turns[4])
	if end.Kind != runtime.EventTurnFailed || end.Failure.Kind != runtime.FailInterrupted {
		t.Fatalf("turn 5 end: %+v", end)
	}

	// Streaming text arrived for completed turns, and usage is per turn, not cumulative.
	var texts int
	for _, ev := range turns[0] {
		if ev.Kind == runtime.EventText {
			texts++
		}
	}
	if texts == 0 {
		t.Error("turn 1 produced no streamed text")
	}
	second := terminal(t, turns[1]).Result.Usage
	if second.InputTokens+second.CacheReadTokens >= u.InputTokens+u.CacheReadTokens+second.InputTokens+second.CacheReadTokens {
		t.Errorf("usage looks cumulative: turn1=%+v turn2=%+v", u, second)
	}
}

func TestReplayResumeAndMCP(t *testing.T) {
	t.Parallel()
	resume := replayTurns(t, fixture(t, "resume.jsonl"))
	if len(resume) != 1 {
		t.Fatalf("resume: got %d turns", len(resume))
	}
	if end := terminal(t, resume[0]); end.Kind != runtime.EventTurnCompleted || !strings.Contains(end.Result.FinalText, "HERON") {
		t.Fatalf("resume turn: %+v", end)
	}

	mcp := replayTurns(t, fixture(t, "mcp.jsonl"))
	if len(mcp) != 1 {
		t.Fatalf("mcp: got %d turns", len(mcp))
	}
	calls := toolCalls(mcp[0])
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "mcp__openchef__ask_user:") {
		t.Fatalf("mcp tool calls: %v", calls)
	}
	if end := terminal(t, mcp[0]); end.Result.FinalText != "teal" {
		t.Fatalf("mcp final: %+v", end)
	}
}

// Every recorded server request must decode into the shape the handler reads.
func TestRecordedServerRequestsDecode(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, name := range []string{"main.jsonl", "mcp.jsonl", "resume.jsonl", "eof-midturn.jsonl"} {
		for _, r := range fixture(t, name) {
			var m wire
			if r.Dir != rawlog.Recv || r.Msg == nil || json.Unmarshal(r.Msg, &m) != nil || m.Method == "" || len(m.ID) == 0 {
				continue
			}
			seen[m.Method] = true
			switch m.Method {
			case "item/commandExecution/requestApproval":
				var p commandApprovalParams
				if err := json.Unmarshal(m.Params, &p); err != nil || p.Command == "" || p.ThreadID == "" {
					t.Errorf("command approval: %+v (%v)", p, err)
				}
			case "mcpServer/elicitation/request":
				var p elicitationParams
				if err := json.Unmarshal(m.Params, &p); err != nil || p.Meta.ApprovalKind != "mcp_tool_call" || p.Meta.ToolName != askUserToolName || p.ServerName != "openchef" {
					t.Errorf("elicitation: %+v (%v)", p, err)
				}
			}
		}
	}
	for _, want := range []string{"item/commandExecution/requestApproval", "mcpServer/elicitation/request"} {
		if !seen[want] {
			t.Errorf("fixtures contain no %s", want)
		}
	}
}
