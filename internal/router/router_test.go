package router

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/quota"
	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/task"
)

func TestEffortTables(t *testing.T) {
	t.Parallel()
	want := map[Profile][4]runtime.Effort{ // low, normal, high, critical
		ProfileMinimal:  {"low", "low", "low", "low"},
		ProfileEconomy:  {"low", "low", "medium", "medium"},
		ProfileBalanced: {"low", "medium", "high", "high"},
		ProfileQuality:  {"medium", "high", "max", "max"},
	}
	imps := []task.Importance{task.ImportanceLow, task.ImportanceNormal, task.ImportanceHigh, task.ImportanceCritical}
	for p, row := range want {
		for i, imp := range imps {
			// A fixed profile ignores quota entirely.
			got := ResolveEffort(EffortInput{Importance: imp, Profile: p, Health: quota.Red, Surplus: true})
			if got.Level != row[i] {
				t.Errorf("%s/%s = %s; want %s", p, imp, got.Level, row[i])
			}
		}
	}
}

func TestAutoEffort(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   EffortInput
		want runtime.Effort
	}{
		{"pro starts economy", EffortInput{Importance: task.ImportanceNormal, PlanTier: "pro", Health: quota.Green}, "low"},
		{"max starts balanced", EffortInput{Importance: task.ImportanceNormal, PlanTier: "max", Health: quota.Green}, "medium"},
		{"pressure lowers", EffortInput{Importance: task.ImportanceHigh, PlanTier: "max", Health: quota.Yellow}, "medium"},
		{"red lowers", EffortInput{Importance: task.ImportanceHigh, PlanTier: "max", Health: quota.Red}, "medium"},
		{"surplus raises", EffortInput{Importance: task.ImportanceHigh, PlanTier: "max", Health: quota.Green, Surplus: true}, "max"},
		{"surplus needs green", EffortInput{Importance: task.ImportanceHigh, PlanTier: "max", Health: quota.Yellow, Surplus: true}, "medium"},
		{"clamped at low", EffortInput{Importance: task.ImportanceLow, PlanTier: "pro", Health: quota.Red}, "low"},
		// Entry plans never exceed high automatically: critical on Plus with surplus.
		{"entry cap", EffortInput{Importance: task.ImportanceCritical, PlanTier: "plus", Health: quota.Green, Surplus: true}, "high"},
		{"entry critical", EffortInput{Importance: task.ImportanceCritical, PlanTier: "pro", Health: quota.Green}, "medium"},
		{"pin wins", EffortInput{Importance: task.ImportanceLow, PlanTier: "pro", Pin: runtime.EffortMax}, "max"},
		{"unknown health", EffortInput{Importance: task.ImportanceNormal, PlanTier: "max", Health: quota.Unknown}, "medium"},
	}
	for _, c := range cases {
		got := ResolveEffort(c.in)
		if got.Level != c.want || got.Reason == "" {
			t.Errorf("%s: %s (%s); want %s", c.name, got.Level, got.Reason, c.want)
		}
	}
}

func cands() []Candidate {
	return []Candidate{
		{Agent: "opus", Pool: "claude", Tier: Frontier, Health: quota.Green, Headroom: 0.5, Weight: 50},
		{Agent: "gpt", Pool: "chatgpt", Tier: Frontier, Health: quota.Green, Headroom: 0.5, Weight: 30},
		{Agent: "gemini", Pool: "google", Tier: Frontier, Health: quota.Green, Headroom: 0.5, Weight: 20},
		{Agent: "haiku", Pool: "claude", Tier: Fast, Health: quota.Green, Headroom: 0.9, Weight: 0},
	}
}

func TestTierFilterNeverDowngrades(t *testing.T) {
	t.Parallel()
	c := cands()
	d, err := Choose(Request{Strategy: StrategyOrdered, Importance: task.ImportanceLow}, c, nil)
	if err != nil || d.Agent != "haiku" {
		t.Fatalf("low importance = %+v, %v; want the fast agent", d, err)
	}
	for i := range 3 {
		c[i].Health = quota.Exhausted
	}
	if _, err := Choose(Request{Strategy: StrategyOrdered, Importance: task.ImportanceHigh}, c, nil); !errors.Is(err, ErrNoCandidate) {
		t.Fatalf("high importance with frontier exhausted = %v; must not downgrade", err)
	}
	d, err = Choose(Request{Strategy: StrategyOrdered, Importance: task.ImportanceHigh, AllowDowngrade: true}, c, nil)
	if err != nil || d.Agent != "haiku" || !d.Downgraded {
		t.Fatalf("allowed downgrade = %+v, %v", d, err)
	}
}

