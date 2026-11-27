package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/ugurboz/openchef/internal/procs"
	"github.com/ugurboz/openchef/internal/task"
)

// KillLeftovers is the daemon's first recovery step (§5.3 step 2): every session the
// store still records as live belonged to a previous daemon. Its process is killed if it
// is still the same process (PID and start token must both match, so a recycled PID is
// never hit), and the session is marked closed. Turns and attempts stay as they are;
// Run resumes them. It returns how many processes were still alive.
func (e *Engine) KillLeftovers(ctx context.Context) (int, error) {
	live, err := e.cfg.Store.LiveSessions(ctx)
	if err != nil {
		return 0, fmt.Errorf("engine: %w", err)
	}
	killed := 0
	var errs []error
	for i := range live {
		s := &live[i]
		alive, err := procs.KillIfAlive(procs.Identity{PID: s.PID, StartToken: s.StartToken})
		if err != nil && !errors.Is(err, procs.ErrUnsupported) {
			errs = append(errs, fmt.Errorf("engine: kill leftover pid %d of session %d: %w", s.PID, s.ID, err))
			continue
		}
		if alive {
			killed++
		}
		if err := e.cfg.Store.CloseSession(ctx, s.ID); err != nil {
			errs = append(errs, fmt.Errorf("engine: %w", err))
		}
	}
	return killed, errors.Join(errs...)
}

// killLeftover kills a session's process if it is still the recorded one.
func killLeftover(s *task.Session) error {
	if _, err := procs.KillIfAlive(procs.Identity{PID: s.PID, StartToken: s.StartToken}); err != nil &&
		!errors.Is(err, procs.ErrUnsupported) {
		return fmt.Errorf("engine: kill leftover pid %d of session %d: %w", s.PID, s.ID, err)
	}
	return nil
}
