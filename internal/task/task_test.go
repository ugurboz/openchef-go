package task

import (
	"errors"
	"testing"
)

func TestNewIDRoundTrip(t *testing.T) {
	t.Parallel()
	seen := map[ID]bool{}
	for range 1000 {
		id, err := NewID()
		if err != nil {
			t.Fatalf("NewID: %v", err)
		}
		if _, err := ParseID(string(id)); err != nil {
			t.Fatalf("ParseID(%q): %v", id, err)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestParseIDRejects(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"", "abc", "0123456789a", "ABCDEFGHJK", "abcdefghil", "abc/efghjk", "../../etc0"} {
		if _, err := ParseID(s); !errors.Is(err, ErrInvalid) {
			t.Errorf("ParseID(%q) = %v; want ErrInvalid", s, err)
		}
	}
}

func TestTaskTransitions(t *testing.T) {
	t.Parallel()
	allowed := []struct{ from, to Status }{
		{StatusPlanning, StatusAwaitingApproval},
		{StatusAwaitingApproval, StatusRunning},
		{StatusRunning, StatusDone},
		{StatusRunning, StatusReviewing},
		{StatusRunning, StatusWaitingQuota},
		{StatusRunning, StatusNeedsAttention},
		{StatusReviewing, StatusRunning},
		{StatusWaitingQuota, StatusRunning},
		{StatusNeedsAttention, StatusRunning},
		{StatusNeedsAttention, StatusCancelled},
	}
	for _, c := range allowed {
		if err := c.from.CheckTransition(c.to); err != nil {
			t.Errorf("%s → %s: %v", c.from, c.to, err)
		}
	}
	rejected := []struct{ from, to Status }{
		{StatusDone, StatusRunning},
		{StatusFailed, StatusRunning},
		{StatusCancelled, StatusRunning},
		{StatusPlanning, StatusRunning},
		{StatusAwaitingApproval, StatusDone},
		{StatusWaitingQuota, StatusDone},
		{StatusRunning, StatusRunning},
		{StatusRunning, "bogus"},
		{"bogus", StatusRunning},
	}
	for _, c := range rejected {
		err := c.from.CheckTransition(c.to)
		var te *TransitionError
		if !errors.Is(err, ErrInvalidTransition) || !errors.As(err, &te) || te.Entity != "task" {
			t.Errorf("%s → %s = %v; want task TransitionError", c.from, c.to, err)
		}
	}
}

func TestTerminal(t *testing.T) {
	t.Parallel()
	for _, s := range []Status{StatusDone, StatusFailed, StatusCancelled} {
		if !s.Terminal() {
			t.Errorf("%s not terminal", s)
		}
	}
	for _, s := range []Status{StatusPlanning, StatusRunning, StatusWaitingQuota, StatusNeedsAttention, "bogus"} {
		if s.Terminal() {
			t.Errorf("%s terminal", s)
		}
	}
	if AttemptRunning.Terminal() || !AttemptFailedOver.Terminal() {
		t.Error("attempt terminal states wrong")
	}
}

func TestEntityTransitions(t *testing.T) {
	t.Parallel()
	checks := []struct {
		name string
		err  error
		ok   bool
	}{
		{"step pending→running", StepPending.CheckTransition(StepRunning), true},
		{"step running→pending", StepRunning.CheckTransition(StepPending), true},
		{"step failed→pending", StepFailed.CheckTransition(StepPending), true},
		{"step verified→running", StepVerified.CheckTransition(StepRunning), false},
		{"step pending→verified", StepPending.CheckTransition(StepVerified), false},
		{"attempt running→failed_over", AttemptRunning.CheckTransition(AttemptFailedOver), true},
		{"attempt succeeded→running", AttemptSucceeded.CheckTransition(AttemptRunning), false},
		{"session starting→open", SessionStarting.CheckTransition(SessionOpen), true},
		{"session starting→closed", SessionStarting.CheckTransition(SessionClosed), true},
		{"session closed→open", SessionClosed.CheckTransition(SessionOpen), false},
		{"turn sent→interrupted", TurnSent.CheckTransition(TurnInterrupted), true},
		{"turn completed→failed", TurnCompleted.CheckTransition(TurnFailed), false},
	}
	for _, c := range checks {
		if (c.err == nil) != c.ok {
			t.Errorf("%s: err = %v; want ok = %v", c.name, c.err, c.ok)
		}
	}
}

func TestKindInitialStatus(t *testing.T) {
	t.Parallel()
	if KindRun.InitialStatus() != StatusRunning || KindPipeline.InitialStatus() != StatusPlanning {
		t.Fatal("initial status wrong")
	}
}

func TestImportanceAtLeast(t *testing.T) {
	t.Parallel()
	if got := ImportanceLow.AtLeast(ImportanceHigh); got != ImportanceHigh {
		t.Errorf("low.AtLeast(high) = %s", got)
	}
	if got := ImportanceCritical.AtLeast(ImportanceNormal); got != ImportanceCritical {
		t.Errorf("critical.AtLeast(normal) = %s", got)
	}
}

func validTask(t *testing.T) Task {
	t.Helper()
	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	return Task{
		ID: id, Kind: KindRun, Status: StatusRunning, Prompt: "fix it", RepoRoot: "/repo",
		Mode: ModeIsolated, Backend: BackendLocal, Importance: ImportanceNormal,
	}
}

func TestTaskValidate(t *testing.T) {
	t.Parallel()
	base := validTask(t)
	if err := base.Validate(); err != nil {
		t.Fatalf("valid task: %v", err)
	}
	mutations := map[string]func(*Task){
		"id":         func(x *Task) { x.ID = "nope" },
		"kind":       func(x *Task) { x.Kind = "x" },
		"status":     func(x *Task) { x.Status = "x" },
		"prompt":     func(x *Task) { x.Prompt = "  " },
		"repo":       func(x *Task) { x.RepoRoot = "" },
		"mode":       func(x *Task) { x.Mode = "" },
		"backend":    func(x *Task) { x.Backend = "" },
		"importance": func(x *Task) { x.Importance = "" },
	}
	for name, mut := range mutations {
		tk := base
		mut(&tk)
		if err := tk.Validate(); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Validate = %v; want ErrInvalid", name, err)
		}
	}
}

func step(i int) Step {
	return Step{Index: i, Title: "t", Prompt: "p", Importance: ImportanceNormal, Status: StepPending}
}

func TestValidatePlan(t *testing.T) {
	t.Parallel()
	if err := ValidatePlan([]Step{step(1), step(2)}); err != nil {
		t.Fatalf("valid plan: %v", err)
	}
	tooMany := make([]Step, MaxSteps+1)
	for i := range tooMany {
		tooMany[i] = step(i + 1)
	}
	bad := map[string][]Step{
		"empty":    nil,
		"too many": tooMany,
		"gap":      {step(1), step(3)},
		"zero":     {step(0)},
		"title":    {{Index: 1, Prompt: "p", Importance: ImportanceLow, Status: StepPending}},
		"retries":  {{Index: 1, Title: "t", Prompt: "p", Importance: ImportanceLow, Status: StepPending, Retries: -1}},
	}
	for name, steps := range bad {
		if err := ValidatePlan(steps); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: ValidatePlan = %v; want ErrInvalid", name, err)
		}
	}
}
