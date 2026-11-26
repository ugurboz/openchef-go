package permissions

import (
	"errors"
	"fmt"
	"path"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// ErrUnparsable means a command line could not be parsed; the evaluator asks.
var ErrUnparsable = errors.New("permissions: command cannot be parsed")

// maxDepth bounds `sh -c` nesting.
const maxDepth = 4

// Simple is one simple command of a command line (§9.2): every one must be allowed for
// the whole line to be allowed.
type Simple struct {
	// Args are the command's words with quotes removed. A word the shell would expand
	// at run time ($VAR, $(…), globs) makes the command dynamic; Args then holds the
	// static words before it.
	Args []string
	// Dynamic is set when some word (or the command name) is only known at run time,
	// or when the command runs code it receives (eval, xargs, sudo, source …). A
	// dynamic command is never allowed by an allow rule.
	Dynamic bool
	// Writes and Reads are redirection targets (> >> &> and <), checked like file
	// edits and reads. Unknown targets make the command dynamic.
	Writes []string
	Reads  []string
}

// Line is the command as matched against Bash rules: its words joined by spaces.
func (s *Simple) Line() string { return strings.Join(s.Args, " ") }

// opaque commands run code given to them as data, so what they do cannot be decided
// from the command line.
var opaque = map[string]bool{
	"eval": true, "source": true, ".": true, "xargs": true, "sudo": true, "doas": true, "su": true,
	"exec": true, "trap": true, "parallel": true, "watch": true, "ssh": true,
}

// wrappers run the command that follows their own options, which is what is matched.
var wrappers = map[string]bool{
	"env": true, "nohup": true, "time": true, "nice": true, "timeout": true, "command": true,
	"builtin": true, "stdbuf": true, "ionice": true,
}

var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true}

// Split parses a command line into its simple commands, recursing into subshells,
// command substitutions, process substitutions and `sh -c` strings.
func Split(command string) ([]Simple, error) {
	return split(command, 0)
}

func split(command string, depth int) ([]Simple, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("%w: shells nested too deeply", ErrUnparsable)
	}
	f, err := syntax.NewParser(syntax.Variant(syntax.LangBash)).Parse(strings.NewReader(command), "")
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnparsable, err)
	}
	var (
		out     []Simple
		walkErr error
	)
	syntax.Walk(f, func(n syntax.Node) bool {
		if walkErr != nil {
			return false
		}
		switch n := n.(type) {
		case *syntax.Stmt:
			call, ok := n.Cmd.(*syntax.CallExpr)
			if !ok || call == nil {
				// Compound commands (if, for, while, case, functions, [[ ]], (( )))
				// hold their own statements, which Walk visits. A function body or
				// loop is still made of simple commands; redirections on a compound
				// command are checked here.
				if n.Cmd != nil && len(n.Redirs) > 0 {
					s := Simple{Args: []string{"(compound)"}}
					redirects(&s, n.Redirs)
					out = append(out, s)
				}
				if _, isDecl := n.Cmd.(*syntax.DeclClause); isDecl {
					out = append(out, Simple{Args: []string{"declare"}, Dynamic: true})
				}
				return true
			}
			cmds, err := simple(call, n.Redirs, depth)
			if err != nil {
				walkErr = err
				return false
			}
			out = append(out, cmds...)
			return true
		case *syntax.FuncDecl:
			// A function can shadow any command name, which defeats name-based rules.
			name := "function"
			if n.Name != nil { // nil for malformed input such as "()0"
				name += " " + n.Name.Value
			}
			out = append(out, Simple{Args: []string{name}, Dynamic: true})
			return true
		}
		return true
	})
	if walkErr != nil {
		return nil, walkErr
	}
	return out, nil
}

// simple converts a call (and the redirections of its statement) into Simple commands;
// `sh -c` strings expand into their own commands.
func simple(call *syntax.CallExpr, redirs []*syntax.Redirect, depth int) ([]Simple, error) {
	s := Simple{}
	redirects(&s, redirs)
	if len(call.Args) == 0 {
		// Only assignments (FOO=bar). Harmless by itself, but an assignment can carry a
		// command substitution, which Walk visits separately.
		if len(s.Writes) == 0 && len(s.Reads) == 0 && !s.Dynamic {
			return nil, nil
		}
		s.Args = []string{"(assignment)"}
		return []Simple{s}, nil
	}
	if len(call.Assigns) > 0 {
		// FOO=x cmd: variables like PATH, LD_PRELOAD or GIT_SSH_COMMAND change what cmd
		// does, so a prefixed command is never auto-allowed.
		s.Dynamic = true
	}
	words := make([]string, 0, len(call.Args))
	for _, w := range call.Args {
		lit, ok := literal(w)
		if !ok {
			s.Dynamic = true
			break
		}
		words = append(words, lit)
	}
	s.Args = words
	if len(words) == 0 {
		return []Simple{s}, nil
	}
	return unwrap(s, depth)
}

