package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/config"
	"github.com/ugurboz/openchef/internal/engine"
	"github.com/ugurboz/openchef/internal/quota"
	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/task"
)

var rnow = time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)

// fakeQuota serves fixed pool states; estimates per pool.
type fakeQuota struct {
	states    map[string]quota.State
	estimates map[string]float64
}

func (f *fakeQuota) State(pool string) quota.State { return f.states[pool] }
func (f *fakeQuota) Health(pool string) (quota.Health, time.Time) {
	s := f.states[pool]
	return quota.HealthOf(&s, rnow), quota.ResetAt(&s)
}
func (f *fakeQuota) Estimate(pool string) (float64, bool) {
	e, ok := f.estimates[pool]
	return e, ok
}

func used(u float64, reset time.Duration) quota.State {
	return quota.State{Updated: rnow, Windows: []quota.Window{{Name: "five_hour", Utilization: u, ResetsAt: rnow.Add(reset)}}}
}

func routerConfig(strategy string) *config.Config {
	return &config.Config{
		Pools: map[string]config.Pool{
			"claude-max": {Provider: "anthropic", Billing: "subscription", PlanTier: "max", EffortProfile: "auto"},
			"chatgpt":    {Provider: "openai", Billing: "subscription", PlanTier: "plus", EffortProfile: "auto"},
		},
		Agents: map[string]config.Agent{
			"opus": {Runtime: "claude", Pool: "claude-max", Tier: "frontier"},
			"gpt":  {Runtime: "codex", Pool: "chatgpt", Tier: "frontier"},
		},
		Routing: map[string]config.Phase{"implement": {Strategy: strategy, Agents: []string{"opus", "gpt"},
			Weights: map[string]int{"opus": 3, "gpt": 1}}},
	}
}

func route(t *testing.T, r *quotaRouter, imp task.Importance, pin string, exclude ...string) (engine.Agent, error) {
	t.Helper()
	tk := &task.Task{ID: "t", Importance: task.ImportanceNormal, AgentPin: pin}
	return r.Route(context.Background(), tk, &task.Step{Importance: imp}, exclude)
}

func TestRoutingFollowsQuota(t *testing.T) {
	t.Parallel()
	fq := &fakeQuota{states: map[string]quota.State{}, estimates: map[string]float64{}}
	r := newQuotaRouter(routerConfig("auto"), fq, func() time.Time { return rnow })
	pick := func(want string) {
		t.Helper()
		a, err := route(t, r, task.ImportanceNormal, "")
		if err != nil || a.Name != want {
			t.Fatalf("routed to %q, %v; want %s", a.Name, err, want)
		}
	}

	// More headroom wins.
	fq.states["claude-max"], fq.states["chatgpt"] = used(0.80, 4*time.Hour), used(0.30, 4*time.Hour)
	pick("gpt")
	fq.states["claude-max"], fq.states["chatgpt"] = used(0.20, 4*time.Hour), used(0.50, 4*time.Hour)
	pick("opus")
	// An exhausted pool takes nothing.
	fq.states["claude-max"] = used(1, 2*time.Hour)
	pick("gpt")
	// Admission: the step does not fit gpt's headroom with the margin; opus has room.
	fq.states["claude-max"], fq.states["chatgpt"] = used(0.50, 4*time.Hour), used(0.75, 4*time.Hour)
	fq.estimates["chatgpt"], fq.estimates["claude-max"] = 0.25, 0.25
	pick("opus")
	// Red takes small steps only.
	fq.states["claude-max"], fq.states["chatgpt"] = used(0.95, 4*time.Hour), used(0.90, 4*time.Hour)
	fq.estimates["chatgpt"], fq.estimates["claude-max"] = 0.04, 0.10
	pick("gpt")
	// The engine's exclusion (ran out on this step) is respected.
	if a, err := route(t, r, task.ImportanceNormal, "", "chatgpt"); err == nil {
		t.Fatalf("routed to %s although opus cannot take the step and gpt is excluded", a.Name)
	}
	// Nothing fits: wait until the earliest reset.
	fq.states["claude-max"], fq.states["chatgpt"] = used(1, 3*time.Hour), used(1, 2*time.Hour)
	_, err := route(t, r, task.ImportanceNormal, "")
	var na *engine.NoAgentError
	if !errors.As(err, &na) || !na.ResumeAt.Equal(rnow.Add(2*time.Hour)) {
		t.Fatalf("no agent: %v", err)
	}
}

func TestPinnedAgentIsAdmittedNotChosen(t *testing.T) {
	t.Parallel()
	fq := &fakeQuota{states: map[string]quota.State{"claude-max": used(0.9, time.Hour), "chatgpt": used(0.1, time.Hour)}}
	r := newQuotaRouter(routerConfig("auto"), fq, func() time.Time { return rnow })
	if a, err := route(t, r, task.ImportanceNormal, "opus"); err != nil || a.Name != "opus" {
		t.Fatalf("pin: %s, %v", a.Name, err) // a pin overrides the better-scored gpt
	}
	fq.states["claude-max"] = used(1, time.Hour)
	var na *engine.NoAgentError
	if _, err := route(t, r, task.ImportanceNormal, "opus"); !errors.As(err, &na) || !na.ResumeAt.Equal(rnow.Add(time.Hour)) {
		t.Fatalf("exhausted pin: %v", err)
	}
	if _, err := route(t, r, task.ImportanceNormal, "ghost"); !errors.Is(err, engine.ErrNoAgent) {
		t.Fatalf("unknown pin: %v", err)
	}
}

func TestWeightedSharesAndEffort(t *testing.T) {
	t.Parallel()
	fq := &fakeQuota{states: map[string]quota.State{"claude-max": used(0.1, 4*time.Hour), "chatgpt": used(0.1, 4*time.Hour)}}
	r := newQuotaRouter(routerConfig("weighted"), fq, func() time.Time { return rnow })
	counts := map[string]int{}
	for range 40 {
		a, err := route(t, r, task.ImportanceNormal, "")
		if err != nil {
			t.Fatal(err)
		}
		counts[a.Name]++
	}
	if d := counts["opus"] - 30; d < -1 || d > 1 {
		t.Fatalf("weighted 3:1 over 40 steps: %v", counts)
	}

	// Effort: a Plus plan under pressure is frugal; Max with room is not.
	fq.states["chatgpt"] = used(0.7, 4*time.Hour)
	r = newQuotaRouter(routerConfig("ordered"), fq, func() time.Time { return rnow })
	a, err := route(t, r, task.ImportanceHigh, "gpt")
	if err != nil || a.Effort != runtime.EffortLow {
		t.Fatalf("plus, yellow, high importance: %s, %v", a.Effort, err)
	}
	a, err = route(t, r, task.ImportanceHigh, "opus")
	if err != nil || a.Effort != runtime.EffortHigh {
		t.Fatalf("max, green, high importance: %s, %v", a.Effort, err)
	}
}
