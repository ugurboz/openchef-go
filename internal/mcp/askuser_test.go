package mcp

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/runtime/linerpc"
)

type fakeAsker struct {
	mu     sync.Mutex
	asked  []string
	opts   [][]string
	answer string
	err    error
	block  bool
}

func (f *fakeAsker) Ask(ctx context.Context, q string, options []string) (string, error) {
	f.mu.Lock()
	f.asked = append(f.asked, q)
	f.opts = append(f.opts, options)
	block := f.block
	f.mu.Unlock()
	if block {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return f.answer, f.err
}

// client starts the server on pipes and returns a JSON-RPC client for it.
func client(t *testing.T, ask Asker) *linerpc.Peer {
	t.Helper()
	cr, sw := io.Pipe()
	sr, cw := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, sr, sw, ask, "test") }()
	c := linerpc.New(cr, cw, linerpc.Options{})
	t.Cleanup(func() {
		_ = cw.Close()
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Serve: %v", err)
		}
		_ = sw.Close()
	})
	return c
}

type callResult struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	IsError bool `json:"isError"`
}

func call(t *testing.T, c *linerpc.Peer, args any) callResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var r callResult
	if err := c.Call(ctx, "tools/call", map[string]any{"name": ToolName, "arguments": args}, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestHandshakeAndToolList(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := client(t, &fakeAsker{})
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
		Capabilities map[string]any `json:"capabilities"`
	}
	if err := c.Call(ctx, "initialize", map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "claude-code"}}, &init); err != nil {
		t.Fatal(err)
	}
	if init.ProtocolVersion != "2025-03-26" || init.ServerInfo.Name != ServerName || init.Capabilities["tools"] == nil {
		t.Fatalf("initialize: %+v", init)
	}
	if err := c.Notify("notifications/initialized", nil); err != nil {
		t.Fatal(err)
	}
	var unknown struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := c.Call(ctx, "initialize", map[string]any{"protocolVersion": "1999-01-01"}, &unknown); err != nil || unknown.ProtocolVersion != protocolVersions[0] {
		t.Fatalf("unknown version: %+v, %v", unknown, err)
	}
	var list struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := c.Call(ctx, "tools/list", nil, &list); err != nil {
		t.Fatal(err)
	}
	// Only ask_user: every tool a server lists is offered to the model.
	if len(list.Tools) != 1 || list.Tools[0].Name != ToolName {
		t.Fatalf("tools %+v", list.Tools)
	}
	if err := c.Call(ctx, "ping", nil, nil); err != nil {
		t.Fatal(err)
	}
	var rpc *linerpc.Error
	if err := c.Call(ctx, "resources/list", nil, nil); !errors.As(err, &rpc) || rpc.Code != linerpc.CodeMethodNotFound {
		t.Fatalf("unknown method: %v", err)
	}
	if err := c.Call(ctx, "tools/call", map[string]any{"name": "approve"}, nil); !errors.As(err, &rpc) {
		t.Fatalf("unknown tool: %v", err)
	}
}

func TestAskUserAnswers(t *testing.T) {
	t.Parallel()
	ask := &fakeAsker{answer: "9090"}
	c := client(t, ask)
	r := call(t, c, map[string]any{"question": "  Which port?  ", "options": []string{"8080", "9090"}})
	if r.IsError || len(r.Content) != 1 || r.Content[0].Text != "9090" {
		t.Fatalf("result %+v", r)
	}
	ask.mu.Lock()
	if ask.asked[0] != "Which port?" || !slices.Equal(ask.opts[0], []string{"8080", "9090"}) {
		t.Errorf("asked %q %v", ask.asked, ask.opts)
	}
	ask.mu.Unlock()

	if r := call(t, c, map[string]any{"question": ""}); !r.IsError {
		t.Errorf("empty question: %+v", r)
	}
	if r := call(t, c, map[string]any{"question": strings.Repeat("x", maxQuestion+1)}); !r.IsError {
		t.Errorf("huge question: %+v", r)
	}
}

func TestAskUserUnanswered(t *testing.T) {
	t.Parallel()
	for _, ask := range []*fakeAsker{{answer: ""}, {err: errors.New("daemon not running")}} {
		r := call(t, client(t, ask), map[string]any{"question": "?"})
		if r.IsError || !strings.Contains(r.Content[0].Text, "Make the most reasonable choice") {
			t.Errorf("%+v: %+v", ask, r)
		}
	}
}

// When the agent goes away mid-question, the server stops without blocking.
func TestServeEndsWithInput(t *testing.T) {
	t.Parallel()
	sr, cw := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- Serve(context.Background(), sr, io.Discard, &fakeAsker{block: true}, "test") }()
	if _, err := cw.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"ask_user","arguments":{"question":"?"}}}` + "\n")); err != nil {
		t.Fatal(err)
	}
	_ = cw.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after EOF")
	}
}
