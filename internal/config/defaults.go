package config

import "slices"

// runtimeDefaults describes the pool and agents `openchef init` creates for a runtime.
// Model IDs are deliberately not hard-coded where they change between releases: Claude
// Code's aliases (opus, sonnet, haiku) are stable, Codex and agy use their own default
// model when Model is empty.
type runtimeDefaults struct {
	pool     string
	poolSpec Pool
	agents   []namedAgent
}

type namedAgent struct {
	name  string
	agent Agent
}

var defaultsByRuntime = map[string]runtimeDefaults{
	"claude": {
		pool:     "claude",
		poolSpec: Pool{Provider: "anthropic", Billing: "subscription", PlanTier: "pro"},
		agents: []namedAgent{
			{"opus", Agent{Runtime: "claude", Model: "opus", Tier: "frontier"}},
			{"sonnet", Agent{Runtime: "claude", Model: "sonnet", Tier: "standard"}},
			{"haiku", Agent{Runtime: "claude", Model: "haiku", Tier: "fast"}},
		},
	},
	"codex": {
		pool:     "chatgpt",
		poolSpec: Pool{Provider: "openai", Billing: "subscription", PlanTier: "plus"},
		agents: []namedAgent{
			{"gpt", Agent{Runtime: "codex", Tier: "frontier"}},
		},
	},
	"agy": {
		pool:     "google",
		poolSpec: Pool{Provider: "google", Billing: "subscription", PlanTier: "pro"},
		agents: []namedAgent{
			{"gemini", Agent{Runtime: "agy", Tier: "frontier"}},
		},
	},
}

// Preferred agent order per phase. Agents that were not generated are skipped.
var defaultPhaseOrder = map[string][]string{
	"plan":      {"opus", "gpt", "gemini"},
	"implement": {"opus", "gpt", "gemini", "sonnet"},
	"review":    {"gpt", "gemini", "opus"},
	"helper":    {"haiku", "gemini", "gpt"},
}

// Default builds a configuration for the given runtimes (unknown names are ignored).
// planTiers optionally overrides the plan tier per runtime, e.g. {"claude": "max"}.
func Default(runtimes []string, planTiers map[string]string) *Config {
	cfg := &Config{
		Pools:   map[string]Pool{},
		Agents:  map[string]Agent{},
		Routing: map[string]Phase{},
	}
	for _, rt := range runtimes {
		d, ok := defaultsByRuntime[rt]
		if !ok {
			continue
		}
		pool := d.poolSpec
		if tier, ok := planTiers[rt]; ok && tier != "" {
			pool.PlanTier = tier
		}
		pool.MaxConcurrent = DefaultMaxConcurrent
		pool.EffortProfile = DefaultEffortProfile
		cfg.Pools[d.pool] = pool
		for _, na := range d.agents {
			a := na.agent
			a.Pool = d.pool
			cfg.Agents[na.name] = a
		}
	}
	for _, phase := range Phases {
		var agents []string
		for _, name := range defaultPhaseOrder[phase] {
			if _, ok := cfg.Agents[name]; ok && !slices.Contains(agents, name) {
				agents = append(agents, name)
			}
		}
		if len(agents) > 0 {
			cfg.Routing[phase] = Phase{Strategy: DefaultStrategy, Agents: agents}
		}
	}
	return cfg
}

// SupportedRuntimes lists the runtimes Default knows, in display order.
func SupportedRuntimes() []string { return slices.Clone(Runtimes) }
