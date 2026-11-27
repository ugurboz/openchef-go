package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validTOML = `
[pools.claude]
provider = "anthropic"
billing  = "subscription"
plan_tier = "max"

[pools.api]
provider    = "openai"
billing     = "api"
api_key_env = "OPENAI_API_KEY"
effort_profile = "economy"
max_concurrent = 4

[agents.opus]
runtime = "claude"
model   = "opus"
pool    = "claude"
tier    = "frontier"
effort  = "high"

[agents.gpt]
runtime = "codex"
pool    = "api"
tier    = "frontier"

[routing.implement]
strategy = "weighted"
agents   = ["opus", "gpt"]
weights  = { opus = 60, gpt = 40 }

[routing.review]
agents = ["gpt", "opus"]
`

func TestParseValidAppliesDefaults(t *testing.T) {
	t.Parallel()
	cfg, err := Parse([]byte(validTOML))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	claude := cfg.Pools["claude"]
	if claude.MaxConcurrent != DefaultMaxConcurrent || claude.EffortProfile != DefaultEffortProfile {
		t.Errorf("pool defaults not applied: %+v", claude)
	}
	if api := cfg.Pools["api"]; api.MaxConcurrent != 4 || api.EffortProfile != "economy" {
		t.Errorf("explicit pool values overwritten: %+v", api)
	}
	if got := cfg.Routing["review"].Strategy; got != DefaultStrategy {
		t.Errorf("routing strategy default = %q", got)
	}
	if got := cfg.Routing["implement"].Weights["opus"]; got != 60 {
		t.Errorf("weights not decoded: %d", got)
	}
}

func TestParseRejectsUnknownKeysWithPosition(t *testing.T) {
	t.Parallel()
	raw := validTOML + "\n[agents.opus2]\nruntime = \"claude\"\npool = \"claude\"\ntier = \"fast\"\nmodel_id = \"haiku\"\n"
	_, err := Parse([]byte(raw))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("got %v, want *ValidationError", err)
	}
	if !strings.Contains(err.Error(), `unknown key "agents.opus2.model_id"`) || !strings.Contains(err.Error(), "line ") {
		t.Fatalf("error should name the key and line: %v", err)
	}
}

func TestParseSyntaxErrorHasPosition(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte("[pools.claude\nprovider = 1"))
	if err == nil || !strings.Contains(err.Error(), "line 1") {
		t.Fatalf("got %v", err)
	}
}

func TestValidateReportsEveryProblem(t *testing.T) {
	t.Parallel()
	raw := `
[pools.p]
provider = "acme"
billing  = "api"
plan_tier = "gold"
effort_profile = "turbo"

[pools.q]
provider = "google"
billing  = "subscription"
api_key_env = "X"

[agents.a]
runtime = "cursor"
pool    = "missing"
tier    = "ultra"
effort  = "xhigh"

[routing.deploy]
agents = []

[routing.implement]
strategy = "weighted"
agents = ["a", "a", "ghost"]
weights = { a = 0, other = 3 }

[routing.review]
agents = ["a"]
weights = { a = 1 }
`
	_, err := Parse([]byte(raw))
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("got %v, want *ValidationError", err)
	}
	wantFragments := []string{
		`pools.p.provider: "acme"`,
		"pools.p.api_key_env: required for api billing",
		`pools.p.plan_tier: "gold"`,
		`pools.p.effort_profile: "turbo"`,
		"pools.q.api_key_env: only allowed",
		`agents.a.runtime: "cursor"`,
		`agents.a.tier: "ultra"`,
		`agents.a.effort: "xhigh"`,
		`agents.a.pool: unknown pool "missing"`,
		`routing.deploy: "deploy" is not one of`,
		"routing.deploy.agents: at least one agent",
		`routing.implement.agents: "a" listed twice`,
		`routing.implement.agents: unknown agent "ghost"`,
		"routing.implement.weights.a: must be a positive integer",
		`routing.implement.weights: "other" is not in routing.implement.agents`,
		`routing.review.weights: only used with strategy = "weighted"`,
	}
	msg := err.Error()
	for _, f := range wantFragments {
		if !strings.Contains(msg, f) {
			t.Errorf("missing problem %q in:\n%s", f, msg)
		}
	}
	if len(ve.Problems) != len(wantFragments) {
		t.Errorf("got %d problems, want %d:\n%s", len(ve.Problems), len(wantFragments), msg)
	}
}

func TestValidateEmpty(t *testing.T) {
	t.Parallel()
	_, err := Parse([]byte(""))
	if err == nil || !strings.Contains(err.Error(), "at least one pool") || !strings.Contains(err.Error(), "at least one agent") {
		t.Fatalf("got %v", err)
	}
}

func TestWeightedNeedsWeights(t *testing.T) {
	t.Parallel()
	raw := strings.Replace(validTOML, "weights  = { opus = 60, gpt = 40 }", "", 1)
	if _, err := Parse([]byte(raw)); err == nil || !strings.Contains(err.Error(), "weights: required") {
		t.Fatalf("got %v", err)
	}
}

