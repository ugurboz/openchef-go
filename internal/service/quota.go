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
)

// QuotaDeps are the quota monitor's collaborators.
type QuotaDeps struct {
	Store *store.DB
	// Probes are the L2 probes the configuration allows (§10.2).
	Probes   []*quota.Prober
	Interval time.Duration
	Now      func() time.Time
	Log      *slog.Logger
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

	pending chan quota.Observation
	cancel  context.CancelFunc
	wg      sync.WaitGroup
	refresh sync.Mutex // one manual refresh at a time
}

// NewQuota builds the monitor. Call Start before use.
func NewQuota(d QuotaDeps) (*Quota, error) {
	if d.Store == nil {
		return nil, errors.New("service: quota needs a store")
	}
	q := &Quota{db: d.Store, probes: d.Probes, interval: d.Interval, now: d.Now, log: d.Log,
		pending: make(chan quota.Observation, observationBuffer)}
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
	select {
	case q.pending <- o:
	default:
		q.log.Warn("quota observation not stored: writer behind", "pool", o.Pool)
	}
}

func (q *Quota) write(ctx context.Context) {
	save := func(o *quota.Observation) {
		// Detached from ctx: what was observed before shutdown is still stored.
		if err := q.db.RecordObservation(context.WithoutCancel(ctx), o); err != nil {
			q.log.Error("store quota observation", "pool", o.Pool, "err", err)
		}
	}
	for {
		select {
		case o := <-q.pending:
			save(&o)
		case <-ctx.Done():
			for {
				select {
				case o := <-q.pending:
					save(&o)
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
