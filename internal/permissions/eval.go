package permissions

import (
	"fmt"
	"path/filepath"
	goruntime "runtime"
	"strings"

	"github.com/ugurboz/openchef/internal/runtime"
)

// Decision is the evaluator's answer.
type Decision int

// Decisions, from most to least permissive.
const (
	Allow Decision = iota
	Ask
	Deny
)

func (d Decision) String() string {
	switch d {
	case Allow:
		return "allow"
	case Ask:
		return "ask"
	default:
		return "deny"
	}
}

// Verdict explains a decision.
type Verdict struct {
	Decision Decision
	// Rule is the rule that decided, if any ("invariant: …" for built-ins).
	Rule string
	// Reason is shown to the agent on deny and to the user on ask.
	Reason string
}

// Policy is a merged permission configuration.
type Policy struct {
	Mode             runtime.Mode
	Allow, Ask, Deny []Rule
}

// Evaluator decides requests for one workspace (§9.2).
type Evaluator struct {
	policy    Policy
	workspace string
	home      string
	// private are directories no agent may read: OpenChef's own state and secrets.
	private []string
	// invariant holds the built-in deny rules (history-rewriting git, protected dirs).
	invariant []Rule
}

// protected are workspace directories only OpenChef writes (§8.2, §9.2).
var protected = []string{".git", ".openchef", ".claude"}

// NewEvaluator returns an evaluator for an agent working in workspace. home is the
// user's home directory, private lists directories agents may not read at all (e.g.
// ~/.openchef).
func NewEvaluator(p Policy, workspace, home string, private []string) (*Evaluator, error) {
	if !filepath.IsAbs(workspace) {
		return nil, fmt.Errorf("permissions: workspace %q is not absolute", workspace)
	}
	if p.Mode == "" {
		p.Mode = runtime.ModeManual
	}
	e := &Evaluator{policy: p, workspace: filepath.Clean(workspace), home: home, private: private}
	for _, raw := range runtime.InvariantDenyRules {
		r, err := ParseRule(raw)
		if err != nil {
			return nil, fmt.Errorf("permissions: built-in rule %q: %w", raw, err)
		}
		e.invariant = append(e.invariant, r)
	}
	return e, nil
}

// Mode is the effective permission mode.
func (e *Evaluator) Mode() runtime.Mode { return e.policy.Mode }

// Evaluate decides one request: invariants, then deny, ask, allow rules, then the mode's
// default (§9.2). Deny always wins and no mode overrides it.
func (e *Evaluator) Evaluate(req Request) Verdict {
	var v Verdict
	switch {
	case req.Tool == "Bash":
		v = e.bash(req.Command)
	case editTools[req.Tool], readTools[req.Tool]:
		v = e.files(req.Tool, req.Paths)
	default:
		v = e.other(req)
	}
	return e.applyMode(req, v)
}

// applyMode applies the mode to what the rules said (§9.1, §9.3):
//
//   - deny is final in every mode;
//   - plan is read-only: anything but reading is denied, even if a rule allows it;
//   - a rule's allow or ask stands, except that dontAsk refuses what needs asking;
//   - with no rule: reads inside the workspace are allowed; edits inside it are allowed
//     in acceptEdits, auto and bypassPermissions and ask in manual; everything else
//     asks, is denied in dontAsk, and is allowed in bypassPermissions.
func (e *Evaluator) applyMode(req Request, v Verdict) Verdict {
	mode := e.policy.Mode
	read := readTools[req.Tool]
	if v.Decision == Deny {
		return v
	}
	if mode == runtime.ModePlan && !read {
		return Verdict{Decision: Deny, Reason: "plan mode is read-only"}
	}
	if v.Rule != "" {
		if v.Decision == Ask && mode == runtime.ModeDontAsk {
			return Verdict{Decision: Deny, Rule: v.Rule, Reason: v.Reason + " (dontAsk mode refuses what needs asking)"}
		}
		return v
	}
	if mode == runtime.ModeBypassPermissions {
		return Verdict{Decision: Allow, Reason: "bypassPermissions"}
	}
	if v.Decision == Allow {
		switch {
		case read:
			return v
		case editTools[req.Tool] && (mode == runtime.ModeAcceptEdits || mode == runtime.ModeAuto):
			return v
		case editTools[req.Tool] && mode == runtime.ModeManual:
			return Verdict{Decision: Ask, Reason: "file changes need approval in manual mode"}
		}
	}
	if mode == runtime.ModeDontAsk {
		return Verdict{Decision: Deny, Reason: v.Reason + " (dontAsk mode refuses what needs asking)"}
	}
	return Verdict{Decision: Ask, Reason: v.Reason}
}

