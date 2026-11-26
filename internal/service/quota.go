package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/ugurboz/openchef/internal/quota"
	"github.com/ugurboz/openchef/internal/store"
)

// probesFlag is the stored kill switch (`openchef quota probes off`, §10.2).
const probesFlag = "quota.probes"

// Quota defaults.
const (
	// DefaultProbeInterval spaces scheduled L2 probes; the prober itself enforces 30 s.
	DefaultProbeInterval = 5 * time.Minute
	// quotaHistory is how much history a restarted daemon replays: a weekly window.
	quotaHistory = 8 * 24 * time.Hour
	// observationBuffer bounds observations waiting to be stored.
	observationBuffer = 256
	// costHistory is how many step costs per pool are kept for estimates.
	costHistory = 20
)

// QuotaDeps are the quota monitor's collaborators.
type QuotaDeps struct {
	Store *store.DB
	// Probes are the L2 probes the configuration allows (§10.2).
	Probes   []*quota.Prober
	Interval time.Duration
	Now      func() time.Time
	Log      *slog.Logger
	// Budgets lists every API pool with its USD limits (§10.3); only these pools'
	// spending is recorded (subscriptions report notional costs that bill nothing).
	Budgets map[string]Budget
	// Location decides when a day and a month begin for budgets; nil is local time.
	Location *time.Location
}

// Budget is an API pool's spending limit; zero fields are unlimited.
type Budget struct {
	DailyUSD, MonthlyUSD float64
}

// Quota tracks every pool's quota (§10): it merges L1 observations from agent streams
// and L2 probe results, persists them so a restarted daemon routes by what it knew,
// runs the probes on a schedule, and holds the kill switch.
type Quota struct {
	db       *store.DB
	tracker  *quota.Tracker
	probes   []*quota.Prober
	interval time.Duration
	now      func() time.Time
	log      *slog.Logger

	pending chan record
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	refresh sync.Mutex // one manual refresh at a time

	budgets map[string]Budget
	loc     *time.Location

	mu      sync.Mutex
	costs   map[string][]float64     // pool → recent step costs, oldest first (L3)
	started map[int64]attemptStart   // attempts in flight
	spent   map[string][]store.Spend // API pools' spending this month
}

// record is one write for the background writer: an observation or a step cost.
type record struct {
	obs   *quota.Observation
	cost  *stepCost
	spend *spendRecord
}

type spendRecord struct {
	pool string
	usd  float64
	at   time.Time
}

type stepCost struct {
	pool    string
	attempt int64
	share   float64
	at      time.Time
}

type attemptStart struct {
	pool  string
	state quota.State
}

// NewQuota builds the monitor. Call Start before use.
func NewQuota(d QuotaDeps) (*Quota, error) {
	if d.Store == nil {
		return nil, errors.New("service: quota needs a store")
	}
	q := &Quota{db: d.Store, probes: d.Probes, interval: d.Interval, now: d.Now, log: d.Log,
		pending: make(chan record, observationBuffer), costs: map[string][]float64{}, started: map[int64]attemptStart{},
		budgets: d.Budgets, loc: d.Location, spent: map[string][]store.Spend{}}
	if q.loc == nil {
		q.loc = time.Local
	}
	if q.interval <= 0 {
		q.interval = DefaultProbeInterval
	}
	if q.now == nil {
		q.now = time.Now
	}
	if q.log == nil {
		q.log = slog.New(slog.DiscardHandler)
	}
	q.tracker = quota.NewTracker(q.now)
	return q, nil
}

// Start replays recent observations, applies the stored kill switch and starts the
// writer and the probe schedule.
func (q *Quota) Start(ctx context.Context) error {
	since := q.now().Add(-quotaHistory)
	if err := q.db.PruneObservations(ctx, since); err != nil {
		return fmt.Errorf("service: %w", err)
	}
	past, err := q.db.Observations(ctx, since)
	if err != nil {
		return fmt.Errorf("service: %w", err)
	}
	for i := range past {
		q.tracker.Observe(past[i])
	}
	costs, err := q.db.StepCosts(ctx, costHistory)
	if err != nil {
		return fmt.Errorf("service: %w", err)
	}
	q.mu.Lock()
	q.costs = costs
	q.mu.Unlock()
	spent, err := q.db.SpendSince(ctx, monthStart(q.now().In(q.loc)))
	if err != nil {
		return fmt.Errorf("service: %w", err)
	}
	q.mu.Lock()
	q.spent = spent
	q.mu.Unlock()
	for pool, b := range q.budgets {
		if b.DailyUSD > 0 || b.MonthlyUSD > 0 {
			q.tracker.Observe(q.budgetObservation(pool, q.now()))
		}
	}
	flag, err := q.db.Flag(ctx, probesFlag)
	if err != nil {
		return fmt.Errorf("service: %w", err)
	}
	if flag == "off" {
		for _, p := range q.probes {
			p.SetEnabled(false)
		}
	}
	life, cancel := context.WithCancel(context.WithoutCancel(ctx)) // outlives Start's request
	q.cancel = cancel
	q.wg.Go(func() { q.write(life) })
	if len(q.probes) > 0 {
		q.wg.Go(func() { q.schedule(life) })
	}
	return nil
}

