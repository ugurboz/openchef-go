// Package service is OpenChef's application layer: it turns user requests (run, cancel,
// retry, watch) into store transitions and engine runs, resumes work after a daemon
// restart, and wakes tasks parked in waiting_quota when their quota resets. The daemon's
// API handlers and, later, the TUI call it; it knows nothing about sockets or terminals.
package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ugurboz/openchef/internal/engine"
	"github.com/ugurboz/openchef/internal/gitws"
	"github.com/ugurboz/openchef/internal/store"
	"github.com/ugurboz/openchef/internal/task"
)

// ErrInvalidRequest means a request cannot be carried out as given.
var ErrInvalidRequest = errors.New("service: invalid request")

// ErrStopTimeout means Stop returned while engine runs were still stopping; their
// store is still in use.
var ErrStopTimeout = errors.New("service: engine runs did not stop in time")

// Deps are the service's collaborators.
type Deps struct {
	Store *store.DB
	// NewEngine builds the engine with the service as its event sink.
	NewEngine func(sink engine.Sink) (*engine.Engine, error)
	// EventDir is ~/.openchef/sessions; each task's events.jsonl lives below it.
	EventDir string
	Log      *slog.Logger
	Now      func() time.Time
	// Notify shows a desktop notification (best effort) for a request nobody is
	// watching; nil disables notifications.
	Notify func(title, body string)
	// ApprovalTimeout overrides task.ApprovalTimeout (tests).
	ApprovalTimeout time.Duration
}

// Service runs tasks. Methods are safe for concurrent use.
type Service struct {
	db     *store.DB
	eng    *engine.Engine
	events *events
	log    *slog.Logger
	now    func() time.Time
	notify func(title, body string)
	queue  approvals
	// approvalTimeout is how long a request waits for the user.
	approvalTimeout time.Duration

	life   context.Context //nolint:containedctx // the service's lifetime: Stop cancels every task
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu      sync.Mutex
	running map[task.ID]context.CancelFunc
	timers  map[task.ID]*time.Timer
	stopped bool
}

// New builds a service. Call Start to resume earlier work.
func New(d Deps) (*Service, error) {
	if d.Store == nil || d.NewEngine == nil || d.EventDir == "" {
		return nil, errors.New("service: Store, NewEngine and EventDir are required")
	}
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	s := &Service{db: d.Store, log: log, now: now, notify: d.Notify, queue: approvals{waiting: map[int64]chan task.Approval{}},
		running: map[task.ID]context.CancelFunc{}, timers: map[task.ID]*time.Timer{}}
	s.approvalTimeout = d.ApprovalTimeout
	if s.approvalTimeout <= 0 {
		s.approvalTimeout = task.ApprovalTimeout
	}
	s.events = newEvents(d.EventDir, func(err error) { log.Error("event log", "err", err) })
	eng, err := d.NewEngine(s.events)
	if err != nil {
		return nil, fmt.Errorf("service: %w", err)
	}
	s.eng = eng
	s.life, s.cancel = context.WithCancel(context.Background())
	return s, nil
}

// Start recovers after a daemon restart (§5.3): it kills processes left by the previous
// daemon, expires the requests they were waiting on, resumes running tasks and
// schedules waiting_quota ones.
func (s *Service) Start(ctx context.Context) error {
	killed, err := s.eng.KillLeftovers(ctx)
	if err != nil {
		s.log.Error("kill leftover processes", "err", err)
	}
	if killed > 0 {
		s.log.Warn("killed processes left by a previous daemon", "count", killed)
	}
	if err := s.expireStale(ctx); err != nil {
		return err
	}
	active, err := s.db.Tasks(ctx, store.TaskFilter{Active: true})
	if err != nil {
		return fmt.Errorf("service: %w", err)
	}
	for i := range active {
		t := &active[i]
		switch t.Status {
		case task.StatusRunning:
			s.log.Info("resuming task", "task", t.ID)
			s.drive(t.ID) //nolint:contextcheck // a run lives for the service, not for the request that started it
		case task.StatusWaitingQuota:
			s.schedule(t.ID, t.ResumeAt) //nolint:contextcheck // the wake-up runs on the service lifetime
		default:
			// planning, awaiting_approval, reviewing (M5) and needs_attention wait for
			// the user or a later milestone.
		}
	}
	return nil
}

// Stop ends every engine run, leaving tasks for recovery on the next start (§5.4), and
// waits for them until ctx ends.
func (s *Service) Stop(ctx context.Context) error {
	s.mu.Lock()
	s.stopped = true
	for id, t := range s.timers {
		t.Stop()
		delete(s.timers, id)
	}
	s.mu.Unlock()
	s.cancel()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	var err error
	select {
	case <-done:
	case <-ctx.Done():
		err = fmt.Errorf("service: tasks still stopping: %w", ErrStopTimeout)
	}
	return errors.Join(err, s.events.Close())
}

