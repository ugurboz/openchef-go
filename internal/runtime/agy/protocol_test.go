package agy

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/runtime/linerpc"
)

func opts(levels ...string) []configOption {
	o := configOption{ID: "thought_level", CurrentValue: "high"}
	for _, l := range levels {
		o.Options = append(o.Options, struct {
			Value string `json:"value"`
		}{l})
	}
	return []configOption{{ID: "model", CurrentValue: "gemini-3.8-flash"}, o}
}

func TestThoughtLevel(t *testing.T) {
	t.Parallel()
	full := opts("low", "medium", "high", "xhigh")
	flash := opts("low", "medium", "high")
	cases := []struct {
		e    runtime.Effort
		o    []configOption
		want string
	}{
		{runtime.EffortLow, full, "low"},
		{runtime.EffortMedium, full, "medium"},
		{runtime.EffortHigh, full, "high"},
		{runtime.EffortMax, full, "xhigh"},
		{runtime.EffortMax, flash, "high"}, // the model offers no xhigh
		{runtime.EffortDefault, full, ""},
		{runtime.EffortLow, []configOption{{ID: "model"}}, ""}, // no thinking option
		{runtime.EffortLow, opts("medium", "high"), "medium"},  // lowest offered
	}
	for _, c := range cases {
		if got := thoughtLevel(c.e, c.o); got != c.want {
			t.Errorf("thoughtLevel(%q) = %q, want %q", c.e, got, c.want)
		}
	}
}

func TestUsageAndStop(t *testing.T) {
	t.Parallel()
	var r promptResult
	if err := json.Unmarshal([]byte(`{"stopReason":"end_turn","usage":{"inputTokens":2271,"outputTokens":3,"cachedReadTokens":17962,"cachedWriteTokens":0,"thoughtTokens":7,"totalTokens":20243}}`), &r); err != nil {
		t.Fatal(err)
	}
	want := runtime.Usage{InputTokens: 2271, OutputTokens: 10, CacheReadTokens: 17962}
	if u := r.usage(); u != want {
		t.Fatalf("usage %+v", u)
	}
	if (promptResult{}).usage() != (runtime.Usage{}) {
		t.Fatal("missing usage must be zero")
	}
	if got := addUsage(want, want); got.InputTokens != 4542 || got.OutputTokens != 20 || got.CacheReadTokens != 35924 {
		t.Fatalf("addUsage %+v", got)
	}
	for reason, want := range map[string]runtime.FailureKind{"cancelled": runtime.FailInterrupted, "max_tokens": runtime.FailUnknown, "refusal": runtime.FailUnknown, "???": runtime.FailUnknown} {
		if k, failed := stopFailure(reason, ""); !failed || k != want {
			t.Errorf("stopFailure(%s) = %s %t", reason, k, failed)
		}
	}
	if _, failed := stopFailure("end_turn", ""); failed {
		t.Error("end_turn is success")
	}
	if k, _ := stopFailure("cancelled", runtime.FailTimeout); k != runtime.FailTimeout {
		t.Errorf("our stop reason must win: %s", k)
	}
}

func TestClassifyError(t *testing.T) {
	t.Parallel()
	cases := map[string]runtime.FailureKind{
		"json-rpc error -32603: RESOURCE_EXHAUSTED: quota exceeded": runtime.FailQuotaExhausted,
		"You have no remaining credits":                             runtime.FailQuotaExhausted,
		"429 Too Many Requests":                                     runtime.FailRateLimited,
		"model is overloaded":                                       runtime.FailRateLimited,
		"UNAUTHENTICATED: please login":                             runtime.FailAuth,
		"prompt is too long for the context window":                 runtime.FailContextOverflow,
		"something else":                                            runtime.FailUnknown,
	}
	for msg, want := range cases {
		if got := classifyError(errors.New(msg)); got != want {
			t.Errorf("%q: %s, want %s", msg, got, want)
		}
	}
	if got := classifyError(fmt.Errorf("prompt: %w", linerpc.ErrClosed)); got != runtime.FailCrashed {
		t.Errorf("closed: %s", got)
	}
}

func TestToolNames(t *testing.T) {
	t.Parallel()
	cases := []struct{ name, kind, ask, want string }{
		{"run_command", "execute", "", "Bash"},
		{"write_file", "edit", "", "Write"},
		{"replace_file_content", "edit", "", "Edit"},
		{"view_file", "read", "", "Read"},
		{"read_url_content", "fetch", "", "WebFetch"},
		{"mcp_openchef_ask_user", "other", "openchef", "mcp__openchef__ask_user"},
		{"mcp_open_chef_ask_user", "other", "open_chef", "mcp__open_chef__ask_user"}, // server name with "_"
		{"mcp_github_list_prs", "other", "openchef", "mcp__github__list_prs"},
		{"brand_new_exec", "execute", "", "Bash"}, // unknown name, known kind
		{"brand_new", "other", "", "brand_new"},
		{"", "think", "", "think"},
	}
	for _, c := range cases {
		if got := toolName(c.name, c.kind, c.ask); got != c.want {
			t.Errorf("toolName(%s,%s) = %s, want %s", c.name, c.kind, got, c.want)
		}
	}
}

