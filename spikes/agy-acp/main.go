// Command agy-acp is the agy part of spike M1.2 (stdin EOF) and records what `agy acp`
// advertises for modes, models and effort (docs/architecture.md §6.4, §7.4).
//
// Usage: go build -o /tmp/agy-spike ./agy-acp && /tmp/agy-spike -out <dir>
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/ugurboz/openchef/spikes/internal/mcpserver"
	"github.com/ugurboz/openchef/spikes/internal/rpc"
)

var argv = []string{"acp", "--sandbox", "--disable-telemetry", "--include-default-customizations=false", "--cli_dir_name", "openchef-spike"}

type findings struct {
	InitializeKeys []string        `json:"initializeResultKeys"`
	AgentCaps      json.RawMessage `json:"agentCapabilities"`
	SessionNewKeys []string        `json:"sessionNewResultKeys"`
	Modes          json.RawMessage `json:"modes"`
	ModelIDs       []string        `json:"modelIds"`
	ConfigOptions  json.RawMessage `json:"configOptions,omitempty"`
	PromptStop     string          `json:"promptStopReason"`
	PromptUsage    json.RawMessage `json:"promptUsage,omitempty"`
	UpdateKinds    map[string]int  `json:"sessionUpdateKinds"`
	SetThought     json.RawMessage `json:"setConfigOptionThoughtLevel,omitempty"`
	SetThoughtErr  string          `json:"setConfigOptionThoughtLevelError,omitempty"`
	ExitEOFIdle    string          `json:"exitAfterEOFIdle"`
	ExitEOFMidTurn string          `json:"exitAfterEOFMidTurn"`
	Errors         []string        `json:"errors"`
}

func main() {
	out := flag.String("out", "", "output directory")
	model := flag.String("model", "gemini-3.8-flash-low", "cheapest model")
	only := flag.String("only", "", "m18: run spike M1.8 (M18_ONLY narrows it to named runs)")
	mcp := flag.Bool("mcp-server", false, "internal: run as MCP server")
	mcpLog := flag.String("mcp-log", "", "internal: MCP call log")
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
	if *only == "m18" {
		m := *model
		if m == "gemini-3.8-flash-low" {
			m = "gemini-3.8-flash" // M1.8 sets effort separately (thought_level)
		}
		if err := runM18(*out, m); err != nil {
			fmt.Fprintln(os.Stderr, "spike failed:", err)
			os.Exit(1)
		}
		return
	}
	if err := run(*out, *model); err != nil {
		fmt.Fprintln(os.Stderr, "spike failed:", err)
		os.Exit(1)
	}
}

