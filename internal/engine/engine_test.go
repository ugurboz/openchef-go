package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/gitws"
	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/runtime/fake"
	"github.com/ugurboz/openchef/internal/store"
	"github.com/ugurboz/openchef/internal/task"
)

func TestRunVerifiesAndCommits(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.claude.Push(fake.Turn{Edit: writeDone, Text: "created"})
	id := h.runTask(h.verify)
	if err := h.engine().Run(context.Background(), id); err != nil {
		t.Fatalf("Run: %v", err)
	}

	tk := h.task(id)
	if tk.Status != task.StatusDone {
		t.Fatalf("status %s (%s)", tk.Status, tk.Reason)
	}
	s := h.step(id)
	if s.Status != task.StepVerified || s.VerifiedCommit == "" || s.Retries != 0 {
		t.Fatalf("step %+v", s)
	}
	files := git(t, tk.Worktree, "show", "--name-only", "--format=", s.VerifiedCommit)
	if files != "done.txt" {
		t.Errorf("committed %q", files)
	}
	if msg := git(t, tk.Worktree, "log", "-1", "--format=%B", s.VerifiedCommit); !strings.Contains(msg, "OpenChef-Agent: opus") {
		t.Errorf("message %q", msg)
	}
	if user := git(t, h.repo, "status", "--porcelain"); user != "" {
		t.Errorf("user tree touched: %q", user)
	}
	a := h.attempts(id)
	if len(a) != 1 || a[0].Status != task.AttemptSucceeded || a[0].Agent != "opus" {
		t.Fatalf("attempts %+v", a)
	}
	h.noLiveSessions()

	turns := h.claude.Turns()
	if len(turns) != 1 || !strings.Contains(turns[0], "Step 1 of 1: create done.txt") || !strings.Contains(turns[0], h.verify) {
		t.Errorf("prompt %q", turns)
	}
	open := h.claude.Calls()[0]
	if open.Kind != fake.CallOpen || open.Spec.Dir != tk.Worktree || open.Spec.Model != "claude-opus-5-5" ||
		open.Spec.Effort != runtime.EffortMedium || open.Spec.OnSpawn == nil || !strings.HasPrefix(open.Spec.LogPath, h.cfg.SessionRoot) {
		t.Errorf("session spec %+v", open.Spec)
	}
	want := []EventKind{EventStepStarted, EventAttemptStarted, EventVerifyStarted, EventVerifyFinished,
		EventAttemptEnded, EventStepFinished, EventTaskStatus}
	if got := h.sink.kinds(); !slices.Equal(got, want) {
		t.Errorf("events %v\nwant   %v", got, want)
	}
}

// The session row is recorded from OnSpawn before the first message (§5.1).
func TestSessionRecordedFromSpawn(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.claude.Push(fake.Turn{Edit: writeDone})
	id := h.runTask("")
	if err := h.engine().Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	sessions, err := h.db.TaskSessions(context.Background(), id, false)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("sessions %+v, %v", sessions, err)
	}
	s := sessions[0]
	if s.PID <= 1<<22 || s.NativeID != "claude-session-1" || s.State != task.SessionClosed || s.LogDir == "" {
		t.Fatalf("session %+v", s)
	}
}

func TestStepWithoutVerifyIsAcceptedOnCompletion(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.claude.Push(fake.Completed("nothing to change"))
	id := h.runTask("")
	if err := h.engine().Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if tk := h.task(id); tk.Status != task.StatusDone {
		t.Fatalf("status %s", tk.Status)
	}
	if s := h.step(id); s.VerifiedCommit == "" {
		t.Fatal("empty step not recorded by a commit")
	}
}

