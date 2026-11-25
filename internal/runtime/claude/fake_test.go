package claude

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The test binary doubles as a scripted `claude -p` in stream-json mode. It mirrors the
// behaviour recorded in testdata/claude and m1-findings §9–§10:
//   - --resume fails without a transcript ("No conversation found"); --session-id fails
//     when one exists. A transcript is created when the first message is processed.
//   - one result per user message; result_index counts per process from 0.
//   - interrupt answers {"still_queued":[]} and ends the running turn with
//     error_during_execution / aborted_streaming.
//
// The user message text selects the turn's behaviour (see fakeTurn). Environment:
// OPENCHEF_FAKE_CLAUDE enables the fake ("1", or "permidle" to send a permission
// request outside any turn); OPENCHEF_FAKE_LOG receives argv and every stdin line;
// OPENCHEF_FAKE_STATE is the transcript directory.
const (
	fakeEnv      = "OPENCHEF_FAKE_CLAUDE"
	fakeLogEnv   = "OPENCHEF_FAKE_LOG"
	fakeStateEnv = "OPENCHEF_FAKE_STATE"
)

func TestMain(m *testing.M) {
	if scenario := os.Getenv(fakeEnv); scenario != "" {
		os.Exit(runFake(scenario))
	}
	interruptWait = 1500 * time.Millisecond
	closeWait = 1 * time.Second
	os.Exit(m.Run())
}

type fake struct {
	scenario   string
	schema     bool
	transcript string

	outMu sync.Mutex
	out   *json.Encoder
	logMu sync.Mutex
	log   *os.File

	mu        sync.Mutex
	index     int         // next result_index
	running   string      // behaviour of the turn in progress
	pending   int         // user messages received but not started
	interrupt chan string // delivers the interrupt request id to the turn
	replies   map[string]chan json.RawMessage
	queue     chan string
}

func runFake(scenario string) int {
	f := &fake{
		scenario: scenario, out: json.NewEncoder(os.Stdout),
		replies: map[string]chan json.RawMessage{}, queue: make(chan string, 16),
	}
	if p := os.Getenv(fakeLogEnv); p != "" {
		if lf, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
			f.log = lf
			defer func() { _ = lf.Close() }() // fake log; best effort
		}
	}
	args := os.Args[1:]
	f.record(map[string]any{"pid": os.Getpid(), "argv": args})
	id, resume := "", false
	if i := slices.Index(args, "--resume"); i >= 0 && i+1 < len(args) {
		id, resume = args[i+1], true
	}
	if i := slices.Index(args, "--session-id"); i >= 0 && i+1 < len(args) {
		id = args[i+1]
	}
	f.schema = slices.Contains(args, "--json-schema")
	f.transcript = filepath.Join(os.Getenv(fakeStateEnv), id)
	_, statErr := os.Stat(f.transcript)
	switch {
	case resume && statErr != nil:
		fmt.Fprintf(os.Stderr, "No conversation found with session ID: %s\n", id)
		return 1
	case !resume && statErr == nil:
		fmt.Fprintf(os.Stderr, "Error: Session ID %s is already in use.\n", id)
		return 1
	}

	go f.turns()
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		f.record(map[string]any{"pid": os.Getpid(), "in": json.RawMessage(line)})
		var m struct {
			Type      string          `json:"type"`
			RequestID string          `json:"request_id"`
			Request   json.RawMessage `json:"request"`
			Response  struct {
				RequestID string          `json:"request_id"`
				Response  json.RawMessage `json:"response"`
			} `json:"response"`
			Message struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &m) != nil {
			continue
		}
		switch m.Type {
		case "control_request":
			var req struct {
				Subtype string `json:"subtype"`
			}
			_ = json.Unmarshal(m.Request, &req)
			switch req.Subtype {
			case "initialize":
				f.controlOK(m.RequestID, map[string]any{"commands": []any{}})
				if f.scenario == "permidle" {
					go f.permission("idle-1", "Bash", map[string]any{"command": "ls"})
				}
			case "interrupt":
				f.onInterrupt(m.RequestID)
			default:
				f.send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": m.RequestID, "error": "unknown"}})
			}
		case "control_response":
			f.mu.Lock()
			ch := f.replies[m.Response.RequestID]
			f.mu.Unlock()
			if ch != nil {
				ch <- m.Response.Response
			}
		case "user":
			text := ""
			if len(m.Message.Content) > 0 {
				text = m.Message.Content[0].Text
			}
			f.mu.Lock()
			f.pending++
			f.mu.Unlock()
			f.queue <- text
		}
	}
	// EOF: like Claude, finish what is running, then exit.
	close(f.queue)
	for {
		f.mu.Lock()
		busy := f.running != ""
		f.mu.Unlock()
		if !busy {
			return 0
		}
		time.Sleep(10 * time.Millisecond)
	}
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

