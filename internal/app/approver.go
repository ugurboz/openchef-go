package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ugurboz/openchef/internal/pathguard"
	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/service"
	"github.com/ugurboz/openchef/internal/task"
)

// asker queues a request for the user and waits for the answer (service.Service.Ask).
type asker interface {
	Ask(ctx context.Context, req service.Request) (task.Approval, error)
}

// approver is the permission policy until the evaluator is wired in (M4.4, D-020,
// D-022): file changes inside the task's worktree are allowed, except under the
// invariant directories (.git, .openchef, .claude) and outside the worktree, which are
// denied; everything else (shell commands, other tools) and every question goes to the
// user through the approval queue.
type approver struct {
	queue asker
	task  task.ID
	agent string
	dir   string
}

const unansweredQuestion = "No one is available to answer right now. Make the most reasonable choice, " +
	"say which one you made in your final answer, and continue."

func (a approver) Approve(ctx context.Context, req runtime.ApprovalRequest) (runtime.Decision, error) {
	if req.Kind == runtime.ApproveFileChange {
		return a.fileChange(req), nil
	}
	raw, err := json.Marshal(req)
	if err != nil {
		return runtime.Deny("OpenChef could not record this request, so it refused it."), nil //nolint:nilerr // the agent is told; the turn goes on
	}
	got, err := a.queue.Ask(ctx, service.Request{Task: a.task, Agent: a.agent, Kind: kindOf(req.Kind),
		Tool: req.Tool, Summary: summary(&req), Raw: raw})
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

// fileChange allows writes inside the worktree and refuses the rest with the reason.
func (a approver) fileChange(req runtime.ApprovalRequest) runtime.Decision {
	if len(req.Paths) == 0 {
		return runtime.Deny("OpenChef could not tell which files this change touches, so it refused it.")
	}
	for _, p := range req.Paths {
		why, err := pathguard.Check(a.dir, p)
		if err != nil {
			return runtime.Deny("OpenChef could not check this path, so it refused the change: " + err.Error())
		}
		if why != "" {
			return runtime.Deny(strings.ToUpper(why[:1]) + why[1:] + ".")
		}
	}
	return runtime.Allow()
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