// RunRequest starts a single-step task (`openchef run`).
type RunRequest struct {
	Prompt string
	// Dir is any directory inside the user's repository.
	Dir string
	// Verify is the verification command; empty accepts the step on completion.
	Verify string
	// Agent pins an agent; empty lets the router choose.
	Agent      string
	Importance task.Importance
}

// Run creates a run task and starts it.
func (s *Service) Run(ctx context.Context, req RunRequest) (task.Task, error) {
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return task.Task{}, fmt.Errorf("%w: empty prompt", ErrInvalidRequest)
	}
	repo, err := gitws.Open(ctx, req.Dir)
	if err != nil {
		return task.Task{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
	}
	importance := req.Importance
	if importance == "" {
		importance = task.ImportanceNormal
	}
	id, err := task.NewID()
	if err != nil {
		return task.Task{}, fmt.Errorf("service: %w", err)
	}
	t := task.Task{
		ID: id, Kind: task.KindRun, Status: task.StatusRunning, Prompt: prompt, RepoRoot: repo.Root,
		Mode: task.ModeIsolated, Backend: task.BackendLocal, Importance: importance, AgentPin: req.Agent,
	}
	steps := []task.Step{{
		Index: 1, Title: title(prompt), Prompt: prompt, Verify: strings.TrimSpace(req.Verify),
		Importance: importance, Status: task.StepPending,
	}}
	if err := s.db.CreateTask(ctx, &t, steps); err != nil {
		if errors.Is(err, task.ErrInvalid) {
			return task.Task{}, fmt.Errorf("%w: %w", ErrInvalidRequest, err)
		}
		return task.Task{}, fmt.Errorf("service: %w", err)
	}
	s.events.Publish(engine.Event{TaskID: id, At: s.now(), Kind: engine.EventTaskStatus, Status: string(t.Status), Message: "created"})
	s.drive(id) //nolint:contextcheck // a run lives for the service, not for the request that started it
	return t, nil
}

// title is the first line of prompt, cut to 72 characters (a commit subject).
func title(prompt string) string {
	line, _, _ := strings.Cut(prompt, "\n")
	line = strings.TrimSpace(line)
	if utf8.RuneCountInString(line) <= 72 {
		return line
	}
	r := []rune(line)
	return strings.TrimSpace(string(r[:71])) + "…"
}

// Detail is everything known about one task.
type Detail struct {
	Task     task.Task
	Steps    []task.Step
	Attempts []task.Attempt
	// Driving is true while an engine run is in progress in this daemon.
	Driving bool
}

// Get returns a task's detail.
func (s *Service) Get(ctx context.Context, id task.ID) (Detail, error) {
	t, err := s.db.Task(ctx, id)
	if err != nil {
		return Detail{}, fmt.Errorf("service: %w", err)
	}
	steps, err := s.db.Steps(ctx, id)
	if err != nil {
		return Detail{}, fmt.Errorf("service: %w", err)
	}
	attempts, err := s.db.Attempts(ctx, id)
	if err != nil {
		return Detail{}, fmt.Errorf("service: %w", err)
	}
	s.mu.Lock()
	_, driving := s.running[id]
	s.mu.Unlock()
	return Detail{Task: t, Steps: steps, Attempts: attempts, Driving: driving}, nil
}

// List returns tasks, newest first.
func (s *Service) List(ctx context.Context, f store.TaskFilter) ([]task.Task, error) {
	ts, err := s.db.Tasks(ctx, f)
	if err != nil {
		return nil, fmt.Errorf("service: %w", err)
	}
	return ts, nil
}

// Cancel moves a task to cancelled and stops its engine run. The store transition comes
// first, so nothing the engine does afterwards is authorized (D-018).
func (s *Service) Cancel(ctx context.Context, id task.ID) (task.Task, error) {
	for {
		t, err := s.db.Task(ctx, id)
		if err != nil {
			return task.Task{}, fmt.Errorf("service: %w", err)
		}
		if t.Status.Terminal() {
			return task.Task{}, fmt.Errorf("%w: task %s is already %s", ErrInvalidRequest, id, t.Status)
		}
		got, err := s.db.TransitionTask(ctx, id, t.Status, task.StatusCancelled, store.TaskChange{Reason: "cancelled by the user"})
		if errors.Is(err, store.ErrConflict) {
			continue // the engine moved it meanwhile; cancel from the new state
		}
		if err != nil {
			return task.Task{}, fmt.Errorf("service: %w", err)
		}
		s.mu.Lock()
		c, driving := s.running[id]
		if driving {
			c()
		}
		if tm := s.timers[id]; tm != nil {
			tm.Stop()
			delete(s.timers, id)
		}
		s.mu.Unlock()
		s.events.Publish(engine.Event{TaskID: id, At: s.now(), Kind: engine.EventTaskStatus, Status: string(task.StatusCancelled), Message: got.Reason})
		if !driving {
			s.events.closeTask(id) // a driven task's log is closed when its run ends
		}
		return got, nil
	}
}

