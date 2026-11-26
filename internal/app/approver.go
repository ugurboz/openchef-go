package app

import (
	"context"
	"strings"

	"github.com/ugurboz/openchef/internal/pathguard"
	"github.com/ugurboz/openchef/internal/runtime"
)

// workspaceApprover is the M3 permission policy until the evaluator and approval queue of
// M4 (§9) exist: file changes inside the task's worktree are allowed, except under the
// invariant directories (.git, .openchef, .claude); shell commands and other tools are
// refused with an explanation. OpenChef runs the step's verification itself, so a step
// still completes without the agent running commands.
type workspaceApprover struct {
	dir string
}

const commandsDenied = "OpenChef M3 does not let agents run shell commands or other tools yet " +
	"(permissions arrive in M4). Edit files only; OpenChef runs the step's verification command itself."

func (a workspaceApprover) Approve(_ context.Context, req runtime.ApprovalRequest) (runtime.Decision, error) {
	if req.Kind != runtime.ApproveFileChange {
		return runtime.Deny(commandsDenied), nil
	}
	if len(req.Paths) == 0 {
		return runtime.Deny("OpenChef could not tell which files this change touches, so it refused it."), nil
	}
	for _, p := range req.Paths {
		if why := a.outside(p); why != "" {
			return runtime.Deny(why), nil
		}
	}
	return runtime.Allow(), nil
}

// outside explains why p may not be written, or returns "".
func (a workspaceApprover) outside(p string) string {
	why, err := pathguard.Check(a.dir, p)
	if err != nil {
		return "OpenChef could not check this path, so it refused the change: " + err.Error()
	}
	if why != "" {
		return strings.ToUpper(why[:1]) + why[1:] + "."
	}
	return ""
}

func (workspaceApprover) Answer(context.Context, runtime.Question) (string, error) {
	return "No one is available to answer right now. Make the most reasonable choice, " +
		"say which one you made in your final answer, and continue.", nil
}
