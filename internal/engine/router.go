package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/task"
)

// Agent is a routed (runtime, model, pool) triple (§4) with its resolved effort.
type Agent struct {
	Name    string
	Runtime string
	Model   string
	Pool    string
	Effort  runtime.Effort
}

// ErrNoAgent means no agent is eligible for a step right now. A *NoAgentError says
// when one may be.
var ErrNoAgent = errors.New("engine: no eligible agent")

// NoAgentError carries the earliest time an agent may become eligible again.
type NoAgentError struct {
	// ResumeAt is zero when nothing will change by itself.
	ResumeAt time.Time
	Reason   string
}

func (e *NoAgentError) Error() string {
	if e.ResumeAt.IsZero() {
		return fmt.Sprintf("engine: no eligible agent: %s", e.Reason)
	}
	return fmt.Sprintf("engine: no eligible agent until %s: %s", e.ResumeAt.Format(time.RFC3339), e.Reason)
}

// Is reports whether target is ErrNoAgent.
func (e *NoAgentError) Is(target error) bool { return target == ErrNoAgent }

// Router picks the agent for an attempt at a step (§7.3). Exclude lists pools that must
// not be used for this attempt (they ran out of quota on this step).
type Router interface {
	Route(ctx context.Context, t *task.Task, s *task.Step, exclude []string) (Agent, error)
}

// Ordered routes to the first agent whose pool is not excluded, or to the task's pinned
// agent. It is the M3 router; quota-aware selection strategies arrive in M4 (§7.4).
type Ordered struct {
	Agents []Agent
	// Cooldown reports until when a pool is unusable (quota reset); zero means usable.
	Cooldown func(pool string) time.Time
}

// Route implements Router.
func (o *Ordered) Route(_ context.Context, t *task.Task, _ *task.Step, exclude []string) (Agent, error) {
	var earliest time.Time
	for _, a := range o.Agents {
		if t.AgentPin != "" && a.Name != t.AgentPin {
			continue
		}
		if slices.Contains(exclude, a.Pool) {
			continue
		}
		if o.Cooldown != nil {
			if until := o.Cooldown(a.Pool); !until.IsZero() {
				if earliest.IsZero() || until.Before(earliest) {
					earliest = until
				}
				continue
			}
		}
		return a, nil
	}
	reason := "every configured agent's pool is exhausted"
	if t.AgentPin != "" {
		reason = fmt.Sprintf("pinned agent %q is unavailable", t.AgentPin)
	}
	return Agent{}, &NoAgentError{ResumeAt: earliest, Reason: reason}
}
