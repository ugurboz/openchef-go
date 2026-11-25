package gitws

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// testRunner isolates git from the developer's and CI's configuration.
func testRunner(t *testing.T) runner {
	t.Helper()
	bin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	env := append(gitEnv(os.Environ()), "HOME="+home, "XDG_CONFIG_HOME="+home, "USERPROFILE="+home,
		"GIT_CONFIG_NOSYSTEM=1")
	return runner{bin: bin, env: env}
}

// must runs git and fails the test on error.
func must(t *testing.T, g runner, dir string, args ...string) string {
	t.Helper()
	out, err := g.run(context.Background(), dir, nil, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// newRepo creates a repository with one commit, an identity, and an ignore rule.
func newRepo(t *testing.T) (*Repo, runner) {
	t.Helper()
	g := testRunner(t)
	dir := filepath.Join(t.TempDir(), "my repo")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	must(t, g, dir, "init", "--quiet", "--initial-branch=main")
	must(t, g, dir, "config", "user.name", "Test User")
	must(t, g, dir, "config", "user.email", "test@example.com")
	write(t, filepath.Join(dir, "README.md"), "hello\n")
	write(t, filepath.Join(dir, ".gitignore"), "*.log\n")
	write(t, filepath.Join(dir, "gone.txt"), "delete me\n")
	must(t, g, dir, "add", ".")
	must(t, g, dir, "commit", "--quiet", "-m", "initial")
	r, err := openWith(context.Background(), dir, g)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	return r, g
}

func newWorktree(t *testing.T, r *Repo) (wt *Worktree, base string) {
	t.Helper()
	base, err := r.Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "worktrees", r.ID(), "k3m9x2q7ab")
	wt, err = r.CreateWorktree(context.Background(), path, "openchef/k3m9x2q7ab", base)
	if err != nil {
		t.Fatalf("CreateWorktree: %v", err)
	}
	return wt, base
}

func TestOpenResolvesMainTree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, g := newRepo(t)
	sub := filepath.Join(r.Root, "pkg", "deep")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	fromSub, err := openWith(ctx, sub, g)
	if err != nil || fromSub.Root != r.Root {
		t.Fatalf("from subdir: %+v, %v", fromSub, err)
	}
	wt, _ := newWorktree(t, r)
	fromWT, err := openWith(ctx, wt.Path, g)
	if err != nil || fromWT.Root != r.Root || fromWT.ID() != r.ID() {
		t.Fatalf("from worktree: %+v, %v", fromWT, err)
	}
	if id := r.ID(); !strings.HasPrefix(id, "my_repo-") || len(id) != len("my_repo-")+12 {
		t.Errorf("ID = %q", id)
	}
	if _, err := openWith(ctx, t.TempDir(), g); !errors.Is(err, ErrNotRepository) {
		t.Errorf("non-repo = %v; want ErrNotRepository", err)
	}
}

// Creating and using a task worktree never touches the user's checkout.
func TestWorktreeLeavesUserTreeAlone(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, g := newRepo(t)
	write(t, filepath.Join(r.Root, "README.md"), "user edit in progress\n")
	write(t, filepath.Join(r.Root, "notes.txt"), "untracked\n")
	before := must(t, g, r.Root, "status", "--porcelain")
	branchBefore := must(t, g, r.Root, "symbolic-ref", "HEAD")

	wt, base := newWorktree(t, r)
	write(t, filepath.Join(wt.Path, "feature.go"), "package x\n")
	if _, err := wt.CommitStep(ctx, base, StepCommit{Task: "k3m9x2q7ab", Step: 1, Agent: "opus", Pool: "claude-max", Subject: "Add x"}); err != nil {
		t.Fatal(err)
	}
	if err := wt.ResetTo(ctx, base); err != nil {
		t.Fatal(err)
	}

	if after := must(t, g, r.Root, "status", "--porcelain"); after != before {
		t.Errorf("user status changed:\n%s\n→\n%s", before, after)
	}
	if b := must(t, g, r.Root, "symbolic-ref", "HEAD"); b != branchBefore {
		t.Errorf("user branch changed: %s → %s", branchBefore, b)
	}
	if got := read(t, filepath.Join(r.Root, "README.md")); got != "user edit in progress\n" {
		t.Errorf("user file changed: %q", got)
	}
	if strings.HasPrefix(wt.Path, r.Root) {
		t.Errorf("worktree %s inside the repository", wt.Path)
	}
}

func TestCreateWorktreeIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, _ := newRepo(t)
	wt, base := newWorktree(t, r)
	again, err := r.CreateWorktree(ctx, wt.Path, wt.Branch, base)
	if err != nil || again.Path != wt.Path {
		t.Fatalf("second create: %+v, %v", again, err)
	}
	if _, err := r.CreateWorktree(ctx, wt.Path, "openchef/other", base); !errors.Is(err, ErrWorktreeMismatch) {
		t.Errorf("other branch = %v; want ErrWorktreeMismatch", err)
	}
	plain := filepath.Join(t.TempDir(), "plain")
	if err := os.Mkdir(plain, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateWorktree(ctx, plain, "openchef/plain", base); !errors.Is(err, ErrWorktreeMismatch) {
		t.Errorf("plain dir = %v; want ErrWorktreeMismatch", err)
	}
	if _, err := r.CreateWorktree(ctx, "relative/path", "openchef/rel", base); err == nil {
		t.Error("relative path accepted")
	}
	if _, err := r.CreateWorktree(ctx, filepath.Join(t.TempDir(), "x"), "bad..name", base); err == nil {
		t.Error("invalid branch name accepted")
	}
}

func TestCommitStep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, g := newRepo(t)
	wt, base := newWorktree(t, r)
	write(t, filepath.Join(wt.Path, "api", "limit.go"), "package api\n")
	write(t, filepath.Join(wt.Path, "README.md"), "changed\n")
	write(t, filepath.Join(wt.Path, "debug.log"), "ignored\n")
	write(t, filepath.Join(wt.Path, ControlDir, "patches", "step-1-opus.patch"), "x")
	if err := os.Remove(filepath.Join(wt.Path, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	changed, err := wt.Changed(ctx)
	slices.Sort(changed)
	if err != nil || !slices.Equal(changed, []string{"README.md", "api/limit.go", "gone.txt"}) {
		t.Fatalf("Changed = %v, %v", changed, err)
	}

	c := StepCommit{Task: "k3m9x2q7ab", Step: 1, Agent: "opus", Pool: "claude-max", Subject: "Add rate limiting\nignored second line"}
	sha, err := wt.CommitStep(ctx, base, c)
	if err != nil {
		t.Fatalf("CommitStep: %v", err)
	}
	files := strings.Fields(must(t, g, wt.Path, "show", "--name-only", "--format=", sha))
	slices.Sort(files)
	if !slices.Equal(files, []string{"README.md", "api/limit.go", "gone.txt"}) {
		t.Errorf("committed files = %v", files)
	}
	msg := must(t, g, wt.Path, "log", "-1", "--format=%B", sha)
	for _, want := range []string{"Add rate limiting\n", "OpenChef-Task: k3m9x2q7ab", "OpenChef-Step: 1", "OpenChef-Agent: opus", "OpenChef-Pool: claude-max"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message lacks %q:\n%s", want, msg)
		}
	}
	if author := must(t, g, wt.Path, "log", "-1", "--format=%an <%ae>", sha); author != "Test User <test@example.com>" {
		t.Errorf("author = %s", author)
	}

	// A replay after a crash finds the existing commit instead of committing twice.
	write(t, filepath.Join(wt.Path, "later.go"), "package later\n")
	again, err := wt.CommitStep(ctx, base, c)
	if err != nil || again != sha {
		t.Fatalf("replay = %s, %v; want %s", again, err, sha)
	}
	if n := must(t, g, wt.Path, "rev-list", "--count", base+"..HEAD"); n != "1" {
		t.Errorf("%s commits after replay", n)
	}

	// A step with nothing to commit is still recorded.
	if err := os.Remove(filepath.Join(wt.Path, "later.go")); err != nil {
		t.Fatal(err)
	}
	empty, err := wt.CommitStep(ctx, base, StepCommit{Task: "k3m9x2q7ab", Step: 2, Agent: "gpt", Pool: "chatgpt-plus"})
	if err != nil || empty == sha {
		t.Fatalf("empty step commit = %s, %v", empty, err)
	}
	if found, ok, err := wt.FindStepCommit(ctx, base, "k3m9x2q7ab", 2); err != nil || !ok || found != empty {
		t.Errorf("FindStepCommit = %s, %t, %v", found, ok, err)
	}
	if _, ok, err := wt.FindStepCommit(ctx, base, "otherrtask", 1); err != nil || ok {
		t.Errorf("other task found: %t, %v", ok, err)
	}
	if _, err := wt.CommitStep(ctx, base, StepCommit{Task: "k3m9x2q7ab"}); err == nil {
		t.Error("incomplete StepCommit accepted")
	}
}

