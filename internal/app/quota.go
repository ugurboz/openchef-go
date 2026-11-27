package app

import (
	"os"
	"sort"

	"github.com/ugurboz/openchef/internal/config"
	"github.com/ugurboz/openchef/internal/engine"
	"github.com/ugurboz/openchef/internal/quota"
	"github.com/ugurboz/openchef/internal/runtime"
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

// observer receives quota observations (service.Quota).
type observer interface {
	Observe(quota.Observation)
}

// quotaSink records the quota signals in agent streams (L1, §10.1) before passing every
// event on. It never blocks: Observe does not.
type quotaSink struct {
	next   engine.Sink
	quota  observer
	poolOf func(agent string) string
}

func (s quotaSink) Publish(ev engine.Event) {
	if ev.Kind == engine.EventAgent && ev.Runtime != nil && ev.Runtime.Kind == runtime.EventQuota && ev.Runtime.Quota != nil {
		if pool := s.poolOf(ev.Agent); pool != "" {
			s.quota.Observe(quota.FromSignal(pool, ev.At, ev.Runtime.Quota))
		}
	}
	s.next.Publish(ev)
}
