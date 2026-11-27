package config

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// Enumerations used by the configuration. Values match docs/architecture.md §7.4 and §13.
var (
	Providers      = []string{"anthropic", "openai", "google"}
	Billings       = []string{"subscription", "api", "free"}
	PlanTiers      = []string{"free", "pro", "plus", "max", "ultra", "team", "enterprise", "api"}
	EffortProfiles = []string{"auto", "minimal", "economy", "balanced", "quality"}
	Runtimes       = []string{"claude", "codex", "agy"}
	Tiers          = []string{"frontier", "standard", "fast"}
	Efforts        = []string{"low", "medium", "high", "max"}
	Strategies     = []string{"auto", "weighted", "ordered"}
	Phases         = []string{"plan", "implement", "review", "helper"}
)

// Config is the content of config.toml.
type Config struct {
	Pools   map[string]Pool  `toml:"pools"`
	Agents  map[string]Agent `toml:"agents"`
	Routing map[string]Phase `toml:"routing"`
	Quota   Quota            `toml:"quota"`
}

// Quota configures quota tracking (§10).
type Quota struct {
	// Probes turns L2 probes on or off for every provider; unset means on (§10.2).
	Probes *bool `toml:"probes,omitempty"`
	// Providers switches probes per provider ("anthropic", "openai").
	Providers map[string]bool `toml:"providers,omitempty"`
}

// ProbesEnabled reports whether L2 probes may run for provider.
func (q Quota) ProbesEnabled(provider string) bool {
	if q.Probes != nil && !*q.Probes {
		return false
	}
	if on, ok := q.Providers[provider]; ok {
		return on
	}
	return true
}

// Pool is one quota bucket the user owns.
type Pool struct {
	Provider      string `toml:"provider"`
	Billing       string `toml:"billing"`
	PlanTier      string `toml:"plan_tier,omitempty"`
	MaxConcurrent int    `toml:"max_concurrent,omitempty"`
	EffortProfile string `toml:"effort_profile,omitempty"`
	APIKeyEnv     string `toml:"api_key_env,omitempty"`
	// DailyBudgetUSD and MonthlyBudgetUSD limit API pools (§10.3); 0 means no limit.
	DailyBudgetUSD   float64 `toml:"daily_budget_usd,omitempty"`
	MonthlyBudgetUSD float64 `toml:"monthly_budget_usd,omitempty"`
}

// Agent is a named (runtime, model, pool) triple.
type Agent struct {
	Runtime string `toml:"runtime"`
	// Model is passed to the runtime as-is. Empty means the runtime's default model.
	Model  string `toml:"model,omitempty"`
	Pool   string `toml:"pool"`
	Tier   string `toml:"tier"`
	Effort string `toml:"effort,omitempty"`
}

// Phase configures agent selection for one pipeline phase.
type Phase struct {
	Strategy string         `toml:"strategy,omitempty"`
	Agents   []string       `toml:"agents"`
	Weights  map[string]int `toml:"weights,omitempty"`
}

// Defaults applied by Load when a field is omitted.
const (
	DefaultMaxConcurrent = 2
	DefaultEffortProfile = "auto"
	DefaultStrategy      = "auto"
)

// ErrNotFound means the configuration file does not exist (run `openchef init`).
var ErrNotFound = errors.New("config: configuration file not found")

// ValidationError lists every problem found in a configuration, each prefixed with the
// TOML key it concerns.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return "config: invalid configuration:\n  - " + strings.Join(e.Problems, "\n  - ")
}

