package claude

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/runtime"
)

func intp(i int) *int { return &i }

func TestOutcomeClassification(t *testing.T) {
	t.Parallel()
	exhausted := &runtime.QuotaSignal{Exhausted: true, Windows: []runtime.QuotaWindow{
		{Name: "five_hour", Utilization: 1, ResetsAt: time.Unix(2000, 0)},
		{Name: "weekly", Utilization: 1, ResetsAt: time.Unix(1000, 0)},
		{Name: "other", Utilization: 0.5, ResetsAt: time.Unix(10, 0)},
	}}
	cases := []struct {
		name  string
		r     resultMessage
		stop  runtime.FailureKind
		quota *runtime.QuotaSignal
		want  runtime.FailureKind
	}{
		{"aborted", resultMessage{Subtype: "error_during_execution", IsError: true, TerminalReason: "aborted_streaming"}, "", nil, runtime.FailInterrupted},
		{"aborted by our timeout", resultMessage{Subtype: "error_during_execution", IsError: true, TerminalReason: "aborted_streaming"}, runtime.FailTimeout, nil, runtime.FailTimeout},
		{"budget is not quota", resultMessage{Subtype: "error_max_budget_usd", IsError: true}, "", nil, runtime.FailUnknown},
		{"schema retries", resultMessage{Subtype: "error_max_structured_output_retries", IsError: true}, "", nil, runtime.FailProtocol},
		{"429", resultMessage{Subtype: "success", IsError: true, APIErrorStatus: intp(429)}, "", nil, runtime.FailRateLimited},
		{"429 exhausted", resultMessage{Subtype: "success", IsError: true, APIErrorStatus: intp(429)}, "", exhausted, runtime.FailQuotaExhausted},
		{"529", resultMessage{Subtype: "success", IsError: true, APIErrorStatus: intp(529)}, "", nil, runtime.FailRateLimited},
		{"401", resultMessage{Subtype: "success", IsError: true, APIErrorStatus: intp(401)}, "", nil, runtime.FailAuth},
		{"413", resultMessage{Subtype: "success", IsError: true, APIErrorStatus: intp(413)}, "", nil, runtime.FailContextOverflow},
		{"usage limit text", resultMessage{Subtype: "success", IsError: true, Result: "Claude AI usage limit reached|1796072400"}, "", nil, runtime.FailQuotaExhausted},
		{"prompt too long", resultMessage{Subtype: "success", IsError: true, Result: "Prompt is too long"}, "", nil, runtime.FailContextOverflow},
		{"login", resultMessage{Subtype: "success", IsError: true, Result: "Invalid API key · Please run /login"}, "", nil, runtime.FailAuth},
		{"unknown, our stop wins", resultMessage{Subtype: "error_during_execution", IsError: true}, runtime.FailTimeout, nil, runtime.FailTimeout},
		{"auth beats our stop", resultMessage{Subtype: "success", IsError: true, APIErrorStatus: intp(401)}, runtime.FailTimeout, nil, runtime.FailAuth},
	}
	for _, c := range cases {
		ev := c.r.outcome(c.stop, c.quota)
		if ev.Kind != runtime.EventTurnFailed || ev.Failure.Kind != c.want {
			t.Errorf("%s: got %s %+v, want %s", c.name, ev.Kind, ev.Failure, c.want)
		}
	}
	ev := resultMessage{Subtype: "success", IsError: true, APIErrorStatus: intp(429)}.outcome("", exhausted)
	if !ev.Failure.ResetsAt.Equal(time.Unix(1000, 0)) {
		t.Errorf("ResetsAt %v, want the earliest exhausted window", ev.Failure.ResetsAt)
	}
}

