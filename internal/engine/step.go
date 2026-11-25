package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"time"

	"github.com/ugurboz/openchef/internal/gitws"
	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/store"
	"github.com/ugurboz/openchef/internal/task"
	"github.com/ugurboz/openchef/internal/verify"
)

// attemptEnd says what the step loop does after an attempt.
type attemptEnd int

const (
	stepVerified attemptEnd = iota // next step
	failover                       // route to another pool and try again
	parked                         // the task left running (needs_attention, waiting_quota)
)

type attemptResult struct {
	end attemptEnd
	// For failover: the exhausted pool, its reset time, the agent and its saved patch.
	pool     string
	resetsAt time.Time
	from     string
	patch    string
}

// runStep works on step i until it is verified (true) or the task is parked (false).
func (r *run) runStep(ctx context.Context, i int) (bool, error) {
	s := &r.steps[i]
	if err := r.requireNoRunningAttempt(ctx, s.Index); err != nil {
		return false, err
	}
	// A step commit is only made after verification passed, so a commit found for a
	// step that is not verified yet means a crash came between the two: finish it.
	if done, err := r.adoptCommit(ctx, i); err != nil || done {
		return done, err
	}
	if s.Status == task.StepFailed {
		if err := r.stepTo(ctx, i, task.StepPending, store.StepChange{}); err != nil {
			return false, err
		}
	}
	if s.Status == task.StepPending {
		if err := r.stepTo(ctx, i, task.StepRunning, store.StepChange{}); err != nil {
			return false, err
		}
		r.publish(Event{Kind: EventStepStarted, Step: s.Index, Message: s.Title})
	}

	var exclude []string
	retries := 0 // feedback turns on this step, across attempts (§7.2: per step)
	kind, msg := task.TurnPrompt, stepPrompt(&r.t, s, len(r.steps))
	var earliestReset time.Time // of the pools excluded so far
	for {
		agent, err := r.e.cfg.Router.Route(ctx, &r.t, s, exclude)
		if errors.Is(err, ErrNoAgent) {
			return false, r.noAgent(ctx, err, earliestReset)
		}
		if err != nil {
			return false, fmt.Errorf("engine: route step %d: %w", s.Index, err)
		}
		res, err := r.attempt(ctx, i, agent, kind, msg, &retries)
		if err != nil {
			return false, err
		}
		switch res.end {
		case stepVerified:
			return true, nil
		case parked:
			return false, nil
		case failover:
			exclude = append(exclude, res.pool)
			if !res.resetsAt.IsZero() && (earliestReset.IsZero() || res.resetsAt.Before(earliestReset)) {
				earliestReset = res.resetsAt
			}
			kind = task.TurnHandoff
			msg = handoffPrompt(&r.t, s, len(r.steps), res.from, r.lastVerified(i), res.patch)
			r.publish(Event{Kind: EventNotice, Step: s.Index,
				Message: fmt.Sprintf("%s ran out of quota on pool %s; failing over", res.from, res.pool)})
		}
	}
}

func (r *run) requireNoRunningAttempt(ctx context.Context, step int) error {
	attempts, err := r.e.cfg.Store.Attempts(ctx, r.t.ID)
	if err != nil {
		return fmt.Errorf("engine: %w", err)
	}
	for i := range attempts {
		if attempts[i].StepIndex == step && attempts[i].Status == task.AttemptRunning {
			return fmt.Errorf("engine: task %s step %d attempt %d: %w", r.t.ID, step, attempts[i].ID, ErrNeedsRecovery)
		}
	}
	return nil
}

// adoptCommit marks step i verified if the branch already has its commit.
func (r *run) adoptCommit(ctx context.Context, i int) (bool, error) {
	s := &r.steps[i]
	sha, found, err := r.wt.FindStepCommit(ctx, r.t.BaseCommit, string(r.t.ID), s.Index)
	if err != nil {
		return false, fmt.Errorf("engine: look for step %d commit: %w", s.Index, err)
	}
	if !found {
		return false, nil
	}
	if s.Status != task.StepRunning {
		if s.Status == task.StepFailed {
			if err := r.stepTo(ctx, i, task.StepPending, store.StepChange{}); err != nil {
				return false, err
			}
		}
		if err := r.stepTo(ctx, i, task.StepRunning, store.StepChange{}); err != nil {
			return false, err
		}
	}
	if err := r.stepTo(ctx, i, task.StepVerified, store.StepChange{VerifiedCommit: sha}); err != nil {
		return false, err
	}
	r.publish(Event{Kind: EventStepFinished, Step: s.Index, Status: string(task.StepVerified),
		Message: sha + " (found after a restart)"})
	return true, nil
}