func (e *Evaluator) firstMatch(rules []Rule, match func(*Rule) bool) *Rule {
	for i := range rules {
		if match(&rules[i]) {
			return &rules[i]
		}
	}
	return nil
}

// bash decides a command line: every simple command must be allowed; any denied one
// denies the whole line; an unparsable line asks.
func (e *Evaluator) bash(command string) (v Verdict) {
	defer func() {
		// The parser handles untrusted input; a bug in it must not take the daemon
		// down or let a command through.
		if p := recover(); p != nil {
			v = Verdict{Decision: Ask, Reason: fmt.Sprintf("the command could not be analysed (%v)", p)}
		}
	}()
	cmds, err := Split(command)
	if err != nil {
		return Verdict{Decision: Ask, Reason: "the command could not be parsed: " + err.Error()}
	}
	if len(cmds) == 0 {
		return Verdict{Decision: Ask, Reason: "the command runs nothing recognizable"}
	}
	worst := Verdict{Decision: Allow}
	allowedByRule := true
	for i := range cmds {
		s := &cmds[i]
		match := func(r *Rule) bool { return r.matchesTool("Bash") && r.matchCommand(s) }
		if r := e.firstMatch(e.invariant, match); r != nil {
			return Verdict{Decision: Deny, Rule: "invariant: " + r.Raw,
				Reason: fmt.Sprintf("%q is reserved for OpenChef, which owns git history and refs", s.Line())}
		}
		if r := e.firstMatch(e.policy.Deny, match); r != nil {
			return Verdict{Decision: Deny, Rule: r.Raw, Reason: fmt.Sprintf("%q is denied by %s", s.Line(), r.Raw)}
		}
		for _, w := range s.Writes {
			if v := e.files("Write", []string{e.abs(w)}); v.Decision == Deny {
				return v
			} else if v.Decision > worst.Decision {
				worst = v
			}
		}
		for _, rd := range s.Reads {
			if v := e.files("Read", []string{e.abs(rd)}); v.Decision == Deny {
				return v
			}
		}
		if r := e.firstMatch(e.policy.Ask, match); r != nil {
			worst = maxVerdict(worst, Verdict{Decision: Ask, Rule: r.Raw, Reason: fmt.Sprintf("%q needs approval (%s)", s.Line(), r.Raw)})
			continue
		}
		if s.Dynamic {
			allowedByRule = false
			worst = maxVerdict(worst, Verdict{Decision: Ask, Reason: fmt.Sprintf("%q is only known at run time", s.Line())})
			continue
		}
		if e.firstMatch(e.policy.Allow, match) == nil {
			allowedByRule = false
			worst = maxVerdict(worst, Verdict{Decision: Ask, Reason: fmt.Sprintf("no rule allows %q", s.Line())})
		}
	}
	if worst.Decision == Allow && allowedByRule {
		return Verdict{Decision: Allow, Rule: "allow rules", Reason: "every command is allowed"}
	}
	return worst
}

func maxVerdict(a, b Verdict) Verdict {
	if b.Decision > a.Decision || (b.Decision == a.Decision && a.Rule == "" && b.Rule != "") {
		return b
	}
	return a
}