// unwrap resolves wrappers, `sh -c` and opaque commands.
func unwrap(s Simple, depth int) ([]Simple, error) {
	name := path.Base(s.Args[0])
	switch {
	case opaque[name]:
		s.Dynamic = true
		return []Simple{s}, nil
	case shells[name]:
		script, ok := shellScript(s.Args[1:])
		if !ok {
			if len(s.Args) == 1 {
				return []Simple{s}, nil // an interactive shell; matched as such
			}
			s.Dynamic = true // a script file or stdin: contents unknown
			return []Simple{s}, nil
		}
		if s.Dynamic {
			return []Simple{s}, nil // the script word itself is not static
		}
		inner, err := split(script, depth+1)
		if err != nil {
			return nil, err
		}
		for i := range inner {
			inner[i].Writes = append(inner[i].Writes, s.Writes...)
			inner[i].Reads = append(inner[i].Reads, s.Reads...)
		}
		return inner, nil
	case wrappers[name]:
		rest := skipWrapperOptions(name, s.Args[1:])
		if len(rest) == 0 {
			return []Simple{s}, nil
		}
		inner := Simple{Args: rest, Dynamic: s.Dynamic, Writes: s.Writes, Reads: s.Reads}
		return unwrap(inner, depth)
	case name == "find":
		for _, a := range s.Args {
			if a == "-exec" || a == "-execdir" || a == "-ok" || a == "-okdir" || a == "-delete" {
				s.Dynamic = true
			}
		}
	}
	return []Simple{s}, nil
}

// shellScript returns the -c argument of a shell invocation.
func shellScript(args []string) (string, bool) {
	for i, a := range args {
		if a == "-c" && i+1 < len(args) {
			return args[i+1], true
		}
		if strings.HasPrefix(a, "-") && strings.Contains(a, "c") && !strings.HasPrefix(a, "--") && i+1 < len(args) {
			return args[i+1], true // -ec, -xc …
		}
		if !strings.HasPrefix(a, "-") {
			return "", false
		}
	}
	return "", false
}

// skipWrapperOptions drops a wrapper's own options (and assignments for env, the
// duration for timeout) and returns the wrapped command.
func skipWrapperOptions(name string, args []string) []string {
	i := 0
	for i < len(args) {
		a := args[i]
		switch {
		case name == "env" && strings.Contains(a, "=") && !strings.HasPrefix(a, "-"):
			i++
		case strings.HasPrefix(a, "-"):
			i++
			// Options that take a value as the next word.
			if (name == "nice" && a == "-n") || (name == "timeout" && (a == "-s" || a == "-k" || a == "--signal" || a == "--kill-after")) ||
				(name == "env" && (a == "-u" || a == "-C" || a == "-S")) || (name == "ionice" && (a == "-c" || a == "-n")) {
				i++
			}
		case name == "timeout":
			return args[i+1:] // the duration
		default:
			return args[i:]
		}
	}
	return nil
}

// redirects records the targets of redirections.
func redirects(s *Simple, redirs []*syntax.Redirect) {
	for _, r := range redirs {
		if r.Word == nil {
			continue
		}
		target, ok := literal(r.Word)
		switch r.Op {
		case syntax.RdrOut, syntax.AppOut, syntax.RdrAll, syntax.AppAll, syntax.RdrClob, syntax.AppClob, syntax.RdrAllClob, syntax.AppAllClob, syntax.RdrInOut:
			if !ok {
				s.Dynamic = true
				continue
			}
			if target == "/dev/null" || target == "/dev/stdout" || target == "/dev/stderr" {
				continue
			}
			s.Writes = append(s.Writes, target)
		case syntax.RdrIn:
			if !ok {
				s.Dynamic = true
				continue
			}
			s.Reads = append(s.Reads, target)
		case syntax.DplOut, syntax.DplIn:
			// 2>&1 and friends: file descriptors, not files, unless the word is a path.
			if ok && !isFD(target) {
				s.Writes = append(s.Writes, target)
			}
		default:
			// Here-documents and here-strings feed stdin; no file is touched.
		}
	}
}

func isFD(s string) bool {
	if s == "-" {
		return true
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return s != ""
}

// literal returns the value of a word that needs no expansion at run time.
func literal(w *syntax.Word) (string, bool) {
	var b strings.Builder
	for i, p := range w.Parts {
		switch p := p.(type) {
		case *syntax.Lit:
			if strings.ContainsAny(p.Value, "*?[") {
				return "", false // a glob expands at run time
			}
			if strings.HasPrefix(p.Value, "~") {
				// ~ and ~/x expand to the home directory, which the evaluator knows;
				// ~user forms depend on the system.
				if i != 0 || (p.Value != "~" && !strings.HasPrefix(p.Value, "~/")) {
					return "", false
				}
			}
			b.WriteString(p.Value)
		case *syntax.SglQuoted:
			if p.Dollar {
				return "", false // $'…' escapes: decoded by the shell
			}
			b.WriteString(p.Value)
		case *syntax.DblQuoted:
			for _, q := range p.Parts {
				lit, ok := q.(*syntax.Lit)
				if !ok {
					return "", false
				}
				b.WriteString(lit.Value)
			}
		default:
			return "", false
		}
	}
	return b.String(), true
}