func TestVerificationFailureIsFedBack(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.claude.Push(fake.Completed("forgot"), fake.Turn{Edit: writeDone})
	id := h.runTask(h.verify)
	if err := h.engine().Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if tk := h.task(id); tk.Status != task.StatusDone {
		t.Fatalf("status %s (%s)", tk.Status, tk.Reason)
	}
	if s := h.step(id); s.Retries != 1 {
		t.Errorf("retries %d", s.Retries)
	}
	turns := h.claude.Turns()
	if len(turns) != 2 || !strings.Contains(turns[1], "missing done.txt") ||
		!strings.Contains(turns[1], "exit code 1") || !strings.Contains(turns[1], "Step 1 of 1") {
		t.Fatalf("retry message %q", turns)
	}
	if n := len(h.attempts(id)); n != 1 {
		t.Errorf("%d attempts; a retry stays in the same session", n)
	}
}

func TestRetriesExhaustedNeedsAttention(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.claude.Push(fake.Completed("a"), fake.Completed("b"), fake.Completed("c"))
	id := h.runTask(h.verify)
	if err := h.engine().Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	tk := h.task(id)
	if tk.Status != task.StatusNeedsAttention || !strings.Contains(tk.Reason, "after 2 retries") {
		t.Fatalf("task %s: %s", tk.Status, tk.Reason)
	}
	if s := h.step(id); s.Status != task.StepFailed || s.Retries != 2 {
		t.Fatalf("step %+v", s)
	}
	if a := h.attempts(id); a[0].Status != task.AttemptFailed {
		t.Fatalf("attempt %+v", a)
	}
	if n := len(h.claude.Turns()); n != 3 {
		t.Errorf("%d turns; want 1 + 2 retries", n)
	}
	h.noLiveSessions()
}

func TestQuotaFailsOverWithPatch(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	reset := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	h.claude.Push(fake.Turn{Edit: writeFile("partial.txt", "half done\n"),
		Fail: &runtime.Failure{Kind: runtime.FailQuotaExhausted, Message: "limit reached", ResetsAt: reset}})
	h.codex.Push(fake.Turn{Edit: writeDone})
	id := h.runTask(h.verify)
	if err := h.engine().Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	tk := h.task(id)
	if tk.Status != task.StatusDone {
		t.Fatalf("status %s (%s)", tk.Status, tk.Reason)
	}
	a := h.attempts(id)
	if len(a) != 2 || a[0].Status != task.AttemptFailedOver || a[0].Pool != "claude-max" ||
		a[1].Status != task.AttemptSucceeded || a[1].Agent != "gpt" {
		t.Fatalf("attempts %+v", a)
	}
	patch := filepath.Join(tk.Worktree, ".openchef", "patches", fmt.Sprintf("step-1-opus-%d.patch", a[0].ID))
	body, err := os.ReadFile(patch)
	if err != nil || !strings.Contains(string(body), "half done") {
		t.Fatalf("patch %q, %v", body, err)
	}
	s := h.step(id)
	if files := git(t, tk.Worktree, "show", "--name-only", "--format=", s.VerifiedCommit); files != "done.txt" {
		t.Errorf("commit has %q; partial work must be reset, not committed", files)
	}
	handoff := h.codex.Turns()
	if len(handoff) != 1 || !strings.Contains(handoff[0], "taking over this step from another agent (opus)") ||
		!strings.Contains(handoff[0], fmt.Sprintf(".openchef/patches/step-1-opus-%d.patch", a[0].ID)) {
		t.Fatalf("handoff %q", handoff)
	}
	if n := h.sink.notices(); !slices.ContainsFunc(n, func(m string) bool { return strings.Contains(m, "failing over") }) {
		t.Errorf("notices %q", n)
	}
	if !slices.Equal(h.exhausted, []string{"claude-max"}) {
		t.Errorf("PoolExhausted %v", h.exhausted)
	}
	h.noLiveSessions()
}

