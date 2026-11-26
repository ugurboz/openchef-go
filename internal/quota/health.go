package quota

import "time"

// Health is a pool's routing state (§10.3).
type Health int

// Health levels, best first.
const (
	Green Health = iota
	Yellow
	Red
	Exhausted
	// Unknown means no observation yet; the router treats it like green but marks it.
	Unknown
)

func (h Health) String() string {
	switch h {
	case Green:
		return "green"
	case Yellow:
		return "yellow"
	case Red:
		return "red"
	case Exhausted:
		return "exhausted"
	default:
		return "unknown"
	}
}

// Thresholds of §10.3: green < 60 % ≤ yellow < 85 % ≤ red < 99 % ≤ exhausted.
const (
	yellowAt    = 0.60
	redAt       = 0.85
	exhaustedAt = 0.99
	// StaleAfter makes data this old degrade health by one level.
	StaleAfter = 15 * time.Minute
)

// HealthOf rates a pool state at now: the worse of its windows, one level worse when
// the data is stale, exhausted when the provider said so.
func HealthOf(s *State, now time.Time) Health {
	if s.Exhausted {
		return Exhausted
	}
	if len(s.Windows) == 0 {
		return Unknown
	}
	worst := 0.0
	for _, w := range s.Windows {
		worst = max(worst, w.Utilization)
	}
	var h Health
	switch {
	case worst >= exhaustedAt:
		return Exhausted // a full window is full however old the reading
	case worst >= redAt:
		h = Red
	case worst >= yellowAt:
		h = Yellow
	default:
		h = Green
	}
	if now.Sub(s.Updated) > StaleAfter && h < Red {
		h++
	}
	return h
}

// ResetAt is when an exhausted pool is expected to take work again: the latest reset
// among its full windows (or ExhaustedUntil); zero if unknown.
func ResetAt(s *State) time.Time {
	at := s.ExhaustedUntil
	for _, w := range s.Windows {
		if w.Utilization >= exhaustedAt && w.ResetsAt.After(at) {
			at = w.ResetsAt
		}
	}
	return at
}