func TestOutcomeSuccess(t *testing.T) {
	t.Parallel()
	var r resultMessage
	raw := `{"type":"result","subtype":"success","is_error":false,"result":"DONE","duration_ms":1500,
		"total_cost_usd":0.012,"permission_denials":[{"tool_name":"Bash"}],
		"structured_output":{"ok":true},"result_index":0,
		"modelUsage":{"claude-sonnet-5-5":{},"claude-haiku-5-5":{}},
		"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":40}}`
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		t.Fatal(err)
	}
	ev := r.outcome("", nil)
	if ev.Kind != runtime.EventTurnCompleted {
		t.Fatalf("%+v", ev)
	}
	res := ev.Result
	want := runtime.Usage{InputTokens: 10, OutputTokens: 20, CacheReadTokens: 30, CacheWriteTokens: 40, CostUSD: 0.012}
	if res.FinalText != "DONE" || res.Usage != want || res.Denials != 1 || res.Duration != 1500*time.Millisecond ||
		string(res.Structured) != `{"ok":true}` || !slices.Equal(res.Models, []string{"claude-haiku-5-5", "claude-sonnet-5-5"}) {
		t.Fatalf("%+v", res)
	}
	r.StructuredOutput = json.RawMessage("null")
	if ev := r.outcome("", nil); ev.Result.Structured != nil {
		t.Fatal("null structured_output must be dropped")
	}
}

func TestQuotaSignal(t *testing.T) {
	t.Parallel()
	// Recorded shape (testdata/claude): overageStatus "rejected" is the normal state of
	// accounts without overage and must not mean exhausted.
	raw := `{"overageStatus":"rejected","rateLimitType":"seven_day","resetsAt":1796072400,"status":"allowed_warning",
		"unifiedWindows":{"five_hour":{"resetsAt":1795560600,"status":"allowed","utilization":0.57},
		"seven_day":{"resetsAt":1796072400,"status":"allowed","utilization":0.35}}}`
	sig := quotaSignal(json.RawMessage(raw))
	if sig == nil || sig.Exhausted || len(sig.Windows) != 2 {
		t.Fatalf("%+v", sig)
	}
	fh, wk := sig.Windows[0], sig.Windows[1]
	if fh.Name != "five_hour" || fh.Utilization != 0.57 || fh.Duration != 5*time.Hour || !fh.ResetsAt.Equal(time.Unix(1795560600, 0)) {
		t.Errorf("five_hour %+v", fh)
	}
	if wk.Name != "weekly" || wk.Utilization != 0.35 || wk.Duration != 7*24*time.Hour {
		t.Errorf("weekly %+v", wk)
	}

	rej := quotaSignal(json.RawMessage(`{"status":"rejected","overageStatus":"rejected","unifiedWindows":{"five_hour":{"status":"rejected","utilization":0.99,"resetsAt":5}}}`))
	if rej == nil || !rej.Exhausted || rej.Windows[0].Utilization != 1 {
		t.Fatalf("rejected: %+v", rej)
	}
	if sig := quotaSignal(json.RawMessage(`{"status":"rejected","overageStatus":"allowed"}`)); sig != nil {
		t.Fatalf("overage absorbs the limit: %+v", sig)
	}
	if quotaSignal(json.RawMessage(`not json`)) != nil || quotaSignal(json.RawMessage(`{}`)) != nil {
		t.Fatal("empty or invalid info must give no signal")
	}
	if sig := quotaSignal(json.RawMessage(`{"unifiedWindows":{"x":{"utilization":7}}}`)); sig.Windows[0].Utilization != 1 {
		t.Fatal("utilization not clamped")
	}
}