// noAgent parks the task: waiting_quota when a pool resets at a known time,
// needs_attention otherwise.
func (r *run) noAgent(ctx context.Context, err error, earliestReset time.Time) error {
	var resumeAt time.Time
	if na, ok := errors.AsType[*NoAgentError](err); ok {
		resumeAt = na.ResumeAt
	}
	if resumeAt.IsZero() {
		resumeAt = earliestReset
	}
	if resumeAt.IsZero() {
		return r.park(ctx, task.StatusNeedsAttention, err.Error(), time.Time{})
	}
	return r.park(ctx, task.StatusWaitingQuota, err.Error(), resumeAt)
}

func (r *run) stepTo(ctx context.Context, i int, to task.StepStatus, c store.StepChange) error {
	s, err := r.e.cfg.Store.TransitionStep(ctx, r.t.ID, r.steps[i].Index, r.steps[i].Status, to, c)
	if err != nil {
		return fmt.Errorf("engine: %w", err)
	}
	r.steps[i] = s
	return nil
}

// attempt is one agent's work on step i: one session (resumed once after a crash) and
// as many turns as the retry budget allows.
func (r *run) attempt(ctx context.Context, i int, agent Agent, kind task.TurnKind, msg string, retries *int) (attemptResult, error) {
	s := &r.steps[i]
	lim := r.e.cfg.Limits
	a := task.Attempt{TaskID: r.t.ID, StepIndex: s.Index, Agent: agent.Name, Runtime: agent.Runtime, Model: agent.Model, Pool: agent.Pool}
	if err := r.e.cfg.Store.StartAttempt(ctx, &a); err != nil {
		return attemptResult{}, fmt.Errorf("engine: %w", err)
	}
	r.publish(Event{Kind: EventAttemptStarted, Step: s.Index, Attempt: a.ID, Agent: agent.Name})

	seq := 1
	ls, err := r.openSession(ctx, &a, agent, nil, 0, seq)
	if err != nil {
		if stoppedBy(err) || ctx.Err() != nil {
			return attemptResult{}, err
		}
		return r.giveUp(ctx, i, &a, nil, "agent "+agent.Name+" could not start: "+err.Error())
	}
	defer func() {
		if cerr := r.closeSession(ctx, ls); cerr != nil {
			r.publish(Event{Kind: EventNotice, Step: s.Index, Attempt: a.ID, Message: cerr.Error()})
		}
	}()

	rateLimits, resumed := 0, false
	retry := func(next string, nextKind task.TurnKind, why string) (bool, error) {
		if *retries >= lim.MaxRetries {
			return false, nil
		}
		*retries++
		if err := r.stepTo(ctx, i, task.StepRunning, store.StepChange{CountRetry: true}); err != nil {
			return false, err
		}
		r.publish(Event{Kind: EventNotice, Step: s.Index, Attempt: a.ID,
			Message: fmt.Sprintf("retry %d of %d: %s", *retries, lim.MaxRetries, why)})
		msg, kind = next, nextKind
		return true, nil
	}

	for {
		out, err := r.runTurn(ctx, ls, &a, agent, kind, msg)
		if err != nil {
			return attemptResult{}, err
		}
		switch out.end {
		case endCompleted:
			res, err := r.verify(ctx, s)
			if err != nil {
				return attemptResult{}, err
			}
			if res == nil || res.Passed {
				return r.commit(ctx, i, &a, agent)
			}
			ok, err := retry(retryPrompt(&r.t, s, len(r.steps), res), task.TurnRetry, "verification "+res.Summary())
			if err != nil {
				return attemptResult{}, err
			}
			if !ok {
				return r.giveUp(ctx, i, &a, ls, fmt.Sprintf("step %d: verification %s after %d retries", s.Index, res.Summary(), lim.MaxRetries))
			}
		case endLoop, endStall:
			why := out.why
			next := stallPrompt(&r.t, s, len(r.steps), why)
			if out.end == endLoop {
				next = loopPrompt(&r.t, s, len(r.steps), lim.LoopRepeats)
			}
			ok, err := retry(next, task.TurnNudge, why)
			if err != nil {
				return attemptResult{}, err
			}
			if !ok {
				return r.giveUp(ctx, i, &a, ls, fmt.Sprintf("step %d: %s, retries exhausted", s.Index, why))
			}
		case endFailed:
			f := out.failure
			switch f.Kind {
			case runtime.FailRateLimited:
				if rateLimits >= lim.RateLimitRetries {
					return r.exhausted(ctx, i, &a, ls, agent, f)
				}
				rateLimits++
				if err := r.backoff(ctx, s, &a, f, rateLimits); err != nil {
					return attemptResult{}, err
				}
			case runtime.FailQuotaExhausted:
				return r.exhausted(ctx, i, &a, ls, agent, f)
			case runtime.FailCrashed, runtime.FailProtocol, runtime.FailInterrupted, runtime.FailTimeout:
				if resumed {
					return r.giveUp(ctx, i, &a, ls, fmt.Sprintf("agent %s failed twice (%s): %s", agent.Name, f.Kind, f.Message))
				}
				resumed = true
				seq++
				ls, err = r.resume(ctx, &a, agent, ls, seq)
				if err != nil {
					if stoppedBy(err) || ctx.Err() != nil {
						return attemptResult{}, err
					}
					return r.giveUp(ctx, i, &a, nil, fmt.Sprintf("agent %s could not resume: %v", agent.Name, err))
				}
				msg, kind = resumePrompt(&r.t, s, len(r.steps)), task.TurnRecovery
			case runtime.FailAuth:
				return r.giveUp(ctx, i, &a, ls, fmt.Sprintf("agent %s (%s) is not logged in; run %#q to log in, then %#q: %s",
					agent.Name, agent.Runtime, agent.Runtime, "openchef retry "+string(r.t.ID), f.Message))
			default:
				return r.giveUp(ctx, i, &a, ls, fmt.Sprintf("agent %s failed (%s): %s", agent.Name, f.Kind, f.Message))
			}
		}
	}
}

