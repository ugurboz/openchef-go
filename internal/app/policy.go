package app

import (
	"errors"
	"fmt"
	"os"

	"github.com/ugurboz/openchef/internal/config"
	"github.com/ugurboz/openchef/internal/permissions"
	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/task"
)

// policies builds each session's permission policy from the settings files (§9.1):
// the user's ~/.openchef/settings.toml and the repository's project and local files,
// read again for every session so edits apply to the next one.
type policies struct {
	settings string // user settings file
	home     string // the user's home directory, for ~/ rules
	// private are OpenChef's own files, which no agent may read or write.
	private []string
}

func newPolicies(paths config.Paths) (policies, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return policies{}, fmt.Errorf("app: locate home directory: %w", err)
	}
	// Everything under the OpenChef home except the worktrees agents work in.
	private := []string{paths.Config, paths.Settings, paths.State, paths.Logs, paths.Sessions, paths.Socket, paths.Lock}
	return policies{settings: paths.Settings, home: home, private: private}, nil
}

// defaultMode is a repository's configured default permission mode.
func (p policies) defaultMode(repoRoot string) (task.PermissionMode, error) {
	pol, err := permissions.Load(p.settings, repoRoot)
	if err != nil {
		return "", fmt.Errorf("app: %w", err)
	}
	return task.PermissionMode(pol.Mode), nil
}

var errBypassOutsideWorktree = errors.New("app: bypassPermissions is only allowed in an isolated worktree")

// session is one session's policy: the evaluator its approver consults, and the rules
// passed natively to CLIs that decide some requests themselves (§9.3).
type session struct {
	mode      runtime.Mode
	eval      *permissions.Evaluator
	denyRules []string
	askRules  []string
}

func (p policies) session(t *task.Task) (session, error) {
	pol, err := permissions.Load(p.settings, t.RepoRoot)
	if err != nil {
		return session{}, fmt.Errorf("app: %w", err)
	}
	mode, err := runtime.ParseMode(string(t.Permissions))
	if err != nil {
		return session{}, fmt.Errorf("app: task %s: %w", t.ID, err)
	}
	if mode == runtime.ModeBypassPermissions && (t.Worktree == "" || t.Mode != task.ModeIsolated) {
		return session{}, errBypassOutsideWorktree
	}
	pol.Mode = mode // the task's mode, chosen when it was created, wins over defaultMode
	eval, err := permissions.NewEvaluator(pol, t.Worktree, p.home, p.private)
	if err != nil {
		return session{}, fmt.Errorf("app: %w", err)
	}
	s := session{mode: mode, eval: eval, denyRules: append([]string(nil), runtime.InvariantDenyRules...)}
	for _, r := range pol.Deny {
		s.denyRules = append(s.denyRules, r.Raw)
	}
	for _, r := range pol.Ask {
		s.askRules = append(s.askRules, r.Raw)
	}
	return s, nil
}
