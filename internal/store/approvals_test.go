package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"

	"github.com/ugurboz/openchef/internal/task"
)

func queue(t *testing.T, db *DB, tk task.ID, kind task.ApprovalKind) task.Approval {
	t.Helper()
	a := task.Approval{TaskID: tk, Agent: "opus", Kind: kind, Tool: "Bash", Summary: "go test ./..."}
	if kind == task.ApprovalQuestion {
		a.Tool, a.Summary, a.Options = "", "Which port?", []string{"8080", "9090"}
	}
	if err := db.CreateApproval(context.Background(), &a, json.RawMessage(`{"command":"go test ./..."}`)); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestApprovalLifecycle(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openDomain(t)
	tk := createRun(t, db)
	cmd := queue(t, db, tk.ID, task.ApprovalCommand)
	q := queue(t, db, tk.ID, task.ApprovalQuestion)
	if cmd.ID == 0 || cmd.Status != task.ApprovalPending || cmd.ExpiresAt.Sub(cmd.CreatedAt) != task.ApprovalTimeout {
		t.Fatalf("created %+v", cmd)
	}
	pending, err := db.Approvals(ctx, ApprovalFilter{Pending: true})
	if err != nil || len(pending) != 2 || pending[0].ID != cmd.ID || !slices.Equal(pending[1].Options, q.Options) {
		t.Fatalf("pending %+v, %v", pending, err)
	}

	got, err := db.DecideApproval(ctx, cmd.ID, task.Decision{Status: task.ApprovalAllowed, DecidedBy: task.DecidedByUser})
	if err != nil || got.Status != task.ApprovalAllowed || got.DecidedAt.IsZero() {
		t.Fatalf("decide %+v, %v", got, err)
	}
	// A second answer reports the first one instead of overwriting it.
	_, err = db.DecideApproval(ctx, cmd.ID, task.Decision{Status: task.ApprovalDenied, DecidedBy: task.DecidedByUser})
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Got != string(task.ApprovalAllowed) {
		t.Fatalf("second decision: %v", err)
	}
	// Kinds and decisions must match.
	for _, d := range []task.Decision{
		{Status: task.ApprovalAllowed, DecidedBy: task.DecidedByUser},
		{Status: task.ApprovalAnswered, DecidedBy: task.DecidedByUser},
		{Status: task.ApprovalPending, DecidedBy: task.DecidedByUser},
		{Status: task.ApprovalAnswered, Answer: "8080"},
	} {
		if _, err := db.DecideApproval(ctx, q.ID, d); !errors.Is(err, task.ErrInvalid) {
			t.Errorf("%+v on a question: %v", d, err)
		}
	}
	got, err = db.DecideApproval(ctx, q.ID, task.Decision{Status: task.ApprovalAnswered, Answer: "8080", DecidedBy: task.DecidedByUser})
	if err != nil || got.Answer != "8080" {
		t.Fatalf("answer %+v, %v", got, err)
	}
	if _, err := db.DecideApproval(ctx, 999, task.Decision{Status: task.ApprovalDenied, DecidedBy: task.DecidedByUser}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing approval: %v", err)
	}
	all, err := db.Approvals(ctx, ApprovalFilter{Task: tk.ID, Limit: 1})
	if err != nil || len(all) != 1 || all[0].ID != q.ID {
		t.Fatalf("newest one: %+v, %v", all, err)
	}
}

func TestApprovalNeedsActiveTask(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openDomain(t)
	tk := createRun(t, db)
	if _, err := db.TransitionTask(ctx, tk.ID, task.StatusRunning, task.StatusCancelled, TaskChange{Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	a := task.Approval{TaskID: tk.ID, Agent: "opus", Kind: task.ApprovalCommand, Summary: "ls"}
	if err := db.CreateApproval(ctx, &a, nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("approval for a cancelled task: %v", err)
	}
	a.Kind = "launch"
	if err := db.CreateApproval(ctx, &a, nil); !errors.Is(err, task.ErrInvalid) {
		t.Fatalf("bad kind: %v", err)
	}
}

func TestExpirePendingOnRestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openDomain(t)
	tk := createRun(t, db)
	a := queue(t, db, tk.ID, task.ApprovalCommand)
	b := queue(t, db, tk.ID, task.ApprovalCommand)
	if _, err := db.DecideApproval(ctx, b.ID, task.Decision{Status: task.ApprovalDenied, DecidedBy: task.DecidedByUser}); err != nil {
		t.Fatal(err)
	}
	expired, err := db.ExpirePending(ctx, "the daemon restarted")
	if err != nil || len(expired) != 1 || expired[0].ID != a.ID || expired[0].DecidedBy != task.DecidedByDaemon {
		t.Fatalf("expired %+v, %v", expired, err)
	}
	// Still visible, with the reason.
	got, err := db.Approval(ctx, a.ID)
	if err != nil || got.Status != task.ApprovalExpired || got.Answer != "the daemon restarted" {
		t.Fatalf("after restart %+v, %v", got, err)
	}
}

func TestDecideApprovalRace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := openDomain(t)
	a := queue(t, db, createRun(t, db).ID, task.ApprovalCommand)
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := range 8 {
		wg.Go(func() {
			st := task.ApprovalAllowed
			if i%2 == 1 {
				st = task.ApprovalDenied
			}
			if _, err := db.DecideApproval(ctx, a.ID, task.Decision{Status: st, DecidedBy: task.DecidedByUser}); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			} else if !errors.Is(err, ErrConflict) {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d decisions won", wins)
	}
}
