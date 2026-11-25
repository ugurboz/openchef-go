package engine

import (
	"fmt"
	"strings"

	"github.com/ugurboz/openchef/internal/task"
	"github.com/ugurboz/openchef/internal/verify"
)

// Every message restates the step in full (§7.2), so a resumed or fresh session never
// depends on what an earlier turn said.

func restate(t *task.Task, s *task.Step, total int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Step %d of %d: %s\n\n%s\n", s.Index, total, s.Title, strings.TrimSpace(s.Prompt))
	if len(s.Files) > 0 {
		fmt.Fprintf(&b, "\nScope: work only in %s.\n", strings.Join(s.Files, ", "))
	}
	if s.Verify != "" {
		fmt.Fprintf(&b, "\nWhen you finish, this command must pass in the workspace: %s\n", s.Verify)
	}
	b.WriteString("\nOpenChef commits your changes after verification. Do not run git commands that " +
		"create commits or move refs (commit, reset, rebase, checkout, switch, merge, push, tag).\n")
	if t.Kind == task.KindPipeline {
		fmt.Fprintf(&b, "\nOverall request: %s\n", strings.TrimSpace(t.Prompt))
	}
	return b.String()
}

func stepPrompt(t *task.Task, s *task.Step, total int) string { return restate(t, s, total) }

func retryPrompt(t *task.Task, s *task.Step, total int, res *verify.Result) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Verification %s. Fix the problem and finish the step.\n\n", res.Summary())
	b.WriteString(restate(t, s, total))
	fmt.Fprintf(&b, "\nOutput of %s:\n```\n%s\n```\n", s.Verify, strings.TrimRight(res.Output, "\n"))
	return b.String()
}

func loopPrompt(t *task.Task, s *task.Step, total, repeats int) string {
	return fmt.Sprintf("You repeated the same tool call %d times in a row, so OpenChef stopped the turn. "+
		"Step back, find out why it is not working, and take a different approach.\n\n%s",
		repeats, restate(t, s, total))
}

func stallPrompt(t *task.Task, s *task.Step, total int, why string) string {
	return fmt.Sprintf("OpenChef stopped the last turn: %s. Continue from the current state of the "+
		"workspace and finish the step.\n\n%s", why, restate(t, s, total))
}

func resumePrompt(t *task.Task, s *task.Step, total int) string {
	return "Your previous process ended unexpectedly. Check the current state of the workspace " +
		"and continue.\n\n" + restate(t, s, total)
}

func handoffPrompt(t *task.Task, s *task.Step, total int, from, lastCommit, patch string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are taking over this step from another agent (%s) that ran out of quota. ", from)
	fmt.Fprintf(&b, "The workspace has been reset to the last verified commit %s.", short(lastCommit))
	if patch != "" {
		fmt.Fprintf(&b, " Its unfinished work is saved in %s; read it, and apply it with "+
			"`git apply --binary %s` if it helps.", patch, patch)
	}
	b.WriteString("\n\n")
	b.WriteString(restate(t, s, total))
	return b.String()
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
