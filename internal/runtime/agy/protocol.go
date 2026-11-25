package agy

import (
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/runtime/linerpc"
)

// Wire types of `agy acp` (Agent Client Protocol), as recorded with agy 1.5.16
// (docs/research/m1-findings.md §11, testdata/agy). Unknown fields are ignored.

type configOption struct {
	ID           string `json:"id"`
	CurrentValue string `json:"currentValue"`
	Options      []struct {
		Value string `json:"value"`
	} `json:"options"`
}

type sessionResult struct {
	SessionID     string         `json:"sessionId"`
	ConfigOptions []configOption `json:"configOptions"`
}

type configResult struct {
	ConfigOptions []configOption `json:"configOptions"`
}

func findOption(opts []configOption, id string) (configOption, bool) {
	for _, o := range opts {
		if o.ID == id {
			return o, true
		}
	}
	return configOption{}, false
}

// Thinking levels: agy's values and the normalized scale. "xhigh" is agy's highest.
var thoughtToEffort = map[string]runtime.Effort{
	"low": runtime.EffortLow, "medium": runtime.EffortMedium, "high": runtime.EffortHigh,
	"xhigh": runtime.EffortMax, "max": runtime.EffortMax,
}

// thoughtLevel picks the agy thought_level for e among the values the current model
// offers; "" means leave agy's default.
func thoughtLevel(e runtime.Effort, opts []configOption) string {
	o, ok := findOption(opts, "thought_level")
	if !ok || e == runtime.EffortDefault {
		return ""
	}
	byEffort := map[runtime.Effort]string{}
	supported := make([]runtime.Effort, 0, len(o.Options))
	for _, v := range o.Options {
		if n, ok := thoughtToEffort[v.Value]; ok {
			if _, dup := byEffort[n]; !dup {
				supported = append(supported, n)
			}
			byEffort[n] = v.Value
		}
	}
	return byEffort[e.Nearest(supported)]
}

type promptResult struct {
	StopReason string `json:"stopReason"`
	Usage      *struct {
		InputTokens       int `json:"inputTokens"`
		OutputTokens      int `json:"outputTokens"`
		CachedReadTokens  int `json:"cachedReadTokens"`
		CachedWriteTokens int `json:"cachedWriteTokens"`
		ThoughtTokens     int `json:"thoughtTokens"`
	} `json:"usage"`
}

// usage is per turn (m1-findings §11). Thought tokens are billed as output.
func (r promptResult) usage() runtime.Usage {
	if r.Usage == nil {
		return runtime.Usage{}
	}
	return runtime.Usage{
		InputTokens:      r.Usage.InputTokens,
		OutputTokens:     r.Usage.OutputTokens + r.Usage.ThoughtTokens,
		CacheReadTokens:  r.Usage.CachedReadTokens,
		CacheWriteTokens: r.Usage.CachedWriteTokens,
	}
}

func addUsage(a, b runtime.Usage) runtime.Usage {
	return runtime.Usage{
		InputTokens:      a.InputTokens + b.InputTokens,
		OutputTokens:     a.OutputTokens + b.OutputTokens,
		CacheReadTokens:  a.CacheReadTokens + b.CacheReadTokens,
		CacheWriteTokens: a.CacheWriteTokens + b.CacheWriteTokens,
		CostUSD:          a.CostUSD + b.CostUSD,
	}
}

// stopFailure maps a non-success stop reason. ok is false for end_turn.
func stopFailure(reason string, stop runtime.FailureKind) (kind runtime.FailureKind, ok bool) {
	switch reason {
	case "end_turn":
		return "", false
	case "cancelled":
		if stop != "" {
			return stop, true
		}
		return runtime.FailInterrupted, true
	default: // max_tokens, max_turn_requests, refusal, or an unknown reason
		return runtime.FailUnknown, true
	}
}

