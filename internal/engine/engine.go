// Package engine drives tasks through their steps (docs/architecture.md §7): it opens
// agent sessions on routed agents, runs turns with timeouts and loop detection, verifies,
// commits, retries, fails over to another pool when quota runs out, and parks a task in
// waiting_quota or needs_attention when it cannot continue.
//
// Every state change goes through the store before the side effect it authorizes
// (write-ahead rule, §11). A store conflict on the task means someone else (a CLI
// cancel) moved it out of the active states; the engine then stops without touching it.
// A cancelled context (daemon shutdown) stops the engine without changing any state, so
// recovery can resume exactly where it stopped (§5.3).
package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/ugurboz/openchef/internal/gitws"
	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/store"
	"github.com/ugurboz/openchef/internal/task"
)

// Limits bound the work on one step (§7.2). Zero fields take the defaults.
type Limits struct {
	// MaxRetries is how many times a step's failure (verification, loop, stall) is fed
	// back to the agent before the task needs attention. Default 2.
	MaxRetries int
	// TurnTimeout bounds one turn. Default 30 min.
	TurnTimeout time.Duration
	// IdleTimeout bounds the time without any event in a turn. Default 10 min.
	IdleTimeout time.Duration
	// LoopRepeats identical consecutive tool calls count as a loop. Default 5.
	LoopRepeats int
	// RateLimitRetries bounds backoffs on rate_limited per attempt. Default 3.
	RateLimitRetries int
	// MaxBackoff caps one rate-limit wait. Default 5 min.
	MaxBackoff time.Duration
	// VerifyTimeout bounds one verification run. Default 10 min.
	VerifyTimeout time.Duration
	// StopWait bounds waiting for a turn to end after an interrupt. Default 30 s.
	StopWait time.Duration
}

func (l Limits) withDefaults() Limits {
	def := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	if l.MaxRetries <= 0 {
		l.MaxRetries = 2
	}
	if l.LoopRepeats <= 0 {
		l.LoopRepeats = 5
	}
	if l.RateLimitRetries <= 0 {
		l.RateLimitRetries = 3
	}
	def(&l.TurnTimeout, 30*time.Minute)
	def(&l.IdleTimeout, 10*time.Minute)
	def(&l.MaxBackoff, 5*time.Minute)
	def(&l.VerifyTimeout, 10*time.Minute)
	def(&l.StopWait, 30*time.Second)
	return l
}

// Config wires the engine to the rest of the daemon.
type Config struct {
	Store    *store.DB
	Runtimes map[string]runtime.Runtime
	Router   Router
	// SpecFor returns the session settings for an agent working on a task: permission
	// mode and rules, Env, Approver, AskUser. The engine sets Dir, Model, Effort,
	// LogPath and OnSpawn itself.
	SpecFor func(t *task.Task, a Agent) (runtime.SessionSpec, error)
	// VerifyEnv is the environment of verification commands (see verify.MinimalEnv).
	VerifyEnv []string
	// WorktreeRoot is ~/.openchef/worktrees; SessionRoot is ~/.openchef/sessions.
	WorktreeRoot string
	SessionRoot  string
	Limits       Limits
	// PoolExhausted, if set, learns that a pool ran out of quota until a time (zero:
	// unknown), so routing can cool it down (§10.3).
	PoolExhausted func(pool string, until time.Time)
	// Sink receives live events; nil discards them.
	Sink Sink
	// Sleep waits for d or until ctx ends; tests replace it. Default: a timer.
	Sleep func(ctx context.Context, d time.Duration) error
	// Now is the clock; default time.Now.
	Now func() time.Time
}

// Engine runs tasks. It holds no per-task state, so one Engine serves every task.
type Engine struct {
	cfg Config
}

// New checks cfg and returns an Engine.
func New(cfg Config) (*Engine, error) {
	switch {
	case cfg.Store == nil:
		return nil, errors.New("engine: Config.Store is required")
	case len(cfg.Runtimes) == 0:
		return nil, errors.New("engine: Config.Runtimes is empty")
	case cfg.Router == nil:
		return nil, errors.New("engine: Config.Router is required")
	case cfg.SpecFor == nil:
		return nil, errors.New("engine: Config.SpecFor is required")
	case !filepath.IsAbs(cfg.WorktreeRoot) || !filepath.IsAbs(cfg.SessionRoot):
		return nil, errors.New("engine: WorktreeRoot and SessionRoot must be absolute")
	}
	cfg.Limits = cfg.Limits.withDefaults()
	if cfg.Sink == nil {
		cfg.Sink = discard{}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleep
	}
	return &Engine{cfg: cfg}, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("engine: wait: %w", ctx.Err())
	}
}

// ErrNotActive means the task is not (or no longer) running or reviewing: Run was
// called on a parked or finished task, or a cancel moved it under the engine, which then
// unwinds without further writes.
var ErrNotActive = errors.New("engine: task is not active")

// stoppedBy reports whether err means the task is no longer the engine's to drive.
func stoppedBy(err error) bool {
	if errors.Is(err, ErrNotActive) {
		return true
	}
	ce, ok := errors.AsType[*store.ConflictError](err)
	return ok && ce.Entity == "task"
}