func TestCapturePatchAndReset(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, g := newRepo(t)
	wt, base := newWorktree(t, r)

	patch := filepath.Join(wt.Path, ControlDir, "patches", "step-1-opus.patch")
	if saved, err := wt.CapturePatch(ctx, patch); err != nil || saved {
		t.Fatalf("clean tree: saved = %t, %v", saved, err)
	}
	if _, err := os.Stat(patch); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("patch written for a clean tree: %v", err)
	}

	write(t, filepath.Join(wt.Path, "README.md"), "partial work\n")
	write(t, filepath.Join(wt.Path, "new", "file.go"), "package new\n")
	if err := os.WriteFile(filepath.Join(wt.Path, "blob.bin"), []byte{0, 1, 2, 255, 0}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(wt.Path, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(wt.Path, "trace.log"), "ignored stays out\n")

	saved, err := wt.CapturePatch(ctx, patch)
	if err != nil || !saved {
		t.Fatalf("CapturePatch = %t, %v", saved, err)
	}
	if body := read(t, patch); strings.Contains(body, "trace.log") || strings.Contains(body, ControlDir) {
		t.Errorf("patch includes ignored or control files")
	}
	if err := wt.ResetTo(ctx, base); err != nil {
		t.Fatalf("ResetTo: %v", err)
	}
	if changed, err := wt.Changed(ctx); err != nil || len(changed) != 0 {
		t.Fatalf("after reset: %v, %v", changed, err)
	}
	if _, err := os.Stat(patch); err != nil {
		t.Fatalf("reset removed the saved patch: %v", err)
	}

	// The handoff agent (or a human) can re-apply the saved work exactly.
	must(t, g, wt.Path, "apply", "--binary", patch)
	if got := read(t, filepath.Join(wt.Path, "README.md")); got != "partial work\n" {
		t.Errorf("README after apply = %q", got)
	}
	if b, err := os.ReadFile(filepath.Join(wt.Path, "blob.bin")); err != nil || !slices.Equal(b, []byte{0, 1, 2, 255, 0}) {
		t.Errorf("binary after apply = %v, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(wt.Path, "gone.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("deletion not in patch: %v", err)
	}
}

func TestResetMovesBackToVerifiedCommit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, _ := newRepo(t)
	wt, base := newWorktree(t, r)
	write(t, filepath.Join(wt.Path, "a.go"), "package a\n")
	first, err := wt.CommitStep(ctx, base, StepCommit{Task: "k3m9x2q7ab", Step: 1, Agent: "opus", Pool: "p"})
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(wt.Path, "b.go"), "package b\n")
	if _, err := wt.CommitStep(ctx, base, StepCommit{Task: "k3m9x2q7ab", Step: 2, Agent: "opus", Pool: "p"}); err != nil {
		t.Fatal(err)
	}
	if err := wt.ResetTo(ctx, first); err != nil {
		t.Fatal(err)
	}
	if head, err := wt.Head(ctx); err != nil || head != first {
		t.Fatalf("head = %s, %v; want %s", head, err, first)
	}
}

func TestRemoveWorktree(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, g := newRepo(t)
	wt, _ := newWorktree(t, r)
	write(t, filepath.Join(wt.Path, "dirty.go"), "package dirty\n")
	if err := r.RemoveWorktree(ctx, wt.Path, wt.Branch, true); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	if _, err := os.Stat(wt.Path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("worktree still on disk: %v", err)
	}
	if out := must(t, g, r.Root, "branch", "--list", wt.Branch); out != "" {
		t.Errorf("branch kept: %q", out)
	}
	if err := r.RemoveWorktree(ctx, wt.Path, wt.Branch, true); err != nil {
		t.Errorf("second remove = %v; want nil", err)
	}
}

func TestFallbackIdentity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r, g := newRepo(t)
	must(t, g, r.Root, "config", "--unset", "user.name")
	must(t, g, r.Root, "config", "--unset", "user.email")
	wt, base := newWorktree(t, r)
	sha, err := wt.CommitStep(ctx, base, StepCommit{Task: "k3m9x2q7ab", Step: 1, Agent: "opus", Pool: "p"})
	if err != nil {
		t.Fatalf("CommitStep without identity: %v", err)
	}
	if author := must(t, g, wt.Path, "log", "-1", "--format=%an <%ae>", sha); author != "OpenChef <openchef@localhost>" {
		t.Errorf("author = %s", author)
	}
}

func TestGitEnv(t *testing.T) {
	t.Parallel()
	got := gitEnv([]string{"PATH=/bin", "GIT_DIR=/evil", "git_work_tree=/x", "LANG=tr_TR.UTF-8", "HOME=/h"})
	want := []string{"PATH=/bin", "HOME=/h", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "GIT_OPTIONAL_LOCKS=0"}
	if !slices.Equal(got, want) {
		t.Fatalf("gitEnv = %v", got)
	}
}
