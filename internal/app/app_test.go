package app

import (
	"context"
	"os"
	"path/filepath"
	goruntime "runtime"
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

func TestApproverFileChanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	ws := filepath.Join(dir, "ws")
	if err := os.MkdirAll(filepath.Join(ws, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(dir, filepath.Join(ws, "escape")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	q := &fakeQueue{status: task.ApprovalAllowed}
	a := approver{queue: q, task: "t", agent: "opus", dir: ws}
	edit := func(paths ...string) runtime.ApprovalRequest {
		return runtime.ApprovalRequest{Kind: runtime.ApproveFileChange, Tool: "Edit", Paths: paths}
	}
	allowed := []runtime.ApprovalRequest{edit(filepath.Join(ws, "main.go")), edit("rel/new.go")}
	denied := []runtime.ApprovalRequest{
		edit(),
		edit(filepath.Join(dir, "outside.txt")),
		edit(filepath.Join(ws, "escape", "outside.txt")),
		edit(filepath.Join(ws, ".git", "hooks", "pre-commit")),
		edit(filepath.Join(ws, "main.go"), filepath.Join(ws, ".claude", "settings.json")),
	}
	if goruntime.GOOS == "darwin" || goruntime.GOOS == "windows" {
		denied = append(denied, edit(filepath.Join(ws, ".GIT", "hooks", "post-commit")))
	}
	for _, r := range allowed {
		if d, err := a.Approve(ctx, r); err != nil || !d.Allow {
			t.Errorf("%+v denied: %+v %v", r, d, err)
		}
	}
	for _, r := range denied {
		if d, err := a.Approve(ctx, r); err != nil || d.Allow || d.Message == "" {
			t.Errorf("%+v allowed: %+v %v", r, d, err)
		}
	}
	if len(q.asked) != 0 {
		t.Errorf("file changes went to the user: %+v", q.asked)
	}
	if d, _ := (approver{queue: q}).Approve(ctx, edit("x.go")); d.Allow {
		t.Error("no workspace allowed a relative write")
	}
}

func TestApproverAsksTheUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
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
		d, err := approver{queue: q, task: "t", agent: "opus", dir: t.TempDir()}.Approve(ctx, cmd)
		if err != nil || d.Allow != tc.allow || !strings.Contains(d.Message, tc.msg) {
			t.Errorf("%s: %+v, %v", tc.status, d, err)
		}
		if len(q.asked) != 1 || q.asked[0].Summary != cmd.Command || q.asked[0].Kind != task.ApprovalCommand || q.asked[0].Agent != "opus" {
			t.Errorf("asked %+v", q.asked)
		}
	}
	q := &fakeQueue{status: task.ApprovalAnswered, answer: "8080"}
	got, err := approver{queue: q}.Answer(ctx, runtime.Question{Text: "Which port?", Options: []string{"8080"}})
	if err != nil || got != "8080" || q.asked[0].Kind != task.ApprovalQuestion {
		t.Fatalf("answer %q, %v, %+v", got, err, q.asked)
	}
	q = &fakeQueue{status: task.ApprovalExpired}
	if got, _ := (approver{queue: q}).Answer(ctx, runtime.Question{Text: "?"}); got != unansweredQuestion {
		t.Fatalf("expired question answered %q", got)
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
