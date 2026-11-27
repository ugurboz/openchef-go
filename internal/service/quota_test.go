package service

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/quota"
	"github.com/ugurboz/openchef/internal/store"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// claudeProbe answers every request with body, counting calls.
func claudeProbe(pool, body string, calls *atomic.Int64, now func() time.Time) *quota.Prober {
	client := &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}
	creds := func(context.Context) (string, string, error) { return "token", "acct", nil }
	return quota.ClaudeProbe(pool, creds, client, now)
}

const usageBody = `{"five_hour": {"utilization": 70.0, "resets_at": "2026-11-26T14:00:00+00:00"},
 "seven_day": {"utilization": 10.0, "resets_at": "2026-11-30T09:00:00+00:00"}}`

func openQuotaStore(t *testing.T, dir string) *store.DB {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestQuotaObservePersistAndRestore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	db := openQuotaStore(t, t.TempDir())
	q, err := NewQuota(QuotaDeps{Store: db, Now: clock})
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Start(ctx); err != nil {
		t.Fatal(err)
	}
	q.Observe(quota.Observation{Pool: "claude-max", Source: quota.SourceStream, At: now,
		Windows: []quota.Window{{Name: "five_hour", Utilization: 0.9, ResetsAt: now.Add(time.Hour)}}})
	if h, _ := q.Health("claude-max"); h != quota.Red {
		t.Fatalf("health %s", h)
	}
	q.Stop() // flushes the writer

	again, _ := NewQuota(QuotaDeps{Store: db, Now: clock})
	if err := again.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer again.Stop()
	r := again.Report(ctx, false)
	if len(r.Pools) != 1 || r.Pools[0].Health != quota.Red || r.Pools[0].State.Windows[0].Utilization != 0.9 {
		t.Fatalf("restored %+v", r.Pools)
	}
}

func TestQuotaProbesAndKillSwitch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	dir := t.TempDir()
	db := openQuotaStore(t, dir)
	var calls atomic.Int64
	start := func() *Quota {
		//nolint:contextcheck // probes read credentials under their own timeout
		q, err := NewQuota(QuotaDeps{Store: db, Now: clock, Interval: time.Hour,
			Probes: []*quota.Prober{claudeProbe("claude-max", usageBody, &calls, clock)}})
		if err != nil {
			t.Fatal(err)
		}
		if err := q.Start(ctx); err != nil {
			t.Fatal(err)
		}
		return q
	}
	q := start()
	// The schedule probes once at start.
	waitFor(t, func() bool { h, _ := q.Health("claude-max"); return h == quota.Yellow })
	if err := q.SetProbes(ctx, false); err != nil {
		t.Fatal(err)
	}
	before := calls.Load()
	r := q.Report(ctx, true)
	if calls.Load() != before || len(r.Probes) != 1 || !strings.Contains(r.Probes[0].Problem, "disabled") {
		t.Fatalf("probes off but ran: %d→%d, %+v", before, calls.Load(), r.Probes)
	}
	q.Stop()

	// The switch outlives the daemon.
	q = start()
	defer q.Stop()
	if r := q.Report(ctx, true); calls.Load() != before || r.Probes[0].Problem == "" {
		t.Fatalf("switch lost on restart: %+v", r.Probes)
	}
	if err := q.SetProbes(ctx, true); err != nil {
		t.Fatal(err)
	}
	if r := q.Report(ctx, true); calls.Load() != before+1 || r.Probes[0].Problem != "" {
		t.Fatalf("probes back on: %d, %+v", calls.Load(), r.Probes)
	}
}

func TestQuotaShapeChangeDisablesProbe(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := func() time.Time { return time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC) }
	var calls atomic.Int64
	q, _ := NewQuota(QuotaDeps{Store: openQuotaStore(t, t.TempDir()), Now: clock, Interval: time.Hour,
		Probes: []*quota.Prober{claudeProbe("claude-max", `{"five_hour": {"used": 3}}`, &calls, clock)}})
	if err := q.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer q.Stop()
	waitFor(t, func() bool { return calls.Load() == 1 })
	var r QuotaReport
	waitFor(t, func() bool { r = q.Report(ctx, false); return r.Probes[0].Problem != "" })
	if !strings.Contains(r.Probes[0].Problem, "changed shape") {
		t.Fatalf("problem %q", r.Probes[0].Problem)
	}
}