// Load reads, decodes (rejecting unknown keys), defaults and validates path.
func Load(path string) (*Config, error) {
	//nolint:gosec // G304: path is OpenChef's own config location.
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, path)
	}
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	cfg, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// Parse decodes and validates TOML content.
func Parse(raw []byte) (*Config, error) {
	var cfg Config
	dec := toml.NewDecoder(bytes.NewReader(raw)).DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, describeDecodeError(err)
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func describeDecodeError(err error) error {
	var strict *toml.StrictMissingError
	if errors.As(err, &strict) {
		problems := make([]string, 0, len(strict.Errors))
		for i := range strict.Errors {
			de := &strict.Errors[i]
			row, col := de.Position()
			problems = append(problems, fmt.Sprintf("line %d, column %d: unknown key %q", row, col, strings.Join(de.Key(), ".")))
		}
		return &ValidationError{Problems: problems}
	}
	var de *toml.DecodeError
	if errors.As(err, &de) {
		row, col := de.Position()
		return fmt.Errorf("config: line %d, column %d: %w", row, col, err)
	}
	return fmt.Errorf("config: decode: %w", err)
}

func (c *Config) applyDefaults() {
	for name, p := range c.Pools {
		if p.MaxConcurrent == 0 {
			p.MaxConcurrent = DefaultMaxConcurrent
		}
		if p.EffortProfile == "" {
			p.EffortProfile = DefaultEffortProfile
		}
		c.Pools[name] = p
	}
	for name, ph := range c.Routing {
		if ph.Strategy == "" {
			ph.Strategy = DefaultStrategy
		}
		c.Routing[name] = ph
	}
}

// Validate checks internal consistency and returns a *ValidationError listing every
// problem, in a stable order.
func (c *Config) Validate() error {
	var problems []string
	add := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }
	oneOf := func(key, value string, allowed []string) {
		if !slices.Contains(allowed, value) {
			add("%s: %q is not one of %s", key, value, strings.Join(allowed, ", "))
		}
	}

	if len(c.Pools) == 0 {
		add("pools: at least one pool is required")
	}
	for _, name := range sortedKeys(c.Pools) {
		p := c.Pools[name]
		key := "pools." + name
		oneOf(key+".provider", p.Provider, Providers)
		oneOf(key+".billing", p.Billing, Billings)
		if p.PlanTier != "" {
			oneOf(key+".plan_tier", p.PlanTier, PlanTiers)
		}
		oneOf(key+".effort_profile", p.EffortProfile, EffortProfiles)
		if p.MaxConcurrent < 1 {
			add("%s.max_concurrent: must be at least 1", key)
		}
		switch {
		case p.Billing == "api" && p.APIKeyEnv == "":
			add("%s.api_key_env: required for api billing (subscription keys are never used implicitly)", key)
		case p.Billing != "api" && p.APIKeyEnv != "":
			add("%s.api_key_env: only allowed with billing = \"api\"", key)
		}
		if p.DailyBudgetUSD < 0 || p.MonthlyBudgetUSD < 0 {
			add("%s: budgets cannot be negative", key)
		}
		if p.Billing != "api" && (p.DailyBudgetUSD > 0 || p.MonthlyBudgetUSD > 0) {
			add("%s: USD budgets apply to billing = \"api\" pools; subscriptions have quota windows", key)
		}
	}

	if len(c.Agents) == 0 {
		add("agents: at least one agent is required")
	}
	for _, name := range sortedKeys(c.Agents) {
		a := c.Agents[name]
		key := "agents." + name
		oneOf(key+".runtime", a.Runtime, Runtimes)
		oneOf(key+".tier", a.Tier, Tiers)
		if a.Effort != "" {
			oneOf(key+".effort", a.Effort, Efforts)
		}
		if _, ok := c.Pools[a.Pool]; !ok {
			add("%s.pool: unknown pool %q", key, a.Pool)
		}
	}

	for _, phase := range sortedKeys(c.Routing) {
		ph := c.Routing[phase]
		key := "routing." + phase
		oneOf(key, phase, Phases)
		oneOf(key+".strategy", ph.Strategy, Strategies)
		if len(ph.Agents) == 0 {
			add("%s.agents: at least one agent is required", key)
		}
		seen := map[string]bool{}
		for _, a := range ph.Agents {
			if _, ok := c.Agents[a]; !ok {
				add("%s.agents: unknown agent %q", key, a)
			}
			if seen[a] {
				add("%s.agents: %q listed twice", key, a)
			}
			seen[a] = true
		}
		validateWeights(key, ph, seen, add)
	}
	for _, name := range sortedKeys(c.Quota.Providers) {
		if !slices.Contains(Providers, name) {
			add("quota.providers.%s: unknown provider (want one of %s)", name, strings.Join(Providers, ", "))
		}
	}

	if len(problems) > 0 {
		return &ValidationError{Problems: problems}
	}
	return nil
}

func validateWeights(key string, ph Phase, listed map[string]bool, add func(string, ...any)) {
	if ph.Strategy != "weighted" {
		if len(ph.Weights) > 0 {
			add("%s.weights: only used with strategy = \"weighted\"", key)
		}
		return
	}
	if len(ph.Weights) == 0 {
		add("%s.weights: required with strategy = \"weighted\"", key)
		return
	}
	for _, a := range sortedKeys(ph.Weights) {
		w := ph.Weights[a]
		if !listed[a] {
			add("%s.weights: %q is not in %s.agents", key, a, key)
		}
		if w <= 0 {
			add("%s.weights.%s: must be a positive integer", key, a)
		}
	}
}

// Marshal encodes the configuration as TOML.
func (c *Config) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf).SetIndentTables(false)
	if err := enc.Encode(c); err != nil {
		return nil, fmt.Errorf("config: encode: %w", err)
	}
	return dropEmptyParentTables(buf.Bytes()), nil
}

// dropEmptyParentTables removes header-only lines such as "[pools]" that the encoder
// emits before sub-tables like "[pools.claude]". They are valid but redundant TOML, and
// the generated file is meant to be read and edited by people.
func dropEmptyParentTables(raw []byte) []byte {
	lines := strings.Split(string(raw), "\n")
	out := lines[:0]
	for _, line := range lines {
		switch strings.TrimSpace(line) {
		case "[pools]", "[agents]", "[routing]":
			continue
		}
		out = append(out, line)
	}
	return []byte(strings.Join(out, "\n"))
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
