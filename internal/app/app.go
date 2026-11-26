// Package app is the composition root: it builds the daemon's object graph (store,
// runtime adapters, router, engine, service, API) from configuration. Nothing else
// constructs these pieces, so every other package depends only on interfaces and
// plain values.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/ugurboz/openchef/internal/config"
	"github.com/ugurboz/openchef/internal/daemon"
	"github.com/ugurboz/openchef/internal/engine"
	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/runtime/agy"
	"github.com/ugurboz/openchef/internal/runtime/claude"
	"github.com/ugurboz/openchef/internal/runtime/codex"
	"github.com/ugurboz/openchef/internal/service"
	"github.com/ugurboz/openchef/internal/store"
	"github.com/ugurboz/openchef/internal/task"
	"github.com/ugurboz/openchef/internal/verify"
)

// Daemon returns the daemon's options. Its Init (run while the daemon holds the lock)
// builds the service, recovers work left by a previous daemon and exposes the task API.
func Daemon(paths config.Paths, version string, log *slog.Logger) daemon.Options {
	return daemon.Options{
		Paths: paths, Version: version, Log: log,
		Init: func(ctx context.Context) (daemon.Services, error) { return services(ctx, paths, version, log) },
	}
}

func services(ctx context.Context, paths config.Paths, version string, log *slog.Logger) (daemon.Services, error) {
	cfg, err := config.Load(paths.Config)
	if errors.Is(err, config.ErrNotFound) {
		return daemon.Services{}, fmt.Errorf("%w (run `openchef init` first)", err)
	}
	if err != nil {
		return daemon.Services{}, err
	}
	db, err := store.Open(ctx, paths.State)
	if err != nil {
		return daemon.Services{}, err
	}
	runtimes := map[string]runtime.Runtime{
		claude.Name: claude.New(claude.Options{}),
		"codex":     codex.New(codex.Options{ClientVersion: version}),
		"agy":       agy.New(agy.Options{ClientVersion: version}),
	}
	cool := &cooldowns{until: map[string]time.Time{}, now: time.Now}
	router := &engine.Ordered{Agents: Agents(cfg), Cooldown: cool.get}
	env := os.Environ()
	queue := &lateQueue{}
	svc, err := service.New(service.Deps{ //nolint:contextcheck // notifications are fire-and-forget with their own timeout
		Store: db, EventDir: paths.Sessions, Log: log, Notify: desktopNotifier(log),
		NewEngine: func(sink engine.Sink) (*engine.Engine, error) {
			return engine.New(engine.Config{
				Store: db, Runtimes: runtimes, Router: router, Sink: sink,
				SpecFor: func(t *task.Task, a engine.Agent) (runtime.SessionSpec, error) {
					return runtime.SessionSpec{
						Mode:      runtime.ModeAcceptEdits,
						DenyRules: runtime.InvariantDenyRules,
						Env:       sessionEnv(env, cfg.Pools[a.Pool]),
						Approver:  approver{queue: queue, task: t.ID, agent: a.Name, dir: t.Worktree},
					}, nil
				},
				VerifyEnv:     verify.MinimalEnv(env),
				WorktreeRoot:  paths.Worktrees,
				SessionRoot:   paths.Sessions,
				PoolExhausted: cool.set,
			})
		},
	})
	if err != nil {
		return daemon.Services{}, errors.Join(err, db.Close())
	}
	queue.svc = svc // before Start: no session runs until then
	if err := svc.Start(ctx); err != nil {
		return daemon.Services{}, errors.Join(err, db.Close())
	}
	return daemon.Services{
		Methods: daemon.ServiceMethods(svc),
		OnStop: func(ctx context.Context) error {
			if err := svc.Stop(ctx); err != nil {
				// Runs that did not stop still use the store; the process is exiting,
				// so leave the database to the OS rather than closing it under them.
				return err
			}
			return db.Close()
		},
	}, nil
}

// Agents lists the configured agents in routing order: the implement phase's list first,
// then the remaining agents by name.
func Agents(cfg *config.Config) []engine.Agent {
	order := slices.Clone(cfg.Routing["implement"].Agents)
	var rest []string
	for name := range cfg.Agents {
		if !slices.Contains(order, name) {
			rest = append(rest, name)
		}
	}
	slices.Sort(rest)
	order = append(order, rest...)
	out := make([]engine.Agent, 0, len(order))
	for _, name := range order {
		a, ok := cfg.Agents[name]
		if !ok {
			continue
		}
		out = append(out, engine.Agent{Name: name, Runtime: a.Runtime, Model: a.Model, Pool: a.Pool, Effort: runtime.Effort(a.Effort)})
	}
	return out
}

// cooldowns remembers pools that ran out of quota until their reset. It is the M3
// stand-in for quota tracking (§10, M4); unknown resets cool down for an hour.
type cooldowns struct {
	mu    sync.Mutex
	until map[string]time.Time
	now   func() time.Time
}

func (c *cooldowns) set(pool string, until time.Time) {
	if until.IsZero() {
		until = c.now().Add(time.Hour)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.until[pool] = until
}

func (c *cooldowns) get(pool string) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	u := c.until[pool]
	if !u.IsZero() && !c.now().Before(u) {
		delete(c.until, pool)
		return time.Time{}
	}
	return u
}
