package e2e

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ugurboz/openchef/internal/procs"
	"github.com/ugurboz/openchef/internal/runtime"
)

// procRuntime is a runtime whose sessions are real child processes (the test binary in
// agent mode), so the acceptance test can check that no agent process outlives a killed
// daemon. A turn completes once the flag file exists, writing done.txt into the
// workspace; until then it waits, like an agent thinking.
type procRuntime struct {
	exe  string
	flag string
	n    atomic.Int64
}

func (r *procRuntime) Name() string { return "claude" }

func (r *procRuntime) Open(ctx context.Context, spec runtime.SessionSpec) (runtime.Session, error) {
	return r.start(ctx, spec, fmt.Sprintf("conv-%d-%d", os.Getpid(), r.n.Add(1)))
}

func (r *procRuntime) Resume(ctx context.Context, ref runtime.SessionRef, spec runtime.SessionSpec) (runtime.Session, error) {
	return r.start(ctx, spec, ref.ID)
}

func (r *procRuntime) start(ctx context.Context, spec runtime.SessionSpec, id string) (runtime.Session, error) {
	p, err := procs.Start(ctx, procs.Spec{Path: r.exe, Dir: spec.Dir, Env: append(os.Environ(), agentEnv+"=1")})
	if err != nil {
		return nil, fmt.Errorf("procfake: start: %w", err)
	}
	if err := spec.Spawned(ctx, p.Identity()); err != nil {
		return nil, errors.Join(err, p.Close(context.WithoutCancel(ctx)))
	}
	go func() { _, _ = io.Copy(io.Discard, p.Stdout) }()
	return &procSession{rt: r, p: p, ref: runtime.SessionRef{Runtime: r.Name(), ID: id}, dir: spec.Dir}, nil
}

type procSession struct {
	rt  *procRuntime
	p   *procs.Process
	ref runtime.SessionRef
	dir string

	mu   sync.Mutex
	stop chan struct{}
}

func (s *procSession) Ref() runtime.SessionRef { return s.ref }

func (s *procSession) Turn(ctx context.Context, in runtime.TurnInput) (<-chan runtime.Event, error) {
	if _, err := fmt.Fprintln(s.p.Stdin, "turn"); err != nil {
		return nil, fmt.Errorf("procfake: %w: %w", runtime.ErrSessionClosed, err)
	}
	stop := make(chan struct{})
	s.mu.Lock()
	s.stop = stop
	s.mu.Unlock()
	ch := make(chan runtime.Event, 2)
	go func() {
		defer close(ch)
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-tick.C:
				if _, err := os.Stat(s.rt.flag); err == nil {
					_ = os.WriteFile(filepath.Join(s.dir, "done.txt"), []byte(in.Text), 0o600)
					ch <- runtime.Event{Kind: runtime.EventTurnCompleted, At: time.Now(), Result: &runtime.TurnResult{FinalText: "done"}}
					return
				}
			case <-stop:
				ch <- runtime.Event{Kind: runtime.EventTurnFailed, At: time.Now(), Failure: &runtime.Failure{Kind: runtime.FailInterrupted}}
				return
			case <-ctx.Done():
				ch <- runtime.Event{Kind: runtime.EventTurnFailed, At: time.Now(), Failure: &runtime.Failure{Kind: runtime.FailInterrupted}}
				return
			case <-s.p.Done():
				ch <- runtime.Event{Kind: runtime.EventTurnFailed, At: time.Now(), Failure: &runtime.Failure{Kind: runtime.FailCrashed}}
				return
			}
		}
	}()
	return ch, nil
}

func (s *procSession) Interrupt(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stop != nil {
		close(s.stop)
		s.stop = nil
	}
	return nil
}

func (s *procSession) Close(ctx context.Context) error {
	_ = s.Interrupt(ctx) // ends a turn in progress
	if err := s.p.Close(ctx); err != nil {
		return fmt.Errorf("procfake: close: %w", err)
	}
	return nil
}

// agentMain is the agent child: it reads turns until stdin closes (as real CLIs do when
// their parent dies, §5.1) and then exits.
func agentMain() int {
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
	}
	return 0
}