// resume closes the crashed session and continues its conversation in a new process.
func (r *run) resume(ctx context.Context, a *task.Attempt, agent Agent, old *liveSession, seq int) (*liveSession, error) {
	ref := old.s.Ref()
	if err := r.closeSession(ctx, old); err != nil {
		r.publish(Event{Kind: EventNotice, Step: a.StepIndex, Attempt: a.ID, Message: err.Error()})
	}
	r.publish(Event{Kind: EventNotice, Step: a.StepIndex, Attempt: a.ID, Agent: agent.Name, Message: "resuming the session after a failure"})
	ls, err := r.openSession(ctx, a, agent, &ref, old.row, seq)
	if err != nil {
		return nil, err
	}
	*old = *ls // the deferred close in attempt now closes the new session
	return old, nil
}

func (r *run) backoff(ctx context.Context, s *task.Step, a *task.Attempt, f *runtime.Failure, n int) error {
	wait := time.Duration(1<<(n-1)) * 30 * time.Second
	if !f.ResetsAt.IsZero() {
		if until := f.ResetsAt.Sub(r.e.cfg.Now()); until > 0 {
			wait = until
		}
	}
	wait = min(wait, r.e.cfg.Limits.MaxBackoff)
	r.publish(Event{Kind: EventNotice, Step: s.Index, Attempt: a.ID, Message: fmt.Sprintf("rate limited; waiting %s", wait.Round(time.Second))})
	return r.e.cfg.Sleep(ctx, wait)
}

// verify runs the step's verification command; nil means the step has none.
func (r *run) verify(ctx context.Context, s *task.Step) (*verify.Result, error) {
	if s.Verify == "" {
		return nil, nil //nolint:nilnil // no command, nothing to report
	}
	r.publish(Event{Kind: EventVerifyStarted, Step: s.Index, Message: s.Verify})
	res, err := verify.Run(ctx, verify.Spec{
		Command: s.Verify, Dir: r.wt.Path, Env: r.e.cfg.VerifyEnv, Timeout: r.e.cfg.Limits.VerifyTimeout,
	})
	if err != nil {
		return nil, fmt.Errorf("engine: verify step %d: %w", s.Index, err)
	}
	r.publish(Event{Kind: EventVerifyFinished, Step: s.Index, Verify: &res})
	return &res, nil
}

