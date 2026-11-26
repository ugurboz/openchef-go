package app

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ugurboz/openchef/internal/config"
	"github.com/ugurboz/openchef/internal/engine"
	"github.com/ugurboz/openchef/internal/quota"
	"github.com/ugurboz/openchef/internal/router"
	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/task"
)

// quotaView is what routing reads from the quota monitor (service.Quota).
type quotaView interface {
	State(pool string) quota.State
	Health(pool string) (quota.Health, time.Time)
	Estimate(pool string) (float64, bool)
}

// surplusWindow: green headroom of at least 40 % that resets within this long would go
// unused, so auto effort may rise (§7.4).
const surplusWindow = time.Hour

// quotaRouter routes by quota (§7.3, §7.4, §10.3): each configured agent becomes a
// candidate with its pool's health, headroom and reset; admission excludes pools that
// cannot take the step; the phase's strategy picks; the effort is resolved for the
// chosen pool.
type quotaRouter struct {
	cfg    *config.Config
	agents []engine.Agent // config order (Agents)
	quota  quotaView
	now    func() time.Time

	mu       sync.Mutex
	counters router.MapCounters // weighted shares; kept for the daemon's life
}

func newQuotaRouter(cfg *config.Config, q quotaView, now func() time.Time) *quotaRouter {
	if now == nil {
		now = time.Now
	}
	return &quotaRouter{cfg: cfg, agents: Agents(cfg), quota: q, now: now, counters: router.MapCounters{}}
}

// Route implements engine.Router.
func (r *quotaRouter) Route(_ context.Context, t *task.Task, s *task.Step, exclude []string) (engine.Agent, error) {
	phase := r.cfg.Routing["implement"]
	importance := s.Importance.AtLeast(t.Importance)
	now := r.now()
	var (
		cands    []router.Candidate
		earliest time.Time
		refused  []string
	)
	for _, a := range r.agents {
		if t.AgentPin != "" && a.Name != t.AgentPin {
			continue
		}
		pool := r.cfg.Pools[a.Pool]
		st := r.quota.State(a.Pool)
		h, resetAt := r.quota.Health(a.Pool)
		est, known := r.quota.Estimate(a.Pool)
		ok, why := quota.Admit(h, &st, est, known)
		excluded := slices.Contains(exclude, a.Pool)
		if excluded {
			why = "ran out on this step"
		}
		if !ok || excluded {
			refused = append(refused, a.Name+": "+why)
			if !resetAt.IsZero() && (earliest.IsZero() || resetAt.Before(earliest)) {
				earliest = resetAt
			}
		}
		headroom, resetIn := quota.Headroom(&st, now)
		cands = append(cands, router.Candidate{
			Agent: a.Name, Pool: a.Pool, Tier: router.Tier(r.cfg.Agents[a.Name].Tier), Health: h,
			Headroom: headroom, ResetIn: resetIn, API: pool.Billing == "api",
			Excluded: !ok || excluded, Weight: phase.Weights[a.Name],
		})
	}
	noAgent := func(reason string) error {
		if len(refused) > 0 {
			reason += " (" + strings.Join(refused, "; ") + ")"
		}
		return &engine.NoAgentError{ResumeAt: earliest, Reason: reason}
	}
	var pick string
	switch {
	case t.AgentPin != "":
		// A pin disables selection (§7.3), not admission.
		if len(cands) == 0 {
			return engine.Agent{}, noAgent(fmt.Sprintf("pinned agent %q is not configured", t.AgentPin))
		}
		if cands[0].Excluded {
			return engine.Agent{}, noAgent(fmt.Sprintf("pinned agent %q cannot take the step now", t.AgentPin))
		}
		pick = cands[0].Agent
	default:
		strategy := router.Strategy(phase.Strategy)
		if strategy == "" {
			strategy = router.StrategyAuto
		}
		r.mu.Lock()
		d, err := router.Choose(router.Request{Strategy: strategy, Importance: importance}, cands, r.counters)
		r.mu.Unlock()
		if err != nil {
			return engine.Agent{}, noAgent(err.Error())
		}
		pick = d.Agent
	}
	a, _ := r.AgentNamed(pick)
	a.Effort = r.effort(a, importance, now)
	return a, nil
}

// effort resolves the chosen agent's effort for its pool now (§7.4).
func (r *quotaRouter) effort(a engine.Agent, importance task.Importance, now time.Time) runtime.Effort {
	pool := r.cfg.Pools[a.Pool]
	st := r.quota.State(a.Pool)
	h, _ := r.quota.Health(a.Pool)
	headroom, resetIn := quota.Headroom(&st, now)
	return router.ResolveEffort(router.EffortInput{
		Importance: importance, Profile: router.Profile(pool.EffortProfile), PlanTier: pool.PlanTier, Health: h,
		Surplus: h == quota.Green && headroom >= 0.4 && resetIn > 0 && resetIn < surplusWindow,
		Pin:     a.Effort,
	}).Level
}

// AgentNamed returns a configured agent (its configured effort), so a resumed attempt
// keeps its agent.
func (r *quotaRouter) AgentNamed(name string) (engine.Agent, bool) {
	for _, a := range r.agents {
		if a.Name == name {
			return a, true
		}
	}
	return engine.Agent{}, false
}