func (f *fake) controlOK(id string, resp any) {
	f.send(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": id, "response": resp}})
}

func (f *fake) onInterrupt(id string) {
	f.mu.Lock()
	running, ch, pending := f.running, f.interrupt, f.pending
	f.mu.Unlock()
	switch {
	case running == "queued" || (running == "" && pending > 0):
		f.controlOK(id, map[string]any{"still_queued": []string{"msg-1"}})
	case running == "":
		f.controlOK(id, map[string]any{"still_queued": []string{}})
	default:
		f.controlOK(id, map[string]any{"still_queued": []string{}})
		select {
		case ch <- id:
		default:
		}
	}
}

// permission sends can_use_tool and waits for the host's decision.
func (f *fake) permission(id, tool string, input map[string]any) json.RawMessage {
	ch := make(chan json.RawMessage, 1)
	f.mu.Lock()
	f.replies[id] = ch
	f.mu.Unlock()
	f.send(map[string]any{"type": "control_request", "request_id": id, "request": map[string]any{
		"subtype": "can_use_tool", "tool_name": tool, "input": input, "tool_use_id": "tu-" + id,
		"description": "fake request", "blocked_path": "/work/x.txt",
	}})
	select {
	case r := <-ch:
		return r
	case <-time.After(20 * time.Second):
		return nil
	}
}

func (f *fake) turns() {
	for text := range f.queue {
		f.mu.Lock()
		f.running = strings.SplitN(text, ":", 2)[0]
		f.pending--
		f.interrupt = make(chan string, 1)
		f.mu.Unlock()
		if !strings.HasPrefix(text, "queued") { // a still-queued message never started
			if tf, err := os.OpenFile(f.transcript, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600); err == nil {
				_ = tf.Close() // an empty transcript: the session now exists
			}
		}
		f.fakeTurn(text)
		f.mu.Lock()
		f.running = ""
		f.mu.Unlock()
	}
}

func (f *fake) result(subtype string, isError bool, text string, extra map[string]any) {
	f.mu.Lock()
	idx := f.index
	f.index++
	f.mu.Unlock()
	r := map[string]any{
		"type": "result", "subtype": subtype, "is_error": isError, "result": text,
		"result_index": idx, "duration_ms": 42, "total_cost_usd": 0.001,
		"permission_denials": []any{}, "modelUsage": map[string]any{"claude-haiku-5-5": map[string]any{}},
		"usage": map[string]any{"input_tokens": 100, "output_tokens": 10, "cache_read_input_tokens": 40, "cache_creation_input_tokens": 5},
	}
	for k, v := range extra {
		r[k] = v
	}
	f.send(r)
}

func (f *fake) rateLimit(status string, util float64) {
	reset := time.Now().Add(time.Hour).Unix()
	f.send(map[string]any{"type": "rate_limit_event", "rate_limit_info": map[string]any{
		"status": status, "overageStatus": "rejected", "unifiedWindows": map[string]any{
			"five_hour": map[string]any{"status": status, "utilization": util, "resetsAt": reset},
		},
	}})
}

func (f *fake) waitInterrupt() {
	f.mu.Lock()
	ch := f.interrupt
	f.mu.Unlock()
	<-ch
}

// working streams a marker so tests know the turn has started before they interrupt.
func (f *fake) working() {
	f.send(map[string]any{"type": "stream_event", "parent_tool_use_id": nil, "event": map[string]any{
		"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": "working"},
	}})
}

func (f *fake) aborted() {
	f.result("error_during_execution", true, "", map[string]any{"terminal_reason": "aborted_streaming"})
}

