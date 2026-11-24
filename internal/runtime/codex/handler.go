package codex

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/runtime/linerpc"
)

// handleRequest answers requests Codex sends to OpenChef.
func (s *session) handleRequest(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case "item/commandExecution/requestApproval":
		var p commandApprovalParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, invalidParams(method, err)
		}
		req := runtime.ApprovalRequest{
			Kind: runtime.ApproveCommand, Tool: "Bash", Command: p.Command,
			Description: p.Reason, Raw: params,
		}
		if p.Cwd != "" {
			req.Paths = []string{p.Cwd}
		}
		d := s.approve(ctx, req)
		return map[string]any{"decision": v2Decision(d)}, nil

	case "item/fileChange/requestApproval":
		var p fileChangeApprovalParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, invalidParams(method, err)
		}
		s.mu.Lock()
		paths := append([]string(nil), s.items[p.ItemID]...)
		s.mu.Unlock()
		if p.GrantRoot != "" {
			paths = append(paths, p.GrantRoot)
		}
		d := s.approve(ctx, runtime.ApprovalRequest{
			Kind: runtime.ApproveFileChange, Tool: "Edit", Paths: paths,
			Description: p.Reason, Raw: params,
		})
		return map[string]any{"decision": v2Decision(d)}, nil

	case "mcpServer/elicitation/request":
		var p elicitationParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, invalidParams(method, err)
		}
		if p.Meta.ApprovalKind != "mcp_tool_call" {
			// Free-form elicitations (forms) are not supported; declining lets the
			// agent continue without the answer.
			return map[string]any{"action": "decline"}, nil
		}
		if s.spec.AskUser != nil && p.ServerName == s.spec.AskUser.Name && p.Meta.ToolName == askUserToolName {
			return map[string]any{"action": "accept", "content": map[string]any{}}, nil
		}
		d := s.approve(ctx, runtime.ApprovalRequest{
			Kind: runtime.ApproveTool, Tool: "mcp__" + p.ServerName + "__" + p.Meta.ToolName,
			Input: p.Meta.ToolParams, Description: p.Message, Raw: params,
		})
		if d.Allow {
			return map[string]any{"action": "accept", "content": map[string]any{}}, nil
		}
		return map[string]any{"action": "decline"}, nil

	case "item/tool/requestUserInput":
		var p userInputParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, invalidParams(method, err)
		}
		answers := map[string]any{}
		for _, q := range p.Questions {
			opts := make([]string, 0, len(q.Options))
			for _, o := range q.Options {
				opts = append(opts, o.Label)
			}
			ans, err := s.spec.Approver.Answer(ctx, runtime.Question{Session: s.Ref(), Text: q.Question, Options: opts})
			if err != nil {
				return nil, fmt.Errorf("answer question: %w", err)
			}
			answers[q.ID] = map[string]any{"answers": []string{ans}}
		}
		return map[string]any{"answers": answers}, nil

	case "execCommandApproval", "applyPatchApproval":
		// Legacy (v1) approval requests. Treated like their v2 counterparts.
		kind, tool := runtime.ApproveCommand, "Bash"
		if method == "applyPatchApproval" {
			kind, tool = runtime.ApproveFileChange, "Edit"
		}
		d := s.approve(ctx, runtime.ApprovalRequest{Kind: kind, Tool: tool, Raw: params})
		decision := "denied"
		if d.Allow {
			decision = "approved"
			if d.ForSession {
				decision = "approved_for_session"
			}
		}
		return map[string]any{"decision": decision}, nil

	case "account/chatgptAuthTokens/refresh":
		return nil, &linerpc.Error{Code: linerpc.CodeServer, Message: "OpenChef does not manage Codex credentials; run `codex login`"}

	default:
		return nil, &linerpc.Error{Code: linerpc.CodeMethodNotFound, Message: "OpenChef does not handle " + method}
	}
}

// approve asks the approver. Failures to decide (cancelled context, approver error)
// deny: an undecided request must never run.
func (s *session) approve(ctx context.Context, req runtime.ApprovalRequest) runtime.Decision {
	req.Session = s.Ref()
	d, err := s.spec.Approver.Approve(ctx, req)
	if err != nil {
		d = runtime.Deny("OpenChef could not decide this request: " + err.Error())
	}
	if !d.Allow {
		s.mu.Lock()
		ts := s.turn
		s.mu.Unlock()
		if ts != nil {
			ts.mu.Lock()
			ts.denials++
			ts.mu.Unlock()
		}
	}
	return d
}

func v2Decision(d runtime.Decision) string {
	switch {
	case d.Allow && d.ForSession:
		return "acceptForSession"
	case d.Allow:
		return "accept"
	default:
		return "decline"
	}
}

func invalidParams(method string, err error) error {
	return &linerpc.Error{Code: -32602, Message: fmt.Sprintf("invalid %s params: %v", method, err)}
}