func TestOrdered(t *testing.T) {
	t.Parallel()
	c := cands()
	c[0].Excluded = true
	d, err := Choose(Request{Strategy: StrategyOrdered, Importance: task.ImportanceCritical}, c, nil)
	if err != nil || d.Agent != "gpt" {
		t.Fatalf("%+v, %v", d, err)
	}
}

// The M4 exit criterion: weighted shares hold within ±1 step at every point.
func TestWeightedSharesWithinOneStep(t *testing.T) {
	t.Parallel()
	c := cands()[:3]
	counters := MapCounters{}
	got := map[string]int{}
	for n := 1; n <= 1000; n++ {
		d, err := Choose(Request{Strategy: StrategyWeighted, Importance: task.ImportanceHigh}, c, counters)
		if err != nil {
			t.Fatal(err)
		}
		got[d.Agent]++
		for _, cand := range c {
			ideal := float64(n) * float64(cand.Weight) / 100
			if dev := math.Abs(float64(got[cand.Agent]) - ideal); dev > 1 {
				t.Fatalf("after %d picks %s has %d, ideal %.1f", n, cand.Agent, got[cand.Agent], ideal)
			}
		}
	}
	if got["opus"] != 500 || got["gpt"] != 300 || got["gemini"] != 200 {
		t.Fatalf("1000 picks: %v", got)
	}
}

// An unavailable agent's share goes to the others in proportion.
func TestWeightedRedistributes(t *testing.T) {
	t.Parallel()
	c := cands()[:3]
	c[1].Excluded = true // gpt unavailable
	counters := MapCounters{}
	got := map[string]int{}
	for range 700 {
		d, err := Choose(Request{Strategy: StrategyWeighted, Importance: task.ImportanceHigh}, c, counters)
		if err != nil {
			t.Fatal(err)
		}
		got[d.Agent]++
	}
	if got["gpt"] != 0 || got["opus"] != 500 || got["gemini"] != 200 {
		t.Fatalf("shares %v; want 5:2", got)
	}
}

// Counters persisted across "restarts" keep the sequence exact.
func TestWeightedCountersPersist(t *testing.T) {
	t.Parallel()
	c := cands()[:3]
	shared := MapCounters{}
	got := map[string]int{}
	for range 10 {
		restored := MapCounters{}
		for k, v := range shared {
			restored[k] = v
		}
		for range 10 {
			d, _ := Choose(Request{Strategy: StrategyWeighted, Importance: task.ImportanceHigh}, c, restored)
			got[d.Agent]++
		}
		shared = restored
	}
	if got["opus"] != 50 || got["gpt"] != 30 || got["gemini"] != 20 {
		t.Fatalf("100 picks over 10 restarts: %v", got)
	}
}

func TestAutoStrategy(t *testing.T) {
	t.Parallel()
	pick := func(c []Candidate) string {
		t.Helper()
		d, err := Choose(Request{Strategy: StrategyAuto, Importance: task.ImportanceHigh}, c, nil)
		if err != nil {
			t.Fatal(err)
		}
		return d.Agent
	}
	c := cands()
	if got := pick(c); got != "opus" {
		t.Errorf("equal scores: %s; ties keep config order", got)
	}
	c[1].Headroom = 0.9
	if got := pick(c); got != "gpt" {
		t.Errorf("more headroom: %s", got)
	}
	c[2].Headroom, c[2].ResetIn = 0.8, time.Hour
	if got := pick(c); got != "gemini" {
		t.Errorf("headroom that resets soon is worth more: %s", got)
	}
	c[2].FailureRate = 1
	if got := pick(c); got != "gpt" {
		t.Errorf("failing agent avoided: %s", got)
	}
	c[1].API = true
	if got := pick(c); got != "opus" {
		t.Errorf("API pools rank below subscriptions: %s", got)
	}
}
