package codex

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/runtime"
)

func TestFailureKind(t *testing.T) {
	t.Parallel()
	cases := map[string]runtime.FailureKind{
		`"usageLimitExceeded"`:                            runtime.FailQuotaExhausted,
		`"sessionBudgetExceeded"`:                         runtime.FailQuotaExhausted,
		`"rateLimitExceeded"`:                             runtime.FailRateLimited,
		`"serverOverloaded"`:                              runtime.FailRateLimited,
		`{"httpConnectionFailed":{"httpStatusCode":502}}`: runtime.FailRateLimited,
		`"unauthorized"`:                                  runtime.FailAuth,
		`"contextWindowExceeded"`:                         runtime.FailContextOverflow,
		`"cyberPolicy"`:                                   runtime.FailUnknown,
		`null`:                                            runtime.FailUnknown,
		``:                                                runtime.FailUnknown,
	}
	for raw, want := range cases {
		if got := failureKind(json.RawMessage(raw)); got != want {
			t.Errorf("failureKind(%s) = %s, want %s", raw, got, want)
		}
	}
}

func TestRateLimitSignal(t *testing.T) {
	t.Parallel()
	mins := func(m int64) *int64 { return &m }
	reset := time.Now().Add(time.Hour).Unix()
	snap := rateLimitSnapshot{
		Primary:   &rateLimitWindow{UsedPercent: 25, WindowDurationMins: mins(300), ResetsAt: &reset},
		Secondary: &rateLimitWindow{UsedPercent: 140, WindowDurationMins: mins(10080)},
	}
	sig := snap.signal()
	if sig == nil || len(sig.Windows) != 2 {
		t.Fatalf("signal = %+v", sig)
	}
	if w := sig.Windows[0]; w.Name != "five_hour" || w.Utilization != 0.25 || w.ResetsAt.Unix() != reset || w.Duration != 5*time.Hour {
		t.Errorf("primary = %+v", w)
	}
	if w := sig.Windows[1]; w.Name != "weekly" || w.Utilization != 1 {
		t.Errorf("secondary must be clamped to 1: %+v", w)
	}
	if !sig.Exhausted {
		t.Error("a full window means exhausted")
	}

	if (rateLimitSnapshot{}).signal() != nil {
		t.Error("empty snapshot must give no signal")
	}
	reached := rateLimitSnapshot{RateLimitReachedType: json.RawMessage(`"primary"`)}
	if s := reached.signal(); s == nil || !s.Exhausted {
		t.Error("rateLimitReachedType must mark exhaustion")
	}
	if windowName(30*24*time.Hour) != "window_43200m" || windowName(0) != "unknown" {
		t.Error("windowName")
	}
}

func TestRateLimitMergeKeepsMissingWindows(t *testing.T) {
	t.Parallel()
	base := rateLimitSnapshot{PlanType: "plus", Primary: &rateLimitWindow{UsedPercent: 10}, Secondary: &rateLimitWindow{UsedPercent: 20}}
	merged := base.merge(rateLimitSnapshot{Primary: &rateLimitWindow{UsedPercent: 30}})
	if merged.Primary.UsedPercent != 30 || merged.Secondary.UsedPercent != 20 || merged.PlanType != "plus" {
		t.Fatalf("merged = %+v", merged)
	}
}

func TestSupportedEfforts(t *testing.T) {
	t.Parallel()
	var res modelListResponse
	_ = json.Unmarshal([]byte(`{"data":[
		{"id":"a","isDefault":true,"supportedReasoningEfforts":[{"reasoningEffort":"low"},{"reasoningEffort":"xhigh"},{"reasoningEffort":"max"}]},
		{"id":"b","model":"b-model","supportedReasoningEfforts":[{"reasoningEffort":"medium"}]}]}`), &res)
	if got := res.supportedEfforts(""); len(got) != 2 || got[0] != runtime.EffortLow || got[1] != runtime.EffortMax {
		t.Errorf("default model efforts = %v", got)
	}
	if got := res.supportedEfforts("b-model"); len(got) != 1 || got[0] != runtime.EffortMedium {
		t.Errorf("by model name = %v", got)
	}
	if got := res.supportedEfforts("missing"); got != nil {
		t.Errorf("unknown model = %v", got)
	}
}

func TestTokenUsageSplitsCache(t *testing.T) {
	t.Parallel()
	u := tokenBreakdown{InputTokens: 100, CachedInputTokens: 30, CacheWriteInputTokens: 5, OutputTokens: 7}.usage()
	if u.InputTokens != 70 || u.CacheReadTokens != 30 || u.CacheWriteTokens != 5 || u.OutputTokens != 7 {
		t.Fatalf("usage = %+v", u)
	}
}

func TestToolTranslation(t *testing.T) {
	t.Parallel()
	exit1 := 1
	call := toolCall(threadItem{ID: "c", Type: "commandExecution", Command: "go test ./..."})
	if call == nil || call.Name != "Bash" || call.Signature != "Bash:go test ./..." {
		t.Fatalf("command call = %+v", call)
	}
	if res := toolResult(threadItem{ID: "c", Type: "commandExecution", Status: "completed", ExitCode: &exit1}); res == nil || !res.IsError {
		t.Fatalf("non-zero exit must be an error: %+v", res)
	}
	edit := toolCall(threadItem{ID: "f", Type: "fileChange", Changes: []fileChange{{Path: "a.go"}, {Path: "b.go"}}})
	if edit == nil || edit.Name != "Edit" || edit.Signature != "Edit:a.go,b.go" {
		t.Fatalf("file change call = %+v", edit)
	}
	mcp := toolCall(threadItem{ID: "m", Type: "mcpToolCall", Server: "openchef", Tool: "ask_user", Arguments: json.RawMessage(`{"q":1}`)})
	if mcp == nil || mcp.Name != "mcp__openchef__ask_user" {
		t.Fatalf("mcp call = %+v", mcp)
	}
	if toolCall(threadItem{Type: "reasoning"}) != nil {
		t.Error("non-tool items must not produce tool calls")
	}
	long := make([]byte, maxToolOutput*3)
	for i := range long {
		long[i] = 'x'
	}
	if got := truncate(string(long)); len(got) > maxToolOutput+10 {
		t.Errorf("output not truncated: %d bytes", len(got))
	}
}

func TestV2Decision(t *testing.T) {
	t.Parallel()
	if v2Decision(runtime.Allow()) != "accept" ||
		v2Decision(runtime.Decision{Allow: true, ForSession: true}) != "acceptForSession" ||
		v2Decision(runtime.Deny("x")) != "decline" {
		t.Fatal("decision mapping")
	}
}
