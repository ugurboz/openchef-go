package store

import (
	"context"
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/quota"
)

func TestObservationsRoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openDomain(t)
	t0 := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	reset := t0.Add(3 * time.Hour)
	for i, o := range []quota.Observation{
		{Pool: "claude-max", Source: quota.SourceStream, At: t0, Windows: []quota.Window{{Name: "five_hour", Utilization: 0.4, ResetsAt: reset, Duration: 5 * time.Hour}}},
		{Pool: "claude-max", Source: quota.SourceProbe, At: t0.Add(time.Minute), Exhausted: true},
		{Pool: "chatgpt", Source: quota.SourceProbe, At: t0.Add(2 * time.Hour)},
	} {
		if err := db.RecordObservation(ctx, &o); err != nil {
			t.Fatalf("%d: %v", i, err)
		}
	}
	got, err := db.Observations(ctx, t0.Add(30*time.Second))
	if err != nil || len(got) != 2 || !got[0].Exhausted || len(got[0].Windows) != 0 {
		t.Fatalf("since: %+v, %v", got, err)
	}
	all, err := db.Observations(ctx, time.Time{})
	if err != nil || len(all) != 3 || all[0].Windows[0].ResetsAt != reset || all[0].Windows[0].Duration != 5*time.Hour {
		t.Fatalf("all: %+v, %v", all, err)
	}
	if err := db.PruneObservations(ctx, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if left, _ := db.Observations(ctx, time.Time{}); len(left) != 1 || left[0].Pool != "chatgpt" {
		t.Fatalf("after prune: %+v", left)
	}
	if err := db.RecordObservation(ctx, &quota.Observation{Pool: "p", Source: "L9", At: t0}); err == nil {
		t.Fatal("unknown source stored")
	}
}

func TestFlags(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openDomain(t)
	if v, err := db.Flag(ctx, "quota.probes"); err != nil || v != "" {
		t.Fatalf("unset: %q, %v", v, err)
	}
	for _, v := range []string{"off", "on"} {
		if err := db.SetFlag(ctx, "quota.probes", v); err != nil {
			t.Fatal(err)
		}
		if got, err := db.Flag(ctx, "quota.probes"); err != nil || got != v {
			t.Fatalf("flag %q, %v; want %q", got, err, v)
		}
	}
}

func TestStepCosts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openDomain(t)
	at := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	for i, c := range []struct {
		pool  string
		share float64
	}{{"a", 0.01}, {"a", 0.02}, {"b", 0.5}, {"a", 0.03}} {
		if err := db.RecordStepCost(ctx, c.pool, 0, c.share, at.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.StepCosts(ctx, 2)
	if err != nil || len(got["a"]) != 2 || got["a"][0] != 0.02 || got["a"][1] != 0.03 || len(got["b"]) != 1 {
		t.Fatalf("costs %v, %v", got, err)
	}
	if err := db.RecordStepCost(ctx, "a", 0, 1.5, at); err == nil {
		t.Fatal("share above 1 stored")
	}
}

func TestSpend(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openDomain(t)
	at := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	for i, usd := range []float64{0.5, 1.25, 2} {
		if err := db.RecordSpend(ctx, "api", usd, at.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := db.SpendSince(ctx, at.Add(30*time.Minute))
	if err != nil || len(got["api"]) != 2 || got["api"][0].USD != 1.25 || !got["api"][1].At.Equal(at.Add(2*time.Hour)) {
		t.Fatalf("spend %v, %v", got, err)
	}
	if err := db.RecordSpend(ctx, "api", -1, at); err == nil {
		t.Fatal("negative spend stored")
	}
}
