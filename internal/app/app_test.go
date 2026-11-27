package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ugurboz/openchef/internal/config"
	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/service"
	"github.com/ugurboz/openchef/internal/task"
)

// fakeQueue answers every request with its status/answer and records what it was asked.
type fakeQueue struct {
	status task.ApprovalStatus
	answer string
	asked  []service.Request
}

func (q *fakeQueue) Ask(_ context.Context, req service.Request) (task.Approval, error) {
	q.asked = append(q.asked, req)
	return task.Approval{Kind: req.Kind, Status: q.status, Answer: q.answer}, nil
}

func testPolicies(t *testing.T, settings string) (pols policies, ws string) {
	t.Helper()
	dir := t.TempDir()
	user := filepath.Join(dir, "settings.toml")
	if settings != "" {
		if err := os.WriteFile(user, []byte(settings), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ws = filepath.Join(dir, "ws")
	if err := os.MkdirAll(filepath.Join(ws, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	return policies{settings: user, home: filepath.Join(dir, "home"), private: []string{filepath.Join(dir, "state.db")}}, ws
}

func testTask(ws string, mode task.PermissionMode) *task.Task {
	return &task.Task{ID: "t", RepoRoot: ws, Worktree: ws, Mode: task.ModeIsolated, Permissions: mode}
}

// The §9.3 matrix as each adapter normalizes requests (Claude, Codex and agy tool
// names and shapes), for every mode: what the approver decides at once and what it asks.
func TestApproverModeMatrix(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pols, ws := testPolicies(t, "[permissions]\nallow = [\"Bash(go test:*)\"]\ndeny = [\"Bash(rm:*)\"]\n")
	inside, outside := filepath.Join(ws, "main.go"), filepath.Join(filepath.Dir(ws), "elsewhere.go")
	type shape struct {
		name string
		req  runtime.ApprovalRequest
	}
	shapes := map[string][]shape{
		"edit inside": {
			{"claude", runtime.ApprovalRequest{Kind: runtime.ApproveFileChange, Tool: "Edit", Paths: []string{inside}}},
			{"codex", runtime.ApprovalRequest{Kind: runtime.ApproveFileChange, Tool: "Edit", Paths: []string{inside}}},
			{"agy", runtime.ApprovalRequest{Kind: runtime.ApproveFileChange, Tool: "Write", Paths: []string{inside}}},
		},
		"edit outside": {
			{"claude", runtime.ApprovalRequest{Kind: runtime.ApproveFileChange, Tool: "Write", Paths: []string{outside}}},
			{"codex", runtime.ApprovalRequest{Kind: runtime.ApproveFileChange, Tool: "Edit", Paths: []string{inside, outside}}},
			{"agy", runtime.ApprovalRequest{Kind: runtime.ApproveFileChange, Tool: "Edit", Paths: []string{outside}}},
		},
		"allowed command": {
			{"claude", runtime.ApprovalRequest{Kind: runtime.ApproveCommand, Tool: "Bash", Command: "go test ./..."}},
			{"codex", runtime.ApprovalRequest{Kind: runtime.ApproveCommand, Tool: "Bash", Command: "go test ./...", Paths: []string{ws}}},
		},
		"unknown command": {
			{"agy", runtime.ApprovalRequest{Kind: runtime.ApproveCommand, Tool: "Bash", Command: "make"}},
		},
		"denied command": {
			{"codex", runtime.ApprovalRequest{Kind: runtime.ApproveCommand, Tool: "Bash", Command: "go test ./... && rm -rf ~"}},
		},
	}
	const (
		allow = "allow"
		ask   = "ask"
		deny  = "deny"
	)
	want := map[task.PermissionMode]map[string]string{
		task.PermissionsManual:            {"edit inside": ask, "edit outside": deny, "allowed command": allow, "unknown command": ask, "denied command": deny},
		task.PermissionsAcceptEdits:       {"edit inside": allow, "edit outside": deny, "allowed command": allow, "unknown command": ask, "denied command": deny},
		task.PermissionsAuto:              {"edit inside": allow, "edit outside": deny, "allowed command": allow, "unknown command": ask, "denied command": deny},
		task.PermissionsPlan:              {"edit inside": deny, "edit outside": deny, "allowed command": deny, "unknown command": deny, "denied command": deny},
		task.PermissionsDontAsk:           {"edit inside": deny, "edit outside": deny, "allowed command": allow, "unknown command": deny, "denied command": deny},
		task.PermissionsBypassPermissions: {"edit inside": allow, "edit outside": deny, "allowed command": allow, "unknown command": allow, "denied command": deny},
	}
	for mode, cases := range want {
		ps, err := pols.session(testTask(ws, mode))
		if err != nil {
			t.Fatalf("%s: %v", mode, err)
		}
		for what, expect := range cases {
			for _, sh := range shapes[what] {
				q := &fakeQueue{status: task.ApprovalAllowed}
				d, err := approver{queue: q, eval: ps.eval, task: "t", agent: sh.name}.Approve(ctx, sh.req)
				got := deny
				switch {
				case err != nil:
					got = "error: " + err.Error()
				case len(q.asked) > 0:
					got = ask
				case d.Allow:
					got = allow
				}
				if got != expect {
					t.Errorf("%s, %s from %s: %s (%s); want %s", mode, what, sh.name, got, d.Message, expect)
				}
			}
		}
	}
}

func TestSessionPolicy(t *testing.T) {
	t.Parallel()
	pols, ws := testPolicies(t, "[permissions]\ndefaultMode = \"acceptEdits\"\ndeny = [\"Read(./.env)\"]\nask = [\"Bash(git push:*)\"]\n")
	if m, err := pols.defaultMode(ws); err != nil || m != task.PermissionsAcceptEdits {
		t.Fatalf("default mode %q, %v", m, err)
	}
	ps, err := pols.session(testTask(ws, task.PermissionsManual))
	if err != nil {
		t.Fatal(err)
	}
	// The task's mode wins; deny and ask rules reach the CLI natively, after the invariants.
	if ps.mode != runtime.ModeManual || !slices.Contains(ps.denyRules, "Read(./.env)") ||
		!slices.Contains(ps.askRules, "Bash(git push:*)") || len(ps.denyRules) != len(runtime.InvariantDenyRules)+1 {
		t.Fatalf("session %+v", ps)
	}
	team := testTask(ws, task.PermissionsBypassPermissions)
	team.Mode = task.ModeTeam
	if _, err := pols.session(team); !errors.Is(err, errBypassOutsideWorktree) {
		t.Fatalf("bypass in team mode: %v", err)
	}
	bad, ws2 := testPolicies(t, "[permissions]\ndeny = [\"Bash(\"]\n")
	if _, err := bad.session(testTask(ws2, task.PermissionsManual)); err == nil {
		t.Fatal("a broken deny rule loaded")
	}
}

func TestApproverAsksTheUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	pols, ws := testPolicies(t, "")
	ps, err := pols.session(testTask(ws, task.PermissionsAcceptEdits))
	if err != nil {
		t.Fatal(err)
	}
	cmd := runtime.ApprovalRequest{Kind: runtime.ApproveCommand, Tool: "Bash", Command: "go test ./...\nrm -rf /tmp/x"}
	for _, tc := range []struct {
		status task.ApprovalStatus
		answer string
		allow  bool
		msg    string
	}{
		{task.ApprovalAllowed, "", true, ""},
		{task.ApprovalDenied, "use make test", false, "They said: use make test"},
		{task.ApprovalExpired, "No one answered within 15m0s.", false, "No one answered"},
	} {
		q := &fakeQueue{status: tc.status, answer: tc.answer}
		d, err := approver{queue: q, eval: ps.eval, task: "t", agent: "opus"}.Approve(ctx, cmd)
		if err != nil || d.Allow != tc.allow || !strings.Contains(d.Message, tc.msg) {
			t.Errorf("%s: %+v, %v", tc.status, d, err)
		}
		if len(q.asked) != 1 || q.asked[0].Summary != cmd.Command || q.asked[0].Kind != task.ApprovalCommand || q.asked[0].Agent != "opus" {
			t.Errorf("asked %+v", q.asked)
		}
	}
	// A mode change is always the user's call.
	q := &fakeQueue{status: task.ApprovalDenied}
	if d, _ := (approver{queue: q, eval: ps.eval}).Approve(ctx, runtime.ApprovalRequest{Kind: runtime.ApproveModeChange, Tool: "ExitPlanMode"}); d.Allow || len(q.asked) != 1 {
		t.Errorf("mode change: %+v, asked %d", d, len(q.asked))
	}
	q = &fakeQueue{status: task.ApprovalAnswered, answer: "8080"}
	got, err := approver{queue: q}.Answer(ctx, runtime.Question{Text: "Which port?", Options: []string{"8080"}})
	if err != nil || got != "8080" || q.asked[0].Kind != task.ApprovalQuestion {
		t.Fatalf("answer %q, %v, %+v", got, err, q.asked)
	}
	q = &fakeQueue{status: task.ApprovalExpired}
	if got, _ := (approver{queue: q}).Answer(ctx, runtime.Question{Text: "?"}); got != unansweredQuestion {
		t.Fatalf("expired question answered %q", got)
	}
}

// The domain and runtime layers name the same permission modes.
func TestPermissionModesMatchRuntime(t *testing.T) {
	t.Parallel()
	if len(task.PermissionModes) != len(runtime.Modes) {
		t.Fatalf("%v vs %v", task.PermissionModes, runtime.Modes)
	}
	for i, m := range runtime.Modes {
		if string(task.PermissionModes[i]) != string(m) {
			t.Errorf("mode %d: %s vs %s", i, task.PermissionModes[i], m)
		}
	}
}

func TestSessionEnv(t *testing.T) {
	t.Parallel()
	from := []string{"PATH=/bin", "ANTHROPIC_API_KEY=a", "ANTHROPIC_AUTH_TOKEN=b", "ANTHROPIC_BASE_URL=https://evil", "CLAUDE_CODE_USE_BEDROCK=1",
		"OPENAI_API_KEY=c", "GOOGLE_APPLICATION_CREDENTIALS=/k.json", "HOME=/h", "STRIPE_API_KEY=keep"}
	sub := sessionEnv(from, config.Pool{Billing: "subscription"})
	if !slices.Equal(sub, []string{"PATH=/bin", "HOME=/h", "STRIPE_API_KEY=keep"}) {
		t.Errorf("subscription env = %v", sub)
	}
	api := sessionEnv(from, config.Pool{Billing: "api", APIKeyEnv: "OPENAI_API_KEY"})
	if !slices.Contains(api, "OPENAI_API_KEY=c") || slices.Contains(api, "ANTHROPIC_API_KEY=a") {
		t.Errorf("API pool env = %v", api)
	}
}

func TestAgentsOrder(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		Agents: map[string]config.Agent{
			"b": {Runtime: "codex", Pool: "p2"}, "a": {Runtime: "claude", Pool: "p1", Effort: "high"}, "c": {Runtime: "agy", Pool: "p3"},
		},
		Routing: map[string]config.Phase{"implement": {Agents: []string{"c", "ghost"}}},
	}
	got := Agents(cfg)
	names := make([]string, 0, len(got))
	for _, a := range got {
		names = append(names, a.Name)
	}
	if !slices.Equal(names, []string{"c", "a", "b"}) || got[1].Effort != runtime.EffortHigh {
		t.Fatalf("agents %+v", got)
	}
}
