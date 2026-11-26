package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/engine"
	"github.com/ugurboz/openchef/internal/runtime/fake"
	"github.com/ugurboz/openchef/internal/store"
	"github.com/ugurboz/openchef/internal/task"
)

// runningTask starts a task whose agent hangs, so requests can be queued for it.
func runningTask(t *testing.T, e *env, s *Service) task.ID {
	t.Helper()
	e.claude.Push(fake.Turn{Hang: true})
	tk, err := s.Run(context.Background(), RunRequest{Prompt: "p", Dir: e.repo})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(e.claude.Turns()) == 1 })
	return tk.ID
}

type asked struct {
	a   task.Approval
	err error
}

func ask(ctx context.Context, s *Service, req Request) <-chan asked {
	out := make(chan asked, 1)
	go func() {
		a, err := s.Ask(ctx, req)
		out <- asked{a, err}
	}()
	return out
}

func pendingOne(t *testing.T, s *Service, id task.ID) task.Approval {
	t.Helper()
	var got []task.Approval
	waitFor(t, func() bool {
		var err error
		got, err = s.Approvals(context.Background(), store.ApprovalFilter{Task: id, Pending: true})
		return err == nil && len(got) == 1
	})
	return got[0]
}

func TestAskAndDecide(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	var mu sync.Mutex
	var notes []string
	s := e.serviceWith(func(d *Deps) {
		d.Notify = func(_, body string) { mu.Lock(); notes = append(notes, body); mu.Unlock() }
	})
	defer stop(t, s)
	id := runningTask(t, e, s)
	_, sub, err := s.Watch(ctx, id, false)
	if err != nil {
		t.Fatal(err)
	}

	res := ask(ctx, s, Request{Task: id, Agent: "opus", Kind: task.ApprovalCommand, Tool: "Bash", Summary: "go test ./...\nrm -rf build"})
	p := pendingOne(t, s, id)
	if p.Summary != "go test ./...\nrm -rf build" {
		t.Fatalf("summary changed: %q", p.Summary) // people approve exactly what runs
	}
	if _, err := s.Decide(ctx, p.ID, task.Decision{Status: task.ApprovalAnswered, Answer: "x"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("answering a command: %v", err)
	}
	if _, err := s.Decide(ctx, p.ID, task.Decision{Status: task.ApprovalAllowed}); err != nil {
		t.Fatal(err)
	}
	r := <-res
	if r.err != nil || r.a.Status != task.ApprovalAllowed || r.a.DecidedBy != task.DecidedByUser {
		t.Fatalf("ask returned %+v, %v", r.a, r.err)
	}
	if _, err := s.Decide(ctx, p.ID, task.Decision{Status: task.ApprovalDenied}); !errors.Is(err, ErrApprovalSettled) {
		t.Fatalf("second decision: %v", err)
	}
	// The viewer saw the request and its decision; it was watched, so no notification.
	var kinds []engine.EventKind
	waitFor(t, func() bool {
		select {
		case ev := <-sub.C:
			if ev.Approval != nil {
				kinds = append(kinds, ev.Kind)
			}
		default:
		}
		return len(kinds) == 2
	})
	if kinds[0] != engine.EventApprovalRequested || kinds[1] != engine.EventApprovalDecided {
		t.Fatalf("events %v", kinds)
	}
	s.Unwatch(id, sub)

	// Unwatched: a question notifies the desktop.
	res = ask(ctx, s, Request{Task: id, Agent: "opus", Kind: task.ApprovalQuestion, Summary: "Which port?", Options: []string{"8080"}})
	q := pendingOne(t, s, id)
	if _, err := s.Decide(ctx, q.ID, task.Decision{Status: task.ApprovalAnswered, Answer: "9090"}); err != nil {
		t.Fatal(err)
	}
	if r := <-res; r.err != nil || r.a.Answer != "9090" {
		t.Fatalf("answer %+v, %v", r.a, r.err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(notes) != 1 {
		t.Fatalf("notifications %q", notes)
	}
}

func TestAskTimesOut(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	s := e.serviceWith(func(d *Deps) { d.ApprovalTimeout = 50 * time.Millisecond })
	defer stop(t, s)
	id := runningTask(t, e, s)
	a, err := s.Ask(context.Background(), Request{Task: id, Agent: "opus", Kind: task.ApprovalCommand, Tool: "Bash", Summary: "make"})
	if err != nil || a.Status != task.ApprovalExpired || a.DecidedBy != task.DecidedByTimeout {
		t.Fatalf("%+v, %v", a, err)
	}
}

func TestAskWithdrawnWhenAgentStops(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	s := e.service()
	defer stop(t, s)
	id := runningTask(t, e, s)
	ctx, cancel := context.WithCancel(context.Background())
	res := ask(ctx, s, Request{Task: id, Agent: "opus", Kind: task.ApprovalTool, Tool: "WebFetch", Summary: "https://example.com"})
	p := pendingOne(t, s, id)
	cancel()
	r := <-res
	if !errors.Is(r.err, context.Canceled) || r.a.Status != task.ApprovalWithdrawn {
		t.Fatalf("%+v, %v", r.a, r.err)
	}
	if got, _ := e.db.Approval(context.Background(), p.ID); got.Status != task.ApprovalWithdrawn {
		t.Fatalf("stored %+v", got)
	}
}

// A request still pending when the daemon dies stays listed, settled as expired by the
// next daemon, which tells the task's viewers.
func TestRestartExpiresPendingRequests(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	e := newEnv(t)
	first := e.service()
	id := runningTask(t, e, first)
	res := ask(ctx, first, Request{Task: id, Agent: "opus", Kind: task.ApprovalCommand, Tool: "Bash", Summary: "make"})
	p := pendingOne(t, first, id)
	stop(t, first)
	if r := <-res; !errors.Is(r.err, context.Canceled) {
		t.Fatalf("ask across shutdown: %+v, %v", r.a, r.err)
	}
	if got, _ := e.db.Approval(ctx, p.ID); got.Status != task.ApprovalPending {
		t.Fatalf("shutdown settled the request: %+v", got)
	}

	e.claude.Push(fake.Turn{Edit: writeFile})
	second := e.service()
	defer stop(t, second)
	if err := second.Start(ctx); err != nil {
		t.Fatal(err)
	}
	got, err := second.Approvals(ctx, store.ApprovalFilter{Task: id})
	if err != nil || len(got) != 1 || got[0].Status != task.ApprovalExpired || got[0].DecidedBy != task.DecidedByDaemon {
		t.Fatalf("after restart %+v, %v", got, err)
	}
	past, sub, err := second.Watch(ctx, id, true)
	if err != nil {
		t.Fatal(err)
	}
	second.Unwatch(id, sub)
	found := false
	for _, ev := range past {
		found = found || (ev.Kind == engine.EventApprovalDecided && ev.Approval.ID == p.ID)
	}
	if !found {
		t.Error("the expiry was not logged")
	}
}