func keys(raw json.RawMessage) []string {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(raw, &m)
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func run(out, model string) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "openchef-agy-spike-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	repo := filepath.Join(work, "repo")
	if err := rpc.NewGitRepo(repo); err != nil {
		return err
	}
	f := findings{UpdateKinds: map[string]int{}}
	errf := func(format string, a ...any) {
		msg := fmt.Sprintf(format, a...)
		rpc.Logf("ERROR: %s", msg)
		f.Errors = append(f.Errors, msg)
	}
	handler := func(method string, params json.RawMessage) (any, error) {
		if method == "session/request_permission" {
			return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": "allow_once"}}, nil
		}
		return nil, fmt.Errorf("unhandled %s", method)
	}
	start := func(name string) (*rpc.Client, *rpc.Transcript, string, error) {
		tr, err := rpc.NewTranscript(filepath.Join(out, "agy-"+name+".jsonl"), work)
		if err != nil {
			return nil, nil, "", err
		}
		c, err := rpc.Start("agy", argv, repo, tr, handler)
		if err != nil {
			return nil, nil, "", err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		var init json.RawMessage
		if err := c.Call(ctx, "initialize", map[string]any{
			"protocolVersion":    1,
			"clientCapabilities": map[string]any{"fs": map[string]any{"readTextFile": false, "writeTextFile": false}, "terminal": false},
		}, &init); err != nil {
			return nil, nil, "", fmt.Errorf("initialize: %w", err)
		}
		if f.InitializeKeys == nil {
			f.InitializeKeys = keys(init)
			var m map[string]json.RawMessage
			_ = json.Unmarshal(init, &m)
			f.AgentCaps = m["agentCapabilities"]
		}
		var sess json.RawMessage
		if err := c.Call(ctx, "session/new", map[string]any{"cwd": repo, "mcpServers": []any{}}, &sess); err != nil {
			return nil, nil, "", fmt.Errorf("session/new: %w", err)
		}
		var sm struct {
			SessionID     string          `json:"sessionId"`
			Modes         json.RawMessage `json:"modes"`
			ConfigOptions json.RawMessage `json:"configOptions"`
			Models        struct {
				Available []struct {
					ModelID string `json:"modelId"`
				} `json:"availableModels"`
			} `json:"models"`
		}
		_ = json.Unmarshal(sess, &sm)
		if f.SessionNewKeys == nil {
			f.SessionNewKeys = keys(sess)
			f.Modes = sm.Modes
			f.ConfigOptions = sm.ConfigOptions
			for _, m := range sm.Models.Available {
				f.ModelIDs = append(f.ModelIDs, m.ModelID)
			}
		}
		if err := c.Call(ctx, "session/set_model", map[string]any{"sessionId": sm.SessionID, "modelId": model}, nil); err != nil {
			errf("%s: set_model: %v", name, err)
		}
		if f.SetThought == nil && f.SetThoughtErr == "" {
			var r json.RawMessage
			if err := c.Call(ctx, "session/set_config_option", map[string]any{"sessionId": sm.SessionID, "configId": "thought_level", "value": "low"}, &r); err != nil {
				f.SetThoughtErr = err.Error()
			} else {
				f.SetThought = r
			}
		}
		return c, tr, sm.SessionID, nil
	}

	// 1. One short prompt; record update kinds and stop reason.
	c, tr, sid, err := start("prompt")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	done := make(chan json.RawMessage, 1)
	go func() {
		var res json.RawMessage
		if err := c.Call(ctx, "session/prompt", map[string]any{"sessionId": sid, "prompt": []any{map[string]any{"type": "text", "text": "Reply with the single word OK. Do not use tools."}}}, &res); err != nil {
			errf("prompt: %v", err)
		}
		done <- res
	}()
	collect := func(stop <-chan json.RawMessage) json.RawMessage {
		for {
			select {
			case m := <-c.Notifications():
				var p struct {
					Update struct {
						Kind string `json:"sessionUpdate"`
					} `json:"update"`
				}
				_ = json.Unmarshal(m.Params, &p)
				f.UpdateKinds[m.Method+":"+p.Update.Kind]++
			case res := <-stop:
				return res
			}
		}
	}
	res := collect(done)
	cancel()
	var pr struct {
		StopReason string          `json:"stopReason"`
		Usage      json.RawMessage `json:"usage"`
	}
	_ = json.Unmarshal(res, &pr)
	f.PromptStop, f.PromptUsage = pr.StopReason, pr.Usage
	_ = c.CloseStdin()
	if d, ok := c.WaitExit(30 * time.Second); ok {
		f.ExitEOFIdle = d.Round(time.Millisecond).String()
	} else {
		f.ExitEOFIdle = "did not exit within 30s"
		_ = c.Kill()
	}
	tr.Close()

	// 2. EOF in the middle of a turn.
	c, tr, sid, err = start("eof-midturn")
	if err != nil {
		return err
	}
	ctx, cancel = context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	go func() {
		_ = c.Call(ctx, "session/prompt", map[string]any{"sessionId": sid, "prompt": []any{map[string]any{"type": "text", "text": "Write a 600-word story about a lighthouse keeper. Do not use tools."}}}, nil)
	}()
	if _, err := c.Await(2*time.Minute, func(m rpc.Message) bool { return m.Method == "session/update" }); err != nil {
		errf("eof: no update: %v", err)
	}
	tr.Note("note", "closing stdin mid-turn")
	_ = c.CloseStdin()
	if d, ok := c.WaitExit(60 * time.Second); ok {
		f.ExitEOFMidTurn = d.Round(time.Millisecond).String()
	} else {
		f.ExitEOFMidTurn = "did not exit within 60s"
		_ = c.Kill()
	}
	tr.Close()

	scrub, err := rpc.NewTranscript(filepath.Join(work, "scrub.jsonl"), work)
	if err != nil {
		return err
	}
	defer scrub.Close()
	raw, _ := json.MarshalIndent(f, "", "  ")
	rpc.Logf("%s", raw)
	return os.WriteFile(filepath.Join(out, "agy-findings.json"), scrub.ScrubJSON(raw), 0o644)
}
