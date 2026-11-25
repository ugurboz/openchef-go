package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/runtime"
)

// TestLive drives the real Claude Code CLI through the paths the fake cannot prove:
// a first turn with an output schema (the process is replaced before any transcript
// exists, so --session-id must be reused), a turn without the schema (--resume), and
// Resume in a new session. It needs a logged-in `claude` and costs three cheap turns,
// so it only runs with OPENCHEF_LIVE=1. OPENCHEF_LIVE_CLAUDE_MODEL overrides the model.
func TestLive(t *testing.T) {
	if os.Getenv("OPENCHEF_LIVE") != "1" {
		t.Skip("set OPENCHEF_LIVE=1 to run against the real claude CLI")
	}
	drainTimeout = 4 * time.Minute // real turns can be slow; TestLive is never parallel
	interruptWait, closeWait = 10*time.Second, 3*time.Second
	model := os.Getenv("OPENCHEF_LIVE_CLAUDE_MODEL")
	if model == "" {
		model = "haiku"
	}
	dir := t.TempDir()
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "ANTHROPIC_API_KEY=") { // subscription only (§13)
			env = append(env, kv)
		}
	}
	appr := &recordingApprover{decision: runtime.Deny("live test denies everything"), cancelled: make(chan struct{})}
	spec := runtime.SessionSpec{
		Dir: dir, Model: model, Effort: runtime.EffortLow, Mode: runtime.ModeManual,
		Env: env, Approver: appr, LogPath: filepath.Join(dir, "claude-live.jsonl"),
	}
	if p := os.Getenv("OPENCHEF_LIVE_LOG"); p != "" {
		spec.LogPath = p // keep the protocol log after the test for diagnosis or fixtures
	}
	rt := New(Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	s, err := rt.Open(ctx, spec)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	schema := json.RawMessage(`{"type":"object","properties":{"word":{"type":"string"}},"required":["word"],"additionalProperties":false}`)
	end := liveTurn(ctx, t, s, runtime.TurnInput{
		Text:         "Remember the code word KESTREL. Answer with the word READY in the field 'word'.",
		OutputSchema: schema,
	})
	var out struct {
		Word string `json:"word"`
	}
	if err := json.Unmarshal(end.Result.Structured, &out); err != nil || !strings.Contains(strings.ToUpper(out.Word), "READY") {
		t.Fatalf("structured turn: %s (%v)", end.Result.Structured, err)
	}
	if end.Result.Usage.OutputTokens == 0 || len(end.Result.Models) == 0 {
		t.Errorf("usage/models missing: %+v", end.Result)
	}
	end = liveTurn(ctx, t, s, runtime.TurnInput{Text: "What was the code word? Reply with just the word."})
	if !strings.Contains(strings.ToUpper(end.Result.FinalText), "KESTREL") {
		t.Fatalf("turn after dropping the schema lost context: %q", end.Result.FinalText)
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
	end = liveTurn(ctx, t, s2, runtime.TurnInput{Text: "Repeat the code word once more. Reply with just the word."})
	if !strings.Contains(strings.ToUpper(end.Result.FinalText), "KESTREL") {
		t.Fatalf("resumed turn: %q", end.Result.FinalText)
	}
	t.Logf("live ok: session %s, models %v, usage %+v", ref.ID, end.Result.Models, end.Result.Usage)
}

func liveTurn(ctx context.Context, t *testing.T, s runtime.Session, in runtime.TurnInput) runtime.Event {
	t.Helper()
	ch, err := s.Turn(ctx, in)
	if err != nil {
		t.Fatalf("Turn: %v", err)
	}
	end := terminal(t, drain(t, ch))
	if end.Kind != runtime.EventTurnCompleted {
		t.Fatalf("turn failed: %+v", end.Failure)
	}
	return end
}
