package main

// Spike M1.7 (round 2 of the design review, D-006): --restricted combined with the other
// flags the adapter will use.

import (
	"encoding/json"
	"os"
	"path/filepath"
)

func (s *spike) runM17() []m16Result {
	exe := s.exe
	mcpLog := filepath.Join(s.work, "m17-mcp.jsonl")
	mcpCfg := filepath.Join(s.work, "m17-mcp.json")
	cfg, _ := json.Marshal(map[string]any{"mcpServers": map[string]any{"openchef": map[string]any{
		"command": exe, "args": []string{"-mcp-server", "-mcp-log", mcpLog},
	}}})
	_ = os.WriteFile(mcpCfg, cfg, 0o644)
	base := []string{
		"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--model", "haiku", "--effort", "low", "--restricted",
		"--strict-mcp-config", "--mcp-config", mcpCfg,
		"--permission-prompt-tool", "stdio", "--disallowedTools", "AskUserQuestion",
	}
	init := map[string]any{"type": "control_request", "request_id": "init-1", "request": map[string]any{"subtype": "initialize"}}
	with := func(extra ...string) []string { return append(append([]string{}, base...), extra...) }
	allowAsk := `{"permissions":{"allow":["mcp__openchef__ask_user"],"deny":["Write(**/.claude/**)","Edit(**/.claude/**)"]}}`
	schema := `{"type":"object","properties":{"steps":{"type":"array","items":{"type":"string"}}},"required":["steps"],"additionalProperties":false}`
	var out []m16Result

	// A. Planner shape: read-only tools + dontAsk + json-schema + ask_user allowed natively.
	out = append(out, s.m16("m17-planner",
		with("--permission-mode", "dontAsk", "--tools", "Read,Glob,Grep", "--json-schema", schema, "--settings", allowAsk),
		[]m16Step{{send: init, waitFor: "control_response"}, {send: userMsg("First use the ask_user tool from the openchef MCP server to ask me which language to use. Then give a 2-step plan to add a hello script in that language. Do not change files."), waitFor: "result"}},
		nil))

	// B. auto under --restricted, nested .claude path rule, then a second turn; also check
	// whether the session id is reported for resume.
	out = append(out, s.m16("m17-auto",
		with("--permission-mode", "auto", "--tools", "Bash,Read,Edit,Write,Glob,Grep", "--settings", allowAsk),
		[]m16Step{
			{send: init, waitFor: "control_response"},
			{send: userMsg("Using your file writing tool, create sub/.claude/settings.json containing {} and also create ok.txt containing ok. Reply DONE."), waitFor: "result"},
			{send: userMsg("Reply with the single word SECOND."), waitFor: "result"},
		},
		[]string{"sub/.claude/settings.json", "ok.txt"}))
	return out
}

// resumeM17 resumes the auto session to see whether result_index restarts.
func (s *spike) resumeM17(sessionID string) m16Result {
	args := []string{
		"-p", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose",
		"--model", "haiku", "--effort", "low", "--restricted", "--permission-prompt-tool", "stdio",
		"--permission-mode", "acceptEdits", "--resume", sessionID,
	}
	init := map[string]any{"type": "control_request", "request_id": "init-1", "request": map[string]any{"subtype": "initialize"}}
	return s.m16("m17-resume", args, []m16Step{{send: init, waitFor: "control_response"}, {send: userMsg("What single word did you reply with last time? Reply with just that word."), waitFor: "result"}}, nil)
}
