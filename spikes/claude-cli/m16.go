package main

// Spike M1.6 (follow-up after design review, docs/decisions/log.md D-006): verifies
// --restricted, --replay-user-messages, interrupt semantics and error results before the
// Claude adapter is written.

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ugurboz/openchef/spikes/internal/rpc"
)

type m16Result struct {
	Name     string            `json:"name"`
	Args     []string          `json:"args"`
	ExitCode int               `json:"exitCode"`
	Results  []json.RawMessage `json:"results"` // every `result` message (trimmed)
	Control  []json.RawMessage `json:"control"` // control_request/response from Claude
	Replays  int               `json:"replays"` // echoed user messages
	Types    map[string]int    `json:"types"`   // message type counts
	Files    map[string]bool   `json:"files"`   // files created
	Stderr   []string          `json:"stderr"`
	Notes    []string          `json:"notes"`
	Elapsed  string            `json:"elapsed"`
}

// m16Step is one action of a scripted session.
type m16Step struct {
	send      any           // message to write (nil = none)
	waitFor   string        // message type (or "result") to wait for before the next step
	afterType string        // wait until a message of this type arrived, then send
	delay     time.Duration // extra delay before sending
}

func userMsg(text string) map[string]any {
	return map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": text}}}}
}

func (s *spike) m16(name string, args []string, steps []m16Step, files []string) m16Result {
	res := m16Result{Name: name, Args: args, Types: map[string]int{}, Files: map[string]bool{}}
	t0 := time.Now()
	defer func() { res.Elapsed = time.Since(t0).Round(time.Millisecond).String() }()
	repo := filepath.Join(s.work, name, "repo")
	if err := rpc.NewGitRepo(repo); err != nil {
		res.Notes = append(res.Notes, err.Error())
		return res
	}
	tr, err := rpc.NewTranscript(filepath.Join(s.out, "claude-"+name+".jsonl"), s.work)
	if err != nil {
		res.Notes = append(res.Notes, err.Error())
		return res
	}
	defer tr.Close()
	tr.Note("argv", "claude "+strings.Join(args, " "))
	cmd := exec.Command("claude", args...)
	cmd.Dir = repo
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		res.Notes = append(res.Notes, err.Error())
		return res
	}
	var mu sync.Mutex
	seen := map[string]int{}
	cond := sync.NewCond(&mu)
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			tr.Note("stderr", sc.Text())
			mu.Lock()
			res.Stderr = append(res.Stderr, sc.Text())
			mu.Unlock()
		}
	}()
	var wmu sync.Mutex
	write := func(v any) {
		wmu.Lock()
		defer wmu.Unlock()
		b, _ := json.Marshal(v)
		tr.Line("send", b)
		_, _ = stdin.Write(append(b, '\n'))
	}
	readDone := make(chan struct{})
	go func() {
		defer close(readDone)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 1<<26)
		for sc.Scan() {
			line := append([]byte(nil), sc.Bytes()...)
			tr.Line("recv", line)
			var m map[string]json.RawMessage
			if json.Unmarshal(line, &m) != nil {
				continue
			}
			var typ string
			_ = json.Unmarshal(m["type"], &typ)
			mu.Lock()
			res.Types[typ]++
			seen[typ]++
			switch typ {
			case "result":
				trim := map[string]json.RawMessage{}
				for _, k := range []string{"subtype", "is_error", "result", "stop_reason", "terminal_reason", "api_error_status", "num_turns", "result_index", "queued_turn_count", "permission_denials", "modelUsage", "total_cost_usd"} {
					if v, ok := m[k]; ok {
						trim[k] = v
					}
				}
				raw, _ := json.Marshal(trim)
				res.Results = append(res.Results, raw)
			case "control_request", "control_response", "control_cancel_request":
				res.Control = append(res.Control, line)
			case "user":
				res.Replays++
			}
			cond.Broadcast()
			mu.Unlock()
			if typ == "control_request" {
				write(controlResponse(line))
			}
		}
	}()
	exited := make(chan struct{})
	go func() { <-readDone; _ = cmd.Wait(); close(exited) }()

	waitType := func(typ string, n int, d time.Duration) bool {
		deadline := time.Now().Add(d)
		mu.Lock()
		defer mu.Unlock()
		for seen[typ] < n {
			if time.Now().After(deadline) {
				return false
			}
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
		}
		return true
	}
	for i, st := range steps {
		if st.afterType != "" {
			mu.Lock()
			n := seen[st.afterType] + 1
			mu.Unlock()
			if !waitType(st.afterType, n, 3*time.Minute) {
				res.Notes = append(res.Notes, "step timeout waiting for "+st.afterType)
			}
		}
		time.Sleep(st.delay)
		if st.send != nil {
			write(st.send)
		}
		if st.waitFor != "" {
			mu.Lock()
			n := seen[st.waitFor] + 1
			mu.Unlock()
			if !waitType(st.waitFor, n, 3*time.Minute) {
				res.Notes = append(res.Notes, "timeout after step "+itoa(i)+" waiting for "+st.waitFor)
			}
		}
	}
	time.Sleep(1500 * time.Millisecond) // catch late messages (e.g. a second result)
	_ = stdin.Close()
	select {
	case <-exited:
	case <-time.After(60 * time.Second):
		res.Notes = append(res.Notes, "did not exit 60s after EOF; killed")
		_ = cmd.Process.Kill()
		<-exited
	}
	res.ExitCode = cmd.ProcessState.ExitCode()
	for _, f := range files {
		_, err := os.Stat(filepath.Join(repo, f))
		res.Files[f] = err == nil
	}
	rpc.Logf("%-22s exit=%d results=%d replays=%d control=%d files=%v notes=%v", name, res.ExitCode, len(res.Results), res.Replays, len(res.Control), res.Files, res.Notes)
	return res
}

