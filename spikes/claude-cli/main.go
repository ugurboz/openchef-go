// Command claude-cli is spike M1.2 (stdin EOF), M1.3 (permission modes with the prompt
// tool, and the newer --permission-prompts host path) and M1.5 (--json-schema) for
// Claude Code (docs/architecture.md §17).
//
// Usage: go build -o /tmp/claude-spike ./claude-cli && /tmp/claude-spike -out <dir>
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ugurboz/openchef/spikes/internal/mcpserver"
	"github.com/ugurboz/openchef/spikes/internal/rpc"
)

const task = "Do exactly these three things in order, then reply with the single word DONE:\n" +
	"1) Run the shell command: echo m > cmd.txt\n" +
	"2) Using your file editing/writing tool (not the shell), create a file named edit.txt containing the word x\n" +
	"3) Run the shell command: echo denied > denied.txt\n" +
	"Do nothing else. If a step is refused, skip it and continue."

type runResult struct {
	Name              string            `json:"name"`
	Mode              string            `json:"mode"`
	PromptPath        string            `json:"promptPath"` // tool | host | none
	ExitCode          int               `json:"exitCode"`
	ResultSubtype     string            `json:"resultSubtype,omitempty"`
	ResultIsError     bool              `json:"resultIsError"`
	FinalText         string            `json:"finalText"`
	ApproveCalls      []string          `json:"approveCalls"`    // tool names that reached the prompt tool
	ControlRequests   []json.RawMessage `json:"controlRequests"` // --permission-prompts host traffic
	Files             map[string]bool   `json:"files"`
	ResultUsage       json.RawMessage   `json:"resultUsage,omitempty"`
	AssistantUsage    map[string]int    `json:"assistantUsageSum"`
	StructuredOut     json.RawMessage   `json:"structuredOutput,omitempty"`
	ResultKeys        []string          `json:"resultKeys"`
	ExitAfterEOF      string            `json:"exitAfterEOF,omitempty"`
	GotResultAfterEOF bool              `json:"gotResultAfterEOF,omitempty"`
	Elapsed           string            `json:"elapsed"`
	Error             string            `json:"error,omitempty"`
}

type spike struct {
	out, work, exe string
	results        []runResult
}

func main() {
	out := flag.String("out", "", "output directory")
	mcp := flag.Bool("mcp-server", false, "internal: run as MCP server")
	mcpLog := flag.String("mcp-log", "", "internal: MCP call log")
	only := flag.String("only", "", "comma-separated run names to execute (default all)")
	flag.Parse()
	if *mcp {
		if err := mcpserver.Serve(*mcpLog); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if *out == "" {
		fmt.Fprintln(os.Stderr, "-out is required")
		os.Exit(2)
	}
	if err := run(*out, *only); err != nil {
		fmt.Fprintln(os.Stderr, "spike failed:", err)
		os.Exit(1)
	}
}

func run(out, only string) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "openchef-claude-spike-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	s := &spike{out: out, work: work, exe: exe}
	want := func(name string) bool {
		return only == "" || strings.Contains(","+only+",", ","+name+",")
	}

	for _, mode := range []string{"manual", "acceptEdits", "plan", "auto", "dontAsk", "bypassPermissions"} {
		if name := "mode-" + mode; want(name) {
			s.results = append(s.results, s.session(name, mode, "tool", task, nil, false))
		}
	}
	if want("host-manual") {
		s.results = append(s.results, s.session("host-manual", "manual", "host", task, nil, false))
	}
	denySettings := []string{"--settings", `{"permissions":{"deny":["Bash(echo denied:*)","Bash(echo denied *)"]}}`}
	for _, mode := range []string{"acceptEdits", "auto", "bypassPermissions"} {
		if name := "native-deny-" + mode; want(name) {
			s.results = append(s.results, s.session(name, mode, "stdio", task, denySettings, false))
		}
	}
	if want("stdio-manual") {
		s.results = append(s.results, s.session("stdio-manual", "manual", "stdio", task, nil, false))
	}
	if want("host-manual-init") {
		s.results = append(s.results, s.session("host-manual-init", "manual", "host-init", task, nil, false))
	}
	if want("json-schema") {
		schema := `{"type":"object","properties":{"title":{"type":"string"},"steps":{"type":"array","items":{"type":"string"}}},"required":["title","steps"],"additionalProperties":false}`
		s.results = append(s.results, s.session("json-schema", "plan", "none",
			"Plan how to add a hello-world shell script to this repository in exactly 2 steps. Do not change any files.",
			[]string{"--json-schema", schema}, false))
	}
	if want("eof-midturn") {
		s.results = append(s.results, s.session("eof-midturn", "bypassPermissions", "none",
			"Write a 600-word story about a lighthouse keeper. Do not use any tools.",
			[]string{"--include-partial-messages"}, true))
	}
	if want("eof-idle") {
		s.results = append(s.results, s.eofIdle())
	}
	if only == "m17" {
		m := s.runM17()
		var sid string
		if b, err := os.ReadFile(filepath.Join(out, "claude-m17-auto.jsonl")); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				var r struct {
					Msg struct {
						Type      string `json:"type"`
						SessionID string `json:"session_id"`
					} `json:"msg"`
				}
				if json.Unmarshal([]byte(line), &r) == nil && r.Msg.Type == "result" && r.Msg.SessionID != "" {
					sid = r.Msg.SessionID
				}
			}
		}
		if sid != "" {
			m = append(m, s.resumeM17(sid))
		}
		scrub, err := rpc.NewTranscript(filepath.Join(work, "scrub17.jsonl"), work)
		if err != nil {
			return err
		}
		defer scrub.Close()
		raw, _ := json.MarshalIndent(m, "", "  ")
		return os.WriteFile(filepath.Join(out, "claude-m17-findings.json"), scrub.ScrubJSON(raw), 0o644)
	}
	if only == "m16" {
		m := s.runM16()
		scrub, err := rpc.NewTranscript(filepath.Join(work, "scrub16.jsonl"), work)
		if err != nil {
			return err
		}
		defer scrub.Close()
		raw, _ := json.MarshalIndent(m, "", "  ")
		return os.WriteFile(filepath.Join(out, "claude-m16-findings.json"), scrub.ScrubJSON(raw), 0o644)
	}

	scrub, err := rpc.NewTranscript(filepath.Join(work, "scrub.jsonl"), work)
	if err != nil {
		return err
	}
	defer scrub.Close()
	raw, _ := json.MarshalIndent(s.results, "", "  ")
	return os.WriteFile(filepath.Join(out, "claude-findings.json"), scrub.ScrubJSON(raw), 0o644)
}

