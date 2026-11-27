package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ugurboz/openchef/internal/permissions"
	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/service"
	"github.com/ugurboz/openchef/internal/task"
)

// asker queues a request for the user and waits for the answer (service.Service.Ask).
type asker interface {
	Ask(ctx context.Context, req service.Request) (task.Approval, error)
}

// approver decides one session's permission requests (§9.2): the evaluator answers
// allow and deny at once; what needs asking, and every question, goes to the user
// through the approval queue (D-022).
type approver struct {
	queue asker
	eval  *permissions.Evaluator
	task  task.ID
	agent string
}

const unansweredQuestion = "No one is available to answer right now. Make the most reasonable choice, " +
	"say which one you made in your final answer, and continue."

func (a approver) Approve(ctx context.Context, req runtime.ApprovalRequest) (runtime.Decision, error) {
	reason := ""
	if req.Kind != runtime.ApproveModeChange {
		v := a.eval.Evaluate(permissionRequest(&req))
		switch v.Decision {
		case permissions.Allow:
			return runtime.Allow(), nil
		case permissions.Deny:
			msg := "OpenChef refused this: " + v.Reason
			if v.Rule != "" {
				msg += " (" + v.Rule + ")"
			}
			return runtime.Deny(msg + "."), nil
		case permissions.Ask:
			reason = v.Reason
		}
	}
	return a.ask(ctx, &req, reason)
}

// ask puts a request to the user.
func (a approver) ask(ctx context.Context, req *runtime.ApprovalRequest, reason string) (runtime.Decision, error) {
	raw, err := json.Marshal(struct {
		Request *runtime.ApprovalRequest `json:"request"`
		Reason  string                   `json:"reason,omitempty"`
	}{req, reason})
	if err != nil {
		return runtime.Deny("OpenChef could not record this request, so it refused it."), nil //nolint:nilerr // the agent is told; the turn goes on
	}
	got, err := a.queue.Ask(ctx, service.Request{Task: a.task, Agent: a.agent, Kind: kindOf(req.Kind),
		Tool: req.Tool, Summary: summary(req), Raw: raw})
	if err != nil {
		if ctx.Err() != nil {
			return runtime.Decision{}, fmt.Errorf("app: approval: %w", err)
		}
		return runtime.Deny("OpenChef could not ask the user, so it refused this: " + err.Error()), nil
	}
	switch got.Status {
	case task.ApprovalAllowed:
		return runtime.Allow(), nil
	case task.ApprovalDenied:
		msg := "The user denied this."
		if got.Answer != "" {
			msg += " They said: " + got.Answer
		}
		return runtime.Deny(msg), nil
	default:
		return runtime.Deny(got.Answer + " OpenChef denied the request."), nil
	}
}

// permissionRequest is what the evaluator decides on. Tool names follow Claude Code
// (every adapter normalizes to them); a request without one is judged by its kind.
func permissionRequest(req *runtime.ApprovalRequest) permissions.Request {
	tool := req.Tool
	switch {
	case tool == "" && req.Kind == runtime.ApproveCommand:
		tool = "Bash"
	case tool == "" && req.Kind == runtime.ApproveFileChange:
		tool = "Edit"
	}
	out := permissions.Request{Tool: tool, Command: req.Command}
	if tool != "Bash" {
		out.Paths = req.Paths
	}
	if tool == "WebFetch" {
		var in struct {
			URL string `json:"url"`
		}
		_ = json.Unmarshal(req.Input, &in) // no URL: the evaluator asks
		out.URL = in.URL
	}
	return out
}

func (a approver) Answer(ctx context.Context, q runtime.Question) (string, error) {
	got, err := a.queue.Ask(ctx, service.Request{Task: a.task, Agent: a.agent, Kind: task.ApprovalQuestion,
		Summary: q.Text, Options: q.Options})
	if err != nil && ctx.Err() != nil {
		return "", fmt.Errorf("app: question: %w", err)
	}
	if err != nil || got.Status != task.ApprovalAnswered {
		return unansweredQuestion, nil //nolint:nilerr // a question nobody can answer is not an error for the agent
	}
	return got.Answer, nil
}

func kindOf(k runtime.ApprovalKind) task.ApprovalKind {
	switch k {
	case runtime.ApproveCommand:
		return task.ApprovalCommand
	case runtime.ApproveFileChange:
		return task.ApprovalFileChange
	case runtime.ApproveModeChange:
		return task.ApprovalModeChange
	case runtime.ApproveTool:
		return task.ApprovalTool
	}
	return task.ApprovalTool
}

// summary is what the user decides on: the whole command, the paths, or the tool's
// input. It is never shortened.
func summary(req *runtime.ApprovalRequest) string {
	switch {
	case req.Command != "":
		return req.Command
	case len(req.Paths) > 0:
		return strings.Join(req.Paths, ", ")
	case len(req.Input) > 0 && string(req.Input) != "null":
		return string(req.Input)
	case req.Description != "":
		return req.Description
	}
	return "(no details given)"
}

var errNoQueue = errors.New("app: no approval queue")

// lateQueue lets the engine's session specs reach the service, which is built after
// the engine factory that needs it.
type lateQueue struct{ svc *service.Service }

func (l *lateQueue) Ask(ctx context.Context, req service.Request) (task.Approval, error) {
	if l.svc == nil {
		return task.Approval{}, errNoQueue
	}
	a, err := l.svc.Ask(ctx, req)
	if err != nil {
		return a, fmt.Errorf("app: %w", err)
	}
	return a, nil
}