// Retry restarts a task that needs attention (`openchef retry`).
func (s *Service) Retry(ctx context.Context, id task.ID) (task.Task, error) {
	t, err := s.db.Task(ctx, id)
	if err != nil {
		return task.Task{}, fmt.Errorf("service: %w", err)
	}
	if t.Status != task.StatusNeedsAttention && t.Status != task.StatusWaitingQuota {
		return task.Task{}, fmt.Errorf("%w: task %s is %s; only tasks that need attention or wait for quota can be retried",
			ErrInvalidRequest, id, t.Status)
	}
	got, err := s.db.TransitionTask(ctx, id, t.Status, task.StatusRunning, store.TaskChange{})
	if err != nil {
		return task.Task{}, fmt.Errorf("service: %w", err)
	}
	s.mu.Lock()
	if tm := s.timers[id]; tm != nil {
		tm.Stop()
		delete(s.timers, id)
	}
	s.mu.Unlock()
	s.events.Publish(engine.Event{TaskID: id, At: s.now(), Kind: engine.EventTaskStatus, Status: string(task.StatusRunning), Message: "retried by the user"})
	s.drive(id) //nolint:contextcheck // a run lives for the service, not for the request that started it
	return got, nil
}

// Watch returns a task's past events (when replay is set) and a subscription to its new
// ones. The caller must Unwatch.
func (s *Service) Watch(ctx context.Context, id task.ID, replay bool) ([]engine.Event, *Subscription, error) {
	if _, err := s.db.Task(ctx, id); err != nil {
		return nil, nil, fmt.Errorf("service: %w", err)
	}
	return s.events.Subscribe(id, replay)
}

// Unwatch ends a subscription from Watch.
func (s *Service) Unwatch(id task.ID, sub *Subscription) { s.events.Unsubscribe(id, sub) }

// drive starts an engine run for a task unless one is in progress.
func (s *Service) drive(id task.ID) {
	s.mu.Lock()
	if s.stopped || s.running[id] != nil {
		s.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(s.life)
	s.running[id] = cancel
	s.mu.Unlock()
	s.wg.Go(func() {
		defer cancel()
		runErr := s.eng.Run(ctx, id)
		s.mu.Lock()
		delete(s.running, id)
		s.mu.Unlock()
		switch {
		case runErr == nil:
		case errors.Is(runErr, context.Canceled) && s.life.Err() != nil:
			return // shutdown: recovery resumes the task on next start
		default:
			s.log.Error("task run failed", "task", id, "err", runErr)
			s.events.Publish(engine.Event{TaskID: id, At: s.now(), Kind: engine.EventNotice, Message: "run failed: " + runErr.Error()})
		}
		t, err := s.db.Task(context.WithoutCancel(ctx), id)
		if err != nil {
			s.log.Error("read task after run", "task", id, "err", err)
			return
		}
		switch {
		case t.Status == task.StatusWaitingQuota:
			s.schedule(id, t.ResumeAt) //nolint:contextcheck // the wake-up runs on the service lifetime
		case t.Status.Terminal():
			s.events.closeTask(id)
		case t.Status == task.StatusRunning && runErr == nil && s.life.Err() == nil:
			// A Retry landed while this run was still cleaning up: its drive call
			// was a no-op because the run was still registered, so run again. A run
			// that failed is not restarted here, or a persistent error would spin;
			// the next daemon start recovers it.
			s.drive(id) //nolint:contextcheck // a run lives for the service
		}
	})
}

// schedule wakes a waiting_quota task at its resume time.
func (s *Service) schedule(id task.ID, at time.Time) {
	d := max(at.Sub(s.now()), 0)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	if old := s.timers[id]; old != nil {
		old.Stop()
	}
	var tm *time.Timer
	tm = time.AfterFunc(d, func() {
		s.mu.Lock()
		if s.timers[id] != tm || s.stopped {
			s.mu.Unlock()
			return // replaced, cancelled or shutting down
		}
		delete(s.timers, id)
		s.wg.Add(1) // under s.mu, so Stop cannot be waiting yet
		s.mu.Unlock()
		defer s.wg.Done()
		if _, err := s.db.TransitionTask(s.life, id, task.StatusWaitingQuota, task.StatusRunning, store.TaskChange{}); err != nil {
			if !errors.Is(err, store.ErrConflict) && s.life.Err() == nil {
				s.log.Error("wake task", "task", id, "err", err)
			}
			return
		}
		s.events.Publish(engine.Event{TaskID: id, At: s.now(), Kind: engine.EventTaskStatus, Status: string(task.StatusRunning), Message: "quota reset; resuming"})
		s.drive(id)
	})
	s.timers[id] = tm
	s.log.Info("task waits for quota", "task", id, "resume_at", at)
}
