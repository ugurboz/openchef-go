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
	"maps"
	"os"
	"slices"
	"time"

	"github.com/ugurboz/openchef/internal/config"
	"github.com/ugurboz/openchef/internal/daemon"
	"github.com/ugurboz/openchef/internal/engine"
	"github.com/ugurboz/openchef/internal/mcp"
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
	env := os.Environ()
	queue := &lateQueue{}
	pols, err := newPolicies(paths)
	if err != nil {
		return daemon.Services{}, errors.Join(err, db.Close())
	}
	exe, err := os.Executable()
	if err != nil {
		return daemon.Services{}, errors.Join(fmt.Errorf("app: locate openchef executable: %w", err), db.Close())
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return daemon.Services{}, errors.Join(fmt.Errorf("app: locate home directory: %w", err), db.Close())
	}
	//nolint:contextcheck // probes read credentials under their own timeout, per request
	quotas, err := service.NewQuota(service.QuotaDeps{Store: db, Probes: probesFor(cfg, userHome), Log: log, Budgets: budgetsFor(cfg)})
	if err != nil {
		return daemon.Services{}, errors.Join(err, db.Close())
	}
	if err := quotas.Start(ctx); err != nil {
		return daemon.Services{}, errors.Join(err, db.Close())
	}
	poolOf := func(agent string) string { return cfg.Agents[agent].Pool }
	router := newQuotaRouter(cfg, quotas, nil)
	svc, err := service.New(service.Deps{ //nolint:contextcheck // notifications are fire-and-forget with their own timeout
		Store: db, EventDir: paths.Sessions, Log: log, Notify: desktopNotifier(log),
		DefaultPermissions: pols.defaultMode,
		NewEngine: func(sink engine.Sink) (*engine.Engine, error) {
			return engine.New(engine.Config{
				Store: db, Runtimes: runtimes, Router: router, Sink: quotaSink{next: sink, quota: quotas, poolOf: poolOf},
				SpecFor: func(t *task.Task, a engine.Agent) (runtime.SessionSpec, error) {
					ps, err := pols.session(t)
					if err != nil {
						return runtime.SessionSpec{}, err
					}
					return runtime.SessionSpec{
						Mode:      ps.mode,
						DenyRules: ps.denyRules,
						AskRules:  ps.askRules,
						Env:       sessionEnv(env, cfg.Pools[a.Pool]),
						Approver:  approver{queue: queue, eval: ps.eval, task: t.ID, agent: a.Name},
						AskUser:   askUserServer(exe, paths, t.ID, a.Name),
					}, nil
				},
				VerifyEnv:     verify.MinimalEnv(env),
				WorktreeRoot:  paths.Worktrees,
				SessionRoot:   paths.Sessions,
				PoolExhausted: quotas.Exhausted,
			})
		},
	})
	if err != nil {
		quotas.Stop()
		return daemon.Services{}, errors.Join(err, db.Close())
	}
	queue.svc = svc // before Start: no session runs until then
	if err := svc.Start(ctx); err != nil {
		quotas.Stop()
		return daemon.Services{}, errors.Join(err, db.Close())
	}
	methods := daemon.ServiceMethods(svc)
	maps.Copy(methods, daemon.QuotaMethods(quotas))
	return daemon.Services{
		Methods: methods,
		OnStop: func(ctx context.Context) error {
			if err := svc.Stop(ctx); err != nil {
				// Runs that did not stop still use the store; the process is exiting,
				// so leave the database to the OS rather than closing it under them.
				return err
			}
			quotas.Stop() // after the runs: their last observations are stored
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

// askUserServer is the ask_user MCP server for one task's agent: this executable's
// hidden `mcp ask-user` command, which forwards questions to the daemon's queue. The
// tool timeout outlasts the queue's, so the queue, not the CLI, ends a wait.
func askUserServer(exe string, paths config.Paths, id task.ID, agent string) *runtime.MCPServer {
	return &runtime.MCPServer{
		Name: mcp.ServerName, Command: exe,
		Args:        []string{"mcp", "ask-user", "--task", string(id), "--agent", agent},
		Env:         []string{config.HomeEnv + "=" + paths.Home},
		ToolTimeout: task.ApprovalTimeout + time.Minute,
	}
}
