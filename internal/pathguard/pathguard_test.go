package pathguard

import (
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
)

func workspace(t *testing.T) (ws, outside string) {
	t.Helper()
	dir := t.TempDir()
	ws = filepath.Join(dir, "ws")
	outside = filepath.Join(dir, "outside")
	for _, d := range []string{filepath.Join(ws, "src"), filepath.Join(ws, ".git", "hooks"), outside} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return ws, outside
}

func TestCheck(t *testing.T) {
	t.Parallel()
	ws, outside := workspace(t)
	ok := []string{filepath.Join(ws, "src", "a.go"), filepath.Join(ws, "new", "dir", "b.go"), "src/c.go", filepath.Join(ws, ".gitignore"), filepath.Join(ws, "git", "x")}
	for _, p := range ok {
		if why, err := Check(ws, p); err != nil || why != "" {
			t.Errorf("Check(%s) = %q, %v; want allowed", p, why, err)
		}
	}
	bad := []string{filepath.Join(outside, "x"), filepath.Join(ws, "..", "outside", "x"), filepath.Join(ws, ".git", "hooks", "pre-commit"),
		filepath.Join(ws, "sub", ".claude", "settings.json"), filepath.Join(ws, ".openchef", "x"), "../outside/y"}
	if goruntime.GOOS == "darwin" || goruntime.GOOS == "windows" {
		bad = append(bad, filepath.Join(ws, ".GIT", "hooks", "post-commit"), filepath.Join(ws, ".Claude", "settings.json"))
	}
	for _, p := range bad {
		if why, err := Check(ws, p); err != nil || why == "" {
			t.Errorf("Check(%s) = %q, %v; want refused", p, why, err)
		}
	}
	if why, _ := Check("", "/x"); why == "" {
		t.Error("empty workspace allowed a write")
	}
}

func TestSymlinkEscape(t *testing.T) {
	t.Parallel()
	ws, outside := workspace(t)
	if err := os.Symlink(outside, filepath.Join(ws, "vendor")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(ws, ".git"), filepath.Join(ws, "x")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(ws, "vendor", "etc", "passwd"), filepath.Join(ws, "x", "hooks", "post-checkout"), filepath.Join(ws, "vendor")} {
		why, err := Check(ws, p)
		if err != nil || why == "" {
			t.Errorf("Check(%s) = %q, %v; a symlink must not open a way out", p, why, err)
		}
	}
	// A workspace reached through a symlink (macOS /var -> /private/var) still works.
	alias := filepath.Join(filepath.Dir(ws), "alias")
	if err := os.Symlink(ws, alias); err != nil {
		t.Fatal(err)
	}
	if why, err := Check(alias, filepath.Join(ws, "src", "a.go")); err != nil || why != "" {
		t.Errorf("aliased workspace: %q, %v", why, err)
	}
}

func TestWindowsAliases(t *testing.T) {
	if goruntime.GOOS != "windows" {
		t.Skip("Windows name rules")
	}
	t.Parallel()
	ws, _ := workspace(t)
	for _, name := range []string{".git.", ".git ", ".git::$INDEX_ALLOCATION", "GIT~1"} {
		if why, _ := Check(ws, filepath.Join(ws, name, "hooks", "x")); why == "" || !strings.Contains(why, "managed by OpenChef") {
			t.Errorf("%q allowed: %q", name, why)
		}
	}
}