func (s *spike) baseArgs(mode, promptPath, mcpCfg string) []string {
	args := []string{
		"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--model", "haiku", "--effort", "low",
		"--permission-mode", mode,
		"--setting-sources", "local",
		"--disallowedTools", "AskUserQuestion",
		"--mcp-config", mcpCfg, "--strict-mcp-config",
	}
	switch promptPath {
	case "tool":
		args = append(args, "--permission-prompt-tool", "mcp__openchef__approve")
	case "host", "host-init":
		args = append(args, "--permission-prompts", "host")
	case "stdio":
		args = append(args, "--permission-prompt-tool", "stdio")
	}
	return args
}

func (s *spike) session(name, mode, promptPath, prompt string, extra []string, eofMidTurn bool) runResult {
	res := runResult{Name: name, Mode: mode, PromptPath: promptPath, Files: map[string]bool{}, AssistantUsage: map[string]int{}}
	t0 := time.Now()
	defer func() { res.Elapsed = time.Since(t0).Round(time.Millisecond).String() }()

	repo := filepath.Join(s.work, name)
	if err := rpc.NewGitRepo(repo); err != nil {
		res.Error = err.Error()
		return res
	}
	mcpLog := filepath.Join(s.work, name+"-mcp.jsonl")
	mcpCfg := filepath.Join(s.work, name+"-mcp.json")
	cfg := map[string]any{"mcpServers": map[string]any{"openchef": map[string]any{
		"command": s.exe, "args": []string{"-mcp-server", "-mcp-log", mcpLog},
	}}}
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(mcpCfg, raw, 0o644); err != nil {
		res.Error = err.Error()
		return res
	}
	tr, err := rpc.NewTranscript(filepath.Join(s.out, "claude-"+name+".jsonl"), s.work)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	defer tr.Close()

	args := append(s.baseArgs(mode, promptPath, mcpCfg), extra...)
	tr.Note("argv", "claude "+strings.Join(args, " "))
	cmd := exec.Command("claude", args...)
	cmd.Dir = repo
	stdin, _ := cmd.StdinPipe()
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		res.Error = err.Error()
		return res
	}
	go func() {
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			tr.Note("stderr", sc.Text())
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
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()

	if promptPath == "host-init" || promptPath == "stdio" {
		// Agent SDK hosts open the control channel with an initialize request.
		write(map[string]any{"type": "control_request", "request_id": "init-1",
			"request": map[string]any{"subtype": "initialize"}})
	}
	write(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": prompt}}}})

	gotResult := make(chan struct{})
	var eofOnce sync.Once
	var eofAt time.Time
	go func() {
		defer close(gotResult)
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 1<<20), 1<<26)
		for sc.Scan() {
			line := append([]byte(nil), sc.Bytes()...)
			tr.Line("recv", line)
			var ev map[string]json.RawMessage
			if json.Unmarshal(line, &ev) != nil {
				continue
			}
			var typ string
			_ = json.Unmarshal(ev["type"], &typ)
			switch typ {
			case "stream_event":
				if eofMidTurn {
					eofOnce.Do(func() {
						tr.Note("note", "closing stdin mid-turn (first stream_event)")
						eofAt = time.Now()
						_ = stdin.Close()
					})
				}
			case "assistant":
				var a struct {
					Message struct {
						Usage map[string]any `json:"usage"`
					} `json:"message"`
				}
				_ = json.Unmarshal(line, &a)
				for k, v := range a.Message.Usage {
					if f, ok := v.(float64); ok {
						res.AssistantUsage[k] += int(f)
					}
				}
			case "control_request":
				res.ControlRequests = append(res.ControlRequests, line)
				write(controlResponse(line))
			case "result":
				var r map[string]json.RawMessage
				_ = json.Unmarshal(line, &r)
				for k := range r {
					res.ResultKeys = append(res.ResultKeys, k)
				}
				_ = json.Unmarshal(r["subtype"], &res.ResultSubtype)
				_ = json.Unmarshal(r["is_error"], &res.ResultIsError)
				_ = json.Unmarshal(r["result"], &res.FinalText)
				res.ResultUsage = r["usage"]
				res.StructuredOut = r["structured_output"]
				if eofMidTurn {
					res.GotResultAfterEOF = true
				}
				if !eofMidTurn {
					_ = stdin.Close() // end the session after one turn
				}
			}
		}
	}()

	select {
	case <-exited:
	case <-time.After(4 * time.Minute):
		res.Error = "timeout"
		_ = cmd.Process.Kill()
		<-exited
	}
	<-gotResult
	if eofMidTurn && !eofAt.IsZero() {
		res.ExitAfterEOF = time.Since(eofAt).Round(time.Millisecond).String()
	}
	res.ExitCode = cmd.ProcessState.ExitCode()
	for _, f := range []string{"cmd.txt", "edit.txt", "denied.txt"} {
		_, err := os.Stat(filepath.Join(repo, f))
		res.Files[f] = err == nil
	}
	if b, err := os.ReadFile(mcpLog); err == nil {
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			var c struct {
				Tool      string `json:"tool"`
				Arguments struct {
					ToolName string `json:"tool_name"`
				} `json:"arguments"`
			}
			if json.Unmarshal([]byte(l), &c) == nil && c.Tool == "approve" {
				res.ApproveCalls = append(res.ApproveCalls, c.Arguments.ToolName)
			}
		}
	}
	rpc.Logf("%-24s exit=%d subtype=%s approve=%v ctrl=%d files=%v text=%.50q (%s)",
		name, res.ExitCode, res.ResultSubtype, res.ApproveCalls, len(res.ControlRequests), res.Files, res.FinalText, time.Since(t0).Round(time.Second))
	return res
}

