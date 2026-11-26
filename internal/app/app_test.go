package app

import (
	"context"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"testing"

	"github.com/ugurboz/openchef/internal/config"
	"github.com/ugurboz/openchef/internal/runtime"
)

func TestWorkspaceApprover(t *testing.T) {
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
	a := workspaceApprover{dir: ws}
	edit := func(paths ...string) runtime.ApprovalRequest {
		return runtime.ApprovalRequest{Kind: runtime.ApproveFileChange, Tool: "Edit", Paths: paths}
	}
	allowed := []runtime.ApprovalRequest{edit(filepath.Join(ws, "main.go")), edit("rel/new.go")}
	denied := []runtime.ApprovalRequest{
		{Kind: runtime.ApproveCommand, Tool: "Bash", Command: "go test ./..."},
		{Kind: runtime.ApproveTool, Tool: "WebFetch"},
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
	if d, _ := (workspaceApprover{}).Approve(ctx, edit("x.go")); d.Allow {
		t.Error("no workspace allowed a relative write")
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
