package agy

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

// TestLive drives the real agy CLI: a plain turn, a schema turn (prompt, validation and
// possibly one repair), a command that must leave the sandbox (D-015: it has to reach
// the approver), and Resume in a new process with the replayed history dropped. It
// needs a logged-in `agy`, costs four cheap turns, and only runs with OPENCHEF_LIVE=1.
// OPENCHEF_LIVE_AGY_MODEL overrides the model.
func TestLive(t *testing.T) {
	if os.Getenv("OPENCHEF_LIVE") != "1" {
		t.Skip("set OPENCHEF_LIVE=1 to run against the real agy CLI")
	}
	drainTimeout = 4 * time.Minute // TestLive is never parallel
	interruptWait, closeWait = 10*time.Second, 3*time.Second
	model := os.Getenv("OPENCHEF_LIVE_AGY_MODEL")
	if model == "" {
		model = "gemini-3.8-flash"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	// Outside the temp tree on purpose: the adapter refuses it (D-015).
	dir, err := os.MkdirTemp(home, "openchef-agy-live-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	appr := &recordingApprover{decision: runtime.Allow(), cancelled: make(chan struct{})}
	spec := runtime.SessionSpec{
		Dir: dir, Model: model, Effort: runtime.EffortLow, Mode: runtime.ModeManual,
		Env: os.Environ(), Approver: appr, LogPath: filepath.Join(t.TempDir(), "agy-live.jsonl"),
	}
	if p := os.Getenv("OPENCHEF_LIVE_LOG"); p != "" {
		spec.LogPath = p
	}
	rt := New(Options{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	s, err := rt.Open(ctx, spec)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	end := liveTurn(ctx, t, s, runtime.TurnInput{Text: "Remember the code word KESTREL. Reply with the single word READY. Do not use tools."})
	if !strings.Contains(strings.ToUpper(end.Result.FinalText), "READY") || end.Result.Usage.InputTokens == 0 {
		t.Fatalf("first turn: %+v", end.Result)
	}
	sch := json.RawMessage(`{"type":"object","properties":{"word":{"type":"string"}},"required":["word"],"additionalProperties":false}`)
	end = liveTurn(ctx, t, s, runtime.TurnInput{Text: "What was the code word? Do not use tools.", OutputSchema: sch})
	var out struct {
		Word string `json:"word"`
	}
	if err := json.Unmarshal(end.Result.Structured, &out); err != nil || !strings.Contains(strings.ToUpper(out.Word), "KESTREL") {
		t.Fatalf("schema turn: %s (%v)", end.Result.Structured, err)
	}
	end = liveTurn(ctx, t, s, runtime.TurnInput{Text: "Run exactly this shell command: `echo hi > out.txt`. If the sandbox blocks it, run it again with BypassSandbox. Then reply DONE."})
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); err != nil {
		t.Fatalf("command did not write: %v (final %q)", err, end.Result.FinalText)
	}
	if len(appr.list()) == 0 {
		t.Fatal("a write left the sandbox without reaching the approver")
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
	ch, err := s2.Turn(ctx, runtime.TurnInput{Text: "Repeat the code word once more. Reply with just the word. Do not use tools."})
	if err != nil {
		t.Fatal(err)
	}
	evs := drain(t, ch)
	end = terminal(t, evs)
	if end.Kind != runtime.EventTurnCompleted || !strings.Contains(strings.ToUpper(end.Result.FinalText), "KESTREL") {
		t.Fatalf("resumed turn: %+v %+v", end.Result, end.Failure)
	}
	if strings.Contains(streamed(evs), "READY") {
		t.Fatal("replayed history leaked into the resumed turn")
	}
	t.Logf("live ok: session %s, approvals %d, models %v, usage %+v", ref.ID, len(appr.list()), end.Result.Models, end.Result.Usage)
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
