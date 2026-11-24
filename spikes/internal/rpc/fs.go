package rpc

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

func evalSymlinks(p string) (string, error) { return filepath.EvalSymlinks(p) }

// NewGitRepo creates a throwaway git repository with one committed file.
func NewGitRepo(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# spike repo\n"), 0o644); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.email=spike@example.com", "-c", "user.name=spike", "add", "README.md"},
		{"-c", "user.email=spike@example.com", "-c", "user.name=spike", "commit", "-q", "-m", "init"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %v: %w: %s", args, err, out)
		}
	}
	return nil
}