// Stop ends the schedule and stores what is still buffered.
func (q *Quota) Stop() {
	if q.cancel != nil {
		q.cancel()
	}
	q.wg.Wait()
}

// Observe records an observation. It never blocks (engine sinks call it): when the
// writer is behind, the observation still counts for routing but is not stored.
func (q *Quota) Observe(o quota.Observation) {
	q.tracker.Observe(o)
	q.enqueue(record{obs: &o}, o.Pool)
}

func (q *Quota) enqueue(r record, pool string) {
	select {
	case q.pending <- r:
	default:
		q.log.Warn("quota record not stored: writer behind", "pool", pool)
	}
}

// AttemptStarted notes the pool's state when an attempt begins, to measure its cost.
func (q *Quota) AttemptStarted(attempt int64, pool string) {
	s, _ := q.tracker.State(pool)
	q.mu.Lock()
	q.started[attempt] = attemptStart{pool: pool, state: s}
	q.mu.Unlock()
}

// AttemptEnded records the share of its pool's window the attempt used (L3), when the
// observations made during it can tell.
func (q *Quota) AttemptEnded(attempt int64) {
	q.mu.Lock()
	st, ok := q.started[attempt]
	delete(q.started, attempt)
	q.mu.Unlock()
	if !ok {
		return
	}
	now, _ := q.tracker.State(st.pool)
	share, ok := quota.Spent(&st.state, &now)
	if !ok {
		return
	}
	share = min(share, 1)
	q.mu.Lock()
	q.costs[st.pool] = append(q.costs[st.pool], share)
	if c := q.costs[st.pool]; len(c) > costHistory {
		q.costs[st.pool] = c[len(c)-costHistory:]
	}
	q.mu.Unlock()
	q.enqueue(record{cost: &stepCost{pool: st.pool, attempt: attempt, share: share, at: q.now()}}, st.pool)
}

// Spend records money an API pool spent (a turn's reported cost). A pool with a budget
// gets its spending as windows (daily_usd, monthly_usd: utilization = spent/budget,
// reset at the next day or month), so health, admission and routing treat budgets like
// any quota window (§10.3).
func (q *Quota) Spend(pool string, usd float64, at time.Time) {
	b, ok := q.budgets[pool]
	if usd <= 0 || !ok {
		return
	}
	q.enqueue(record{spend: &spendRecord{pool: pool, usd: usd, at: at}}, pool)
	if b.DailyUSD <= 0 && b.MonthlyUSD <= 0 {
		return // unlimited: recorded, nothing to route by
	}
	q.mu.Lock()
	q.spent[pool] = append(q.spent[pool], store.Spend{USD: usd, At: at})
	q.mu.Unlock()
	q.tracker.Observe(q.budgetObservation(pool, at))
}

// budgetObservation turns a budget pool's spending into windows.
func (q *Quota) budgetObservation(pool string, at time.Time) quota.Observation {
	b := q.budgets[pool]
	local := at.In(q.loc)
	day, month := dayStart(local), monthStart(local)
	var today, thisMonth float64
	q.mu.Lock()
	for _, sp := range q.spent[pool] {
		if !sp.At.Before(month) {
			thisMonth += sp.USD
		}
		if !sp.At.Before(day) {
			today += sp.USD
		}
	}
	q.mu.Unlock()
	o := quota.Observation{Pool: pool, Source: quota.SourceEstimate, At: at}
	if b.DailyUSD > 0 {
		o.Windows = append(o.Windows, quota.Window{Name: "daily_usd", Utilization: min(today/b.DailyUSD, 1),
			ResetsAt: day.AddDate(0, 0, 1), Duration: 24 * time.Hour})
	}
	if b.MonthlyUSD > 0 {
		o.Windows = append(o.Windows, quota.Window{Name: "monthly_usd", Utilization: min(thisMonth/b.MonthlyUSD, 1),
			ResetsAt: month.AddDate(0, 1, 0)})
	}
	return o
}

func dayStart(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

func monthStart(t time.Time) time.Time {
	y, m, _ := t.Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, t.Location())
}

// Estimate is the expected cost of the next step on pool (§10.3).
func (q *Quota) Estimate(pool string) (float64, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return quota.Estimate(q.costs[pool])
}

// State is a pool's merged quota state.
func (q *Quota) State(pool string) quota.State {
	s, _ := q.tracker.State(pool)
	return s
}