func TestApprovalRequestNormalization(t *testing.T) {
	t.Parallel()
	cases := []struct {
		req   canUseToolRequest
		kind  runtime.ApprovalKind
		cmd   string
		paths []string
	}{
		{canUseToolRequest{ToolName: "Bash", Input: json.RawMessage(`{"command":"echo m > cmd.txt"}`), BlockedPath: "/w/cmd.txt"}, runtime.ApproveCommand, "echo m > cmd.txt", []string{"/w/cmd.txt"}},
		{canUseToolRequest{ToolName: "Write", Input: json.RawMessage(`{"file_path":"/w/a.txt","content":"x"}`), BlockedPath: "/w/a.txt"}, runtime.ApproveFileChange, "", []string{"/w/a.txt"}},
		{canUseToolRequest{ToolName: "NotebookEdit", Input: json.RawMessage(`{"notebook_path":"/w/n.ipynb"}`)}, runtime.ApproveFileChange, "", []string{"/w/n.ipynb"}},
		{canUseToolRequest{ToolName: "ExitPlanMode", Input: json.RawMessage(`{}`)}, runtime.ApproveModeChange, "", nil},
		{canUseToolRequest{ToolName: "mcp__x__y", Input: json.RawMessage(`{"path":"/p"}`)}, runtime.ApproveTool, "", []string{"/p"}},
		{canUseToolRequest{ToolName: "Bash", Input: json.RawMessage(`garbage`)}, runtime.ApproveCommand, "", nil},
	}
	for _, c := range cases {
		ar := c.req.approvalRequest(json.RawMessage(`{"raw":1}`))
		if ar.Kind != c.kind || ar.Command != c.cmd || !slices.Equal(ar.Paths, c.paths) || ar.Tool != c.req.ToolName || string(ar.Raw) != `{"raw":1}` {
			t.Errorf("%s: %+v", c.req.ToolName, ar)
		}
	}
}

func TestPermissionResponseShape(t *testing.T) {
	t.Parallel()
	allow, _ := json.Marshal(permissionResponse("r1", json.RawMessage(`{"command":"ls"}`), runtime.Allow()))
	if s := string(allow); !strings.Contains(s, `"behavior":"allow"`) || !strings.Contains(s, `"updatedInput":{"command":"ls"}`) || !strings.Contains(s, `"request_id":"r1"`) {
		t.Errorf("allow: %s", s)
	}
	noInput, _ := json.Marshal(permissionResponse("r2", nil, runtime.Allow()))
	if !strings.Contains(string(noInput), `"updatedInput":{}`) {
		t.Errorf("allow without input: %s", noInput)
	}
	deny, _ := json.Marshal(permissionResponse("r3", nil, runtime.Deny("")))
	if s := string(deny); !strings.Contains(s, `"behavior":"deny"`) || !strings.Contains(s, "denied by OpenChef") {
		t.Errorf("deny: %s", s)
	}
}

func TestSmallHelpers(t *testing.T) {
	t.Parallel()
	if a, b := signature("Bash", json.RawMessage(`{"b":1,"a":2}`)), signature("Bash", json.RawMessage(`{ "a":2, "b":1 }`)); a != b {
		t.Errorf("signature not canonical: %s vs %s", a, b)
	}
	if got := signature("X", json.RawMessage(`bad`)); got != "X:bad" {
		t.Errorf("signature of invalid input: %s", got)
	}
	if got := toolResultText(json.RawMessage(`"plain"`)); got != "plain" {
		t.Errorf("string content: %q", got)
	}
	if got := toolResultText(json.RawMessage(`[{"type":"text","text":"a"},{"type":"image"},{"type":"text","text":"b"}]`)); got != "a\nb" {
		t.Errorf("block content: %q", got)
	}
	if toolResultText(nil) != "" || toolResultText(json.RawMessage(`7`)) != "" {
		t.Error("empty content")
	}
	long := strings.Repeat("x", maxToolOutput*3)
	if got := truncate(long); len(got) > maxToolOutput+8 || !strings.Contains(got, "…") {
		t.Errorf("truncate: %d bytes", len(got))
	}
	if n := stillQueued(json.RawMessage(`{"still_queued":[]}`)); n != 0 {
		t.Errorf("still_queued empty: %d", n)
	}
	if n := stillQueued(json.RawMessage(`{"still_queued":["u1","u2"]}`)); n != 2 {
		t.Errorf("still_queued: %d", n)
	}
	if n := stillQueued(nil); n != 0 {
		t.Errorf("still_queued nil: %d", n)
	}
	var tb tailBuffer
	_, _ = tb.Write([]byte(strings.Repeat("a", stderrTail)))
	_, _ = tb.Write([]byte("END\n"))
	if s := tb.String(); len(s) > stderrTail || !strings.HasSuffix(s, "END") {
		t.Errorf("tail buffer: %d bytes", len(s))
	}
}