// commit records a verified step: git commit, then step verified, then attempt
// succeeded. A replay after a crash finds the existing commit by its trailers.
func (r *run) commit(ctx context.Context, i int, a *task.Attempt, agent Agent) (attemptResult, error) {
	s := &r.steps[i]
	if err := r.ensureActive(ctx); err != nil {
		return attemptResult{}, err
	}
	sha, err := r.wt.CommitStep(ctx, r.t.BaseCommit, gitws.StepCommit{
		Task: string(r.t.ID), Step: s.Index, Agent: agent.Name, Pool: agent.Pool, Subject: s.Title,
	})
	if err != nil {
		return attemptResult{}, fmt.Errorf("engine: commit step %d: %w", s.Index, err)
	}
	if err := r.stepTo(ctx, i, task.StepVerified, store.StepChange{VerifiedCommit: sha}); err != nil {
		return attemptResult{}, err
	}
	if err := r.e.cfg.Store.EndAttempt(ctx, a.ID, task.AttemptSucceeded, "verified"); err != nil {
		return attemptResult{}, fmt.Errorf("engine: %w", err)
	}
	r.publish(Event{Kind: EventAttemptEnded, Step: s.Index, Attempt: a.ID, Agent: agent.Name, Status: string(task.AttemptSucceeded), Message: "verified"})
	r.publish(Event{Kind: EventStepFinished, Step: s.Index, Status: string(task.StepVerified), Message: sha})
	return attemptResult{end: stepVerified}, nil
}

// exhausted saves the attempt's partial work as a patch, resets the workspace to the
// last verified commit and ends the attempt so the step can fail over (§7.2).
func (r *run) exhausted(ctx context.Context, i int, a *task.Attempt, ls *liveSession, agent Agent, f *runtime.Failure) (attemptResult, error) {
	s := &r.steps[i]
	if err := r.closeSession(ctx, ls); err != nil {
		r.publish(Event{Kind: EventNotice, Step: s.Index, Attempt: a.ID, Message: err.Error()})
	}
	if err := r.ensureActive(ctx); err != nil {
		return attemptResult{}, err
	}
	// The attempt id keeps patches of several failovers apart and makes a replay of
	// this attempt (crash after the reset) find the patch it saved before.
	rel := path.Join(gitws.ControlDir, "patches", fmt.Sprintf("step-%d-%s-%d.patch", s.Index, agent.Name, a.ID))
	abs := filepath.Join(r.wt.Path, filepath.FromSlash(rel))
	saved, err := r.wt.CapturePatch(ctx, abs)
	if err != nil {
		return attemptResult{}, fmt.Errorf("engine: save partial work of step %d: %w", s.Index, err)
	}
	if !saved {
		if _, statErr := os.Stat(abs); statErr == nil {
			saved = true
		}
	}
	if err := r.wt.ResetTo(ctx, r.lastVerified(i)); err != nil {
		return attemptResult{}, fmt.Errorf("engine: reset step %d: %w", s.Index, err)
	}
	if err := r.e.cfg.Store.EndAttempt(ctx, a.ID, task.AttemptFailedOver, string(f.Kind)); err != nil {
		return attemptResult{}, fmt.Errorf("engine: %w", err)
	}
	if r.e.cfg.PoolExhausted != nil {
		r.e.cfg.PoolExhausted(agent.Pool, f.ResetsAt)
	}
	r.publish(Event{Kind: EventAttemptEnded, Step: s.Index, Attempt: a.ID, Agent: agent.Name, Status: string(task.AttemptFailedOver), Message: f.Message})
	res := attemptResult{end: failover, pool: agent.Pool, resetsAt: f.ResetsAt, from: agent.Name}
	if saved {
		res.patch = rel
	}
	return res, nil
}

// giveUp fails the attempt and the step and parks the task for a human. ls may be nil.
func (r *run) giveUp(ctx context.Context, i int, a *task.Attempt, ls *liveSession, reason string) (attemptResult, error) {
	s := &r.steps[i]
	if ls != nil {
		if err := r.closeSession(ctx, ls); err != nil {
			r.publish(Event{Kind: EventNotice, Step: s.Index, Attempt: a.ID, Message: err.Error()})
		}
	}
	if err := r.e.cfg.Store.EndAttempt(ctx, a.ID, task.AttemptFailed, reason); err != nil {
		return attemptResult{}, fmt.Errorf("engine: %w", err)
	}
	r.publish(Event{Kind: EventAttemptEnded, Step: s.Index, Attempt: a.ID, Agent: a.Agent, Status: string(task.AttemptFailed), Message: reason})
	if err := r.stepTo(ctx, i, task.StepFailed, store.StepChange{}); err != nil {
		return attemptResult{}, err
	}
	r.publish(Event{Kind: EventStepFinished, Step: s.Index, Status: string(task.StepFailed), Message: reason})
	if err := r.park(ctx, task.StatusNeedsAttention, reason, time.Time{}); err != nil {
		return attemptResult{}, err
	}
	return attemptResult{end: parked}, nil
}
