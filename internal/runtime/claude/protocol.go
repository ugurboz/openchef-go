package claude

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ugurboz/openchef/internal/runtime"
)

// Wire types of `claude -p --input-format stream-json --output-format stream-json` and
// the Agent SDK control protocol, as recorded with claude 2.1.325
// (docs/research/m1-findings.md §3, §9, §10). Unknown fields are ignored.

type envelope struct {
	Type            string          `json:"type"`
	Subtype         string          `json:"subtype"`
	SessionID       string          `json:"session_id"`
	ParentToolUseID *string         `json:"parent_tool_use_id"`
	Event           json.RawMessage `json:"event"`
	Message         json.RawMessage `json:"message"`
	RequestID       string          `json:"request_id"`
	Request         json.RawMessage `json:"request"`
	Response        json.RawMessage `json:"response"`
	RateLimitInfo   json.RawMessage `json:"rate_limit_info"`
	Model           string          `json:"model"`
}

// fromSubagent reports whether the message belongs to a subagent rather than the turn
// itself. Subagents are disabled (D-006); this is a safety net.
func (e envelope) fromSubagent() bool { return e.ParentToolUseID != nil && *e.ParentToolUseID != "" }

type streamEvent struct {
	Type  string `json:"type"`
	Delta struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"delta"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	Text      string          `json:"text"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

type chatMessage struct {
	Content []contentBlock `json:"content"`
}

// toolResultText flattens a tool_result content (string or list of text blocks).
func toolResultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []contentBlock
	if json.Unmarshal(raw, &blocks) == nil {
		parts := make([]string, 0, len(blocks))
		for i := range blocks {
			if blocks[i].Text != "" {
				parts = append(parts, blocks[i].Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// structuredOutputTool is the synthetic tool Claude uses for --json-schema output; it is
// plumbing, not agent work, so it never becomes a tool call event.
const structuredOutputTool = "StructuredOutput"

type resultMessage struct {
	Subtype           string                     `json:"subtype"`
	IsError           bool                       `json:"is_error"`
	Result            string                     `json:"result"`
	StructuredOutput  json.RawMessage            `json:"structured_output"`
	TerminalReason    string                     `json:"terminal_reason"`
	APIErrorStatus    *int                       `json:"api_error_status"`
	ResultIndex       *int                       `json:"result_index"`
	DurationMs        int64                      `json:"duration_ms"`
	TotalCostUSD      float64                    `json:"total_cost_usd"`
	PermissionDenials []json.RawMessage          `json:"permission_denials"`
	ModelUsage        map[string]json.RawMessage `json:"modelUsage"`
	Usage             struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

func (r resultMessage) usage() runtime.Usage {
	return runtime.Usage{
		InputTokens:      r.Usage.InputTokens,
		OutputTokens:     r.Usage.OutputTokens,
		CacheReadTokens:  r.Usage.CacheReadInputTokens,
		CacheWriteTokens: r.Usage.CacheCreationInputTokens,
		CostUSD:          r.TotalCostUSD,
	}
}

func (r resultMessage) models() []string {
	out := make([]string, 0, len(r.ModelUsage))
	for m := range r.ModelUsage {
		out = append(out, m)
	}
	sort.Strings(out)
	return out
}

// outcome converts a result into the terminal event. stop, if set, is why OpenChef
// interrupted the turn and wins over Claude's generic abort. quota is the latest
// in-stream quota signal (may be nil).
func (r resultMessage) outcome(stop runtime.FailureKind, quota *runtime.QuotaSignal) runtime.Event {
	usage := r.usage()
	duration := time.Duration(r.DurationMs) * time.Millisecond
	if r.Subtype == "success" && !r.IsError {
		res := &runtime.TurnResult{
			FinalText: r.Result,
			Usage:     usage,
			Denials:   len(r.PermissionDenials),
			Duration:  duration,
			Models:    r.models(),
		}
		if len(r.StructuredOutput) > 0 && string(r.StructuredOutput) != "null" {
			res.Structured = r.StructuredOutput
		}
		return runtime.Event{Kind: runtime.EventTurnCompleted, Result: res}
	}
	kind, msg := r.classify(quota)
	if stop != "" && (kind == runtime.FailInterrupted || kind == runtime.FailUnknown) {
		kind = stop
	}
	f := &runtime.Failure{Kind: kind, Message: msg, Usage: usage}
	if (kind == runtime.FailQuotaExhausted || kind == runtime.FailRateLimited) && quota != nil {
		f.ResetsAt = earliestFullReset(quota)
	}
	return runtime.Event{Kind: runtime.EventTurnFailed, Failure: f}
}

func (r resultMessage) classify(quota *runtime.QuotaSignal) (kind runtime.FailureKind, msg string) {
	msg = r.Subtype
	if r.TerminalReason != "" {
		msg += " (" + r.TerminalReason + ")"
	}
	if r.Result != "" {
		msg += ": " + r.Result
	}
	switch r.Subtype {
	case "error_during_execution":
		if r.TerminalReason == "aborted_streaming" || r.TerminalReason == "aborted" {
			return runtime.FailInterrupted, msg
		}
	case "error_max_budget_usd":
		// A --max-budget-usd cap, not account quota: treating it as quota would park the
		// task in waiting_quota. OpenChef does not pass the flag today.
		return runtime.FailUnknown, msg
	case "error_max_structured_output_retries":
		return runtime.FailProtocol, msg
	}
	if r.APIErrorStatus != nil {
		switch s := *r.APIErrorStatus; {
		case s == 429 || s == 529 || s >= 500:
			if quota != nil && quota.Exhausted {
				return runtime.FailQuotaExhausted, msg
			}
			return runtime.FailRateLimited, msg
		case s == 401 || s == 403:
			return runtime.FailAuth, msg
		case s == 413:
			return runtime.FailContextOverflow, msg
		}
	}
	low := strings.ToLower(r.Result)
	switch {
	case quota != nil && quota.Exhausted,
		strings.Contains(low, "usage limit"), strings.Contains(low, "limit reached"):
		return runtime.FailQuotaExhausted, msg
	case strings.Contains(low, "rate limit"), strings.Contains(low, "overloaded"):
		return runtime.FailRateLimited, msg
	case strings.Contains(low, "prompt is too long"), strings.Contains(low, "context window"):
		return runtime.FailContextOverflow, msg
	case strings.Contains(low, "authentication"), strings.Contains(low, "/login"), strings.Contains(low, "unauthorized"):
		return runtime.FailAuth, msg
	}
	return runtime.FailUnknown, msg
}

func earliestFullReset(q *runtime.QuotaSignal) time.Time {
	var best time.Time
	for _, w := range q.Windows {
		if w.Utilization >= 1 && !w.ResetsAt.IsZero() && (best.IsZero() || w.ResetsAt.Before(best)) {
			best = w.ResetsAt
		}
	}
	return best
}

type rateLimitInfo struct {
	Status         string  `json:"status"`
	OverageStatus  string  `json:"overageStatus"`
	ResetsAt       int64   `json:"resetsAt"`
	Utilization    float64 `json:"utilization"`
	RateLimitType  string  `json:"rateLimitType"`
	UnifiedWindows map[string]struct {
		Status      string  `json:"status"`
		Utilization float64 `json:"utilization"`
		ResetsAt    int64   `json:"resetsAt"`
	} `json:"unifiedWindows"`
}

// quotaSignal normalizes a rate_limit_event. Utilizations are fractions and resets are
// epoch seconds (m1-findings §1). A limit is exhausted only when the request was
// rejected *and* no overage can absorb it; overageStatus "rejected" alone is the normal
// state of accounts without overage.
func quotaSignal(raw json.RawMessage) *runtime.QuotaSignal {
	var info rateLimitInfo
	if json.Unmarshal(raw, &info) != nil {
		return nil
	}
	sig := &runtime.QuotaSignal{}
	names := make([]string, 0, len(info.UnifiedWindows))
	for n := range info.UnifiedWindows {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		w := info.UnifiedWindows[n]
		qw := runtime.QuotaWindow{Name: windowName(n), Utilization: clamp01(w.Utilization)}
		if w.ResetsAt > 0 {
			qw.ResetsAt = time.Unix(w.ResetsAt, 0).UTC()
		}
		switch n {
		case "five_hour":
			qw.Duration = 5 * time.Hour
		case "seven_day":
			qw.Duration = 7 * 24 * time.Hour
		}
		if w.Status == "rejected" {
			qw.Utilization = 1
		}
		sig.Windows = append(sig.Windows, qw)
	}
	if info.Status == "rejected" && info.OverageStatus != "allowed" {
		sig.Exhausted = true
	}
	if len(sig.Windows) == 0 && !sig.Exhausted {
		return nil
	}
	return sig
}

func windowName(n string) string {
	if n == "seven_day" {
		return "weekly"
	}
	return n
}

func clamp01(f float64) float64 { return max(0, min(1, f)) }

// Control protocol.

type canUseToolRequest struct {
	Subtype     string          `json:"subtype"`
	ToolName    string          `json:"tool_name"`
	Input       json.RawMessage `json:"input"`
	Description string          `json:"description"`
	BlockedPath string          `json:"blocked_path"`
	ToolUseID   string          `json:"tool_use_id"`
}

type controlResponseBody struct {
	Subtype   string          `json:"subtype"`
	RequestID string          `json:"request_id"`
	Error     string          `json:"error"`
	Response  json.RawMessage `json:"response"`
}

// approvalRequest normalizes a can_use_tool request.
func (r canUseToolRequest) approvalRequest(raw json.RawMessage) runtime.ApprovalRequest {
	req := runtime.ApprovalRequest{
		Tool: r.ToolName, Input: r.Input, Description: r.Description, Raw: raw,
	}
	var in struct {
		Command      string `json:"command"`
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
		Path         string `json:"path"`
	}
	_ = json.Unmarshal(r.Input, &in)
	switch r.ToolName {
	case "Bash":
		req.Kind = runtime.ApproveCommand
		req.Command = in.Command
	case "Edit", "Write", "MultiEdit", "NotebookEdit":
		req.Kind = runtime.ApproveFileChange
	case "ExitPlanMode", "EnterPlanMode":
		req.Kind = runtime.ApproveModeChange
	default:
		req.Kind = runtime.ApproveTool
	}
	for _, p := range []string{r.BlockedPath, in.FilePath, in.NotebookPath, in.Path} {
		if p != "" && !contains(req.Paths, p) {
			req.Paths = append(req.Paths, p)
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

func permissionResponse(requestID string, input json.RawMessage, d runtime.Decision) map[string]any {
	var decision map[string]any
	if d.Allow {
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		decision = map[string]any{"behavior": "allow", "updatedInput": input}
	} else {
		msg := d.Message
		if msg == "" {
			msg = "denied by OpenChef"
		}
		decision = map[string]any{"behavior": "deny", "message": msg}
	}
	return map[string]any{
		"type": "control_response",
		"response": map[string]any{
			"subtype":    "success",
			"request_id": requestID,
			"response":   decision,
		},
	}
}

func errorResponse(requestID, msg string) map[string]any {
	return map[string]any{
		"type":     "control_response",
		"response": map[string]any{"subtype": "error", "request_id": requestID, "error": msg},
	}
}

func userMessage(text string) map[string]any {
	return map[string]any{
		"type": "user",
		"message": map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "text", "text": text}},
		},
	}
}

// signature canonicalizes a tool call for loop detection: name plus the input with
// object keys sorted.
func signature(name string, input json.RawMessage) string {
	var v any
	if len(input) == 0 || json.Unmarshal(input, &v) != nil {
		return name + ":" + string(input)
	}
	canon, err := json.Marshal(v) // encoding/json sorts map keys
	if err != nil {
		return name + ":" + string(input)
	}
	return name + ":" + string(canon)
}

// stillQueued counts the user messages an interrupt reported as not yet started.
func stillQueued(resp json.RawMessage) int {
	var body struct {
		StillQueued []json.RawMessage `json:"still_queued"`
	}
	if json.Unmarshal(resp, &body) != nil {
		return 0
	}
	return len(body.StillQueued)
}

var errControlRejected = errors.New("claude: control request rejected")

func controlError(subtype, msg string) error {
	return fmt.Errorf("%w: %s: %s", errControlRejected, subtype, msg)
}
