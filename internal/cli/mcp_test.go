package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/runtime/fake"
	"github.com/ugurboz/openchef/internal/runtime/linerpc"
)

// An agent's ask_user call travels MCP server → daemon queue → `openchef answer` and
// the answer comes back as the tool result (M4.5 acceptance).
func TestAskUserThroughTheDaemon(t *testing.T) {
	e := newCLIEnv(t)
	chdir(t, e.repo)
	e.claude.Push(fake.Turn{Hang: true})
	out, err := e.cli("run", "--detach", "work")
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}
	id := strings.Fields(out)[1]

	// The agent's CLI side: pipes to an in-process `openchef mcp ask-user`.
	toServer, serverIn := io.Pipe()
	serverOut, fromServer := io.Pipe()
	cmd := NewRootCommandWith(IO{Out: fromServer, Err: &bytes.Buffer{}}, e.deps)
	cmd.SetIn(toServer)
	cmd.SetArgs([]string{"mcp", "ask-user", "--task", id, "--agent", "opus"})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	served := make(chan error, 1)
	go func() { served <- cmd.ExecuteContext(ctx); _ = fromServer.Close() }()
	agent := linerpc.New(serverOut, serverIn, linerpc.Options{})
	if err := agent.Call(ctx, "initialize", map[string]any{"protocolVersion": "2025-06-18"}, nil); err != nil {
		t.Fatal(err)
	}
	type result struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	got := make(chan result, 1)
	go func() {
		var r result
		if err := agent.Call(ctx, "tools/call", map[string]any{"name": "ask_user",
			"arguments": map[string]any{"question": "Which port?", "options": []string{"8080", "9090"}}}, &r); err != nil {
			t.Error(err)
		}
		got <- r
	}()
	p := e.waitPending()
	if p.Summary != "Which port?" || p.Agent != "opus" || fmt.Sprint(p.TaskID) != id {
		t.Fatalf("pending %+v", p)
	}
	if out, err := e.cli("answer", fmt.Sprint(p.ID), "9090"); err != nil {
		t.Fatalf("answer: %v\n%s", err, out)
	}
	r := <-got
	if len(r.Content) != 1 || r.Content[0].Text != "9090" {
		t.Fatalf("tool result %+v", r)
	}
	_ = serverIn.Close() // the agent exits: the server ends
	if err := <-served; err != nil {
		t.Fatalf("server: %v", err)
	}
	if out, err := e.cli("cancel", id); err != nil {
		t.Fatalf("cancel: %v\n%s", err, out)
	}
}