func (f *fake) fakeTurn(text string) {
	kind, arg, _ := strings.Cut(text, ":")
	switch kind {
	case "ok", "remember":
		if kind == "remember" {
			if err := os.WriteFile(f.transcript, []byte(arg), 0o600); err != nil {
				fmt.Fprintln(os.Stderr, "fake:", err)
			}
		}
		f.rateLimit("allowed", 0.25)
		for _, d := range []string{"Hel", "lo"} {
			f.send(map[string]any{"type": "stream_event", "parent_tool_use_id": nil, "event": map[string]any{
				"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": d},
			}})
		}
		use := map[string]any{"type": "tool_use", "id": "t1", "name": "Read", "input": map[string]any{"file_path": "/work/a"}}
		f.send(map[string]any{"type": "assistant", "parent_tool_use_id": nil, "message": map[string]any{"content": []any{use}}})
		f.send(map[string]any{"type": "assistant", "parent_tool_use_id": nil, "message": map[string]any{"content": []any{use}}}) // repeated, as with partial messages
		f.send(map[string]any{"type": "user", "parent_tool_use_id": nil, "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "t1", "content": "file body"},
		}}})
		// A subagent's message must be ignored.
		sub := "t1"
		f.send(map[string]any{"type": "assistant", "parent_tool_use_id": sub, "message": map[string]any{"content": []any{
			map[string]any{"type": "tool_use", "id": "sub1", "name": "Bash", "input": map[string]any{}},
		}}})
		if f.schema {
			f.send(map[string]any{"type": "assistant", "parent_tool_use_id": nil, "message": map[string]any{"content": []any{
				map[string]any{"type": "tool_use", "id": "so1", "name": structuredOutputTool, "input": map[string]any{"ok": true}},
			}}})
			f.send(map[string]any{"type": "user", "parent_tool_use_id": nil, "message": map[string]any{"content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": "so1", "content": "ok"},
			}}})
			f.result("success", false, `{"ok":true}`, map[string]any{"structured_output": map[string]any{"ok": true}})
			return
		}
		f.result("success", false, "Hello", nil)
	case "recall":
		raw, _ := os.ReadFile(f.transcript)
		f.result("success", false, "recalled "+string(raw), nil)
	case "perm", "askuser":
		tool, input := "Bash", map[string]any{"command": "rm -rf build"}
		if kind == "askuser" {
			tool, input = "mcp__openchef__ask_user", map[string]any{"question": "colour?"}
		}
		resp := f.permission("perm-1", tool, input)
		var d struct {
			Behavior string `json:"behavior"`
		}
		_ = json.Unmarshal(resp, &d)
		denials := []any{}
		if d.Behavior != "allow" {
			denials = append(denials, map[string]any{"tool_name": tool})
		}
		f.result("success", false, "decided "+d.Behavior, map[string]any{"permission_denials": denials})
	case "cancelperm":
		go f.permission("perm-2", "Bash", map[string]any{"command": "sleep 100"})
		time.Sleep(200 * time.Millisecond)
		f.send(map[string]any{"type": "control_cancel_request", "request_id": "perm-2"})
		time.Sleep(200 * time.Millisecond)
		f.result("success", false, "cancelled", nil)
	case "hang":
		f.working()
		f.waitInterrupt()
		f.aborted()
	case "stubborn":
		f.working()
		f.waitInterrupt()
		select {} // never answers: the adapter must kill the process
	case "queued":
		select {} // the interrupt reports the message as still queued
	case "crash":
		fmt.Fprintln(os.Stderr, "boom")
		os.Exit(3)
	case "stale":
		f.send(map[string]any{"type": "result", "subtype": "success", "is_error": false, "result": "STALE", "result_index": 99})
		f.result("success", false, "Hello", nil)
	case "exhausted":
		f.rateLimit("rejected", 1)
		f.result("success", true, "Claude AI usage limit reached", map[string]any{"api_error_status": 429})
	case "flood":
		for range 3000 {
			f.send(map[string]any{"type": "stream_event", "parent_tool_use_id": nil, "event": map[string]any{
				"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": "x"},
			}})
		}
		f.waitInterrupt()
		f.aborted()
	default:
		f.result("success", false, "unknown command "+text, nil)
	}
}
