package gitws

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ControlDir is the per-worktree directory OpenChef owns (patches, scratch files). Its
// contents are never staged or cleaned.
const ControlDir = ".openchef"

// ErrWorktreeMismatch means a path that should hold a task worktree holds something else.
var ErrWorktreeMismatch = errors.New("gitws: path is not the expected worktree")

// Worktree is one task's (or team step's) linked worktree.
type Worktree struct {
	Path   string
	Branch string
	git    runner
}

// CreateWorktree creates branch at base and checks it out in a new linked worktree at
// path. It is idempotent for recovery: if path already is a worktree of this repository
// on branch, it is returned as is; anything else at path is ErrWorktreeMismatch.
func (r *Repo) CreateWorktree(ctx context.Context, path, branch, base string) (*Worktree, error) {
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("gitws: worktree path %q is not absolute", path)
	}
	if err := r.checkBranchName(ctx, branch); err != nil {
		return nil, err
	}
	wt := &Worktree{Path: path, Branch: branch, git: r.git}
	if _, err := os.Stat(path); err == nil {
		if err := r.checkExisting(ctx, wt); err != nil {
			return nil, err
		}
		return wt, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("gitws: stat %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("gitws: create worktree parent: %w", err)
	}
	if _, err := r.git.run(ctx, r.Root, nil, "worktree", "add", "--quiet", "-b", branch, path, base+"^{commit}"); err != nil {
		return nil, err
	}
	return wt, nil
}

func (r *Repo) checkBranchName(ctx context.Context, branch string) error {
	if _, err := r.git.run(ctx, r.Root, nil, "check-ref-format", "--branch", branch); err != nil {
		return fmt.Errorf("gitws: invalid branch name %q: %w", branch, err)
	}
	return nil
}

func (r *Repo) checkExisting(ctx context.Context, wt *Worktree) error {
	top, err := wt.git.line(ctx, wt.Path, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrWorktreeMismatch, wt.Path, err)
	}
	common, err := wt.git.line(ctx, wt.Path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrWorktreeMismatch, wt.Path, err)
	}
	branch, err := wt.git.line(ctx, wt.Path, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return fmt.Errorf("%w: %s has a detached HEAD", ErrWorktreeMismatch, wt.Path)
	}
	if !samePath(top, wt.Path) || !samePath(filepath.Dir(common), r.Root) || branch != wt.Branch {
		return fmt.Errorf("%w: %s (top %s, repository %s, branch %s)",
			ErrWorktreeMismatch, wt.Path, top, filepath.Dir(common), branch)
	}
	return nil
}

func samePath(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

// RemoveWorktree removes the worktree at path (forcefully: it holds only OpenChef's
// work) and prunes git's record of it. With deleteBranch, the branch is deleted too.
// A path that no longer exists is not an error.
func (r *Repo) RemoveWorktree(ctx context.Context, path, branch string, deleteBranch bool) error {
	if _, err := os.Stat(path); err == nil {
		if _, err := r.git.run(ctx, r.Root, nil, "worktree", "remove", "--force", "--force", path); err != nil {
			return err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("gitws: stat %s: %w", path, err)
	}
	if _, err := r.git.run(ctx, r.Root, nil, "worktree", "prune"); err != nil {
		return err
	}
	if deleteBranch && branch != "" {
		if _, err := r.git.run(ctx, r.Root, nil, "branch", "-D", "--", branch); err != nil {
			var ge *GitError
			if errors.As(err, &ge) && strings.Contains(ge.Stderr, "not found") {
				return nil
			}
			return err
		}
	}
	return nil
}

// Head returns the worktree's current commit.
func (w *Worktree) Head(ctx context.Context) (string, error) {
	return w.git.line(ctx, w.Path, "rev-parse", "--verify", "HEAD^{commit}")
}

// Changed returns the paths with uncommitted changes (modified, added, deleted,
// untracked; renames as both sides), excluding ignored files and ControlDir.
func (w *Worktree) Changed(ctx context.Context) ([]string, error) {
	out, err := w.git.run(ctx, w.Path, nil, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames")
	if err != nil {
		return nil, err
	}
	var paths []string
	for entry := range bytes.SplitSeq(out, []byte{0}) {
		if len(entry) < 4 {
			continue
		}
		p := string(entry[3:])
		if p == ControlDir || strings.HasPrefix(p, ControlDir+"/") {
			continue
		}
		paths = append(paths, p)
	}
	return paths, nil
}
