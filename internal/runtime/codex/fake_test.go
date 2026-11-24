package codex

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"
)

// The test binary doubles as a scripted `codex app-server`. OPENCHEF_FAKE_CODEX picks
// the scenario; OPENCHEF_FAKE_LOG receives every request the fake gets, for assertions.
const (
	fakeEnv    = "OPENCHEF_FAKE_CODEX"
	fakeLogEnv = "OPENCHEF_FAKE_LOG"
)

func TestMain(m *testing.M) {
	if scenario := os.Getenv(fakeEnv); scenario != "" {
		os.Exit(runFake(scenario))
	}
	os.Exit(m.Run())
}

type fake struct {
	scenario string
	out      *json.Encoder
	outMu    sync.Mutex
	log      *os.File
	replies  map[string]chan json.RawMessage
	repMu    sync.Mutex
	turns    int
	nextReq  int
	thread   string
}

func runFake(scenario string) int {
	f := &fake{scenario: scenario, out: json.NewEncoder(os.Stdout), replies: map[string]chan json.RawMessage{}, thread: "thr-1"}
	if p := os.Getenv(fakeLogEnv); p != "" {
		lf, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err == nil {
			f.log = lf
			defer func() { _ = lf.Close() }() // fake server log; best effort
		}
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		var m wire
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			continue
		}
		if f.log != nil {
			_, _ = f.log.Write(append(append([]byte(nil), sc.Bytes()...), '\n'))
		}
		if m.Method == "" { // response to one of our requests
			f.repMu.Lock()
			ch := f.replies[string(m.ID)]
			f.repMu.Unlock()
			if ch != nil {
				var full struct {
					Result json.RawMessage `json:"result"`
				}
				_ = json.Unmarshal(sc.Bytes(), &full)
				ch <- full.Result
			}
			continue
		}
		if len(m.ID) == 0 {
			continue // notification (initialized)
		}
		go f.handle(m)
	}
	return 0
}

func (f *fake) send(v any) {
	f.outMu.Lock()
	defer f.outMu.Unlock()
	_ = f.out.Encode(v)
}

func (f *fake) reply(id json.RawMessage, result any) {
	f.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (f *fake) notify(method string, params any) {
	f.send(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

// request sends a server-initiated request and waits for the client's result.
func (f *fake) request(method string, params any) json.RawMessage {
	f.repMu.Lock()
	f.nextReq++
	id := strconv.Itoa(900 + f.nextReq)
	ch := make(chan json.RawMessage, 1)
	f.replies[id] = ch
	f.repMu.Unlock()
	f.send(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "method": method, "params": params})
	select {
	case r := <-ch:
		return r
	case <-time.After(10 * time.Second):
		return nil
	}
}

func (f *fake) handle(m wire) {
	switch m.Method {
	case "initialize":
		f.reply(m.ID, map[string]any{"userAgent": "fake"})
	case "model/list":
		f.reply(m.ID, map[string]any{"data": []any{map[string]any{
			"id": "gpt-test", "model": "gpt-test", "isDefault": true,
			"supportedReasoningEfforts": []any{
				map[string]any{"reasoningEffort": "low"}, map[string]any{"reasoningEffort": "medium"},
				map[string]any{"reasoningEffort": "high"}, map[string]any{"reasoningEffort": "xhigh"},
			},
		}}})
	case "account/rateLimits/read":
		reset := time.Now().Add(time.Hour).Unix()
		f.reply(m.ID, map[string]any{"rateLimits": map[string]any{
			"primary":   map[string]any{"usedPercent": 10, "windowDurationMins": 300, "resetsAt": reset},
			"secondary": map[string]any{"usedPercent": 50, "windowDurationMins": 10080, "resetsAt": reset + 86400},
		}})
	case "thread/start":
		f.reply(m.ID, map[string]any{"thread": map[string]any{"id": f.thread}, "model": "gpt-test"})
	case "thread/resume":
		var p struct {
			ThreadID string `json:"threadId"`
		}
		_ = json.Unmarshal(m.Params, &p)
		f.thread = p.ThreadID
		f.reply(m.ID, map[string]any{"thread": map[string]any{"id": p.ThreadID}})
	case "turn/interrupt":
		f.reply(m.ID, map[string]any{})
		var p struct {
			TurnID string `json:"turnId"`
		}
		_ = json.Unmarshal(m.Params, &p)
		f.notify("turn/completed", map[string]any{"threadId": f.thread, "turn": map[string]any{"id": p.TurnID, "status": "interrupted"}})
	case "turn/start":
		f.turns++
		turn := fmt.Sprintf("turn-%d", f.turns)
		if f.scenario == "stale" {
			// Leftovers of a previous turn, sent before the new turn's id is known (seen
			// live right after thread/resume).
			f.notify("thread/tokenUsage/updated", map[string]any{"threadId": f.thread, "turnId": "old-turn", "tokenUsage": map[string]any{
				"last":  map[string]any{"inputTokens": 1, "cachedInputTokens": 0, "outputTokens": 1, "reasoningOutputTokens": 0, "totalTokens": 2},
				"total": map[string]any{"inputTokens": 5000, "cachedInputTokens": 0, "outputTokens": 500, "reasoningOutputTokens": 0, "totalTokens": 5500},
			}})
			f.notify("turn/completed", map[string]any{"threadId": f.thread, "turn": map[string]any{"id": "old-turn", "status": "failed", "error": map[string]any{"message": "stale"}}})
		}
		f.reply(m.ID, map[string]any{"turn": map[string]any{"id": turn, "status": "inProgress"}})
		if f.scenario == "fastfail" {
			f.notify("turn/completed", map[string]any{"threadId": f.thread, "turn": map[string]any{
				"id": turn, "status": "failed", "error": map[string]any{"message": "denied", "codexErrorInfo": "unauthorized"},
			}})
			return
		}
		f.runTurn(turn, m.Params)
	default:
		f.send(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "unknown " + m.Method}})
	}
}