func TestAllPoolsExhaustedWaitsForEarliestReset(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	early := time.Now().Add(time.Hour).Truncate(time.Second)
	late := early.Add(3 * time.Hour)
	h.claude.Push(fake.Turn{Fail: &runtime.Failure{Kind: runtime.FailQuotaExhausted, ResetsAt: late}})
	h.codex.Push(fake.Turn{Fail: &runtime.Failure{Kind: runtime.FailQuotaExhausted, ResetsAt: early}})
	id := h.runTask(h.verify)
	if err := h.engine().Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	tk := h.task(id)
	if tk.Status != task.StatusWaitingQuota || !tk.ResumeAt.Equal(early) {
		t.Fatalf("task %s resume %v (want %v): %s", tk.Status, tk.ResumeAt, early, tk.Reason)
	}
	if s := h.step(id); s.Status != task.StepPending {
		t.Errorf("step %s; waiting_quota resets running steps", s.Status)
	}
	h.noLiveSessions()
}

func TestQuotaWithoutResetTimeNeedsAttention(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.claude.Push(fake.Failing(runtime.FailQuotaExhausted, "out"))
	h.codex.Push(fake.Failing(runtime.FailQuotaExhausted, "out"))
	id := h.runTask(h.verify)
	if err := h.engine().Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if tk := h.task(id); tk.Status != task.StatusNeedsAttention {
		t.Fatalf("task %s", tk.Status)
	}
}

func TestRateLimitBacksOffInSameSession(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.claude.Push(
		fake.Turn{Fail: &runtime.Failure{Kind: runtime.FailRateLimited, ResetsAt: time.Now().Add(40 * time.Second)}},
		fake.Failing(runtime.FailRateLimited, "slow down"),
		fake.Turn{Edit: writeDone},
	)
	id := h.runTask(h.verify)
	if err := h.engine().Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if tk := h.task(id); tk.Status != task.StatusDone {
		t.Fatalf("status %s", tk.Status)
	}
	if len(h.sleeps) != 2 || h.sleeps[0] < 35*time.Second || h.sleeps[0] > 40*time.Second || h.sleeps[1] != time.Minute {
		t.Errorf("sleeps %v; want ~40s (reset) then 1m (backoff 2)", h.sleeps)
	}
	turns := h.claude.Turns()
	if len(turns) != 3 || turns[0] != turns[1] || turns[1] != turns[2] {
		t.Errorf("rate-limited turns must resend the same message")
	}
	if n := len(h.attempts(id)); n != 1 {
		t.Errorf("%d attempts", n)
	}
}

func TestRateLimitsBeyondBudgetFailOver(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	for range 4 {
		h.claude.Push(fake.Failing(runtime.FailRateLimited, "slow down"))
	}
	h.codex.Push(fake.Turn{Edit: writeDone})
	id := h.runTask(h.verify)
	if err := h.engine().Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if a := h.attempts(id); len(a) != 2 || a[1].Agent != "gpt" {
		t.Fatalf("attempts %+v", a)
	}
}

func TestCrashResumesOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.claude.Push(fake.Failing(runtime.FailCrashed, "process exited"), fake.Turn{Edit: writeDone})
	id := h.runTask(h.verify)
	if err := h.engine().Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if tk := h.task(id); tk.Status != task.StatusDone {
		t.Fatalf("status %s (%s)", tk.Status, tk.Reason)
	}
	calls := h.claude.Calls()
	kinds := make([]fake.CallKind, 0, len(calls))
	var resumeOf string
	for i := range calls {
		kinds = append(kinds, calls[i].Kind)
		if calls[i].Kind == fake.CallResume {
			resumeOf = calls[i].Session
		}
	}
	if resumeOf != "claude-session-1" {
		t.Fatalf("calls %v; want a resume of the first session", kinds)
	}
	if turns := h.claude.Turns(); len(turns) != 2 || !strings.Contains(turns[1], "previous process ended unexpectedly") {
		t.Errorf("turns %q", turns)
	}
	sessions, err := h.db.TaskSessions(context.Background(), id, false)
	if err != nil || len(sessions) != 2 || sessions[1].ResumedFrom != sessions[0].ID || sessions[1].NativeID != sessions[0].NativeID {
		t.Fatalf("sessions %+v, %v", sessions, err)
	}
	h.noLiveSessions()
}

