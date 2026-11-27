package app

import (
	"strconv"
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/config"
	"github.com/ugurboz/openchef/internal/engine"
	"github.com/ugurboz/openchef/internal/quota"
	"github.com/ugurboz/openchef/internal/runtime"
)

func TestProbesFor(t *testing.T) {
	t.Parallel()
	off := false
	cfg := &config.Config{Pools: map[string]config.Pool{
		"claude-max": {Provider: "anthropic", Billing: "subscription"},
		"chatgpt":    {Provider: "openai", Billing: "subscription"},
		"claude-api": {Provider: "anthropic", Billing: "api"},
		"gemini":     {Provider: "google", Billing: "subscription"},
	}}
	names := func(ps []*quota.Prober) []string {
		out := make([]string, 0, len(ps))
		for _, p := range ps {
			out = append(out, p.Name()+":"+p.Pool)
		}
		return out
	}
	if got := names(probesFor(cfg, t.TempDir())); len(got) != 2 || got[0] != "chatgpt:chatgpt" || got[1] != "claude:claude-max" {
		t.Fatalf("probes %v", got) // API pools have no plan usage; agy has no probe
	}
	cfg.Quota.Providers = map[string]bool{"openai": false}
	if got := names(probesFor(cfg, t.TempDir())); len(got) != 1 || got[0] != "claude:claude-max" {
		t.Fatalf("per-provider switch: %v", got)
	}
	cfg.Quota.Probes = &off
	if got := probesFor(cfg, t.TempDir()); len(got) != 0 {
		t.Fatalf("global switch: %v", names(got))
	}
}

type recorder struct {
	got     []quota.Observation
	started map[int64]string
	ended   []int64
	spent   []string
}

func (r *recorder) Observe(o quota.Observation) { r.got = append(r.got, o) }
func (r *recorder) AttemptStarted(a int64, pool string) {
	if r.started == nil {
		r.started = map[int64]string{}
	}
	r.started[a] = pool
}
func (r *recorder) AttemptEnded(a int64) { r.ended = append(r.ended, a) }
func (r *recorder) Spend(pool string, usd float64, _ time.Time) {
	r.spent = append(r.spent, pool+":"+strconv.FormatFloat(usd, 'f', 2, 64))
}

func TestQuotaSinkRecordsStreamSignals(t *testing.T) {
	t.Parallel()
	var passed []engine.Event
	rec := &recorder{}
	s := quotaSink{next: engine.SinkFunc(func(ev engine.Event) { passed = append(passed, ev) }), quota: rec,
		poolOf: func(a string) string { return map[string]string{"opus": "claude-max"}[a] }}
	at := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	signal := &runtime.Event{Kind: runtime.EventQuota, Quota: &runtime.QuotaSignal{Windows: []runtime.QuotaWindow{{Name: "five_hour", Utilization: 0.5}}}}
	s.Publish(engine.Event{Kind: engine.EventAgent, Agent: "opus", At: at, Runtime: signal})
	s.Publish(engine.Event{Kind: engine.EventAgent, Agent: "ghost", At: at, Runtime: signal})
	s.Publish(engine.Event{Kind: engine.EventNotice, Message: "x"})
	s.Publish(engine.Event{Kind: engine.EventAttemptStarted, Attempt: 7, Agent: "opus"})
	s.Publish(engine.Event{Kind: engine.EventAttemptEnded, Attempt: 7, Agent: "opus"})
	if rec.started[7] != "claude-max" || len(rec.ended) != 1 || rec.ended[0] != 7 {
		t.Fatalf("attempt hooks %v %v", rec.started, rec.ended)
	}
	s.Publish(engine.Event{Kind: engine.EventAgent, Agent: "opus", At: at, Runtime: &runtime.Event{Kind: runtime.EventTurnCompleted,
		Result: &runtime.TurnResult{Usage: runtime.Usage{CostUSD: 0.42}}}})
	if len(rec.spent) != 1 || rec.spent[0] != "claude-max:0.42" {
		t.Fatalf("spend %v", rec.spent)
	}
	if len(passed) != 6 {
		t.Fatalf("passed %d events", len(passed))
	}
	if len(rec.got) != 1 || rec.got[0].Pool != "claude-max" || rec.got[0].Source != quota.SourceStream || rec.got[0].Windows[0].Utilization != 0.5 {
		t.Fatalf("observed %+v", rec.got)
	}
}