func itoa(i int) string { return strconv.Itoa(i) }

func (s *spike) runM16() []m16Result {
	base := []string{
		"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--model", "haiku", "--effort", "low", "--strict-mcp-config",
		"--permission-prompt-tool", "stdio", "--disallowedTools", "AskUserQuestion",
	}
	init := map[string]any{"type": "control_request", "request_id": "init-1", "request": map[string]any{"subtype": "initialize"}}
	with := func(extra ...string) []string { return append(append([]string{}, base...), extra...) }
	riskyTask := "Do these in order, then reply DONE (skip any step that is refused):\n" +
		"1) Run: echo a > inside.txt\n" +
		"2) Using your file writing tool create .claude/settings.local.json containing {}\n" +
		"3) Using your file writing tool create the file ../outside.txt containing x\n" +
		"4) Run: echo b > ../outside2.txt"
	var out []m16Result

	// A. --restricted in manual mode with an explicit tool list: what reaches the host?
	out = append(out, s.m16("m16-restricted-manual",
		with("--restricted", "--permission-mode", "manual", "--tools", "Bash,Read,Edit,Write,Glob,Grep", "--replay-user-messages"),
		[]m16Step{{send: init, waitFor: "control_response"}, {send: userMsg(riskyTask), waitFor: "result"}},
		[]string{"inside.txt", ".claude/settings.local.json", "../outside.txt", "../outside2.txt"}))

	// B. --restricted in acceptEdits: are settings/outside writes still gated?
	out = append(out, s.m16("m16-restricted-acceptedits",
		with("--restricted", "--permission-mode", "acceptEdits", "--tools", "Bash,Read,Edit,Write,Glob,Grep"),
		[]m16Step{{send: init, waitFor: "control_response"}, {send: userMsg(riskyTask), waitFor: "result"}},
		[]string{"inside.txt", ".claude/settings.local.json", "../outside.txt", "../outside2.txt"}))

	// C. --restricted + bypassPermissions: refused at start?
	out = append(out, s.m16("m16-restricted-bypass",
		with("--restricted", "--permission-mode", "bypassPermissions"),
		[]m16Step{{send: init, waitFor: "control_response"}, {send: userMsg("Reply OK."), waitFor: "result"}}, nil))

	// D. Interrupt mid-turn, then a second turn. How many results, which subtypes?
	out = append(out, s.m16("m16-interrupt",
		with("--permission-mode", "bypassPermissions", "--include-partial-messages", "--replay-user-messages"),
		[]m16Step{
			{send: init, waitFor: "control_response"},
			{send: userMsg("Write a 600-word story about a lighthouse keeper. Do not use tools.")},
			{afterType: "stream_event", delay: 500 * time.Millisecond, send: map[string]any{"type": "control_request", "request_id": "int-1", "request": map[string]any{"subtype": "interrupt"}}, waitFor: "result"},
			{send: userMsg("Reply with the single word SECOND."), waitFor: "result"},
		}, nil))

	// E. Error result: budget exhausted.
	out = append(out, s.m16("m16-budget",
		with("--permission-mode", "bypassPermissions", "--max-budget-usd", "0.000001"),
		[]m16Step{{send: init, waitFor: "control_response"}, {send: userMsg("Write a haiku about the sea."), waitFor: "result"}, {send: userMsg("Another one."), waitFor: "result"}}, nil))

	// F. Plan mode with an explicit model: which model actually ran?
	out = append(out, s.m16("m16-plan-model",
		with("--permission-mode", "plan", "--settings", `{"permissions":{"deny":["ExitPlanMode"]}}`),
		[]m16Step{{send: init, waitFor: "control_response"}, {send: userMsg("In two sentences, how would you add a README badge? Do not change files."), waitFor: "result"}}, nil))
	return out
}
