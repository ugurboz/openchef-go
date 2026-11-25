package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/task"
)

// turnEnd classifies how a turn ended from the engine's point of view.
type turnEnd int

const (
	endCompleted turnEnd = iota
	endFailed            // the CLI reported a failure (Failure is set)
	endLoop              // OpenChef interrupted a tool-call loop
	endStall             // OpenChef interrupted an idle or overlong turn (Why is set)
)

type turnOutcome struct {
	end     turnEnd
	result  *runtime.TurnResult
	failure *runtime.Failure
	why     string
}

// runTurn records a turn, sends it, streams its events and enforces the turn limits:
// a turn timeout, an idle timeout and loop detection, each of which interrupts the turn.
// A cancelled ctx (shutdown) interrupts the turn and returns ctx's error, leaving the
// turn recorded as sent so recovery retries it.
func (r *run) runTurn(ctx context.Context, ls *liveSession, a *task.Attempt, agent Agent, kind task.TurnKind, msg string) (turnOutcome, error) {
	lim := r.e.cfg.Limits
	row := task.Turn{SessionID: ls.row, Kind: kind, Message: msg}
	if err := r.e.cfg.Store.BeginTurn(ctx, &row); err != nil {
		return turnOutcome{}, fmt.Errorf("engine: %w", err)
	}
	tctx, cancel := context.WithTimeout(ctx, lim.TurnTimeout)
	defer cancel()
	ch, err := ls.s.Turn(tctx, runtime.TurnInput{Text: msg})
	if err != nil {
		out := turnOutcome{end: endFailed, failure: &runtime.Failure{Kind: runtime.FailCrashed, Message: err.Error()}}
		return out, r.endTurn(ctx, row.ID, out)
	}

	idle := time.NewTimer(lim.IdleTimeout)
	defer idle.Stop()
	var (
		stopReason turnEnd
		why        string
		lastSig    string
		repeats    int
		stopBy     <-chan time.Time
		final      *runtime.Event
	)
	interrupt := func(reason turnEnd, w string) {
		if stopBy != nil {
			return
		}
		stopReason, why = reason, w
		// The deadline starts now and the interrupt runs aside: some adapters block in
		// Interrupt until the turn ends, and the loop must keep draining events.
		stopBy = time.After(lim.StopWait)
		go func() {
			ictx, icancel := context.WithTimeout(context.WithoutCancel(ctx), lim.StopWait)
			defer icancel()
			if err := ls.s.Interrupt(ictx); err != nil && !errors.Is(err, runtime.ErrSessionClosed) {
				r.publish(Event{Kind: EventNotice, Step: a.StepIndex, Attempt: a.ID, Agent: agent.Name,
					Message: "interrupt failed: " + err.Error()})
			}
		}()
	}

wait:
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				break wait
			}
			r.publish(Event{Kind: EventAgent, Step: a.StepIndex, Attempt: a.ID, Agent: agent.Name, Runtime: &ev})
			if ev.Kind.Terminal() {
				final = &ev
				break wait
			}
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(lim.IdleTimeout)
			if ev.Kind == runtime.EventToolCall && ev.ToolCall != nil {
				if sig := ev.ToolCall.Signature; sig != "" && sig == lastSig {
					repeats++
				} else {
					lastSig, repeats = sig, 1
				}
				if repeats >= lim.LoopRepeats {
					interrupt(endLoop, fmt.Sprintf("the same tool call was repeated %d times", repeats))
				}
			}
		case <-idle.C:
			interrupt(endStall, fmt.Sprintf("no activity for %s", lim.IdleTimeout))
		case <-ctx.Done():
			// Shutdown: stop the turn but leave it recorded as sent for recovery.
			interrupt(endStall, "OpenChef is shutting down")
			drain(ch, stopBy)
			return turnOutcome{}, fmt.Errorf("engine: %w", ctx.Err())
		case <-stopBy:
			// The CLI ignored the interrupt; the caller closes (kills) the session.
			out := turnOutcome{end: endFailed, failure: &runtime.Failure{Kind: runtime.FailCrashed,
				Message: "the agent did not stop after an interrupt"}}
			return out, r.endTurn(ctx, row.ID, out)
		}
	}

	var out turnOutcome
	switch {
	case final == nil:
		out = turnOutcome{end: endFailed, failure: &runtime.Failure{Kind: runtime.FailProtocol,
			Message: "the turn ended without a result"}}
	case final.Kind == runtime.EventTurnCompleted:
		// Completed despite an interrupt: the work is done, verification decides.
		out = turnOutcome{end: endCompleted, result: final.Result}
	case stopBy != nil:
		out = turnOutcome{end: stopReason, why: why}
	case final.Failure != nil && final.Failure.Kind == runtime.FailTimeout && tctx.Err() != nil && ctx.Err() == nil:
		out = turnOutcome{end: endStall, why: fmt.Sprintf("the turn took longer than %s", lim.TurnTimeout)}
	default:
		f := final.Failure
		if f == nil {
			f = &runtime.Failure{Kind: runtime.FailUnknown, Message: "the turn failed without details"}
		}
		out = turnOutcome{end: endFailed, failure: f}
	}
	return out, r.endTurn(ctx, row.ID, out)
}

// drain consumes a turn's events until it ends or stopBy fires.
func drain(ch <-chan runtime.Event, stopBy <-chan time.Time) {
	for {
		select {
		case ev, ok := <-ch:
			if !ok || ev.Kind.Terminal() {
				return
			}
		case <-stopBy:
			return
		}
	}
}

func (r *run) endTurn(ctx context.Context, id int64, out turnOutcome) error {
	status, outcome := task.TurnCompleted, "completed"
	switch out.end {
	case endCompleted:
	case endFailed:
		status, outcome = task.TurnFailed, string(out.failure.Kind)
	case endLoop:
		status, outcome = task.TurnInterrupted, "loop"
	case endStall:
		status, outcome = task.TurnInterrupted, "stall: "+out.why
	}
	if err := r.e.cfg.Store.EndTurn(ctx, id, status, outcome); err != nil {
		return fmt.Errorf("engine: %w", err)
	}
	return nil
}