func (f *fake) item(turn, typ string, item map[string]any) {
	item["type"] = typ
	f.notify("item/completed", map[string]any{"threadId": f.thread, "turnId": turn, "item": item, "completedAtMs": 1})
}

func (f *fake) complete(turn, final string) {
	f.notify("thread/tokenUsage/updated", map[string]any{"threadId": f.thread, "turnId": turn, "tokenUsage": map[string]any{
		"last":  map[string]any{"inputTokens": 100, "cachedInputTokens": 40, "outputTokens": 10, "reasoningOutputTokens": 0, "totalTokens": 110},
		"total": map[string]any{"inputTokens": 1100, "cachedInputTokens": 440, "outputTokens": 110, "reasoningOutputTokens": 0, "totalTokens": 1210},
	}})
	f.item(turn, "agentMessage", map[string]any{"id": "msg", "text": final, "phase": "final_answer"})
	f.notify("turn/completed", map[string]any{"threadId": f.thread, "turn": map[string]any{"id": turn, "status": "completed", "durationMs": 42}})
}

func (f *fake) runTurn(turn string, params json.RawMessage) {
	f.notify("turn/started", map[string]any{"threadId": f.thread, "turn": map[string]any{"id": turn}})
	switch f.scenario {
	case "ok", "stale":
		var p struct {
			OutputSchema json.RawMessage `json:"outputSchema"`
		}
		_ = json.Unmarshal(params, &p)
		f.notify("item/agentMessage/delta", map[string]any{"threadId": f.thread, "turnId": turn, "itemId": "msg", "delta": "Hel"})
		f.notify("item/agentMessage/delta", map[string]any{"threadId": f.thread, "turnId": turn, "itemId": "msg", "delta": "lo"})
		f.notify("account/rateLimits/updated", map[string]any{"rateLimits": map[string]any{
			"primary": map[string]any{"usedPercent": 12, "windowDurationMins": 300},
		}})
		final := "Hello"
		if len(p.OutputSchema) > 0 {
			final = `{"ok":true}`
		}
		f.complete(turn, final)
	case "approve":
		f.notify("item/started", map[string]any{"threadId": f.thread, "turnId": turn, "item": map[string]any{
			"id": "cmd1", "type": "commandExecution", "command": "rm -rf build", "cwd": "/w", "status": "inProgress", "commandActions": []any{},
		}})
		res := f.request("item/commandExecution/requestApproval", map[string]any{
			"threadId": f.thread, "turnId": turn, "itemId": "cmd1", "startedAtMs": 1, "command": "rm -rf build", "cwd": "/w", "reason": "cleanup",
		})
		var d struct {
			Decision string `json:"decision"`
		}
		_ = json.Unmarshal(res, &d)
		status := "completed"
		if d.Decision != "accept" && d.Decision != "acceptForSession" {
			status = "declined"
		}
		f.item(turn, "commandExecution", map[string]any{"id": "cmd1", "command": "rm -rf build", "cwd": "/w", "status": status, "commandActions": []any{}})
		f.complete(turn, "decision:"+d.Decision)
	case "ask":
		el := f.request("mcpServer/elicitation/request", map[string]any{
			"threadId": f.thread, "serverName": "openchef", "message": "Allow?", "mode": "form",
			"_meta": map[string]any{"codex_approval_kind": "mcp_tool_call", "tool_name": "ask_user"},
		})
		var a struct {
			Action string `json:"action"`
		}
		_ = json.Unmarshal(el, &a)
		ui := f.request("item/tool/requestUserInput", map[string]any{
			"threadId": f.thread, "turnId": turn, "itemId": "q", "isBlocking": true,
			"questions": []any{map[string]any{"id": "q1", "header": "Colour", "question": "Which colour?"}},
		})
		var ans struct {
			Answers map[string]struct {
				Answers []string `json:"answers"`
			} `json:"answers"`
		}
		_ = json.Unmarshal(ui, &ans)
		got := ""
		if q, ok := ans.Answers["q1"]; ok && len(q.Answers) > 0 {
			got = q.Answers[0]
		}
		f.complete(turn, a.Action+":"+got)
	case "quota":
		reset := time.Now().Add(2 * time.Hour).Unix()
		f.notify("account/rateLimits/updated", map[string]any{"rateLimits": map[string]any{
			"primary": map[string]any{"usedPercent": 100, "windowDurationMins": 300, "resetsAt": reset},
		}})
		f.notify("turn/completed", map[string]any{"threadId": f.thread, "turn": map[string]any{
			"id": turn, "status": "failed",
			"error": map[string]any{"message": "You've hit your usage limit", "codexErrorInfo": "usageLimitExceeded"},
		}})
	case "crash":
		os.Exit(3)
	case "hang":
		// Wait for turn/interrupt (handled above).
	case "flood":
		for i := range 2000 {
			f.notify("item/agentMessage/delta", map[string]any{"threadId": f.thread, "turnId": turn, "itemId": "msg", "delta": strconv.Itoa(i)})
		}
	}
}
