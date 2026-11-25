package claude

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ugurboz/openchef/internal/runtime"
)

// Tool sets (D-006). Subagents (Task) and tools that act outside the turn (cron, remote
// triggers, notifications, monitors, worktrees, messaging) are never enabled.
var (
	defaultTools  = []string{"Bash", "Read", "Edit", "Write", "Glob", "Grep", "NotebookEdit", "TodoWrite"}
	readOnlyTools = []string{"Read", "Glob", "Grep"}
	webTools      = []string{"WebFetch", "WebSearch"}
)

// launch describes one process start.
type launch struct {
	sessionID string
	resume    bool
	schema    json.RawMessage
	mcpConfig string // path of the generated --mcp-config file, "" if none
}

// buildArgs maps a session spec to Claude Code flags. It is pure so the mapping can be
// tested exhaustively.
func buildArgs(spec runtime.SessionSpec, l launch) ([]string, error) {
	mode, tools, err := modeAndTools(spec)
	if err != nil {
		return nil, err
	}
	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--restricted",
		"--strict-mcp-config",
		"--permission-prompt-tool", "stdio",
		"--disallowedTools", "AskUserQuestion",
		"--permission-mode", mode,
		"--tools", strings.Join(tools, ","),
	}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	if spec.Effort != runtime.EffortDefault {
		// Claude accepts low|medium|high|xhigh|max, a superset of the normalized scale.
		args = append(args, "--effort", string(spec.Effort))
	}
	settings, err := settingsJSON(spec)
	if err != nil {
		return nil, err
	}
	args = append(args, "--settings", settings)
	if l.mcpConfig != "" {
		args = append(args, "--mcp-config", l.mcpConfig)
	}
	if l.resume {
		args = append(args, "--resume", l.sessionID)
	} else {
		args = append(args, "--session-id", l.sessionID)
	}
	if len(l.schema) > 0 {
		args = append(args, "--json-schema", string(l.schema))
	}
	return args, nil
}

// modeAndTools applies D-008: `--restricted` refuses bypassPermissions, and Claude's own
// plan mode switches models, so OpenChef's plan mode is dontAsk with read-only tools.
func modeAndTools(spec runtime.SessionSpec) (mode string, tools []string, err error) {
	tools = append([]string(nil), defaultTools...)
	if spec.WebAccess {
		tools = append(tools, webTools...)
	}
	switch spec.Mode {
	case runtime.ModeManual, "":
		return "manual", tools, nil
	case runtime.ModeAcceptEdits, runtime.ModeBypassPermissions:
		// bypass: the approver allows whatever rules and invariants do not deny.
		return "acceptEdits", tools, nil
	case runtime.ModeAuto:
		return "auto", tools, nil
	case runtime.ModeDontAsk:
		return "dontAsk", tools, nil
	case runtime.ModePlan:
		return "dontAsk", append([]string(nil), readOnlyTools...), nil
	default:
		return "", nil, fmt.Errorf("claude: unsupported permission mode %q", spec.Mode)
	}
}

// settingsJSON builds the --settings payload: native deny rules (spec + invariants),
// native ask rules, and an allow rule for OpenChef's own ask_user tool so it works even in
// dontAsk (D-007, m1-findings §10).
func settingsJSON(spec runtime.SessionSpec) (string, error) {
	deny := dedupe(append(append([]string(nil), runtime.InvariantDenyRules...), spec.DenyRules...))
	perms := map[string]any{"deny": deny}
	if len(spec.AskRules) > 0 {
		perms["ask"] = dedupe(spec.AskRules)
	}
	if spec.AskUser != nil {
		perms["allow"] = []string{askUserTool(spec.AskUser)}
	}
	raw, err := json.Marshal(map[string]any{"permissions": perms})
	if err != nil {
		return "", fmt.Errorf("claude: encode settings: %w", err)
	}
	return string(raw), nil
}

func askUserTool(m *runtime.MCPServer) string { return "mcp__" + m.Name + "__ask_user" }

// mcpConfigJSON is the --mcp-config file content for OpenChef's ask_user server.
func mcpConfigJSON(m *runtime.MCPServer) ([]byte, error) {
	srv := map[string]any{"command": m.Command, "args": m.Args}
	if len(m.Env) > 0 {
		env := map[string]string{}
		for _, kv := range m.Env {
			if k, v, ok := strings.Cut(kv, "="); ok {
				env[k] = v
			}
		}
		srv["env"] = env
	}
	raw, err := json.Marshal(map[string]any{"mcpServers": map[string]any{m.Name: srv}})
	if err != nil {
		return nil, fmt.Errorf("claude: encode mcp config: %w", err)
	}
	return raw, nil
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
