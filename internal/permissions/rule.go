// Package permissions decides agent permission requests with Claude Code's vocabulary
// (docs/architecture.md §9): modes, allow/ask/deny rules in Claude Code syntax, layered
// settings files, and one evaluator for every runtime. Shell commands are parsed into
// simple commands, never pattern-matched as strings.
package permissions

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"path/filepath"
	"strings"
)

// ErrBadRule means a rule string is malformed.
var ErrBadRule = errors.New("permissions: invalid rule")

// Tool families that share file rules, as in Claude Code: Edit rules cover every tool
// that changes files, Read rules every tool that reads them.
var (
	editTools = map[string]bool{"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true}
	readTools = map[string]bool{"Read": true, "Grep": true, "Glob": true, "LS": true, "NotebookRead": true}
)

// Rule is one parsed permission rule, e.g. Bash(npm run test:*), Edit(src/**),
// WebFetch(domain:github.com), mcp__github__create_issue.
type Rule struct {
	// Raw is the rule as written.
	Raw string
	// Tool is the tool name (or an mcp__server[__tool] prefix).
	Tool string
	// Specifier is the text inside the parentheses; empty matches any use of Tool.
	Specifier string
}

// ParseRule parses a rule in Claude Code syntax.
func ParseRule(raw string) (Rule, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Rule{}, fmt.Errorf("%w: empty", ErrBadRule)
	}
	name, spec := s, ""
	if i := strings.IndexByte(s, '('); i >= 0 {
		if !strings.HasSuffix(s, ")") {
			return Rule{}, fmt.Errorf("%w: %q: missing closing parenthesis", ErrBadRule, raw)
		}
		name, spec = s[:i], s[i+1:len(s)-1]
		if strings.TrimSpace(spec) == "" {
			return Rule{}, fmt.Errorf("%w: %q: empty specifier (write %s alone to match any use)", ErrBadRule, raw, name)
		}
	}
	if name == "" || strings.ContainsAny(name, " ()") {
		return Rule{}, fmt.Errorf("%w: %q: bad tool name", ErrBadRule, raw)
	}
	if strings.HasPrefix(name, "mcp__") && spec != "" {
		return Rule{}, fmt.Errorf("%w: %q: MCP rules take no specifier", ErrBadRule, raw)
	}
	if spec != "" && name == "Bash" {
		if strings.HasSuffix(spec, ":*") && strings.TrimSuffix(spec, ":*") == "" {
			return Rule{}, fmt.Errorf("%w: %q: empty command prefix", ErrBadRule, raw)
		}
	}
	if spec != "" && name == "WebFetch" && !strings.HasPrefix(spec, "domain:") {
		return Rule{}, fmt.Errorf("%w: %q: WebFetch rules take domain:<host>", ErrBadRule, raw)
	}
	return Rule{Raw: s, Tool: name, Specifier: spec}, nil
}

// Request is one thing an agent wants to do, in Claude Code vocabulary.
type Request struct {
	Tool string
	// Command is the shell command line for Bash.
	Command string
	// Paths are the absolute files concerned, for file tools.
	Paths []string
	// URL is the address for WebFetch.
	URL string
}

// matchesTool reports whether r applies to tool at all.
func (r *Rule) matchesTool(tool string) bool {
	switch {
	case strings.HasPrefix(r.Tool, "mcp__"):
		if !strings.HasPrefix(tool, "mcp__") {
			return false
		}
		prefix := strings.TrimSuffix(r.Tool, "*")
		prefix = strings.TrimSuffix(prefix, "__")
		return tool == prefix || strings.HasPrefix(tool, prefix+"__")
	case r.Tool == "Edit":
		return editTools[tool]
	case r.Tool == "Read":
		return readTools[tool]
	default:
		return r.Tool == tool
	}
}

// matchCommand reports whether a Bash rule matches one simple command.
//
//   - Bash(npm run test:*) matches commands whose words start with npm run test.
//   - Bash(npm run *) is a wildcard over the whole command line (* spans spaces).
//   - Bash(npm test) matches exactly.
//
// A dynamic command matches only through its static leading words, and only with a
// prefix rule; deny rules use that to catch e.g. `rm -rf $DIR` with Bash(rm:*).
func (r *Rule) matchCommand(s *Simple) bool {
	if r.Specifier == "" {
		return true
	}
	if prefix, ok := strings.CutSuffix(r.Specifier, ":*"); ok {
		want := strings.Fields(prefix)
		if len(want) > len(s.Args) {
			return false
		}
		for i, w := range want {
			if s.Args[i] != w {
				return false
			}
		}
		return true
	}
	if s.Dynamic {
		return false
	}
	if strings.Contains(r.Specifier, "*") {
		return wildcard(r.Specifier, s.Line())
	}
	return r.Specifier == s.Line()
}

// wildcard matches s against pattern where * stands for any run of characters.
func wildcard(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for i := 1; i < len(parts)-1; i++ {
		j := strings.Index(s, parts[i])
		if j < 0 {
			return false
		}
		s = s[j+len(parts[i]):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}

// matchPath reports whether a file rule matches the absolute path p. Patterns follow
// Claude Code: //abs is absolute, ~/x is under home, /x is relative to the project root
// and x or ./x to the working directory. OpenChef agents work in a worktree with the
// repository's layout, so both resolve against the workspace.
// ** matches any number of directories, * and ? stay within one name.
func (r *Rule) matchPath(p, workspace, home string) bool {
	if r.Specifier == "" {
		return true
	}
	pat := r.Specifier
	switch {
	case strings.HasPrefix(pat, "//"):
		pat = pat[1:]
	case strings.HasPrefix(pat, "~/"):
		pat = filepath.ToSlash(home) + pat[1:]
	case strings.HasPrefix(pat, "/"):
		pat = filepath.ToSlash(workspace) + pat
	default:
		pat = filepath.ToSlash(workspace) + "/" + strings.TrimPrefix(pat, "./")
	}
	// A pattern without a slash after its base (e.g. "*.env") matches at any depth, as
	// in gitignore.
	if !strings.Contains(strings.TrimPrefix(r.Specifier, "./"), "/") {
		pat = filepath.ToSlash(workspace) + "/**/" + strings.TrimPrefix(r.Specifier, "./")
	}
	return globMatch(path.Clean(pat), path.Clean(filepath.ToSlash(p)))
}

// globMatch matches slash-separated paths with *, ? and ** (any directories).
func globMatch(pattern, name string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			pat = pat[1:]
			if len(pat) == 0 {
				return true // ** at the end matches everything below
			}
			for i := 0; i <= len(name); i++ {
				if matchSegments(pat, name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], name[0]); err != nil || !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	// A directory pattern also covers what is inside it (Read(./secrets) covers
	// ./secrets/key).
	return true
}

// matchURL reports whether a WebFetch rule matches raw: domain:example.com matches that
// host, domain:*.example.com its subdomains.
func (r *Rule) matchURL(raw string) bool {
	if r.Specifier == "" {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return false
	}
	host := strings.ToLower(u.Hostname())
	want := strings.ToLower(strings.TrimPrefix(r.Specifier, "domain:"))
	if sub, ok := strings.CutPrefix(want, "*."); ok {
		return strings.HasSuffix(host, "."+sub)
	}
	return host == want
}
