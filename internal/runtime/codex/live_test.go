package codex

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/runtime"
)

// TestLive runs one turn against the real Codex CLI and resumes the thread in a new
// app-server process. It needs a logged-in `codex` and costs a few cheap turns, so it
// only runs with OPENCHEF_LIVE=1. OPENCHEF_LIVE_CODEX_MODEL overrides the model.
func TestLive(t *testing.T) {
	if os.Getenv("OPENCHEF_LIVE") != "1" {
		t.Skip("set OPENCHEF_LIVE=1 to run against the real codex CLI")
	}
	drainTimeout = 4 * time.Minute // real turns can be slow; TestLive is never parallel
	model := os.Getenv("OPENCHEF_LIVE_CODEX_MODEL")
	if model == "" {
		model = "gpt-6-luna"
	}
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.CommandContext(context.Background(), "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "OPENAI_API_KEY=") { // subscription only (§13)
			env = append(env, kv)
		}
	}
	appr := &recordingApprover{decision: runtime.Allow(), answer: "teal"}
	spec := runtime.SessionSpec{
		Dir: dir, Model: model, Effort: runtime.EffortLow, Mode: runtime.ModeManual,
		Env: env, Approver: appr, LogPath: filepath.Join(dir, "codex-live.jsonl"),
	}
	if p := os.Getenv("OPENCHEF_LIVE_LOG"); p != "" {
		spec.LogPath = p // keep the protocol log after the test for diagnosis or fixtures
	}
	rt := New(Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	s, err := rt.Open(ctx, spec)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ch, err := s.Turn(ctx, runtime.TurnInput{Text: "Remember the code word KESTREL. Reply with the single word READY."})
	if err != nil {
		t.Fatal(err)
	}
	end := terminal(t, drain(t, ch))
	if end.Kind != runtime.EventTurnCompleted || !strings.Contains(strings.ToUpper(end.Result.FinalText), "READY") {
		t.Fatalf("first turn: %+v %+v", end.Result, end.Failure)
	}
	if end.Result.Usage.OutputTokens == 0 {
		t.Errorf("usage missing: %+v", end.Result.Usage)
	}
	ref := s.Ref()
	if err := s.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}

	s2, err := rt.Resume(ctx, ref, spec)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	defer func() { _ = s2.Close(ctx) }()
	ch, err = s2.Turn(ctx, runtime.TurnInput{Text: "What was the code word? Reply with just the word."})
	if err != nil {
		t.Fatal(err)
	}
	end = terminal(t, drain(t, ch))
	if end.Kind != runtime.EventTurnCompleted || !strings.Contains(strings.ToUpper(end.Result.FinalText), "KESTREL") {
		t.Fatalf("resumed turn: %+v %+v", end.Result, end.Failure)
	}
	t.Logf("live ok: thread %s, usage %+v", ref.ID, end.Result.Usage)
}
