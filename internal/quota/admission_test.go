package quota

import (
	"testing"
	"time"
)

func TestSpent(t *testing.T) {
	t.Parallel()
	reset := t0.Add(3 * time.Hour)
	before := State{Updated: t0, Windows: []Window{{Name: "five_hour", Utilization: 0.40, ResetsAt: reset}, {Name: "weekly", Utilization: 0.10}}}
	after := State{Updated: t0.Add(10 * time.Minute), Windows: []Window{{Name: "five_hour", Utilization: 0.46, ResetsAt: reset}, {Name: "weekly", Utilization: 0.11}}}
	if got, ok := Spent(&before, &after); !ok || got < 0.0599 || got > 0.0601 {
		t.Fatalf("spent %v, %v", got, ok)
	}
	if _, ok := Spent(&before, &before); ok {
		t.Error("no newer data must not count as a free step")
	}
	reset2 := after
	reset2.Windows = []Window{{Name: "five_hour", Utilization: 0.02, ResetsAt: reset.Add(5 * time.Hour)}}
	if _, ok := Spent(&before, &reset2); ok {
		t.Error("a window that reset during the step says nothing")
	}
	if got, ok := Spent(&State{}, &after); ok || got != 0 {
		t.Errorf("no prior windows: %v %v", got, ok)
	}
}

func TestEstimate(t *testing.T) {
	t.Parallel()
	if _, known := Estimate([]float64{0.1, 0.2}); known {
		t.Error("two samples are not an estimate")
	}
	got, known := Estimate([]float64{0.01, 0.02, 0.03, 0.04, 0.10})
	if !known || got != 0.04 {
		t.Fatalf("p75 = %v", got)
	}
	// Only the last 20 count: an old expensive streak is forgotten.
	costs := make([]float64, 0, 30)
	for range 10 {
		costs = append(costs, 0.5)
	}
	for range 20 {
		costs = append(costs, 0.01)
	}
	if got, _ := Estimate(costs); got != 0.01 {
		t.Fatalf("windowed estimate %v", got)
	}
}

func TestAdmit(t *testing.T) {
	t.Parallel()
	at := func(u float64) *State {
		return &State{Updated: t0, Windows: []Window{{Name: "five_hour", Utilization: u, ResetsAt: t0.Add(time.Hour)}}}
	}
	cases := []struct {
		name     string
		h        Health
		s        *State
		estimate float64
		known    bool
		want     bool
	}{
		{"exhausted", Exhausted, at(1), 0.01, true, false},
		{"unknown estimate", Yellow, at(0.7), 0, false, true},
		{"fits with margin", Yellow, at(0.7), 0.2, true, true},  // 0.26 ≤ 0.30
		{"margin too thin", Yellow, at(0.7), 0.25, true, false}, // 0.325 > 0.30
		{"red small step", Red, at(0.9), 0.04, true, true},
		{"red big step", Red, at(0.9), 0.06, true, false},
		{"no windows", Unknown, &State{}, 0.5, true, true},
	}
	for _, c := range cases {
		if got, why := Admit(c.h, c.s, c.estimate, c.known); got != c.want {
			t.Errorf("%s: %v (%s)", c.name, got, why)
		}
	}
	if h, in := Headroom(at(0.7), t0); h < 0.2999 || h > 0.3001 || in != time.Hour {
		t.Errorf("headroom %v in %v", h, in)
	}
}