// classifyError maps a failed session/prompt call. agy's quota error shape is not
// recorded yet (m1-findings §8), so the classification is by message.
func classifyError(err error) runtime.FailureKind {
	if errors.Is(err, linerpc.ErrClosed) || errors.Is(err, runtime.ErrSessionClosed) {
		return runtime.FailCrashed
	}
	low := strings.ToLower(err.Error())
	switch {
	case strings.Contains(low, "resource_exhausted"), strings.Contains(low, "quota"),
		strings.Contains(low, "usage limit"), strings.Contains(low, "credits"):
		return runtime.FailQuotaExhausted
	case strings.Contains(low, "rate limit"), strings.Contains(low, "429"), strings.Contains(low, "overloaded"),
		strings.Contains(low, "unavailable"):
		return runtime.FailRateLimited
	case strings.Contains(low, "unauthenticated"), strings.Contains(low, "login"), strings.Contains(low, "credential"),
		strings.Contains(low, "permission_denied"), strings.Contains(low, "auth"):
		return runtime.FailAuth
	case strings.Contains(low, "context window"), strings.Contains(low, "too long"), strings.Contains(low, "token limit"):
		return runtime.FailContextOverflow
	}
	return runtime.FailUnknown
}

type updateParams struct {
	SessionID string `json:"sessionId"`
	Update    update `json:"update"`
}

type update struct {
	Kind       string          `json:"sessionUpdate"`
	Content    json.RawMessage `json:"content"`
	ToolCallID string          `json:"toolCallId"`
	Title      string          `json:"title"`
	ToolKind   string          `json:"kind"`
	Name       string          `json:"name"`
	Status     string          `json:"status"`
	RawInput   json.RawMessage `json:"rawInput"`
	RawOutput  json.RawMessage `json:"rawOutput"`
	Locations  []struct {
		Path string `json:"path"`
	} `json:"locations"`
}

// chunkText is the text of an agent_message_chunk.
func (u *update) chunkText() string {
	var c struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(u.Content, &c) != nil || c.Type != "text" {
		return ""
	}
	return c.Text
}

// result normalizes a finished tool call. A command that exited non-zero is an error.
func (u *update) result() *runtime.ToolResult {
	out := struct {
		ExitCode *int   `json:"exit_code"`
		Output   string `json:"output"`
	}{}
	text := ""
	if json.Unmarshal(u.RawOutput, &out) == nil && (out.Output != "" || out.ExitCode != nil) {
		text = out.Output
	} else if s := ""; json.Unmarshal(u.RawOutput, &s) == nil {
		text = s
	}
	if text == "" {
		text = contentText(u.Content)
	}
	isErr := u.Status == "failed" || (out.ExitCode != nil && *out.ExitCode != 0)
	return &runtime.ToolResult{ID: u.ToolCallID, IsError: isErr, Output: text}
}

