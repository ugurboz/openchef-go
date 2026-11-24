package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// mcpScenario checks whether Codex can call an OpenChef-provided MCP tool configured per
// thread, which would replace the unavailable request_user_input tool.
func (s *spike) mcpScenario(repo string) {
	exe, err := os.Executable()
	if err != nil {
		s.errf("mcp: executable: %v", err)
		return
	}
	logPath := filepath.Join(s.work, "mcp-calls.jsonl")
	c, tr, err := s.start(repo, "mcp")
	if err != nil {
		s.errf("mcp start: %v", err)
		return
	}
	defer tr.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var res struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	err = c.Call(ctx, "thread/start", map[string]any{
		"cwd": repo, "model": s.model, "approvalPolicy": "untrusted", "sandbox": "workspace-write",
		"config": map[string]any{
			"mcp_servers": map[string]any{
				"openchef": map[string]any{"command": exe, "args": []string{"-mcp-server", "-mcp-log", logPath}},
			},
		},
	}, &res)
	if err != nil {
		s.errf("mcp thread/start: %v", err)
		return
	}
	r := s.turn(c, res.Thread.ID, "mcp-ask-user",
		"Use the ask_user tool from the openchef MCP server to ask me which colour I prefer. Then reply with exactly the answer you received and nothing else.", nil, nil)
	raw, _ := os.ReadFile(logPath)
	s.f.Checks["mcpAskUserCalled"] = strings.Contains(string(raw), "ask_user")
	s.f.Checks["mcpAnswerUsed"] = strings.Contains(strings.ToLower(r.FinalText), "teal")
	s.f.Checks["mcpApprovalsDuringCall"] = fmt.Sprint(len(s.f.Approvals))
	_ = c.CloseStdin()
	c.WaitExit(10 * time.Second)
}