func TestSecondCrashNeedsAttention(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.claude.Push(fake.Failing(runtime.FailCrashed, "boom"), fake.Failing(runtime.FailProtocol, "garbage"))
	id := h.runTask(h.verify)
	if err := h.engine().Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if tk := h.task(id); tk.Status != task.StatusNeedsAttention || !strings.Contains(tk.Reason, "failed twice") {
		t.Fatalf("task %s: %s", tk.Status, tk.Reason)
	}
	h.noLiveSessions()
}

func TestLoopIsInterruptedAndNudged(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	loop := make([]runtime.Event, 5)
	for i := range loop {
		loop[i] = fake.ToolCall("Bash", "Bash:go test ./...")
	}
	h.claude.Push(fake.Turn{Events: loop, Hang: true}, fake.Turn{Edit: writeDone})
	id := h.runTask(h.verify)
	if err := h.engine().Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if tk := h.task(id); tk.Status != task.StatusDone {
		t.Fatalf("status %s (%s)", tk.Status, tk.Reason)
	}
	turns := h.claude.Turns()
	if len(turns) != 2 || !strings.Contains(turns[1], "repeated the same tool call 5 times") {
		t.Fatalf("turns %q", turns)
	}
	if s := h.step(id); s.Retries != 1 {
		t.Errorf("retries %d; a nudge uses the retry budget", s.Retries)
	}
}

func TestDifferentToolCallsAreNotALoop(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	evs := []runtime.Event{
		fake.ToolCall("Bash", "a"), fake.ToolCall("Bash", "a"), fake.ToolCall("Bash", "b"),
		fake.ToolCall("Bash", "a"), fake.ToolCall("Bash", "a"), fake.ToolCall("Bash", "a"),
	}
	h.claude.Push(fake.Turn{Events: evs, Edit: writeDone})
	id := h.runTask(h.verify)
	if err := h.engine().Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if n := len(h.claude.Turns()); n != 1 {
		t.Errorf("%d turns; no loop expected", n)
	}
}

func TestIdleTurnIsInterrupted(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.cfg.Limits.IdleTimeout = 100 * time.Millisecond
	h.claude.Push(fake.Turn{Hang: true}, fake.Turn{Edit: writeDone})
	id := h.runTask(h.verify)
	if err := h.engine().Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	turns := h.claude.Turns()
	if len(turns) != 2 || !strings.Contains(turns[1], "no activity for 100ms") {
		t.Fatalf("turns %q", turns)
	}
}

func TestTurnTimeout(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.cfg.Limits.TurnTimeout = 100 * time.Millisecond
	h.claude.Push(fake.Turn{Hang: true}, fake.Turn{Edit: writeDone})
	id := h.runTask(h.verify)
	if err := h.engine().Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if turns := h.claude.Turns(); len(turns) != 2 || !strings.Contains(turns[1], "took longer than 100ms") {
		t.Fatalf("turns %q", turns)
	}
}

func TestAuthFailureNeedsAttention(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.claude.Push(fake.Failing(runtime.FailAuth, "token expired"))
	id := h.runTask(h.verify)
	if err := h.engine().Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	tk := h.task(id)
	if tk.Status != task.StatusNeedsAttention || !strings.Contains(tk.Reason, "not logged in") || !strings.Contains(tk.Reason, "openchef retry "+string(id)) {
		t.Fatalf("task %s: %s", tk.Status, tk.Reason)
	}
}

func TestOpenFailureNeedsAttention(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.claude.FailNextOpen(errors.New("claude: version 1.0 is too old"))
	id := h.runTask(h.verify)
	if err := h.engine().Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	tk := h.task(id)
	if tk.Status != task.StatusNeedsAttention || !strings.Contains(tk.Reason, "too old") {
		t.Fatalf("task %s: %s", tk.Status, tk.Reason)
	}
	if a := h.attempts(id); len(a) != 1 || a[0].Status != task.AttemptFailed {
		t.Fatalf("attempts %+v", a)
	}
}

