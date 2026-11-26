package service

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/engine"
	"github.com/ugurboz/openchef/internal/task"
)

func newTestEvents(t *testing.T) *events {
	t.Helper()
	e := newEvents(t.TempDir(), func(err error) { t.Errorf("event log: %v", err) })
	t.Cleanup(func() {
		if err := e.Close(); err != nil {
			t.Error(err)
		}
	})
	return e
}

// A crash can leave the last line without its newline; the next daemon's first event
// must start its own line and still be replayed.
func TestEventLogAfterCutShortLine(t *testing.T) {
	t.Parallel()
	e := newTestEvents(t)
	id := task.ID("t_cut")
	p := e.path(id)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(`{"task":"t_cut","kind":"notice","message":"one"}`+"\n"+`{"task":"t_cu`), 0o600); err != nil {
		t.Fatal(err)
	}
	e.Publish(engine.Event{TaskID: id, At: time.Now(), Kind: engine.EventNotice, Message: "two"})
	past, sub, err := e.Subscribe(id, true)
	if err != nil {
		t.Fatal(err)
	}
	e.Unsubscribe(id, sub)
	if len(past) != 2 || past[0].Message != "one" || past[1].Message != "two" {
		t.Fatalf("replay = %+v", past)
	}
}

// Every event reaches a viewer exactly once, whether by replay or live, while the
// engine keeps publishing during the subscribe.
func TestSubscribeReplayAndLiveExactlyOnce(t *testing.T) {
	t.Parallel()
	e := newTestEvents(t)
	id := task.ID("t_once")
	const total = subscriberBuffer - 1 // a viewer that reads late must not lag
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range total {
			e.Publish(engine.Event{TaskID: id, At: time.Now(), Kind: engine.EventNotice, Step: i})
		}
	})
	time.Sleep(time.Millisecond) // let some events land in the log first
	past, sub, err := e.Subscribe(id, true)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Unsubscribe(id, sub)
	wg.Wait()
	seen := make([]int, 0, total)
	for _, ev := range past {
		seen = append(seen, ev.Step)
	}
	for len(seen) < total {
		select {
		case ev := <-sub.C:
			seen = append(seen, ev.Step)
		case <-time.After(5 * time.Second):
			t.Fatalf("got %d of %d events", len(seen), total)
		}
	}
	for i, step := range seen {
		if step != i {
			t.Fatalf("event %d has step %d: missed or duplicated", i, step)
		}
	}
}
