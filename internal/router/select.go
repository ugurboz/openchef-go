package router

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ugurboz/openchef/internal/quota"
	"github.com/ugurboz/openchef/internal/task"
)

// Strategy picks among eligible agents (§7.4).
type Strategy string

// Strategies.
const (
	StrategyAuto     Strategy = "auto"
	StrategyWeighted Strategy = "weighted"
	StrategyOrdered  Strategy = "ordered"
)

// ErrNoCandidate means no agent passed the filters.
var ErrNoCandidate = errors.New("router: no eligible agent")

// Candidate is one configured agent with the state the router needs. Candidates are
// given in config order, which breaks ties.
type Candidate struct {
	Agent string
	Pool  string
	Tier  Tier
	// Health is the pool's quota health.
	Health quota.Health
	// Headroom is the unused share of the pool's tightest window, in [0,1].
	Headroom float64
	// ResetIn is how soon that window resets (use it or lose it); 0 if unknown.
	ResetIn time.Duration
	// FailureRate is the share of recent attempts on this agent that failed, in [0,1].
	FailureRate float64
	// API marks pay-per-use pools, which rank below subscriptions.
	API bool
	// Excluded removes the candidate for this attempt (pool exhausted on this step,
	// over its concurrency limit, …).
	Excluded bool
	// Weight is the agent's target share for the weighted strategy.
	Weight int
}

// Request describes one routing decision.
type Request struct {
	Strategy   Strategy
	Importance task.Importance
	// AllowDowngrade lets a step use a lower tier than its importance permits when no
	// permitted agent is available (--allow-downgrade). Never implied.
	AllowDowngrade bool
}

// Counters holds the weighted strategy's running state per phase, persisted by the
// caller so shares hold across daemon restarts.
type Counters interface {
	Get(agent string) int
	Set(agent string, v int)
}

// Decision is the chosen agent and why.
type Decision struct {
	Agent  string
	Reason string
	// Downgraded is set when AllowDowngrade let a lower tier through.
	Downgraded bool
}

// Choose filters candidates by tier and health and picks one with the strategy.
func Choose(req Request, cands []Candidate, counters Counters) (Decision, error) {
	eligible := filter(cands, AllowedTiers(req.Importance))
	downgraded := false
	if len(eligible) == 0 && req.AllowDowngrade {
		eligible = filter(cands, []Tier{Frontier, Standard, Fast})
		downgraded = len(eligible) > 0
	}
	if len(eligible) == 0 {
		return Decision{}, fmt.Errorf("%w for %s importance (tiers %v); a step is never silently downgraded",
			ErrNoCandidate, req.Importance, AllowedTiers(req.Importance))
	}
	var d Decision
	switch req.Strategy {
	case StrategyOrdered:
		d = Decision{Agent: eligible[0].Agent, Reason: "first available in order"}
	case StrategyWeighted:
		d = weighted(eligible, counters)
	default:
		d = auto(eligible)
	}
	d.Downgraded = downgraded
	if downgraded {
		d.Reason += " (downgraded: no agent of an allowed tier was available)"
	}
	return d, nil
}

func filter(cands []Candidate, tiers []Tier) []Candidate {
	var out []Candidate
	for _, c := range cands {
		if c.Excluded || c.Health == quota.Exhausted || !slices.Contains(tiers, c.Tier) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// weighted is smooth weighted round-robin: deterministic, and exact shares over time.
// Agents that are unavailable this time simply do not take part, so their share is
// redistributed proportionally among the others.
func weighted(eligible []Candidate, counters Counters) Decision {
	total := 0
	for _, c := range eligible {
		total += max(c.Weight, 0)
	}
	if total == 0 {
		return Decision{Agent: eligible[0].Agent, Reason: "weighted (no weights; first available)"}
	}
	best, bestV := -1, 0
	for i, c := range eligible {
		v := counters.Get(c.Agent) + max(c.Weight, 0)
		counters.Set(c.Agent, v)
		if best < 0 || v > bestV {
			best, bestV = i, v
		}
	}
	pick := eligible[best]
	counters.Set(pick.Agent, bestV-total)
	return Decision{Agent: pick.Agent, Reason: fmt.Sprintf("weighted (%d%% target share)", 100*pick.Weight/total)}
}

// auto spends the subscriptions the user pays for evenly before their windows reset:
// headroom, weighted up when the reset is near, discounted by recent failures and for
// API pools and red health. Ties keep config order.
func auto(eligible []Candidate) Decision {
	best, bestScore := 0, -1.0
	for i, c := range eligible {
		s := score(c)
		if s > bestScore {
			best, bestScore = i, s
		}
	}
	c := eligible[best]
	return Decision{Agent: c.Agent, Reason: fmt.Sprintf("auto (headroom %.0f%%, %s)", 100*c.Headroom, c.Health)}
}

func score(c Candidate) float64 {
	s := c.Headroom
	if c.Health == quota.Unknown {
		s = 0.5 // no data: neither preferred nor avoided
	}
	if c.ResetIn > 0 && c.ResetIn < 24*time.Hour {
		// Use it or lose it: headroom that resets within the day is worth up to 50 % more.
		s *= 1 + 0.5*(1-float64(c.ResetIn)/float64(24*time.Hour))
	}
	s *= 1 - 0.8*c.FailureRate
	if c.API {
		s *= 0.3
	}
	if c.Health == quota.Red {
		s *= 0.5
	}
	return s
}

// MapCounters is an in-memory Counters.
type MapCounters map[string]int

// Get implements Counters.
func (m MapCounters) Get(agent string) int { return m[agent] }

// Set implements Counters.
func (m MapCounters) Set(agent string, v int) { m[agent] = v }