func TestPinnedAgent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.codex.Push(fake.Turn{Edit: writeDone})
	id, err := task.NewID()
	if err != nil {
		t.Fatal(err)
	}
	tk := task.Task{ID: id, Kind: task.KindRun, Status: task.StatusRunning, Prompt: "p", RepoRoot: h.repo,
		Mode: task.ModeIsolated, Backend: task.BackendLocal, Importance: task.ImportanceNormal, AgentPin: "gpt", Permissions: task.PermissionsAcceptEdits}
	if err := h.db.CreateTask(context.Background(), &tk, []task.Step{{Index: 1, Title: "t", Prompt: "p",
		Verify: h.verify, Importance: task.ImportanceNormal, Status: task.StepPending}}); err != nil {
		t.Fatal(err)
	}
	if err := h.engine().Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if n := len(h.claude.Calls()); n != 0 {
		t.Errorf("claude used %d times despite the pin", n)
	}
}

// A cancel from the CLI: the store moves the task first, then the daemon cancels ctx.
func TestCancelStopsTheTask(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.claude.Push(fake.Turn{Hang: true})
	id := h.runTask(h.verify)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		waitFor(t, func() bool { return len(h.claude.Turns()) == 1 })
		if _, err := h.db.TransitionTask(context.Background(), id, task.StatusRunning, task.StatusCancelled, store.TaskChange{Reason: "user"}); err != nil {
			t.Errorf("cancel: %v", err)
		}
		cancel()
	}()
	if err := h.engine().Run(ctx, id); err != nil {
		t.Fatalf("Run = %v; a cancelled task is not an error", err)
	}
	if tk := h.task(id); tk.Status != task.StatusCancelled {
		t.Fatalf("status %s", tk.Status)
	}
	if a := h.attempts(id); a[0].Status != task.AttemptCancelled {
		t.Fatalf("attempt %+v", a)
	}
	h.noLiveSessions()
}

// A daemon shutdown leaves the task, step, attempt and turn as they were for recovery,
// and stops the agent.
func TestShutdownLeavesStateForRecovery(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.claude.Push(fake.Turn{Hang: true})
	id := h.runTask(h.verify)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		waitFor(t, func() bool { return len(h.claude.Turns()) == 1 })
		cancel()
	}()
	err := h.engine().Run(ctx, id)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v; want context.Canceled", err)
	}
	if tk := h.task(id); tk.Status != task.StatusRunning {
		t.Fatalf("status %s", tk.Status)
	}
	if s := h.step(id); s.Status != task.StepRunning {
		t.Fatalf("step %s", s.Status)
	}
	running, err := h.db.RunningAttempts(context.Background())
	if err != nil || len(running) != 1 {
		t.Fatalf("running attempts %+v, %v", running, err)
	}
	sessions, err := h.db.TaskSessions(context.Background(), id, false)
	if err != nil || len(sessions) != 1 {
		t.Fatal(err)
	}
	if _, err := h.db.InFlightTurn(context.Background(), sessions[0].ID); err != nil {
		t.Fatalf("in-flight turn for recovery: %v", err)
	}
	h.noLiveSessions()
}

