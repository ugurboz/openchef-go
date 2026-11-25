package gitws

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Commit trailer keys (§8.2).
const (
	TrailerTask  = "OpenChef-Task"
	TrailerStep  = "OpenChef-Step"
	TrailerAgent = "OpenChef-Agent"
	TrailerPool  = "OpenChef-Pool"
)

// fallbackIdentity is used only when the user has no git identity configured.
var fallbackIdentity = []string{"-c", "user.name=OpenChef", "-c", "user.email=openchef@localhost"}

// StepCommit describes the commit that records a verified step.
type StepCommit struct {
	Task  string
	Step  int
	Agent string
	Pool  string
	// Subject is the first line of the message, e.g. the step title.
	Subject string
}

func (c *StepCommit) message() (string, error) {
	if c.Task == "" || c.Step < 1 || c.Agent == "" || c.Pool == "" {
		return "", fmt.Errorf("gitws: step commit needs task, step, agent and pool: %+v", *c)
	}
	subject := strings.TrimSpace(strings.SplitN(c.Subject, "\n", 2)[0])
	if subject == "" {
		subject = fmt.Sprintf("OpenChef step %d", c.Step)
	}
	return fmt.Sprintf("%s\n\n%s: %s\n%s: %d\n%s: %s\n%s: %s\n", subject,
		TrailerTask, c.Task, TrailerStep, c.Step, TrailerAgent, c.Agent, TrailerPool, c.Pool), nil
}

// CommitStep commits the agent's changes (Changed, so never ControlDir or ignored files)
// as the record of a verified step and returns the commit. It is idempotent: if the
// branch already has a commit for this task and step since base (a replay after a
// crash), that commit is returned and nothing new is committed. A step without changes
// still gets an (empty) commit, so every verified step is recorded in history. Hooks do
// not run.
func (w *Worktree) CommitStep(ctx context.Context, base string, c StepCommit) (string, error) {
	msg, err := c.message()
	if err != nil {
		return "", err
	}
	if sha, ok, err := w.FindStepCommit(ctx, base, c.Task, c.Step); err != nil || ok {
		return sha, err
	}
	paths, err := w.Changed(ctx)
	if err != nil {
		return "", err
	}
	if len(paths) > 0 {
		spec := []byte(strings.Join(paths, "\x00") + "\x00")
		if _, err := w.git.run(ctx, w.Path, spec, "add", "--all", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
			return "", err
		}
	}
	args := []string{"commit", "--quiet", "--no-verify", "--allow-empty", "--cleanup=verbatim", "--file=-"}
	if !w.hasIdentity(ctx) {
		args = append(append([]string(nil), fallbackIdentity...), args...)
	}
	if _, err := w.git.run(ctx, w.Path, []byte(msg), args...); err != nil {
		return "", err
	}
	return w.Head(ctx)
}

func (w *Worktree) hasIdentity(ctx context.Context) bool {
	name, err1 := w.git.line(ctx, w.Path, "config", "user.name")
	email, err2 := w.git.line(ctx, w.Path, "config", "user.email")
	return err1 == nil && err2 == nil && name != "" && email != ""
}

// FindStepCommit looks for the commit recording task's step on the branch since base.
func (w *Worktree) FindStepCommit(ctx context.Context, base, task string, step int) (sha string, found bool, err error) {
	format := "%H%x1f%(trailers:key=" + TrailerTask + ",valueonly,separator=%x2C)%x1f" +
		"%(trailers:key=" + TrailerStep + ",valueonly,separator=%x2C)%x1e"
	out, err := w.git.run(ctx, w.Path, nil, "log", "--format="+format, base+"..HEAD")
	if err != nil {
		return "", false, err
	}
	want := strconv.Itoa(step)
	for rec := range bytes.SplitSeq(out, []byte{0x1e}) {
		fields := strings.Split(strings.TrimSpace(string(rec)), "\x1f")
		if len(fields) != 3 {
			continue
		}
		if strings.TrimSpace(fields[1]) == task && strings.TrimSpace(fields[2]) == want {
			return fields[0], true, nil
		}
	}
	return "", false, nil
}

// CapturePatch saves every uncommitted change (staged, unstaged, untracked; binary
// included; ControlDir excluded) to file, a path inside ControlDir by convention
// (".openchef/patches/step-N-<agent>.patch"). It reports whether there was anything to
// save; no file is written otherwise. The index is left as HEAD's.
func (w *Worktree) CapturePatch(ctx context.Context, file string) (bool, error) {
	exclude := ":(exclude,top)" + ControlDir
	if _, err := w.git.run(ctx, w.Path, nil, "add", "--all", "--", ".", exclude); err != nil {
		return false, err
	}
	patch, diffErr := w.git.run(ctx, w.Path, nil, "diff", "--cached", "--binary", "--no-color", "--no-ext-diff")
	_, resetErr := w.git.run(ctx, w.Path, nil, "reset", "--quiet")
	if err := errors.Join(diffErr, resetErr); err != nil {
		return false, err
	}
	if len(patch) == 0 {
		return false, nil
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return false, fmt.Errorf("gitws: create patch directory: %w", err)
	}
	if err := os.WriteFile(file, patch, 0o600); err != nil {
		return false, fmt.Errorf("gitws: write patch: %w", err)
	}
	return true, nil
}

// ResetTo discards every uncommitted change and moves the worktree's branch to commit
// (the last verified step), keeping ControlDir.
func (w *Worktree) ResetTo(ctx context.Context, commit string) error {
	if _, err := w.git.run(ctx, w.Path, nil, "reset", "--hard", "--quiet", commit+"^{commit}"); err != nil {
		return err
	}
	_, err := w.git.run(ctx, w.Path, nil, "clean", "-d", "--force", "--quiet", "--exclude=/"+ControlDir+"/")
	return err
}
