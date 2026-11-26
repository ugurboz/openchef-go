package agy

import (
	"context"
	"errors"
	"testing"

	"github.com/ugurboz/openchef/internal/procs"
	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/runtime/runtimetest"
)

// The engine records each process before anything is written to it (§5.1).
func TestOnSpawnRunsBeforeFirstWrite(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	rec := &runtimetest.SpawnRecorder{FakeLog: h.logPath}
	h.spec.OnSpawn = rec.OnSpawn
	s := h.open()
	end, _ := turn(t, s, runtime.TurnInput{Text: "ok"})
	if end.Kind != runtime.EventTurnCompleted {
		t.Fatalf("turn ended %+v", end)
	}
	if err := rec.Err(); err != nil {
		t.Fatal(err)
	}
	ids := rec.IDs()
	if len(ids) != 1 || ids[0].PID <= 0 {
		t.Fatalf("OnSpawn calls = %+v; want one", ids)
	}
}

// A rejected process is killed and the session does not start.
func TestOnSpawnRejectKillsProcess(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "1")
	boom := errors.New("task cancelled")
	rec := &runtimetest.SpawnRecorder{FakeLog: h.logPath, Reject: boom}
	h.spec.OnSpawn = rec.OnSpawn
	ctx := context.Background()
	err := runtimetest.OpenAndTurn(ctx, h.rt, h.spec)
	if !errors.Is(err, runtime.ErrSpawnRejected) || !errors.Is(err, boom) {
		t.Fatalf("err = %v; want ErrSpawnRejected wrapping the hook error", err)
	}
	ids := rec.IDs()
	if len(ids) != 1 {
		t.Fatalf("OnSpawn calls = %d; want exactly one (no retry after a rejection)", len(ids))
	}
	if alive, aerr := procs.Alive(ids[0]); aerr != nil || alive {
		t.Fatalf("rejected process alive = %t, %v", alive, aerr)
	}
}