func (e *Evaluator) abs(p string) string {
	if strings.HasPrefix(p, "~/") {
		return filepath.Join(e.home, p[2:])
	}
	if goruntime.GOOS == "windows" && (strings.HasPrefix(p, "/") || strings.HasPrefix(p, `\`)) {
		// Rooted but without a drive (/etc/x in Git Bash, \x in cmd): never inside the
		// workspace. Resolve it on the workspace's volume so it is treated as outside.
		return filepath.Join(filepath.VolumeName(e.workspace)+`\`, p)
	}
	if !filepath.IsAbs(p) {
		return filepath.Join(e.workspace, p)
	}
	return filepath.Clean(p)
}

// files decides a file tool on its paths: invariants, then rules; inside the workspace
// reads are allowed and edits follow the mode.
func (e *Evaluator) files(tool string, paths []string) Verdict {
	if len(paths) == 0 {
		return Verdict{Decision: Ask, Reason: "the request names no file"}
	}
	writing := editTools[tool]
	worst := Verdict{Decision: Allow}
	for _, raw := range paths {
		p := e.abs(raw)
		rel, inside := e.relative(p)
		for _, dir := range e.private {
			if within(p, dir) {
				return Verdict{Decision: Deny, Rule: "invariant: private", Reason: "OpenChef's own files are off limits: " + p}
			}
		}
		if writing {
			if !inside {
				return Verdict{Decision: Deny, Rule: "invariant: workspace", Reason: "only files inside the workspace may be changed: " + p}
			}
			for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
				for _, d := range protected {
					if part == d {
						return Verdict{Decision: Deny, Rule: "invariant: " + d, Reason: "files under " + d + " are managed by OpenChef: " + p}
					}
				}
			}
		}
		match := func(r *Rule) bool { return r.matchesTool(tool) && r.matchPath(p, e.workspace, e.home) }
		if r := e.firstMatch(e.policy.Deny, match); r != nil {
			return Verdict{Decision: Deny, Rule: r.Raw, Reason: p + " is denied by " + r.Raw}
		}
		if r := e.firstMatch(e.policy.Ask, match); r != nil {
			worst = maxVerdict(worst, Verdict{Decision: Ask, Rule: r.Raw, Reason: p + " needs approval (" + r.Raw + ")"})
			continue
		}
		if r := e.firstMatch(e.policy.Allow, match); r != nil {
			worst = maxVerdict(worst, Verdict{Decision: Allow, Rule: r.Raw})
			continue
		}
		if !inside {
			worst = maxVerdict(worst, Verdict{Decision: Ask, Reason: p + " is outside the workspace"})
		}
	}
	return worst
}

func (e *Evaluator) relative(p string) (string, bool) {
	rel, err := filepath.Rel(e.workspace, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

func within(p, dir string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// other decides non-file, non-shell tools (WebFetch, MCP, …) by rules alone.
func (e *Evaluator) other(req Request) Verdict {
	match := func(r *Rule) bool {
		if !r.matchesTool(req.Tool) {
			return false
		}
		if req.Tool == "WebFetch" {
			return r.matchURL(req.URL)
		}
		return r.Specifier == ""
	}
	if r := e.firstMatch(e.policy.Deny, match); r != nil {
		return Verdict{Decision: Deny, Rule: r.Raw, Reason: req.Tool + " is denied by " + r.Raw}
	}
	if r := e.firstMatch(e.policy.Ask, match); r != nil {
		return Verdict{Decision: Ask, Rule: r.Raw, Reason: req.Tool + " needs approval (" + r.Raw + ")"}
	}
	if r := e.firstMatch(e.policy.Allow, match); r != nil {
		return Verdict{Decision: Allow, Rule: r.Raw}
	}
	return Verdict{Decision: Ask, Reason: "no rule allows " + req.Tool}
}
