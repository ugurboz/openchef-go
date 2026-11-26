package app

import (
	"os"
	"sort"
	"time"

	"github.com/ugurboz/openchef/internal/config"
	"github.com/ugurboz/openchef/internal/engine"
	"github.com/ugurboz/openchef/internal/quota"
	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/service"
)

// probesFor builds the L2 probes the configuration allows (§10.2): one per subscription
// pool whose provider has a probe and is not switched off.
func probesFor(cfg *config.Config, home string) []*quota.Prober {
	names := make([]string, 0, len(cfg.Pools))
	for name := range cfg.Pools {
		names = append(names, name)
	}
	sort.Strings(names)
	var out []*quota.Prober
	for _, name := range names {
		p := cfg.Pools[name]
		if p.Billing != "subscription" || !cfg.Quota.ProbesEnabled(p.Provider) {
			continue
		}
		switch p.Provider {
		case "anthropic":
			out = append(out, quota.ClaudeProbe(name, quota.ClaudeCredentials(home, os.Getenv, nil), nil, nil))
		case "openai":
			out = append(out, quota.ChatGPTProbe(name, quota.CodexCredentials(home, os.Getenv), nil, nil))
		}
	}
	return out
}

// observer receives quota observations, spending and attempt boundaries (service.Quota).
type observer interface {
	Observe(quota.Observation)
	Spend(pool string, usd float64, at time.Time)
	AttemptStarted(attempt int64, pool string)
	AttemptEnded(attempt int64)
}

// quotaSink records the quota signals in agent streams (L1, §10.1) before passing every
// event on. It never blocks: Observe does not.
type quotaSink struct {
	next   engine.Sink
	quota  observer
	poolOf func(agent string) string
}

func (s quotaSink) Publish(ev engine.Event) {
	switch ev.Kind { //nolint:exhaustive // only these events carry quota information
	case engine.EventAgent:
		pool := s.poolOf(ev.Agent)
		switch {
		case pool == "" || ev.Runtime == nil:
		case ev.Runtime.Kind == runtime.EventQuota && ev.Runtime.Quota != nil:
			s.quota.Observe(quota.FromSignal(pool, ev.At, ev.Runtime.Quota))
		case ev.Runtime.Kind == runtime.EventTurnCompleted && ev.Runtime.Result != nil:
			s.quota.Spend(pool, ev.Runtime.Result.Usage.CostUSD, ev.At) // API pools only
		}
	case engine.EventAttemptStarted:
		if pool := s.poolOf(ev.Agent); pool != "" {
			s.quota.AttemptStarted(ev.Attempt, pool) // L3: the attempt's cost is measured from here
		}
	case engine.EventAttemptEnded:
		s.quota.AttemptEnded(ev.Attempt)
	}
	s.next.Publish(ev)
}

// budgetsFor lists every API pool with its USD limits.
func budgetsFor(cfg *config.Config) map[string]service.Budget {
	out := map[string]service.Budget{}
	for name, p := range cfg.Pools {
		if p.Billing == "api" {
			out[name] = service.Budget{DailyUSD: p.DailyBudgetUSD, MonthlyUSD: p.MonthlyBudgetUSD}
		}
	}
	return out
}
