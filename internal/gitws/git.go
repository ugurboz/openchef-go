// Package gitws owns every git operation OpenChef performs (docs/architecture.md §8):
// task worktrees outside the user's repository, step commits with OpenChef trailers,
// and patch capture and reset on failover. Agents never move refs; this package does.
//
// The user's working tree and current branch are never written here. Requires git 2.25
// or newer (--pathspec-from-file).
package gitws

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode"
)

// ErrNotRepository means a path is not inside a git working tree.
var ErrNotRepository = errors.New("gitws: not a git repository")

// GitError is a failed git invocation with its stderr.
type GitError struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *GitError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" {
		msg = e.Err.Error()
	}
	return fmt.Sprintf("gitws: git %s: %s", strings.Join(e.Args, " "), msg)
}

func (e *GitError) Unwrap() error { return e.Err }

// runner executes git with a controlled environment.
type runner struct {
	bin string
	env []string
}

func newRunner() (runner, error) {
	bin, err := exec.LookPath("git")
	if err != nil {
		return runner{}, fmt.Errorf("gitws: git not found: %w", err)
	}
	return runner{bin: bin, env: gitEnv(os.Environ())}, nil
}

// gitEnv drops variables that would redirect git (GIT_DIR, GIT_WORK_TREE, GIT_INDEX_FILE,
// ...) and makes output stable and non-interactive.
func gitEnv(from []string) []string {
	out := make([]string, 0, len(from)+3)
	for _, kv := range from {
		k, _, _ := strings.Cut(kv, "=")
		up := strings.ToUpper(k)
		if strings.HasPrefix(up, "GIT_") || up == "LC_ALL" || up == "LANG" || up == "LANGUAGE" {
			continue
		}
		out = append(out, kv)
	}
	return append(out, "GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "GIT_OPTIONAL_LOCKS=0")
}

// run executes git in dir and returns stdout.
func (r runner) run(ctx context.Context, dir string, stdin []byte, args ...string) ([]byte, error) {
	full := append([]string{"-C", dir, "-c", "core.quotePath=false"}, args...)
	//nolint:gosec // G204: the binary is git from PATH and arguments are built by this package.
	cmd := exec.CommandContext(ctx, r.bin, full...)
	cmd.Env = r.env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), &GitError{Args: args, Stderr: stderr.String(), Err: err}
	}
	return stdout.Bytes(), nil
}

func (r runner) line(ctx context.Context, dir string, args ...string) (string, error) {
	out, err := r.run(ctx, dir, nil, args...)
	return strings.TrimSpace(string(out)), err
}

// Repo is the user's repository.
type Repo struct {
	// Root is the absolute path of the main working tree.
	Root string
	git  runner
}

// Open finds the repository containing dir. A linked worktree resolves to its main
// working tree, so tasks started from anywhere in a repository share one repo ID.
func Open(ctx context.Context, dir string) (*Repo, error) {
	g, err := newRunner()
	if err != nil {
		return nil, err
	}
	return openWith(ctx, dir, g)
}

func openWith(ctx context.Context, dir string, g runner) (*Repo, error) {
	common, err := g.line(ctx, dir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %w", ErrNotRepository, dir, err)
	}
	if bare, err := g.line(ctx, dir, "rev-parse", "--is-bare-repository"); err != nil || bare == "true" {
		return nil, fmt.Errorf("%w: %s is bare or unreadable", ErrNotRepository, dir)
	}
	root := filepath.Dir(filepath.Clean(common))
	if filepath.Base(common) != ".git" {
		// Unusual layout (separate git dir): fall back to this tree's top level.
		if root, err = g.line(ctx, dir, "rev-parse", "--show-toplevel"); err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrNotRepository, dir, err)
		}
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	return &Repo{Root: root, git: g}, nil
}

// ID is a stable identifier of the repository for paths under ~/.openchef/worktrees.
func (r *Repo) ID() string {
	sum := sha256.Sum256([]byte(r.Root))
	name := strings.Map(func(c rune) rune {
		if c > unicode.MaxASCII || (!unicode.IsLetter(c) && !unicode.IsDigit(c) && c != '-' && c != '_' && c != '.') {
			return '_'
		}
		return c
	}, filepath.Base(r.Root))
	return strings.TrimLeft(name, ".") + "-" + hex.EncodeToString(sum[:])[:12]
}

// Head returns the commit the user's checkout is on.
func (r *Repo) Head(ctx context.Context) (string, error) {
	return r.git.line(ctx, r.Root, "rev-parse", "--verify", "HEAD^{commit}")
}
