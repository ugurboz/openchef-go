package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/ugurboz/openchef/internal/procs"
	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/store"
	"github.com/ugurboz/openchef/internal/task"
)

// spawnRecorder implements SessionSpec.OnSpawn for one session row: the first process
// creates the row, later processes (respawns within the conversation) update it. Both
// happen before the adapter writes anything to the process (§5.1, D-019).
type spawnRecorder struct {
	db          *store.DB
	attemptID   int64
	runtime     string
	logDir      string
	resumedFrom int64

	mu sync.Mutex
	id int64
}

func (s *spawnRecorder) onSpawn(ctx context.Context, p procs.Identity) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.id != 0 {
		return s.db.SetSessionProcess(ctx, s.id, p.PID, p.StartToken) // the adapter wraps it in ErrSpawnRejected
	}
	row := task.Session{
		AttemptID: s.attemptID, ResumedFrom: s.resumedFrom, Runtime: s.runtime,
		PID: p.PID, StartToken: p.StartToken, LogDir: s.logDir,
	}
	if err := s.db.RecordSession(ctx, &row); err != nil {
		return err // the adapter wraps it in ErrSpawnRejected
	}
	s.id = row.ID
	return nil
}

func (s *spawnRecorder) rowID() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.id
}

// liveSession is an open agent session and its store row.
type liveSession struct {
	s   runtime.Session
	row int64
}

// openSession starts (resume == nil) or resumes a session for an attempt and records
// it. resumedFrom is the closed row a resume continues.
func (r *run) openSession(ctx context.Context, a *task.Attempt, agent Agent, resume *runtime.SessionRef, resumedFrom int64, seq int) (*liveSession, error) {
	rt, ok := r.e.cfg.Runtimes[agent.Runtime]
	if !ok {
		return nil, fmt.Errorf("engine: agent %s: runtime %q is not available", agent.Name, agent.Runtime)
	}
	spec, err := r.e.cfg.SpecFor(&r.t, agent)
	if err != nil {
		return nil, fmt.Errorf("engine: session settings for %s: %w", agent.Name, err)
	}
	logDir := filepath.Join(r.e.cfg.SessionRoot, string(r.t.ID), strconv.FormatInt(a.ID, 10)+"-"+strconv.Itoa(seq))
	rec := &spawnRecorder{db: r.e.cfg.Store, attemptID: a.ID, runtime: agent.Runtime, logDir: logDir, resumedFrom: resumedFrom}
	spec.Dir, spec.Model, spec.Effort = r.wt.Path, agent.Model, agent.Effort
	spec.LogPath = filepath.Join(logDir, "raw.jsonl")
	spec.OnSpawn = rec.onSpawn

	var s runtime.Session
	if resume == nil {
		s, err = rt.Open(ctx, spec)
	} else {
		s, err = rt.Resume(ctx, *resume, spec)
	}
	if err != nil {
		if id := rec.rowID(); id != 0 {
			err = errors.Join(err, r.e.cfg.Store.CloseSession(context.WithoutCancel(ctx), id))
		}
		return nil, fmt.Errorf("engine: start %s session: %w", agent.Name, err)
	}
	ls := &liveSession{s: s, row: rec.rowID()}
	if ls.row == 0 {
		return nil, errors.Join(
			fmt.Errorf("engine: %s adapter did not report its process (OnSpawn)", agent.Runtime),
			s.Close(context.WithoutCancel(ctx)))
	}
	if err := r.e.cfg.Store.OpenSession(ctx, ls.row, s.Ref().ID); err != nil {
		return nil, errors.Join(fmt.Errorf("engine: %w", err), r.closeSession(ctx, ls))
	}
	return ls, nil
}

// closeSession stops the CLI and then records the session closed. It runs on every
// path, including shutdown, so it ignores ctx cancellation.
func (r *run) closeSession(ctx context.Context, ls *liveSession) error {
	if ls == nil {
		return nil
	}
	ctx = context.WithoutCancel(ctx)
	closeErr := ls.s.Close(ctx)
	if closeErr != nil {
		closeErr = fmt.Errorf("engine: close session %d: %w", ls.row, closeErr)
	}
	return errors.Join(closeErr, r.e.cfg.Store.CloseSession(ctx, ls.row))
}