func TestLoad(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := Load(filepath.Join(dir, "missing.toml")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing file: got %v, want ErrNotFound", err)
	}
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(validTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := os.WriteFile(path, []byte("[pools.x]\nprovider = \"acme\""), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("error should mention the file: %v", err)
	}
}

func TestDefaultRoundTrips(t *testing.T) {
	t.Parallel()
	cases := map[string][]string{
		"all":         {"claude", "codex", "agy"},
		"claude only": {"claude"},
		"codex only":  {"codex"},
		"agy only":    {"agy"},
		"with junk":   {"agy", "cursor"},
	}
	for name, runtimes := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg := Default(runtimes, map[string]string{"claude": "max"})
			raw, err := cfg.Marshal()
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			if strings.Contains(string(raw), "[pools]\n") {
				t.Errorf("redundant parent table header in output:\n%s", raw)
			}
			back, err := Parse(raw)
			if err != nil {
				t.Fatalf("generated config does not validate: %v\n%s", err, raw)
			}
			for _, phase := range Phases {
				if len(back.Routing[phase].Agents) == 0 {
					t.Errorf("phase %s has no agents:\n%s", phase, raw)
				}
			}
		})
	}
}

func TestDefaultContent(t *testing.T) {
	t.Parallel()
	cfg := Default([]string{"claude", "codex"}, map[string]string{"claude": "max"})
	if got := cfg.Pools["claude"].PlanTier; got != "max" {
		t.Errorf("plan tier override: got %q", got)
	}
	if got := cfg.Pools["chatgpt"].PlanTier; got != "plus" {
		t.Errorf("default plan tier: got %q", got)
	}
	if _, ok := cfg.Agents["gemini"]; ok {
		t.Error("agy agent generated although agy is not installed")
	}
	if got := strings.Join(cfg.Routing["review"].Agents, ","); got != "gpt,opus" {
		t.Errorf("review order = %s", got)
	}
	if got := strings.Join(cfg.Routing["helper"].Agents, ","); got != "haiku,gpt" {
		t.Errorf("helper order = %s", got)
	}
}

func TestPaths(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(HomeEnv, dir)
	p, err := ResolvePaths()
	if err != nil {
		t.Fatalf("ResolvePaths: %v", err)
	}
	if p.Home != dir || p.Config != filepath.Join(dir, "config.toml") || p.State != filepath.Join(dir, "state.db") {
		t.Fatalf("unexpected paths: %+v", p)
	}
	if _, err := PathsIn(""); err == nil {
		t.Fatal("empty home should fail")
	}
}

func TestQuotaSwitches(t *testing.T) {
	t.Parallel()
	base := "[pools.p]\nprovider = \"anthropic\"\nbilling = \"subscription\"\n" +
		"[agents.a]\nruntime = \"claude\"\npool = \"p\"\ntier = \"frontier\"\n"
	cfg, err := Parse([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Quota.ProbesEnabled("anthropic") {
		t.Error("probes are on by default (§10.2)")
	}
	cfg, err = Parse([]byte(base + "[quota]\nprobes = false\n[quota.providers]\nanthropic = true\n"))
	if err != nil || cfg.Quota.ProbesEnabled("anthropic") {
		t.Errorf("global off must win: %v", err)
	}
	cfg, err = Parse([]byte(base + "[quota.providers]\nopenai = false\n"))
	if err != nil || cfg.Quota.ProbesEnabled("openai") || !cfg.Quota.ProbesEnabled("anthropic") {
		t.Errorf("per provider: %v", err)
	}
	if _, err := Parse([]byte(base + "[quota.providers]\nmistral = false\n")); err == nil {
		t.Error("unknown provider accepted")
	}
}

func TestBudgets(t *testing.T) {
	t.Parallel()
	agent := "[agents.a]\nruntime = \"claude\"\npool = \"p\"\ntier = \"frontier\"\n"
	ok := "[pools.p]\nprovider = \"anthropic\"\nbilling = \"api\"\napi_key_env = \"ANTHROPIC_API_KEY\"\ndaily_budget_usd = 5\nmonthly_budget_usd = 50\n" + agent
	cfg, err := Parse([]byte(ok))
	if err != nil || cfg.Pools["p"].DailyBudgetUSD != 5 || cfg.Pools["p"].MonthlyBudgetUSD != 50 {
		t.Fatalf("api budgets: %v", err)
	}
	for _, bad := range []string{
		"[pools.p]\nprovider = \"anthropic\"\nbilling = \"subscription\"\ndaily_budget_usd = 5\n" + agent,
		"[pools.p]\nprovider = \"anthropic\"\nbilling = \"api\"\napi_key_env = \"K\"\nmonthly_budget_usd = -1\n" + agent,
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("accepted:\n%s", bad)
		}
	}
}
