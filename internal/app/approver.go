package app

import (
	"context"
	"path/filepath"
	"slices"
	"strings"

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

var protectedDirs = []string{".git", ".openchef", ".claude"}

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
	if !filepath.IsAbs(p) {
		p = filepath.Join(a.dir, p)
	}
	rel, err := filepath.Rel(a.dir, filepath.Clean(p))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "Only files inside the task's workspace may be changed: " + p
	}
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if slices.Contains(protectedDirs, part) {
			return "Files under " + part + " are managed by OpenChef and may not be changed."
		}
	}
	return ""
}

func (workspaceApprover) Answer(context.Context, runtime.Question) (string, error) {
	return "No one is available to answer right now. Make the most reasonable choice, " +
		"say which one you made in your final answer, and continue.", nil
}
