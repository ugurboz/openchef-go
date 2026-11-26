package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ugurboz/openchef/internal/engine"
	"github.com/ugurboz/openchef/internal/store"
	"github.com/ugurboz/openchef/internal/task"
)

// ErrApprovalSettled means a request was already settled (by the user, a timeout or the
// daemon) when another decision arrived. Errors matching it are *SettledError.
var ErrApprovalSettled = errors.New("service: request already settled")

// SettledError says how a request was settled before a later decision arrived.
type SettledError struct {
	ID     int64
	Status task.ApprovalStatus
}

func (e *SettledError) Error() string {
	return fmt.Sprintf("request %d was already %s", e.ID, e.Status)
}

// Is reports whether target is ErrApprovalSettled.
func (e *SettledError) Is(target error) bool { return target == ErrApprovalSettled }

// restartReason settles requests left pending by a previous daemon (D-022).
const restartReason = "The daemon restarted while this request was waiting; the agent asks again if it still needs it."

// Request is one thing an agent wants from the user.
type Request struct {
	Task    task.ID
	Agent   string
	Kind    task.ApprovalKind
	Tool    string
	Summary string
	Options []string
	// Raw is the normalized runtime request, kept for audit.
	Raw json.RawMessage
}

// approvals is the queue's in-memory half: who is waiting for which request. The store
// holds the requests themselves, so they survive the daemon and `approvals` lists them.
type approvals struct {
	mu      sync.Mutex
	waiting map[int64]chan task.Approval // buffered 1: a decision never blocks
}

// Ask queues req and waits until the user settles it, it expires (§9.2: 15 minutes,
// then denied), ctx ends (the agent stopped waiting: the request is withdrawn) or the
// service stops (left pending; the next start expires it). It returns the settled
// request.
func (s *Service) Ask(ctx context.Context, req Request) (task.Approval, error) {
	a := task.Approval{TaskID: req.Task, Agent: req.Agent, Kind: req.Kind, Tool: req.Tool,
		Summary: req.Summary, Options: req.Options, // never shortened: people approve exactly what runs
		ExpiresAt: s.now().Add(s.approvalTimeout)}
	if err := s.db.CreateApproval(ctx, &a, req.Raw); err != nil {
		return task.Approval{}, fmt.Errorf("service: %w", err)
	}
	ch := make(chan task.Approval, 1)
	s.queue.mu.Lock()
	s.queue.waiting[a.ID] = ch
	s.queue.mu.Unlock()
	defer func() {
		s.queue.mu.Lock()
		delete(s.queue.waiting, a.ID)
		s.queue.mu.Unlock()
	}()

	s.events.Publish(engine.Event{TaskID: a.TaskID, At: s.now(), Kind: engine.EventApprovalRequested, Agent: a.Agent, Approval: &a})
	if !s.events.watched(a.TaskID) && s.notify != nil {
		s.notify("OpenChef needs you", notification(&a))
	}

	timer := time.NewTimer(max(a.ExpiresAt.Sub(s.now()), 0))
	defer timer.Stop()
	var settle task.Decision
	select {
	case got := <-ch:
		return got, nil
	case <-timer.C:
		settle = task.Decision{Status: task.ApprovalExpired, DecidedBy: task.DecidedByTimeout,
			Answer: fmt.Sprintf("No one answered within %s.", s.approvalTimeout)}
	case <-ctx.Done():
		settle = task.Decision{Status: task.ApprovalWithdrawn, DecidedBy: task.DecidedByDaemon, Answer: "The agent stopped waiting."}
	case <-s.life.Done():
		return task.Approval{}, fmt.Errorf("service: stopping: %w", context.Canceled)
	}
	got, err := s.decide(context.WithoutCancel(ctx), a.ID, settle)
	if errors.Is(err, ErrApprovalSettled) {
		// The user answered at the same moment; their answer stands.
		if got, err = s.db.Approval(context.WithoutCancel(ctx), a.ID); err != nil {
			return task.Approval{}, fmt.Errorf("service: %w", err)
		}
		return got, nil
	}
	if err != nil {
		return task.Approval{}, err
	}
	if settle.Status == task.ApprovalWithdrawn {
		return got, fmt.Errorf("service: request %d withdrawn: %w", got.ID, ctx.Err())
	}
	return got, nil
}

// Decide settles a pending request for the user.
func (s *Service) Decide(ctx context.Context, id int64, d task.Decision) (task.Approval, error) {
	d.DecidedBy = task.DecidedByUser
	return s.decide(ctx, id, d)
}

// decide writes the decision, wakes the waiter and tells the task's viewers.
func (s *Service) decide(ctx context.Context, id int64, d task.Decision) (task.Approval, error) {
	got, err := s.db.DecideApproval(ctx, id, d)
	var ce *store.ConflictError
	switch {
	case errors.As(err, &ce):
		return task.Approval{}, &SettledError{ID: id, Status: task.ApprovalStatus(ce.Got)}
	case errors.Is(err, task.ErrInvalid):
		return task.Approval{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	case err != nil:
		return task.Approval{}, fmt.Errorf("service: %w", err)
	}
	s.queue.mu.Lock()
	if ch := s.queue.waiting[id]; ch != nil {
		ch <- got // buffered and written once: the store accepted exactly one decision
	}
	s.queue.mu.Unlock()
	s.events.Publish(engine.Event{TaskID: got.TaskID, At: s.now(), Kind: engine.EventApprovalDecided, Agent: got.Agent, Approval: &got})
	return got, nil
}

// Approvals lists requests, oldest first.
func (s *Service) Approvals(ctx context.Context, f store.ApprovalFilter) ([]task.Approval, error) {
	out, err := s.db.Approvals(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("service: %w", err)
	}
	return out, nil
}

// expireStale settles requests a previous daemon left pending: nobody waits for them.
func (s *Service) expireStale(ctx context.Context) error {
	expired, err := s.db.ExpirePending(ctx, restartReason)
	if err != nil {
		return fmt.Errorf("service: %w", err)
	}
	for i := range expired {
		a := &expired[i]
		s.events.Publish(engine.Event{TaskID: a.TaskID, At: s.now(), Kind: engine.EventApprovalDecided, Agent: a.Agent, Approval: a})
	}
	if len(expired) > 0 {
		s.log.Info("expired requests left by a previous daemon", "count", len(expired))
	}
	return nil
}

func notification(a *task.Approval) string {
	if a.Kind == task.ApprovalQuestion {
		return fmt.Sprintf("%s asks: %s (openchef answer %d \"…\")", a.Agent, a.Summary, a.ID)
	}
	return fmt.Sprintf("%s wants %s: %s (openchef approve %d)", a.Agent, a.Tool, a.Summary, a.ID)
}
