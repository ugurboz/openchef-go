package turnq

import (
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/runtime"
)

func TestOrderTerminalAndClose(t *testing.T) {
	t.Parallel()
	q := New()
	for i := range 1000 {
		q.Emit(runtime.Event{Kind: runtime.EventText, Text: string(rune('a' + i%26))})
	}
	if !q.Finish(runtime.Event{Kind: runtime.EventTurnCompleted}) {
		t.Fatal("first Finish must end the turn")
	}
	if q.Finish(runtime.Event{Kind: runtime.EventTurnFailed}) {
		t.Fatal("second Finish must be ignored")
	}
	q.Emit(runtime.Event{Kind: runtime.EventText, Text: "late"})
	n := 0
	var last runtime.Event
	for ev := range q.C() {
		if ev.At.IsZero() {
			t.Fatal("At not set")
		}
		if ev.Kind == runtime.EventText && ev.Text != string(rune('a'+n%26)) {
			t.Fatalf("out of order at %d", n)
		}
		last = ev
		n++
	}
	if n != 1001 || last.Kind != runtime.EventTurnCompleted {
		t.Fatalf("got %d events, last %s", n, last.Kind)
	}
	if !q.Done() {
		t.Fatal("Done")
	}
}

func TestProducerNeverBlocks(t *testing.T) {
	t.Parallel()
	q := New()
	done := make(chan struct{})
	go func() {
		for range 10 * Buffer {
			q.Emit(runtime.Event{Kind: runtime.EventText})
		}
		q.Finish(runtime.Event{Kind: runtime.EventTurnCompleted})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("producer blocked on an unread queue")
	}
	q.Discard()
}