// controlResponse answers a --permission-prompts host request. The shape follows the
// Claude Agent SDK control protocol; the spike records whatever arrives either way.
func controlResponse(line []byte) map[string]any {
	var req struct {
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype string          `json:"subtype"`
			Input   json.RawMessage `json:"input"`
		} `json:"request"`
	}
	_ = json.Unmarshal(line, &req)
	var decision map[string]any
	if strings.Contains(string(req.Request.Input), "denied") {
		decision = map[string]any{"behavior": "deny", "message": "denied by spike policy"}
	} else {
		decision = map[string]any{"behavior": "allow", "updatedInput": req.Request.Input}
	}
	return map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": req.RequestID,
			"response":   decision,
		},
	}
}

func (s *spike) eofIdle() runResult {
	res := runResult{Name: "eof-idle", Mode: "manual", PromptPath: "none"}
	repo := filepath.Join(s.work, "eof-idle")
	if err := rpc.NewGitRepo(repo); err != nil {
		res.Error = err.Error()
		return res
	}
	cmd := exec.Command("claude", "-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--model", "haiku")
	cmd.Dir = repo
	stdin, _ := cmd.StdinPipe()
	cmd.Stdout = io.Discard
	if err := cmd.Start(); err != nil {
		res.Error = err.Error()
		return res
	}
	time.Sleep(2 * time.Second)
	t0 := time.Now()
	_ = stdin.Close()
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
		res.ExitAfterEOF = time.Since(t0).Round(time.Millisecond).String()
	case <-time.After(30 * time.Second):
		res.ExitAfterEOF = "did not exit within 30s"
		_ = cmd.Process.Kill()
		<-done
	}
	res.ExitCode = cmd.ProcessState.ExitCode()
	rpc.Logf("%-24s exit=%d after EOF: %s", res.Name, res.ExitCode, res.ExitAfterEOF)
	return res
}