func contentText(raw json.RawMessage) string {
	var blocks []struct {
		Type    string `json:"type"`
		Content struct {
			Text string `json:"text"`
		} `json:"content"`
		Path string `json:"path"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	parts := make([]string, 0, len(blocks))
	for i := range blocks {
		switch {
		case blocks[i].Content.Text != "":
			parts = append(parts, blocks[i].Content.Text)
		case blocks[i].Type == "diff" && blocks[i].Path != "":
			parts = append(parts, "edited "+blocks[i].Path)
		}
	}
	return strings.Join(parts, "\n")
}

// Tool names in Claude Code vocabulary, so permission rules apply uniformly (§9.1).
var toolNames = map[string]string{
	"run_command":                "Bash",
	"write_file":                 "Write",
	"write_to_file":              "Write",
	"replace_file_content":       "Edit",
	"multi_replace_file_content": "Edit",
	"edit_file":                  "Edit",
	"view_file":                  "Read",
	"read_file":                  "Read",
	"list_dir":                   "Glob",
	"find_by_name":               "Glob",
	"grep_search":                "Grep",
	"read_url_content":           "WebFetch",
	"search_web":                 "WebSearch",
}

// toolName maps an agy tool to OpenChef's vocabulary; askUser is the name of
// OpenChef's MCP server ("" if none). MCP tools appear as mcp_<server>_<tool>.
func toolName(name, kind, askUser string) string {
	if n, ok := toolNames[name]; ok {
		return n
	}
	if rest, ok := strings.CutPrefix(name, "mcp_"); ok {
		if askUser != "" {
			if tool, ok := strings.CutPrefix(rest, askUser+"_"); ok {
				return "mcp__" + askUser + "__" + tool
			}
		}
		if i := strings.IndexByte(rest, '_'); i > 0 {
			return "mcp__" + rest[:i] + "__" + rest[i+1:]
		}
		return "mcp__" + rest
	}
	switch kind {
	case "execute":
		return "Bash"
	case "edit", "delete", "move":
		return "Edit"
	case "read":
		return "Read"
	case "fetch":
		return "WebFetch"
	}
	if name != "" {
		return name
	}
	return kind
}

type permissionParams struct {
	SessionID string `json:"sessionId"`
	ToolCall  update `json:"toolCall"`
	Options   []struct {
		OptionID string `json:"optionId"`
		Kind     string `json:"kind"`
	} `json:"options"`
}

// optionID returns the id of the first option of the given kind (D-016: ids changed
// between agy versions, kinds did not).
func (p *permissionParams) optionID(kinds ...string) (string, bool) {
	for _, k := range kinds {
		for _, o := range p.Options {
			if o.Kind == k {
				return o.OptionID, true
			}
		}
	}
	return "", false
}

func selected(id string) map[string]any {
	return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": id}}
}

func cancelled() map[string]any {
	return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}
}

// approvalRequest normalizes a permission request.
func (p *permissionParams) approvalRequest(raw json.RawMessage, askUser string) runtime.ApprovalRequest {
	tc := &p.ToolCall
	req := runtime.ApprovalRequest{
		Tool: toolName(tc.Name, tc.ToolKind, askUser), Input: tc.RawInput, Raw: raw,
	}
	var in struct {
		CommandLine   string `json:"CommandLine"`
		BypassSandbox bool   `json:"BypassSandbox"`
		Summary       string `json:"toolSummary"`
		Path          string `json:"Path"`
		TargetFile    string `json:"TargetFile"`
		AbsolutePath  string `json:"AbsolutePath"`
	}
	_ = json.Unmarshal(tc.RawInput, &in) // best effort: the raw request is kept for audit
	req.Description = in.Summary
	switch {
	case tc.ToolKind == "execute" || req.Tool == "Bash":
		req.Kind = runtime.ApproveCommand
		req.Command = in.CommandLine
		if req.Command == "" {
			req.Command = tc.Title
		}
		if in.BypassSandbox {
			// D-015: the command wants to leave the read-only, offline sandbox.
			req.Description = strings.TrimSpace(req.Description + " (runs outside the agy sandbox)")
		}
	case req.Tool == "Write" || req.Tool == "Edit":
		req.Kind = runtime.ApproveFileChange
	default:
		req.Kind = runtime.ApproveTool
	}
	paths := make([]string, 0, 3+len(tc.Locations))
	paths = append(paths, in.Path, in.TargetFile, in.AbsolutePath)
	for _, l := range tc.Locations {
		paths = append(paths, l.Path)
	}
	for _, path := range paths {
		if path != "" && !contains(req.Paths, path) {
			req.Paths = append(req.Paths, path)
		}
	}
	return req
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// signature canonicalizes a tool call for loop detection.
func signature(name string, input json.RawMessage) string {
	var v any
	if len(input) == 0 || json.Unmarshal(input, &v) != nil {
		return name + ":" + string(input)
	}
	if m, ok := v.(map[string]any); ok {
		delete(m, "toolSummary") // prose that changes between identical calls
		delete(m, "WaitMsBeforeAsync")
	}
	canon, err := json.Marshal(v) // encoding/json sorts map keys
	if err != nil {
		return name + ":" + string(input)
	}
	return name + ":" + string(canon)
}

// mcpServers is the session/new and session/load parameter (ACP stdio servers; env as
// name/value pairs).
func mcpServers(m *runtime.MCPServer) []any {
	if m == nil {
		return []any{}
	}
	env := make([]map[string]string, 0, len(m.Env))
	for _, kv := range m.Env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			env = append(env, map[string]string{"name": k, "value": v})
		}
	}
	sort.Slice(env, func(i, j int) bool { return env[i]["name"] < env[j]["name"] })
	args := m.Args
	if args == nil {
		args = []string{}
	}
	return []any{map[string]any{"name": m.Name, "command": m.Command, "args": args, "env": env}}
}
