package agy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// The test binary doubles as a scripted `agy acp`, mirroring testdata/agy and
// m1-findings §11: session/load replays history before it answers, session/cancel ends
// the prompt with "cancelled", thinking levels depend on the model, permission options
// carry kinds. The prompt's first word selects the behaviour (see prompt).
//
// OPENCHEF_FAKE_AGY enables the fake ("1", or "permidle" to send a permission request
// outside any turn); OPENCHEF_FAKE_LOG receives argv and every stdin line;
// OPENCHEF_FAKE_STATE holds one file per session (its "transcript").
const (
	fakeEnv      = "OPENCHEF_FAKE_AGY"
	fakeLogEnv   = "OPENCHEF_FAKE_LOG"
	fakeStateEnv = "OPENCHEF_FAKE_STATE"
)

func TestMain(m *testing.M) {
	if scenario := os.Getenv(fakeEnv); scenario != "" {
		os.Exit(runFake(scenario))
	}
	interruptWait = 1500 * time.Millisecond
	closeWait = time.Second
	os.Exit(m.Run())
}

type fake struct {
	scenario string
	state    string

	outMu sync.Mutex
	out   *json.Encoder
	logMu sync.Mutex
	log   *os.File

	mu       sync.Mutex
	session  string
	model    string
	next     int
	replies  map[string]chan json.RawMessage
	cancel   chan struct{} // closed by session/cancel for the prompt in progress
	lastKind string
}

func runFake(scenario string) int {
	f := &fake{scenario: scenario, state: os.Getenv(fakeStateEnv), out: json.NewEncoder(os.Stdout), replies: map[string]chan json.RawMessage{}, model: "gemini-big"}
	if p := os.Getenv(fakeLogEnv); p != "" {
		if lf, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			f.log = lf
			defer func() { _ = lf.Close() }() // fake log; best effort
		}
	}
	f.record(map[string]any{"argv": os.Args[1:]})
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		f.record(map[string]any{"in": json.RawMessage(line)})
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		switch {
		case m.Method == "" && len(m.ID) > 0: // answer to our request
			f.mu.Lock()
			ch := f.replies[string(m.ID)]
			f.mu.Unlock()
			if ch != nil {
				ch <- m.Result
			}
		case m.Method == "session/cancel":
			// The channel stays in place once closed: the prompt goroutine may reach
			// waitCancel only after the cancel arrived (it did under stress load).
			f.mu.Lock()
			if f.cancel != nil {
				select {
				case <-f.cancel:
				default:
					close(f.cancel)
				}
			}
			f.mu.Unlock()
		case len(m.ID) > 0:
			if m.Method == "session/prompt" {
				go f.prompt(m.ID, m.Params)
			} else {
				f.handle(m.ID, m.Method, m.Params)
			}
		}
	}
	return 0
}

func (f *fake) record(v any) {
	if f.log == nil {
		return
	}
	raw, _ := json.Marshal(v)
	f.logMu.Lock()
	defer f.logMu.Unlock()
	_, _ = f.log.Write(append(raw, '\n'))
}

func (f *fake) send(v any) {
	f.outMu.Lock()
	defer f.outMu.Unlock()
	_ = f.out.Encode(v)
}

func (f *fake) reply(id json.RawMessage, result any) {
	f.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (f *fake) fail(id json.RawMessage, msg string) {
	f.send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32603, "message": msg}})
}

func (f *fake) update(u map[string]any) {
	f.mu.Lock()
	sid := f.session
	f.mu.Unlock()
	f.send(map[string]any{"jsonrpc": "2.0", "method": "session/update", "params": map[string]any{"sessionId": sid, "update": u}})
}

