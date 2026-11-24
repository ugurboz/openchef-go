// Package mcpserver is a minimal stdio MCP server for spikes. It exposes:
//
//   - ask_user: always answers "teal";
//   - approve: a Claude Code --permission-prompt-tool that allows every request except
//     those whose input mentions "denied".
//
// Every tools/call is appended to a JSONL log so a spike can see what reached it.
package mcpserver

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"

	"github.com/ugurboz/openchef/spikes/internal/rpc"
)

// Serve runs the server on stdin/stdout until EOF.
func Serve(logPath string) error {
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1<<20), 1<<24)
	enc := json.NewEncoder(os.Stdout)
	for in.Scan() {
		var m rpc.Message
		if err := json.Unmarshal(in.Bytes(), &m); err != nil || len(m.ID) == 0 {
			continue // notifications need no answer
		}
		result, rerr := handle(m, logPath)
		resp := rpc.Message{JSONRPC: "2.0", ID: m.ID}
		if rerr != nil {
			resp.Error = rerr
		} else {
			raw, _ := json.Marshal(result)
			resp.Result = raw
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return in.Err()
}

func handle(m rpc.Message, logPath string) (any, *rpc.RPCError) {
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(m.Params, &p)
		return map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "openchef", "version": "0.0.0"},
		}, nil
	case "tools/list":
		return map[string]any{"tools": []any{
			tool("ask_user", "Ask the human user a question and wait for the answer.",
				map[string]any{"question": map[string]any{"type": "string"}}, "question"),
			tool("approve", "Decide whether a tool call may run.",
				map[string]any{
					"tool_name":   map[string]any{"type": "string"},
					"input":       map[string]any{"type": "object"},
					"tool_use_id": map[string]any{"type": "string"},
				}, "tool_name", "input"),
		}}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		_ = json.Unmarshal(m.Params, &p)
		logCall(logPath, p.Name, p.Arguments)
		switch p.Name {
		case "ask_user":
			return text("teal"), nil
		case "approve":
			var a struct {
				ToolName string          `json:"tool_name"`
				Input    json.RawMessage `json:"input"`
			}
			_ = json.Unmarshal(p.Arguments, &a)
			if strings.Contains(string(a.Input), "denied") {
				return jsonText(map[string]any{"behavior": "deny", "message": "denied by spike policy"}), nil
			}
			return jsonText(map[string]any{"behavior": "allow", "updatedInput": a.Input}), nil
		}
		return nil, &rpc.RPCError{Code: -32602, Message: "unknown tool " + p.Name}
	default:
		return nil, &rpc.RPCError{Code: -32601, Message: "method not found"}
	}
}

func tool(name, desc string, props map[string]any, required ...string) map[string]any {
	return map[string]any{
		"name": name, "description": desc,
		"inputSchema": map[string]any{"type": "object", "properties": props, "required": required},
	}
}

func text(s string) map[string]any {
	return map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}}
}

func jsonText(v any) map[string]any {
	raw, _ := json.Marshal(v)
	return text(string(raw))
}

func logCall(path, name string, args json.RawMessage) {
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	raw, _ := json.Marshal(map[string]any{"tool": name, "arguments": args})
	_, _ = f.Write(append(raw, '\n'))
}
