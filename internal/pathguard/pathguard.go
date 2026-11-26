// Package pathguard decides whether a path an agent names is inside a workspace and
// outside the directories only OpenChef may write. It resolves symlinks (a committed
// `vendor -> /` must not let writes escape), compares case-insensitively where the file
// system usually is (macOS, Windows), and refuses Windows name tricks (trailing dots or
// spaces, alternate data streams, 8.3 short names) that would alias a protected
// directory.
package pathguard

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	goruntime "runtime"
	"strings"
)

// Protected are the workspace directories only OpenChef writes (§8.2, §9.2).
var Protected = []string{".git", ".openchef", ".claude"}

// foldCase is true where the default file systems are case-insensitive.
var foldCase = goruntime.GOOS == "darwin" || goruntime.GOOS == "windows"

// Resolve returns p made absolute (relative to base) with every symlink in its existing
// part resolved. The not-yet-existing tail is appended as is: a file the agent is about
// to create lives where its nearest existing parent really is.
func Resolve(base, p string) (string, error) {
	if !filepath.IsAbs(p) {
		if base == "" {
			return "", fmt.Errorf("pathguard: relative path %q without a base", p)
		}
		p = filepath.Join(base, p)
	}
	p = filepath.Clean(p)
	existing, tail := p, ""
	for {
		resolved, err := filepath.EvalSymlinks(existing)
		if err == nil {
			return filepath.Join(resolved, tail), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("pathguard: resolve %s: %w", existing, err)
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return p, nil // nothing exists; nothing to resolve
		}
		tail = filepath.Join(filepath.Base(existing), tail)
		existing = parent
	}
}

// Check reports why an agent may not write p in workspace, or "" if it may. Both paths
// are resolved first.
func Check(workspace, p string) (string, error) {
	if workspace == "" || !filepath.IsAbs(workspace) {
		return "no workspace is known, so no file may be changed", nil
	}
	// Names first, on the path as given: a Windows alias of .git (".git::$INDEX_ALLOCATION")
	// may not even resolve, and must be refused for what it names, not for an I/O error.
	lexical := p
	if !filepath.IsAbs(lexical) {
		lexical = filepath.Join(workspace, lexical)
	}
	if rel, ok := within(filepath.Clean(workspace), filepath.Clean(lexical)); ok {
		if why := protectedIn(rel, p); why != "" {
			return why, nil
		}
	}
	root, err := Resolve("", workspace)
	if err != nil {
		return "", err
	}
	resolved, err := Resolve(workspace, p)
	if err != nil {
		return "", err
	}
	rel, ok := within(root, resolved)
	if !ok {
		return "only files inside the task's workspace may be changed: " + p, nil
	}
	return protectedIn(rel, p), nil
}

// protectedIn explains why rel (relative to the workspace) is under a protected
// directory, or returns "".
func protectedIn(rel, p string) string {
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if name, bad := protected(part); bad {
			return "files under " + name + " are managed by OpenChef and may not be changed: " + p
		}
	}
	return ""
}

// within reports whether p is root or below it, and the relative path.
func within(root, p string) (string, bool) {
	if foldCase {
		root, p = strings.ToLower(root), strings.ToLower(p)
	}
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return rel, true
}

// protected reports whether one path element names (or may alias) a protected
// directory.
func protected(part string) (string, bool) {
	name := part
	if goruntime.GOOS == "windows" {
		if strings.ContainsAny(name, ":~") {
			return name, true // alternate data streams (.git::$INDEX_ALLOCATION), short names (GIT~1)
		}
		name = strings.TrimRight(name, ". ") // ".git." and ".git " open ".git"
	}
	for _, d := range Protected {
		if name == d || (foldCase && strings.EqualFold(name, d)) {
			return d, true
		}
	}
	return "", false
}

// Within reports whether p (resolved) is inside workspace (resolved), for read checks.
func Within(workspace, p string) (bool, error) {
	root, err := Resolve("", workspace)
	if err != nil {
		return false, err
	}
	resolved, err := Resolve(workspace, p)
	if err != nil {
		return false, err
	}
	_, ok := within(root, resolved)
	return ok, nil
}