func (f *fake) chunk(text string) {
	f.update(map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": text}})
}

func (f *fake) configOptions() []any {
	f.mu.Lock()
	model := f.model
	f.mu.Unlock()
	levels := []string{"low", "medium", "high", "xhigh"}
	if model == "gemini-small" {
		levels = []string{"low", "medium"}
	}
	opt := func(id, cur string, values ...string) map[string]any {
		vs := make([]any, 0, len(values))
		for _, v := range values {
			vs = append(vs, map[string]any{"name": v, "value": v})
		}
		return map[string]any{"id": id, "category": id, "type": "select", "currentValue": cur, "options": vs}
	}
	return []any{
		opt("model", model, "gemini-big", "gemini-small"),
		opt("thought_level", "high", levels...),
		opt("mode", "default", "default", "accept-edits", "auto", "plan"),
	}
}

func (f *fake) transcript(id string) string { return filepath.Join(f.state, id) }

func (f *fake) handle(id json.RawMessage, method string, params json.RawMessage) {
	var p struct {
		SessionID string `json:"sessionId"`
		ConfigID  string `json:"configId"`
		Value     string `json:"value"`
	}
	_ = json.Unmarshal(params, &p)
	switch method {
	case "initialize":
		f.reply(id, map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"loadSession": true}})
	case "session/new":
		f.mu.Lock()
		f.session = "sess-" + strconv.Itoa(os.Getpid())
		sid := f.session
		f.mu.Unlock()
		if err := os.WriteFile(f.transcript(sid), nil, 0o600); err != nil {
			f.fail(id, err.Error())
			return
		}
		f.reply(id, map[string]any{"sessionId": sid, "configOptions": f.configOptions()})
		if f.scenario == "permidle" {
			go f.permission("run_command", "execute", map[string]any{"CommandLine": "ls"}, nil)
		}
	case "session/load":
		if _, err := os.Stat(f.transcript(p.SessionID)); err != nil {
			f.fail(id, "session not found: "+p.SessionID)
			return
		}
		f.mu.Lock()
		f.session = p.SessionID
		f.mu.Unlock()
		// Like agy: the history is replayed before the response.
		f.update(map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]any{"type": "text", "text": "OLD QUESTION"}})
		f.chunk("OLD ANSWER")
		f.reply(id, map[string]any{"configOptions": f.configOptions()})
	case "session/set_config_option":
		if p.ConfigID == "model" {
			if p.Value != "gemini-big" && p.Value != "gemini-small" {
				f.fail(id, "unknown model "+p.Value)
				return
			}
			f.mu.Lock()
			f.model = p.Value
			f.mu.Unlock()
		}
		f.reply(id, map[string]any{"configOptions": f.configOptions()})
	default:
		f.send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": -32601, "message": "unknown " + method}})
	}
}

// permission asks the client and returns its answer.
func (f *fake) permission(name, kind string, input map[string]any, locations []any) json.RawMessage {
	f.mu.Lock()
	f.next++
	id := strconv.Itoa(900 + f.next)
	ch := make(chan json.RawMessage, 1)
	f.replies[id] = ch
	sid := f.session
	f.mu.Unlock()
	tc := map[string]any{"toolCallId": "perm-" + id, "name": name, "kind": kind, "title": name, "rawInput": input, "status": "pending"}
	if locations != nil {
		tc["locations"] = locations
	}
	f.send(map[string]any{"jsonrpc": "2.0", "id": json.RawMessage(id), "method": "session/request_permission", "params": map[string]any{
		"sessionId": sid, "toolCall": tc,
		"options": []any{map[string]any{"optionId": "allow", "name": "Allow", "kind": "allow_once"}, map[string]any{"optionId": "deny", "name": "Deny", "kind": "reject_once"}},
	}})
	select {
	case r := <-ch:
		return r
	case <-time.After(20 * time.Second):
		return nil
	}
}

func outcome(r json.RawMessage) string {
	var o struct {
		Outcome struct {
			Outcome  string `json:"outcome"`
			OptionID string `json:"optionId"`
		} `json:"outcome"`
	}
	_ = json.Unmarshal(r, &o)
	if o.Outcome.Outcome == "cancelled" {
		return "cancelled"
	}
	return o.Outcome.OptionID
}

func (f *fake) done(id json.RawMessage, stop string) {
	f.reply(id, map[string]any{"stopReason": stop, "usage": map[string]any{
		"inputTokens": 100, "outputTokens": 10, "cachedReadTokens": 40, "cachedWriteTokens": 0, "thoughtTokens": 5, "totalTokens": 155,
	}})
}

func (f *fake) waitCancel() {
	f.mu.Lock()
	ch := f.cancel
	f.mu.Unlock()
	<-ch
}