func TestRunRejectsInactiveTask(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	id := h.runTask(h.verify)
	if _, err := h.db.TransitionTask(context.Background(), id, task.StatusRunning, task.StatusCancelled, store.TaskChange{}); err != nil {
		t.Fatal(err)
	}
	if err := h.engine().Run(context.Background(), id); !errors.Is(err, ErrNotActive) {
		t.Fatalf("Run = %v; want ErrNotActive", err)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	bad := h.cfg
	bad.WorktreeRoot = "relative"
	if _, err := New(bad); err == nil {
		t.Error("relative WorktreeRoot accepted")
	}
	bad = h.cfg
	bad.Router = nil
	if _, err := New(bad); err == nil {
		t.Error("nil Router accepted")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Error("condition not met in time")
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A cancel during verification, without the daemon cancelling ctx: the engine's next
// write conflicts and Run returns quietly without committing.
func TestCancelDuringVerification(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.claude.Push(fake.Turn{Edit: writeDone})
	id := h.runTask(h.verify)
	h.cfg.Sink = SinkFunc(func(ev Event) {
		h.sink.Publish(ev)
		if ev.Kind == EventVerifyStarted {
			if _, err := h.db.TransitionTask(context.Background(), id, task.StatusRunning, task.StatusCancelled, store.TaskChange{}); err != nil {
				t.Errorf("cancel: %v", err)
			}
		}
	})
	if err := h.engine().Run(context.Background(), id); err != nil {
		t.Fatalf("Run = %v", err)
	}
	tk := h.task(id)
	if tk.Status != task.StatusCancelled {
		t.Fatalf("status %s", tk.Status)
	}
	if n := git(t, tk.Worktree, "rev-list", "--count", tk.BaseCommit+"..HEAD"); n != "0" {
		t.Errorf("%s commits after cancel", n)
	}
	h.noLiveSessions()
}

// An agent that ignores the interrupt is killed (session closed) and resumed once.
func TestStubbornAgentIsReplaced(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.cfg.Limits.IdleTimeout = 50 * time.Millisecond
	h.cfg.Limits.StopWait = 100 * time.Millisecond
	h.claude.Push(fake.Turn{Hang: true, IgnoreInterrupt: true}, fake.Turn{Edit: writeDone})
	id := h.runTask(h.verify)
	if err := h.engine().Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if tk := h.task(id); tk.Status != task.StatusDone {
		t.Fatalf("status %s (%s)", tk.Status, tk.Reason)
	}
	calls := h.claude.Calls()
	if !slices.ContainsFunc(calls, func(c fake.Call) bool { return c.Kind == fake.CallResume }) {
		t.Fatal("stubborn session was not replaced by a resume")
	}
	h.noLiveSessions()
}

// A crash between the step commit and marking the step verified: the next run adopts
// the commit instead of redoing (and mis-recording) the step.
func TestCommittedStepIsAdopted(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t)
	h.claude.Push(fake.Turn{Hang: true})
	id := h.runTask(h.verify)
	sctx, cancel := context.WithCancel(ctx)
	go func() {
		waitFor(t, func() bool { return len(h.claude.Turns()) == 1 })
		cancel()
	}()
	_ = h.engine().Run(sctx, id)

	// Simulate what the crashed daemon had done: committed the step, then died.
	tk := h.task(id)
	repo, err := gitws.Open(ctx, h.repo)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.CreateWorktree(ctx, tk.Worktree, tk.Branch, tk.BaseCommit)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDone(tk.Worktree); err != nil {
		t.Fatal(err)
	}
	sha, err := wt.CommitStep(ctx, tk.BaseCommit, gitws.StepCommit{Task: string(id), Step: 1, Agent: "opus", Pool: "claude-max"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.engine().Run(ctx, id); err != nil {
		t.Fatal(err)
	}
	if tk := h.task(id); tk.Status != task.StatusDone {
		t.Fatalf("status %s (%s)", tk.Status, tk.Reason)
	}
	if s := h.step(id); s.VerifiedCommit != sha {
		t.Fatalf("verified %s; want adopted %s", s.VerifiedCommit, sha)
	}
	if n := len(h.claude.Turns()); n != 1 {
		t.Errorf("%d turns; the adopted step needs no agent", n)
	}
	if a := h.attempts(id); len(a) != 1 || a[0].Status != task.AttemptSucceeded {
		t.Errorf("attempts %+v; the interrupted attempt is the one that succeeded", a)
	}
	h.noLiveSessions()
}

// The retry budget belongs to the step, not to each attempt.
func TestRetryBudgetSpansFailover(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.claude.Push(fake.Completed("a"), fake.Completed("b"), fake.Failing(runtime.FailQuotaExhausted, "out"))
	h.codex.Push(fake.Completed("c"))
	id := h.runTask(h.verify)
	if err := h.engine().Run(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if tk := h.task(id); tk.Status != task.StatusNeedsAttention {
		t.Fatalf("status %s", tk.Status)
	}
	if n := len(h.codex.Turns()); n != 1 {
		t.Errorf("codex got %d turns; the step had used its retries", n)
	}
}
