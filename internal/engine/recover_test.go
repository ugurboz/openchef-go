package engine

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/ugurboz/openchef/internal/procs"
	"github.com/ugurboz/openchef/internal/runtime/fake"
	"github.com/ugurboz/openchef/internal/task"
)

// shutdownMidTurn runs a task until its first turn hangs, then stops the engine as a
// daemon shutdown would.
func (h *harness) shutdownMidTurn() task.ID {
	h.t.Helper()
	h.claude.Push(fake.Turn{Hang: true})
	id := h.runTask(h.verify)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		waitFor(h.t, func() bool { return len(h.claude.Turns()) == 1 })
		cancel()
	}()
	if err := h.engine().Run(ctx, id); !errors.Is(err, context.Canceled) {
		h.t.Fatalf("Run = %v; want context.Canceled", err)
	}
	return id
}

// §5.3: after a restart the interrupted turn is retried in the same conversation with a
// recovery note, and the attempt continues.
func TestRecoveryResumesInterruptedTurn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t)
	id := h.shutdownMidTurn()
	firstMessage := h.claude.Turns()[0]

	e := h.engine()
	if _, err := e.KillLeftovers(ctx); err != nil {
		t.Fatal(err)
	}
	h.claude.Push(fake.Turn{Edit: writeDone})
	if err := e.Run(ctx, id); err != nil {
		t.Fatalf("Run after restart: %v", err)
	}
	if tk := h.task(id); tk.Status != task.StatusDone {
		t.Fatalf("status %s (%s)", tk.Status, tk.Reason)
	}
	calls := h.claude.Calls()
	var resumed string
	for i := range calls {
		if calls[i].Kind == fake.CallResume {
			resumed = calls[i].Session
		}
	}
	if resumed != "claude-session-1" {
		t.Fatalf("no resume of the interrupted conversation: %+v", calls)
	}
	turns := h.claude.Turns()
	if len(turns) != 2 || !strings.HasPrefix(turns[1], "The orchestrator restarted during your last turn") ||
		!strings.Contains(turns[1], "continue Step 1") || !strings.HasSuffix(turns[1], firstMessage) {
		t.Fatalf("recovery message %q", turns)
	}
	if a := h.attempts(id); len(a) != 1 || a[0].Status != task.AttemptSucceeded {
		t.Fatalf("attempts %+v; the same attempt continues", a)
	}
	sessions, err := h.db.TaskSessions(ctx, id, false)
	if err != nil || len(sessions) != 2 || sessions[1].ResumedFrom != sessions[0].ID {
		t.Fatalf("sessions %+v, %v", sessions, err)
	}
	old, err := h.db.Turns(ctx, sessions[0].ID)
	if err != nil || old[0].Status != task.TurnInterrupted || old[0].Outcome != "daemon restart" {
		t.Fatalf("interrupted turn %+v, %v", old, err)
	}
	h.noLiveSessions()
}

// Recovery works even without the explicit sweep: Run settles what it finds.
func TestRecoveryWithoutSweep(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id := h.shutdownMidTurn()
	h.claude.Push(fake.Turn{Edit: writeDone})
	if err := h.engine().Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if tk := h.task(id); tk.Status != task.StatusDone {
		t.Fatalf("status %s (%s)", tk.Status, tk.Reason)
	}
}

// A conversation that cannot be resumed is replaced by a fresh attempt.
func TestRecoveryFallsBackToFreshAttempt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id := h.shutdownMidTurn()
	h.claude.FailNextOpen(errors.New("claude: no conversation found"))
	h.claude.Push(fake.Turn{Edit: writeDone})
	if err := h.engine().Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if tk := h.task(id); tk.Status != task.StatusDone {
		t.Fatalf("status %s (%s)", tk.Status, tk.Reason)
	}
	a := h.attempts(id)
	if len(a) != 2 || a[0].Status != task.AttemptInterrupted || !strings.Contains(a[0].Outcome, "no conversation found") ||
		a[1].Status != task.AttemptSucceeded {
		t.Fatalf("attempts %+v", a)
	}
	if turns := h.claude.Turns(); !strings.HasPrefix(turns[len(turns)-1], "Step 1 of 1") {
		t.Errorf("fresh attempt starts with the step prompt: %q", turns[len(turns)-1])
	}
	h.noLiveSessions()
}

// A session that never reported its native id (the daemon died during the handshake)
// cannot be resumed: its attempt ends and a fresh one starts.
func TestRecoveryOfSessionWithoutNativeID(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t)
	id := h.runTask(h.verify)
	if _, err := h.db.TransitionStep(ctx, id, 1, task.StepPending, task.StepRunning, storeChange()); err != nil {
		t.Fatal(err)
	}
	a := task.Attempt{TaskID: id, StepIndex: 1, Agent: "opus", Runtime: "claude", Pool: "claude-max"}
	if err := h.db.StartAttempt(ctx, &a); err != nil {
		t.Fatal(err)
	}
	if err := h.db.RecordSession(ctx, &task.Session{AttemptID: a.ID, Runtime: "claude", PID: 1 << 23}); err != nil {
		t.Fatal(err)
	}
	h.claude.Push(fake.Turn{Edit: writeDone})
	if err := h.engine().Run(ctx, id); err != nil {
		t.Fatal(err)
	}
	got := h.attempts(id)
	if len(got) != 2 || got[0].Status != task.AttemptInterrupted || got[1].Status != task.AttemptSucceeded {
		t.Fatalf("attempts %+v", got)
	}
	for _, c := range h.claude.Calls() {
		if c.Kind == fake.CallResume {
			t.Fatal("resumed a session without a native id")
		}
	}
	h.noLiveSessions()
}

// KillLeftovers kills a recorded process that is still running and closes its session.
func TestKillLeftovers(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t)
	id := h.shutdownMidTurn()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p, err := procs.Start(ctx, procs.Spec{Path: exe, Env: append(os.Environ(), helperEnv+"=sleep")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close(context.Background()) })
	attempts := h.attempts(id)
	leftover := task.Session{AttemptID: attempts[0].ID, Runtime: "claude", PID: p.Identity().PID, StartToken: p.Identity().StartToken}
	// The attempt is still running after the shutdown, so a second live session can be
	// recorded, standing in for a process the old daemon could not stop.
	if err := h.db.RecordSession(ctx, &leftover); err != nil {
		t.Fatal(err)
	}
	killed, err := h.engine().KillLeftovers(ctx)
	if err != nil || killed != 1 {
		t.Fatalf("KillLeftovers = %d, %v; want 1", killed, err)
	}
	<-p.Done()
	if live, err := h.db.LiveSessions(ctx); err != nil || len(live) != 0 {
		t.Fatalf("live sessions %+v, %v", live, err)
	}
}