func (f *fake) prompt(id, params json.RawMessage) {
	var p struct {
		Prompt []struct {
			Text string `json:"text"`
		} `json:"prompt"`
	}
	_ = json.Unmarshal(params, &p)
	text := ""
	if len(p.Prompt) > 0 {
		text = p.Prompt[0].Text
	}
	kind, arg := text, ""
	if i := strings.IndexAny(text, ":\n "); i >= 0 {
		kind, arg = text[:i], text[i+1:]
		if j := strings.IndexByte(arg, '\n'); j >= 0 {
			arg = arg[:j]
		}
	}
	f.mu.Lock()
	f.cancel = make(chan struct{})
	repair := strings.HasPrefix(text, "Your previous answer")
	if !repair {
		f.lastKind = kind
	}
	last, sid := f.lastKind, f.session
	f.mu.Unlock()

	if repair {
		if last == "neverjson" {
			f.chunk("still prose")
		} else {
			f.chunk(`{"steps":["fixed"]}`)
		}
		f.done(id, "end_turn")
		return
	}
	switch kind {
	case "ok", "remember":
		if kind == "remember" {
			if err := os.WriteFile(f.transcript(sid), []byte(arg), 0o600); err != nil {
				f.fail(id, err.Error())
				return
			}
		}
		f.chunk("Let me look. ")
		input := map[string]any{"CommandLine": "cat a.txt", "toolSummary": "Read a.txt", "WaitMsBeforeAsync": 5000}
		f.update(map[string]any{"sessionUpdate": "tool_call", "toolCallId": "c1", "name": "run_command", "kind": "execute", "title": "cat a.txt", "status": "in_progress", "rawInput": input})
		f.update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "c1", "name": "run_command", "kind": "execute", "status": "pending", "rawInput": input})
		f.update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "c1", "name": "run_command", "kind": "execute", "status": "completed", "rawInput": input,
			"rawOutput": map[string]any{"exit_code": 0, "output": "file body"}})
		f.update(map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "c1", "status": "completed"}) // repeated
		f.update(map[string]any{"sessionUpdate": "usage_update", "used": 155, "size": 1048576})
		f.chunk("Hel")
		f.chunk("lo")
		f.done(id, "end_turn")
	case "recall":
		raw, _ := os.ReadFile(f.transcript(sid))
		f.chunk("recalled " + string(raw))
		f.done(id, "end_turn")
	case "burst": // many updates immediately followed by the response (Barrier)
		for i := range 500 {
			f.chunk(strconv.Itoa(i % 10))
		}
		f.done(id, "end_turn")
	case "perm", "edit", "askuser":
		name, tk, input := "run_command", "execute", map[string]any{"CommandLine": "go build ./...", "BypassSandbox": true, "toolSummary": "Build"}
		var locs []any
		switch kind {
		case "edit":
			name, tk, input = "write_file", "edit", map[string]any{"Path": "/w/a.go", "Content": "x"}
			locs = []any{map[string]any{"path": "/w/a.go"}}
		case "askuser":
			name, tk, input = "mcp_openchef_ask_user", "other", map[string]any{"question": "colour?"}
		}
		ans := outcome(f.permission(name, tk, input, locs))
		f.chunk("decided " + ans)
		f.done(id, "end_turn")
	case "hangperm":
		ans := outcome(f.permission("run_command", "execute", map[string]any{"CommandLine": "sleep 100", "BypassSandbox": true}, nil))
		f.record(map[string]any{"permission_outcome": ans})
		f.done(id, "cancelled")
	case "hang":
		f.chunk("working")
		f.waitCancel()
		f.done(id, "cancelled")
	case "stubborn":
		f.chunk("working")
		select {} // ignores session/cancel: the adapter must kill the process
	case "crash":
		fmt.Fprintln(os.Stderr, "boom")
		os.Exit(3)
	case "quota":
		f.fail(id, "RESOURCE_EXHAUSTED: You have exhausted your quota")
	case "json":
		f.chunk("```json\n{\"steps\":[\"a\",\"b\"]}\n```")
		f.done(id, "end_turn")
	case "badjson", "neverjson":
		f.chunk("Sure, the plan is to do step a.")
		f.done(id, "end_turn")
	case "flood":
		for range 3000 {
			f.chunk("x")
		}
		f.waitCancel()
		f.done(id, "cancelled")
	case "refuse":
		f.done(id, "refusal")
	default:
		f.chunk("unknown " + text)
		f.done(id, "end_turn")
	}
}
