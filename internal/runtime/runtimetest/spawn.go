// Package runtimetest holds test helpers shared by the runtime adapters.
package runtimetest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"

	"github.com/ugurboz/openchef/internal/procs"
	"github.com/ugurboz/openchef/internal/runtime"
)

// SpawnRecorder is a runtime.SessionSpec.OnSpawn that records every process identity and
// checks the §5.1 ordering: the process is alive and, for the first process, the fake CLI
// has not received any input yet.
type SpawnRecorder struct {
	// FakeLog is the fake CLI's log of what it read.
	FakeLog string
	// InputMarker is a substring present only in log lines recording input; "" means
	// `"in":`. Fakes that log raw input lines use "{".
	InputMarker string
	// Reject, if set, is returned from OnSpawn.
	Reject error

	mu       sync.Mutex
	ids      []procs.Identity
	problems []error
}

// OnSpawn implements runtime.SessionSpec.OnSpawn.
func (r *SpawnRecorder) OnSpawn(_ context.Context, id procs.Identity) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if alive, err := procs.Alive(id); err != nil || !alive {
		r.problems = append(r.problems, fmt.Errorf("pid %d not alive at OnSpawn (err: %w)", id.PID, err))
	}
	if len(r.ids) == 0 {
		raw, err := os.ReadFile(r.FakeLog)
		switch {
		case errors.Is(err, fs.ErrNotExist):
		case err != nil:
			r.problems = append(r.problems, err)
		case bytes.Contains(raw, []byte(r.marker())):
			r.problems = append(r.problems, fmt.Errorf("pid %d received input before OnSpawn", id.PID))
		}
	}
	r.ids = append(r.ids, id)
	return r.Reject
}

func (r *SpawnRecorder) marker() string {
	if r.InputMarker == "" {
		return `"in":`
	}
	return r.InputMarker
}

// IDs returns the recorded identities in call order.
func (r *SpawnRecorder) IDs() []procs.Identity {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]procs.Identity(nil), r.ids...)
}

// Err reports ordering violations seen so far.
func (r *SpawnRecorder) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return errors.Join(r.problems...)
}

// OpenAndTurn opens a session and runs one turn, returning the first error: from Open,
// from Turn, or from a failed turn's Err. Adapters differ in whether the process starts
// in Open or in the first Turn; this covers both.
func OpenAndTurn(ctx context.Context, rt runtime.Runtime, spec runtime.SessionSpec) (err error) {
	s, err := rt.Open(ctx, spec)
	if err != nil {
		return err // the adapter's error is what the test inspects
	}
	defer func() { err = errors.Join(err, s.Close(context.WithoutCancel(ctx))) }()
	ch, err := s.Turn(ctx, runtime.TurnInput{Text: "ok"})
	if err != nil {
		return err // the adapter's error is what the test inspects
	}
	for ev := range ch {
		if ev.Kind == runtime.EventTurnFailed {
			return fmt.Errorf("turn failed: %+v", ev)
		}
	}
	return nil
}
