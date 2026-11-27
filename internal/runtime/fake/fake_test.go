package fake

import (
	"context"
	"testing"

	"github.com/ugurboz/openchef/internal/runtime"
)

type allow struct{}

func (allow) Approve(context.Context, runtime.ApprovalRequest) (runtime.Decision, error) {
	return runtime.Allow(), nil
}
func (allow) Answer(context.Context, runtime.Question) (string, error) { return "", nil }

func last(t *testing.T, ch <-chan runtime.Event) runtime.Event {
	t.Helper()
	var end runtime.Event
	for ev := range ch {
		end = ev
	}
	return end
}

func TestScriptAndClose(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rt := New("claude", Completed("hi"), Turn{Hang: true})
	s, err := rt.Open(ctx, runtime.SessionSpec{Approver: allow{}, Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := s.Turn(ctx, runtime.TurnInput{Text: "one"})
	if err != nil {
		t.Fatal(err)
	}
	if end := last(t, ch); end.Kind != runtime.EventTurnCompleted || end.Result.FinalText != "hi" {
		t.Fatalf("first turn %+v", end)
	}
	ch, err = s.Turn(ctx, runtime.TurnInput{Text: "two"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Turn(ctx, runtime.TurnInput{Text: "three"}); err == nil {
		t.Error("concurrent turn accepted")
	}
	if err := s.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if end := last(t, ch); end.Failure == nil || end.Failure.Kind != runtime.FailInterrupted {
		t.Fatalf("hanging turn after Close %+v", end)
	}
	if rt.Live() != 0 || rt.Remaining() != 0 {
		t.Fatalf("live %d remaining %d", rt.Live(), rt.Remaining())
	}
}

func TestScriptExhausted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rt := New("codex")
	s, err := rt.Open(ctx, runtime.SessionSpec{Approver: allow{}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(ctx) }()
	ch, err := s.Turn(ctx, runtime.TurnInput{Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if end := last(t, ch); end.Failure == nil || end.Failure.Kind != runtime.FailProtocol {
		t.Fatalf("exhausted script %+v", end)
	}
}