func TestApprovalRequestNormalization(t *testing.T) {
	t.Parallel()
	parse := func(raw string) permissionParams {
		var p permissionParams
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// Recorded shapes (testdata/agy/m18-bypass.jsonl, m18-nosandbox-review.jsonl).
	bypass := parse(`{"sessionId":"s","toolCall":{"toolCallId":"c1","name":"run_command","kind":"execute","title":"echo a > cmd.txt",
		"rawInput":{"BypassSandbox":true,"CommandLine":"echo a > cmd.txt","toolSummary":"Write a to cmd.txt"}},
		"options":[{"kind":"allow_once","optionId":"allow"},{"kind":"reject_once","optionId":"deny"}]}`)
	r := bypass.approvalRequest(json.RawMessage(`{}`), "openchef")
	if r.Kind != runtime.ApproveCommand || r.Tool != "Bash" || r.Command != "echo a > cmd.txt" || !strings.Contains(r.Description, "outside the agy sandbox") {
		t.Errorf("bypass: %+v", r)
	}
	if id, ok := bypass.optionID("allow_once"); !ok || id != "allow" {
		t.Errorf("allow option %q", id)
	}
	if id, ok := bypass.optionID("reject_once"); !ok || id != "deny" {
		t.Errorf("reject option %q", id)
	}
	if _, ok := bypass.optionID("allow_always"); ok {
		t.Error("allow_always must not be found")
	}
	edit := parse(`{"toolCall":{"toolCallId":"c2","name":"write_file","kind":"edit","title":"File edit.txt",
		"rawInput":{"Content":"x","Path":"/w/edit.txt"},"locations":[{"path":"/w/edit.txt"},{"path":"/w/other.txt"}]}}`)
	r = edit.approvalRequest(nil, "")
	if r.Kind != runtime.ApproveFileChange || r.Tool != "Write" || !slices.Equal(r.Paths, []string{"/w/edit.txt", "/w/other.txt"}) {
		t.Errorf("edit: %+v", r)
	}
	mcp := parse(`{"toolCall":{"name":"mcp_openchef_ask_user","kind":"other","title":"Run mcp_openchef_ask_user","rawInput":{"question":"?"}}}`)
	if r = mcp.approvalRequest(nil, "openchef"); r.Kind != runtime.ApproveTool || r.Tool != "mcp__openchef__ask_user" {
		t.Errorf("mcp: %+v", r)
	}
	plain := parse(`{"toolCall":{"name":"run_command","kind":"execute","title":"ls","rawInput":{}}}`)
	if r = plain.approvalRequest(nil, ""); r.Command != "ls" || strings.Contains(r.Description, "sandbox") {
		t.Errorf("plain: %+v", r)
	}
}

func TestUpdateParsing(t *testing.T) {
	t.Parallel()
	var u update
	_ = json.Unmarshal([]byte(`{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"OK"}}`), &u)
	if u.chunkText() != "OK" {
		t.Errorf("chunk %q", u.chunkText())
	}
	_ = json.Unmarshal([]byte(`{"sessionUpdate":"agent_message_chunk","content":{"type":"image","data":"..."}}`), &u)
	if u.chunkText() != "" {
		t.Error("non-text chunk")
	}
	cases := []struct {
		raw   string
		isErr bool
		out   string
	}{
		{`{"toolCallId":"c","status":"completed","rawOutput":{"exit_code":0,"output":"fine"}}`, false, "fine"},
		{`{"toolCallId":"c","status":"completed","rawOutput":{"exit_code":1,"output":"zsh: operation not permitted"}}`, true, "zsh: operation not permitted"},
		{`{"toolCallId":"c","status":"failed","rawOutput":"permission check failed"}`, true, "permission check failed"},
		{`{"toolCallId":"c","status":"completed","rawOutput":{},"content":[{"type":"diff","path":"/w/a","newText":"x"}]}`, false, "edited /w/a"},
		{`{"toolCallId":"c","status":"completed","content":[{"type":"content","content":{"type":"text","text":"from content"}}]}`, false, "from content"},
	}
	for _, c := range cases {
		var u update
		if err := json.Unmarshal([]byte(c.raw), &u); err != nil {
			t.Fatal(err)
		}
		r := u.result()
		if r.IsError != c.isErr || r.Output != c.out || r.ID != "c" {
			t.Errorf("%s: %+v", c.raw, r)
		}
	}
}

func TestSignatureIgnoresProse(t *testing.T) {
	t.Parallel()
	a := signature("Bash", json.RawMessage(`{"CommandLine":"ls","toolSummary":"List files","WaitMsBeforeAsync":5000}`))
	b := signature("Bash", json.RawMessage(`{"toolSummary":"Show the files","CommandLine":"ls"}`))
	if a != b || !strings.Contains(a, `"CommandLine":"ls"`) {
		t.Errorf("%s vs %s", a, b)
	}
	if signature("X", json.RawMessage(`bad`)) != "X:bad" {
		t.Error("invalid input")
	}
}

func TestMCPServersParam(t *testing.T) {
	t.Parallel()
	if got, _ := json.Marshal(mcpServers(nil)); string(got) != `[]` {
		t.Errorf("nil: %s", got)
	}
	got, _ := json.Marshal(mcpServers(&runtime.MCPServer{Name: "openchef", Command: "/bin/oc", Env: []string{"B=2", "A=1", "bad"}}))
	want := `[{"args":[],"command":"/bin/oc","env":[{"name":"A","value":"1"},{"name":"B","value":"2"}],"name":"openchef"}]`
	if string(got) != want {
		t.Errorf("got %s", got)
	}
}

func TestTruncateAndTail(t *testing.T) {
	t.Parallel()
	if got := truncate(strings.Repeat("x", 3*maxToolOutput)); len(got) > maxToolOutput+8 || !strings.Contains(got, "…") {
		t.Errorf("truncate: %d", len(got))
	}
	var tb tailBuffer
	_, _ = tb.Write([]byte(strings.Repeat("a", stderrTail)))
	_, _ = tb.Write([]byte("END\n"))
	if s := tb.String(); len(s) > stderrTail || !strings.HasSuffix(s, "END") {
		t.Errorf("tail: %d", len(s))
	}
}