// Exhausted records that a pool ran out (a quota failure on a turn) until until, or
// for an hour when the provider gave no reset.
func (q *Quota) Exhausted(pool string, until time.Time) {
	now := q.now()
	if !until.After(now) {
		until = now.Add(time.Hour)
	}
	q.Observe(quota.Observation{Pool: pool, Source: quota.SourceStream, At: now, Exhausted: true,
		Windows: []quota.Window{{Name: "reported", Utilization: 1, ResetsAt: until}}})
}

func (q *Quota) write(ctx context.Context) {
	save := func(r *record) {
		// Detached from ctx: what was observed before shutdown is still stored.
		wctx := context.WithoutCancel(ctx)
		switch {
		case r.obs != nil:
			if err := q.db.RecordObservation(wctx, r.obs); err != nil {
				q.log.Error("store quota observation", "pool", r.obs.Pool, "err", err)
			}
		case r.spend != nil:
			if err := q.db.RecordSpend(wctx, r.spend.pool, r.spend.usd, r.spend.at); err != nil {
				q.log.Error("store spend", "pool", r.spend.pool, "err", err)
			}
		case r.cost != nil:
			c := r.cost
			if err := q.db.RecordStepCost(wctx, c.pool, c.attempt, c.share, c.at); err != nil {
				q.log.Error("store step cost", "pool", c.pool, "err", err)
			}
		}
	}
	for {
		select {
		case r := <-q.pending:
			save(&r)
		case <-ctx.Done():
			for {
				select {
				case r := <-q.pending:
					save(&r)
				default:
					return
				}
			}
		}
	}
}

func (q *Quota) schedule(ctx context.Context) {
	q.probeAll(ctx, true)
	t := time.NewTicker(q.interval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			q.probeAll(ctx, true)
		case <-ctx.Done():
			return
		}
	}
}

// probeAll runs every probe once and returns each one's error (nil on success). On the
// schedule (skipFresh), a pool observed within the interval is skipped: the agents'
// own streams are preferred over undocumented endpoints (§10.2).
func (q *Quota) probeAll(ctx context.Context, skipFresh bool) map[string]error {
	out := make(map[string]error, len(q.probes))
	for _, p := range q.probes {
		if s, ok := q.tracker.State(p.Pool); skipFresh && ok && q.now().Sub(s.Updated) < q.interval {
			continue
		}
		o, err := p.Run(ctx)
		out[p.Pool] = err
		switch {
		case err == nil:
			q.Observe(o)
		case errors.Is(err, quota.ErrDisabled), errors.Is(err, quota.ErrMuted):
		case errors.Is(err, quota.ErrShapeChanged):
			q.log.Error("quota probe disabled: the response changed shape", "probe", p.Name(), "pool", p.Pool, "err", err)
		default:
			q.log.Warn("quota probe failed", "probe", p.Name(), "pool", p.Pool, "err", err)
		}
	}
	return out
}

// PoolQuota is one pool's quota as people read it.
type PoolQuota struct {
	State   quota.State
	Health  quota.Health
	ResetAt time.Time
}

// ProbeStatus is one probe's state ("" when it can run).
type ProbeStatus struct {
	Name, Pool string
	Problem    string
}

// QuotaReport is what `openchef quota` shows.
type QuotaReport struct {
	Pools  []PoolQuota
	Probes []ProbeStatus
}

// Report returns every known pool's quota and every probe's state. With refresh, the
// probes run first (each at most every 30 s).
func (q *Quota) Report(ctx context.Context, refresh bool) QuotaReport {
	var errs map[string]error
	if refresh {
		q.refresh.Lock()
		errs = q.probeAll(ctx, false)
		q.refresh.Unlock()
	}
	now := q.now()
	var r QuotaReport
	for _, s := range q.tracker.States() {
		r.Pools = append(r.Pools, PoolQuota{State: s, Health: quota.HealthOf(&s, now), ResetAt: quota.ResetAt(&s)})
	}
	for _, p := range q.probes {
		ps := ProbeStatus{Name: p.Name(), Pool: p.Pool}
		if err := p.Status(); err != nil {
			ps.Problem = err.Error()
		} else if err := errs[p.Pool]; err != nil {
			ps.Problem = err.Error()
		}
		r.Probes = append(r.Probes, ps)
	}
	return r
}

// Health rates one pool now.
func (q *Quota) Health(pool string) (quota.Health, time.Time) {
	s, _ := q.tracker.State(pool)
	return quota.HealthOf(&s, q.now()), quota.ResetAt(&s)
}

// SetProbes is the kill switch: it persists and applies at once.
func (q *Quota) SetProbes(ctx context.Context, on bool) error {
	v := "off"
	if on {
		v = "on"
	}
	if err := q.db.SetFlag(ctx, probesFlag, v); err != nil {
		return fmt.Errorf("service: %w", err)
	}
	for _, p := range q.probes {
		p.SetEnabled(on)
	}
	return nil
}
