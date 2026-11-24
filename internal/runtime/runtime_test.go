package runtime

import (
	"errors"
	"strings"
	"testing"
)

func TestEffortNearest(t *testing.T) {
	t.Parallel()
	all := []Effort{EffortLow, EffortMedium, EffortHigh, EffortMax}
	cases := []struct {
		name      string
		e         Effort
		supported []Effort
		want      Effort
	}{
		{"exact", EffortHigh, all, EffortHigh},
		{"max falls to high", EffortMax, []Effort{EffortLow, EffortMedium, EffortHigh}, EffortHigh},
		{"medium falls to low", EffortMedium, []Effort{EffortLow, EffortHigh}, EffortLow},
		{"below every level uses lowest", EffortLow, []Effort{EffortMedium, EffortHigh}, EffortMedium},
		{"default stays default", EffortDefault, all, EffortDefault},
		{"no support", EffortHigh, nil, EffortDefault},
		{"unknown supported values ignored", EffortHigh, []Effort{"xhigh", EffortMedium}, EffortMedium},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.e.Nearest(tc.supported); got != tc.want {
				t.Fatalf("Nearest = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEffortValid(t *testing.T) {
	t.Parallel()
	for _, e := range []Effort{EffortDefault, EffortLow, EffortMedium, EffortHigh, EffortMax} {
		if !e.Valid() {
			t.Errorf("%q should be valid", e)
		}
	}
	if Effort("xhigh").Valid() {
		t.Error("xhigh is not part of the normalized scale")
	}
}

func TestParseMode(t *testing.T) {
	t.Parallel()
	for _, m := range Modes {
		got, err := ParseMode(string(m))
		if err != nil || got != m {
			t.Errorf("ParseMode(%q) = %q, %v", m, got, err)
		}
	}
	if got, err := ParseMode("default"); err != nil || got != ModeManual {
		t.Errorf("legacy alias: %q, %v", got, err)
	}
	if _, err := ParseMode("yolo"); err == nil {
		t.Error("unknown mode accepted")
	}
}

func TestTerminalKinds(t *testing.T) {
	t.Parallel()
	for _, k := range []EventKind{EventText, EventToolCall, EventToolResult, EventQuota} {
		if k.Terminal() {
			t.Errorf("%s must not be terminal", k)
		}
	}
	for _, k := range []EventKind{EventTurnCompleted, EventTurnFailed} {
		if !k.Terminal() {
			t.Errorf("%s must be terminal", k)
		}
	}
}

func TestProtocolErrorTruncatesLine(t *testing.T) {
	t.Parallel()
	err := error(&ProtocolError{Runtime: "codex", Reason: "bad", Line: []byte(strings.Repeat("x", 500))})
	if len(err.Error()) > 300 || !strings.Contains(err.Error(), "codex") {
		t.Fatalf("unexpected message: %q", err.Error())
	}
	var pe *ProtocolError
	if !errors.As(err, &pe) {
		t.Fatal("errors.As failed")
	}
}

func TestDecisionHelpers(t *testing.T) {
	t.Parallel()
	if d := Allow(); !d.Allow {
		t.Error("Allow() must allow")
	}
	if d := Deny("no"); d.Allow || d.Message != "no" {
		t.Errorf("Deny() = %+v", d)
	}
}
