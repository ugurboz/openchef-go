// Package router chooses which agent does a step and how hard it thinks
// (docs/architecture.md §7.3–7.4): tiers allowed by importance, effort from importance,
// profile and quota, and the auto, weighted and ordered selection strategies. It is
// pure decision logic; the engine supplies candidates and state.
package router

import (
	"fmt"
	"slices"

	"github.com/ugurboz/openchef/internal/quota"
	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/task"
)

// Tier is an agent's capability class.
type Tier string

// Tiers, strongest first.
const (
	Frontier Tier = "frontier"
	Standard Tier = "standard"
	Fast     Tier = "fast"
)

// AllowedTiers is the importance table of §7.4.
func AllowedTiers(i task.Importance) []Tier {
	switch i {
	case task.ImportanceLow:
		return []Tier{Fast, Standard}
	case task.ImportanceNormal:
		return []Tier{Standard, Frontier}
	default: // high, critical
		return []Tier{Frontier}
	}
}

// Profile is an effort profile (§7.4).
type Profile string

// Profiles.
const (
	ProfileAuto     Profile = "auto"
	ProfileMinimal  Profile = "minimal"
	ProfileEconomy  Profile = "economy"
	ProfileBalanced Profile = "balanced"
	ProfileQuality  Profile = "quality"
)

var levels = []runtime.Effort{runtime.EffortLow, runtime.EffortMedium, runtime.EffortHigh, runtime.EffortMax}

// table is §7.4's effort table: profile × importance.
var table = map[Profile]map[task.Importance]runtime.Effort{
	ProfileMinimal: {
		task.ImportanceLow: runtime.EffortLow, task.ImportanceNormal: runtime.EffortLow,
		task.ImportanceHigh: runtime.EffortLow, task.ImportanceCritical: runtime.EffortLow,
	},
	ProfileEconomy: {
		task.ImportanceLow: runtime.EffortLow, task.ImportanceNormal: runtime.EffortLow,
		task.ImportanceHigh: runtime.EffortMedium, task.ImportanceCritical: runtime.EffortMedium,
	},
	ProfileBalanced: {
		task.ImportanceLow: runtime.EffortLow, task.ImportanceNormal: runtime.EffortMedium,
		task.ImportanceHigh: runtime.EffortHigh, task.ImportanceCritical: runtime.EffortHigh,
	},
	ProfileQuality: {
		task.ImportanceLow: runtime.EffortMedium, task.ImportanceNormal: runtime.EffortHigh,
		task.ImportanceHigh: runtime.EffortMax, task.ImportanceCritical: runtime.EffortMax,
	},
}

// entryPlans start auto at economy; larger plans at balanced (§7.4).
var entryPlans = []string{"free", "pro", "plus"}

// EffortInput is what effort resolution looks at.
type EffortInput struct {
	Importance task.Importance
	// Profile of the pool (or global); empty means auto.
	Profile Profile
	// PlanTier is the pool's plan as recorded by init ("pro", "max", …).
	PlanTier string
	// Health of the pool, for auto's pressure rule.
	Health quota.Health
	// Surplus means the quota model projects that ≥ 40 % of the current window will go
	// unused at its reset (auto's surplus rule).
	Surplus bool
	// Pin, if set (agent, task or --effort), bypasses profile and adaptation.
	Pin runtime.Effort
}

// Effort is a resolved effort with the reason shown in plan approval and status.
type Effort struct {
	Level  runtime.Effort
	Reason string
}

// ResolveEffort applies §7.4's three steps: base from importance, profile, and (auto
// only) quota adaptation, clamped to low…max; on entry plans automatic effort never
// exceeds high.
func ResolveEffort(in EffortInput) Effort {
	if in.Pin != runtime.EffortDefault {
		return Effort{Level: in.Pin, Reason: "pinned"}
	}
	imp := in.Importance
	if !imp.Valid() {
		imp = task.ImportanceNormal
	}
	profile := in.Profile
	if profile == "" {
		profile = ProfileAuto
	}
	if profile != ProfileAuto {
		row, ok := table[profile]
		if !ok {
			row = table[ProfileBalanced]
			profile = ProfileBalanced
		}
		return Effort{Level: row[imp], Reason: fmt.Sprintf("%s importance, %s profile", imp, profile)}
	}

	entry := slices.Contains(entryPlans, in.PlanTier)
	start := ProfileBalanced
	if entry {
		start = ProfileEconomy
	}
	idx := slices.Index(levels, table[start][imp])
	reason := fmt.Sprintf("%s importance, auto (%s start", imp, start)
	switch {
	case in.Health == quota.Yellow || in.Health == quota.Red:
		idx--
		reason += fmt.Sprintf(", -1 for %s quota", in.Health)
	case in.Health == quota.Green && in.Surplus:
		idx++
		reason += ", +1 for unused quota"
	}
	reason += ")"
	idx = max(0, min(idx, len(levels)-1))
	if entry && idx > slices.Index(levels, runtime.EffortHigh) {
		idx = slices.Index(levels, runtime.EffortHigh)
	}
	return Effort{Level: levels[idx], Reason: reason}
}
