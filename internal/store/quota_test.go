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