// Run drives a task until it leaves the active states. It returns nil when the task
// reached done, waiting_quota or needs_attention, or was moved out of the active states
// by someone else; ctx.Err() (wrapped) when ctx was cancelled, leaving the task for
// recovery; and other errors for failures it could not record on the task.
func (e *Engine) Run(ctx context.Context, id task.ID) error {
	r, err := e.newRun(ctx, id)
	if err != nil {
		return err
	}
	err = r.drive(ctx)
	switch {
	case err == nil, stoppedBy(err):
		return nil
	case ctx.Err() != nil:
		// A cancel (CLI) moves the task out of the active states and then cancels ctx;
		// a shutdown only cancels ctx and leaves the task for recovery.
		if t, terr := e.cfg.Store.Task(context.WithoutCancel(ctx), id); terr == nil && !t.Status.Active() {
			return nil
		}
		return fmt.Errorf("engine: task %s: %w", id, ctx.Err())
	}
	// Something unexpected (store, git, verification infrastructure): park the task so
	// a human sees it, and report the error.
	if perr := r.park(context.WithoutCancel(ctx), task.StatusNeedsAttention, err.Error(), time.Time{}); perr != nil && !stoppedBy(perr) {
		return errors.Join(err, perr)
	}
	return err
}

// run is the state of one Run call.
type run struct {
	e     *Engine
	t     task.Task
	steps []task.Step
	repo  *gitws.Repo
	wt    *gitws.Worktree
}

func (e *Engine) newRun(ctx context.Context, id task.ID) (*run, error) {
	t, err := e.cfg.Store.Task(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("engine: %w", err)
	}
	if !t.Status.Active() {
		return nil, fmt.Errorf("engine: task %s is %s: %w", id, t.Status, ErrNotActive)
	}
	steps, err := e.cfg.Store.Steps(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("engine: %w", err)
	}
	return &run{e: e, t: t, steps: steps}, nil
}

func (r *run) publish(ev Event) {
	ev.TaskID = r.t.ID
	if ev.At.IsZero() {
		ev.At = r.e.cfg.Now()
	}
	r.e.cfg.Sink.Publish(ev)
}

func (r *run) drive(ctx context.Context) error {
	if r.t.Status != task.StatusRunning {
		return fmt.Errorf("engine: task %s is %s; only running tasks are driven in M3", r.t.ID, r.t.Status)
	}
	if err := r.workspace(ctx); err != nil {
		return err
	}
	for i := range r.steps {
		if r.steps[i].Status == task.StepVerified {
			continue
		}
		done, err := r.runStep(ctx, i)
		if err != nil || !done {
			return err
		}
	}
	to := task.StatusDone
	if r.t.Kind == task.KindPipeline {
		to = task.StatusReviewing
	}
	return r.setStatus(ctx, to, "", time.Time{})
}

// workspace creates (or, after a crash, finds) the task's worktree. The paths are
// recorded before the worktree exists so a crash in between leaves something to clean.
func (r *run) workspace(ctx context.Context) error {
	repo, err := gitws.Open(ctx, r.t.RepoRoot)
	if err != nil {
		return fmt.Errorf("engine: task %s: %w", r.t.ID, err)
	}
	r.repo = repo
	if r.t.Worktree == "" {
		base, err := repo.Head(ctx)
		if err != nil {
			return fmt.Errorf("engine: task %s: %w", r.t.ID, err)
		}
		branch := "openchef/" + string(r.t.ID)
		path := filepath.Join(r.e.cfg.WorktreeRoot, repo.ID(), string(r.t.ID))
		if err := r.e.cfg.Store.SetWorkspace(ctx, r.t.ID, branch, path, base); err != nil {
			return fmt.Errorf("engine: %w", err)
		}
		r.t.Branch, r.t.Worktree, r.t.BaseCommit = branch, path, base
	}
	wt, err := repo.CreateWorktree(ctx, r.t.Worktree, r.t.Branch, r.t.BaseCommit)
	if err != nil {
		return fmt.Errorf("engine: task %s: %w", r.t.ID, err)
	}
	r.wt = wt
	return nil
}

// lastVerified is the commit the workspace returns to when work on step i is discarded.
func (r *run) lastVerified(i int) string {
	for j := i - 1; j >= 0; j-- {
		if c := r.steps[j].VerifiedCommit; c != "" {
			return c
		}
	}
	return r.t.BaseCommit
}

func (r *run) setStatus(ctx context.Context, to task.Status, reason string, resumeAt time.Time) error {
	t, err := r.e.cfg.Store.TransitionTask(ctx, r.t.ID, r.t.Status, to, store.TaskChange{Reason: reason, ResumeAt: resumeAt})
	if err != nil {
		return fmt.Errorf("engine: %w", err)
	}
	r.t = t
	r.publish(Event{Kind: EventTaskStatus, Status: string(to), Message: reason})
	return nil
}

// park moves the task to a waiting state and ends the run.
func (r *run) park(ctx context.Context, to task.Status, reason string, resumeAt time.Time) error {
	return r.setStatus(ctx, to, reason, resumeAt)
}