// A pool the agents' streams reported on recently is not probed on the schedule.
func TestQuotaScheduleSkipsFreshPools(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	db := openQuotaStore(t, t.TempDir())
	if err := db.RecordObservation(ctx, &quota.Observation{Pool: "claude-max", Source: quota.SourceStream, At: now.Add(-time.Minute),
		Windows: []quota.Window{{Name: "five_hour", Utilization: 0.2}}}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int64
	q, _ := NewQuota(QuotaDeps{Store: db, Now: clock, Interval: time.Hour,
		Probes: []*quota.Prober{claudeProbe("claude-max", usageBody, &calls, clock)}})
	if err := q.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer q.Stop()
	time.Sleep(50 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatal("probed a pool with fresh stream data")
	}
	q.Report(ctx, true) // a manual refresh always probes
	if calls.Load() != 1 {
		t.Fatalf("refresh calls %d", calls.Load())
	}
}

// Each attempt's cost is the window share it used; three make an estimate, and the
// history survives a restart.
func TestQuotaStepCosts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	tick := func() { mu.Lock(); now = now.Add(time.Minute); mu.Unlock() }
	db := openQuotaStore(t, t.TempDir())
	q, _ := NewQuota(QuotaDeps{Store: db, Now: clock})
	if err := q.Start(ctx); err != nil {
		t.Fatal(err)
	}
	reset := now.Add(4 * time.Hour)
	util := 0.10
	observe := func() {
		q.Observe(quota.Observation{Pool: "p", Source: quota.SourceStream, At: clock(),
			Windows: []quota.Window{{Name: "five_hour", Utilization: util, ResetsAt: reset}}})
	}
	observe()
	for i, spend := range []float64{0.02, 0.04, 0.03} {
		tick()
		q.AttemptStarted(int64(i+1), "p")
		util += spend
		tick()
		observe()
		q.AttemptEnded(int64(i + 1))
	}
	q.AttemptStarted(9, "p") // nothing observed during it: no cost recorded
	q.AttemptEnded(9)
	est, known := q.Estimate("p")
	if !known || est < 0.0399 || est > 0.0401 {
		t.Fatalf("estimate %v %v", est, known)
	}
	q.Stop()
	again, _ := NewQuota(QuotaDeps{Store: db, Now: clock})
	if err := again.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer again.Stop()
	if est2, known := again.Estimate("p"); !known || est2 != est {
		t.Fatalf("after restart %v %v", est2, known)
	}
	again.Exhausted("p", time.Time{})
	if h, at := again.Health("p"); h != quota.Exhausted || !at.Equal(clock().Add(time.Hour)) {
		t.Fatalf("exhausted: %s until %v", h, at)
	}
}

// An API pool's spending is a quota window: past its daily budget it is exhausted
// until midnight, and the spending survives a restart. Subscriptions record nothing.
func TestQuotaBudgets(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	loc := time.FixedZone("TRT", 3*3600)
	now := time.Date(2026, 11, 26, 20, 0, 0, 0, loc)
	clock := func() time.Time { return now }
	db := openQuotaStore(t, t.TempDir())
	deps := QuotaDeps{Store: db, Now: clock, Location: loc,
		Budgets: map[string]Budget{"api": {DailyUSD: 2, MonthlyUSD: 100}, "free-api": {}}}
	q, _ := NewQuota(deps)
	if err := q.Start(ctx); err != nil {
		t.Fatal(err)
	}
	q.Spend("api", 1.2, now)
	if h, _ := q.Health("api"); h != quota.Yellow {
		t.Fatalf("60%% of the day's budget: %s", h)
	}
	q.Spend("api", 0.9, now)
	h, until := q.Health("api")
	if h != quota.Exhausted || !until.Equal(time.Date(2026, 11, 27, 0, 0, 0, 0, loc)) {
		t.Fatalf("over budget: %s until %v", h, until)
	}
	q.Spend("claude-max", 5, now) // a subscription's notional cost
	q.Spend("free-api", 3, now)   // unlimited: recorded only
	if h, _ := q.Health("free-api"); h != quota.Unknown {
		t.Fatalf("unlimited pool health %s", h)
	}
	q.Stop()
	spent, err := db.SpendSince(ctx, time.Time{})
	if err != nil || len(spent["api"]) != 2 || len(spent["free-api"]) != 1 || len(spent["claude-max"]) != 0 {
		t.Fatalf("stored %v, %v", spent, err)
	}
	again, _ := NewQuota(deps)
	if err := again.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer again.Stop()
	if h, _ := again.Health("api"); h != quota.Exhausted {
		t.Fatalf("after restart: %s", h)
	}
}
