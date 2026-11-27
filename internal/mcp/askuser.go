// Package mcp is OpenChef's stdio MCP server. It offers exactly one tool, ask_user,
// through which agents put questions to the user (§9.2, M1 findings: a server's every
// tool is offered to the model, so nothing else is exposed). Answers come from the
// daemon's approval queue.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/ugurboz/openchef/internal/runtime/linerpc"
)

// ServerName and ToolName make the tool mcp__openchef__ask_user for Claude.
const (
	ServerName = "openchef"
	ToolName   = "ask_user"
)

// protocolVersions are the MCP revisions this server speaks, newest first.
var protocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// Asker puts a question to the user and returns the answer. An empty answer with a nil
// error means nobody answered.
type Asker interface {
	Ask(ctx context.Context, question string, options []string) (string, error)
}

// Unanswered is the tool's result when nobody answers.
const Unanswered = "No one is available to answer right now. Make the most reasonable choice, " +
	"say which one you made in your final answer, and continue."

const maxQuestion = 4000

const codeInvalidParams = -32602

// Serve runs the server on r/w until r ends or ctx is cancelled.
func Serve(ctx context.Context, r io.Reader, w io.Writer, ask Asker, version string) error {
	s := &server{ask: ask, version: version}
	peer := linerpc.New(r, w, linerpc.Options{Handler: s.handle})
	go func() {
		for range peer.Notifications() { // initialized, cancelled: nothing to do
		}
	}()
	select {
	case <-peer.Done():
		if err := peer.Err(); err != nil {
			return fmt.Errorf("mcp: %w", err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("mcp: %w", ctx.Err())
	}
}

type server struct {
	ask     Asker
	version string
}

func (s *server) handle(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(params, &p) // an unknown or missing version gets our newest
		v := protocolVersions[0]
		if slices.Contains(protocolVersions, p.ProtocolVersion) {
			v = p.ProtocolVersion
		}
		return map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": ServerName, "version": s.version},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		return map[string]any{"tools": []any{toolSpec}}, nil
	case "tools/call":
		return s.call(ctx, params)
	}
	return nil, &linerpc.Error{Code: linerpc.CodeMethodNotFound, Message: "method not found: " + method}
}

var toolSpec = map[string]any{
	"name": ToolName,
	"description": "Ask the user a question when you cannot proceed without their decision. " +
		"Use it sparingly: only for choices that change the result. The call waits until they answer.",
	"inputSchema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"question": map[string]any{"type": "string", "description": "The question, self-contained."},
			"options": map[string]any{"type": "array", "items": map[string]any{"type": "string"},
				"description": "Suggested answers, if the choice is between a few."},
		},
		"required":             []string{"question"},
		"additionalProperties": false,
	},
}

func (s *server) call(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		Name      string `json:"name"`
		Arguments struct {
			Question string   `json:"question"`
			Options  []string `json:"options"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &linerpc.Error{Code: codeInvalidParams, Message: "invalid params: " + err.Error()}
	}
	if p.Name != ToolName {
		return nil, &linerpc.Error{Code: codeInvalidParams, Message: "unknown tool: " + p.Name}
	}
	q := strings.TrimSpace(p.Arguments.Question)
	if q == "" || len(q) > maxQuestion {
		return toolResult(fmt.Sprintf("ask_user needs a question of 1 to %d characters.", maxQuestion), true), nil
	}
	answer, err := s.ask.Ask(ctx, q, p.Arguments.Options)
	switch {
	case errors.Is(err, context.Canceled) || ctx.Err() != nil:
		return nil, &linerpc.Error{Code: linerpc.CodeServer, Message: "cancelled"}
	case err != nil:
		//nolint:nilerr // the agent gets a usable answer; failing the tool call helps no one
		return toolResult("OpenChef could not reach the user ("+err.Error()+"). "+Unanswered, false), nil
	case answer == "":
		return toolResult(Unanswered, false), nil
	}
	return toolResult(answer, false), nil
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []any{map[string]any{"type": "text", "text": text}},
		"isError": isError,
	}
}
