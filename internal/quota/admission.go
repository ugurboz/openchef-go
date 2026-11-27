package quota

import (
	"fmt"
	"slices"
	"time"
)

// Admission rules (§10.3).
const (
	// Margin is the safety factor on a step's estimated cost.
	Margin = 1.3
	// SmallStep is the largest estimated cost (share of a window) a red pool accepts.
	SmallStep = 0.05
	// costSamples is how many recent step costs an estimate looks at.
	costSamples = 20
	// minSamples is how many a percentile needs to mean anything.
	minSamples = 3
	// costPercentile is the estimate: most steps cost no more than this.
	costPercentile = 0.75
)

// Spent is the share of a window one step used (L3, §10.1): the largest utilization
// increase, over the windows present in both readings that did not reset in between.
// ok is false when the readings cannot tell (no data, or nothing newer after the step).
func Spent(before, after *State) (share float64, ok bool) {
	if !after.Updated.After(before.Updated) {
		return 0, false
	}
	for _, a := range after.Windows {
		i := before.window(a.Name)
		if i < 0 {
			continue
		}
		b := before.Windows[i]
		if !b.ResetsAt.IsZero() && !a.ResetsAt.IsZero() && a.ResetsAt.After(b.ResetsAt.Add(resetSlack)) {
			continue // the window reset during the step: the difference means nothing
		}
		share = max(share, a.Utilization-b.Utilization)
		ok = true
	}
	return max(share, 0), ok
}

// Estimate is the expected cost of the next step from recent ones (oldest first): the
// 75th percentile of the last 20. known is false below three samples.
func Estimate(costs []float64) (estimate float64, known bool) {
	if len(costs) > costSamples {
		costs = costs[len(costs)-costSamples:]
	}
	if len(costs) < minSamples {
		return 0, false
	}
	sorted := slices.Clone(costs)
	slices.Sort(sorted)
	idx := int(costPercentile*float64(len(sorted)-1) + 0.5)
	return sorted[idx], true
}

// Headroom is the unused share of the pool's tightest window at now and how soon that
// window resets (0 if unknown). A pool without windows has all its headroom.
func Headroom(s *State, now time.Time) (headroom float64, resetIn time.Duration) {
	headroom = 1
	var reset time.Time
	for _, w := range s.Windows {
		if free := 1 - w.Utilization; free < headroom {
			headroom, reset = free, w.ResetsAt
		}
	}
	if reset.After(now) {
		resetIn = reset.Sub(now)
	}
	return max(headroom, 0), resetIn
}

// Admit decides whether a pool takes a step of the estimated cost (§10.3): exhausted
// pools take nothing; red pools only small steps; any pool only what its headroom
// covers with the margin. An unknown estimate is admitted (there is nothing to compare)
// except where exhaustion already rules it out.
func Admit(h Health, s *State, estimate float64, known bool) (ok bool, why string) {
	switch {
	case h == Exhausted:
		return false, "exhausted"
	case !known:
		return true, ""
	case h == Red && estimate > SmallStep:
		return false, fmt.Sprintf("red: takes only small steps (estimate %.0f%% > %.0f%%)", 100*estimate, 100*SmallStep)
	}
	free, _ := Headroom(s, s.Updated)
	if need := estimate * Margin; len(s.Windows) > 0 && need > free {
		return false, fmt.Sprintf("headroom %.0f%% < %.0f%% (estimate with %.0f%% margin)", 100*free, 100*need, 100*(Margin-1))
	}
	return true, ""
}
