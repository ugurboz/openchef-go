// Package quota tracks how much of each pool's subscription windows is used
// (docs/architecture.md §10): observations from the CLIs' own streams (L1), out-of-band
// probes (L2) and estimates (L3) are normalized to utilization in [0,1] with absolute
// reset times, merged per window, and turned into a health level the router uses.
package quota

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ugurboz/openchef/internal/runtime"
)

// Source says where an observation came from (§10.1).
type Source string

// Sources.
const (
	SourceStream   Source = "L1" // the CLI's own stream (rate_limit_event, rateLimits/updated)
	SourceProbe    Source = "L2" // an out-of-band probe
	SourceEstimate Source = "L3" // cost history
)

// Window is one rate-limit window of a pool, normalized.
type Window struct {
	// Name is "five_hour", "weekly" or a provider-specific name.
	Name string `json:"name"`
	// Utilization is in [0,1].
	Utilization float64 `json:"utilization"`
	// ResetsAt is when the window resets; zero if unknown.
	ResetsAt time.Time `json:"resetsAt,omitzero"`
	// Duration is the window's length; zero if unknown.
	Duration time.Duration `json:"duration,omitzero"`
}

// Observation is what one source said about one pool at one time.
type Observation struct {
	Pool    string    `json:"pool"`
	Source  Source    `json:"source"`
	At      time.Time `json:"at"`
	Windows []Window  `json:"windows"`
	// Exhausted means the provider said the limit is reached and cannot be extended.
	Exhausted bool `json:"exhausted,omitempty"`
}

// FromSignal converts an in-stream runtime signal (L1).
func FromSignal(pool string, at time.Time, s *runtime.QuotaSignal) Observation {
	o := Observation{Pool: pool, Source: SourceStream, At: at, Exhausted: s.Exhausted}
	for _, w := range s.Windows {
		o.Windows = append(o.Windows, Window{Name: canonical(w.Name), Utilization: clamp(w.Utilization), ResetsAt: w.ResetsAt, Duration: w.Duration})
	}
	return o
}

// canonical maps provider window names onto OpenChef's.
func canonical(name string) string {
	switch strings.ToLower(name) {
	case "five_hour", "5h", "session", "primary":
		return "five_hour"
	case "seven_day", "weekly", "weekly_all", "secondary", "7d":
		return "weekly"
	default:
		return name
	}
}

func clamp(u float64) float64 {
	switch {
	case u < 0:
		return 0
	case u > 1:
		return 1
	default:
		return u
	}
}

// resetSlack treats reset times this close as the same window: sources round
// differently (epoch seconds vs ISO strings with fractions).
const resetSlack = 2 * time.Minute

// State is the merged view of one pool.
type State struct {
	Pool      string   `json:"pool"`
	Windows   []Window `json:"windows"`
	Exhausted bool     `json:"exhausted,omitempty"`
	// ExhaustedUntil is when an exhaustion report stops applying (the latest reset of
	// the windows known then); zero means until the next observation.
	ExhaustedUntil time.Time `json:"exhaustedUntil,omitzero"`
	// Updated is the time of the newest observation merged in.
	Updated time.Time `json:"updated"`
	// Sources lists the sources merged so far, for display.
	Sources []Source `json:"sources"`
}

// merge folds o into s (§10.1): within one window utilization only increases, so a
// stale lower reading never undoes a newer higher one; a window with a later reset is a
// new window and replaces the old one; the newest observation wins across sources for
// exhaustion.
func (s *State) merge(o *Observation) {
	for _, w := range o.Windows {
		i := s.window(w.Name)
		if i < 0 {
			s.Windows = append(s.Windows, w)
			continue
		}
		cur := &s.Windows[i]
		switch {
		case !w.ResetsAt.IsZero() && !cur.ResetsAt.IsZero() && w.ResetsAt.After(cur.ResetsAt.Add(resetSlack)):
			*cur = w // a new window
		case !w.ResetsAt.IsZero() && !cur.ResetsAt.IsZero() && w.ResetsAt.Before(cur.ResetsAt.Add(-resetSlack)):
			// An observation of a window that has already been replaced.
		default:
			cur.Utilization = max(cur.Utilization, w.Utilization)
			if cur.ResetsAt.IsZero() {
				cur.ResetsAt = w.ResetsAt
			}
			if cur.Duration == 0 {
				cur.Duration = w.Duration
			}
		}
	}
	sort.Slice(s.Windows, func(i, j int) bool { return s.Windows[i].Name < s.Windows[j].Name })
	if !o.At.Before(s.Updated) {
		s.Exhausted = o.Exhausted
		s.ExhaustedUntil = time.Time{}
		if o.Exhausted {
			for _, w := range o.Windows {
				if w.Utilization >= exhaustedAt && w.ResetsAt.After(s.ExhaustedUntil) {
					s.ExhaustedUntil = w.ResetsAt
				}
			}
		}
		s.Updated = o.At
	}
	found := false
	for _, src := range s.Sources {
		found = found || src == o.Source
	}
	if !found {
		s.Sources = append(s.Sources, o.Source)
	}
}

func (s *State) window(name string) int {
	for i := range s.Windows {
		if s.Windows[i].Name == name {
			return i
		}
	}
	return -1
}

// Tracker holds the merged state of every pool. It is safe for concurrent use.
type Tracker struct {
	mu    sync.Mutex
	pools map[string]*State
	now   func() time.Time
}

// NewTracker returns an empty tracker; now may be nil.
func NewTracker(now func() time.Time) *Tracker {
	if now == nil {
		now = time.Now
	}
	return &Tracker{pools: map[string]*State{}, now: now}
}

// Observe merges an observation.
func (t *Tracker) Observe(o Observation) {
	if o.Pool == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.pools[o.Pool]
	if s == nil {
		s = &State{Pool: o.Pool}
		t.pools[o.Pool] = s
	}
	s.merge(&o)
}

// State returns a copy of a pool's state, with windows past their reset dropped.
func (t *Tracker) State(pool string) (State, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.pools[pool]
	if s == nil {
		return State{Pool: pool}, false
	}
	return t.current(s), true
}

// States returns every pool's state, sorted by pool.
func (t *Tracker) States() []State {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]State, 0, len(t.pools))
	for _, s := range t.pools {
		out = append(out, t.current(s))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pool < out[j].Pool })
	return out
}

// current copies s, forgetting windows that have reset: their usage no longer counts.
func (t *Tracker) current(s *State) State {
	now := t.now()
	c := *s
	c.Windows = nil
	for _, w := range s.Windows {
		if !w.ResetsAt.IsZero() && !now.Before(w.ResetsAt) {
			continue
		}
		c.Windows = append(c.Windows, w)
	}
	if c.Exhausted && !c.ExhaustedUntil.IsZero() && !now.Before(c.ExhaustedUntil) {
		c.Exhausted, c.ExhaustedUntil = false, time.Time{}
	}
	c.Sources = append([]Source(nil), s.Sources...)
	return c
}
