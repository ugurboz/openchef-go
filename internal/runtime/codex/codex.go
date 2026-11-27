// Package codex drives Codex CLI through `codex app-server` (newline-delimited JSON-RPC
// over stdio), as decided in docs/architecture.md §6.3 and verified in
// docs/research/m1-findings.md §1.
package codex

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ugurboz/openchef/internal/procs"
	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/runtime/linerpc"
	"github.com/ugurboz/openchef/internal/runtime/rawlog"
)

// Name is the runtime identifier.
const Name = "codex"

const (
	clientName       = "openchef"
	handshakeTimeout = 60 * time.Second
)

// Options configure the runtime.
type Options struct {
	// Binary is the codex executable (default "codex", resolved via PATH).
	Binary string
	// Args are the arguments that start the app server (default ["app-server"]).
	// Tests replace both to run a scripted fake.
	Args []string
	// ClientVersion is reported to Codex in initialize.
	ClientVersion string
}

// Runtime implements runtime.Runtime for Codex.
type Runtime struct{ opts Options }

// New returns a Codex runtime.
func New(opts Options) *Runtime {
	if opts.Binary == "" {
		opts.Binary = "codex"
	}
	if opts.Args == nil {
		opts.Args = []string{"app-server"}
	}
	if opts.ClientVersion == "" {
		opts.ClientVersion = "0.0.0"
	}
	return &Runtime{opts: opts}
}

// Name implements runtime.Runtime.
func (*Runtime) Name() string { return Name }

// Open starts a new thread.
func (r *Runtime) Open(ctx context.Context, spec runtime.SessionSpec) (runtime.Session, error) {
	s, err := r.start(ctx, spec)
	if err != nil {
		return nil, err
	}
	var res threadResponse
	if err := s.call(ctx, "thread/start", s.threadParams(nil), &res); err != nil {
		return nil, s.fail(ctx, err)
	}
	if res.Thread.ID == "" {
		return nil, s.fail(ctx, &runtime.ProtocolError{Runtime: Name, Reason: "thread/start returned no thread id"})
	}
	s.ref = runtime.SessionRef{Runtime: Name, ID: res.Thread.ID}
	return s, nil
}

// Resume rejoins an existing thread in a new app-server process.
func (r *Runtime) Resume(ctx context.Context, ref runtime.SessionRef, spec runtime.SessionSpec) (runtime.Session, error) {
	if ref.Runtime != Name || ref.ID == "" {
		return nil, fmt.Errorf("codex: cannot resume %+v", ref)
	}
	s, err := r.start(ctx, spec)
	if err != nil {
		return nil, err
	}
	params := s.threadParams(map[string]any{"threadId": ref.ID, "excludeTurns": true})
	var res threadResponse
	if err := s.call(ctx, "thread/resume", params, &res); err != nil {
		return nil, s.fail(ctx, err)
	}
	s.ref = ref
	return s, nil
}

func (r *Runtime) start(ctx context.Context, spec runtime.SessionSpec) (*session, error) {
	if spec.Approver == nil {
		return nil, errors.New("codex: SessionSpec.Approver is required")
	}
	if !spec.Effort.Valid() {
		return nil, fmt.Errorf("codex: invalid effort %q", spec.Effort)
	}
	var log *rawlog.Log
	if spec.LogPath != "" {
		l, err := rawlog.Open(spec.LogPath)
		if err != nil {
			return nil, err
		}
		log = l
	}
	proc, err := procs.Start(ctx, procs.Spec{
		Path:   r.opts.Binary,
		Args:   r.opts.Args,
		Dir:    spec.Dir,
		Env:    spec.Env,
		Stderr: log.LineWriter(rawlog.Stderr),
	})
	if err != nil {
		_ = log.Close() // the start error is the one worth reporting
		return nil, fmt.Errorf("codex: start app-server: %w", err)
	}
	if err := spec.Spawned(ctx, proc.Identity()); err != nil {
		return nil, errors.Join(fmt.Errorf("codex: %w", err), proc.Close(context.WithoutCancel(ctx)), log.Close())
	}
	s := newSession(spec, proc, log)
	s.peer = linerpc.New(proc.Stdout, proc.Stdin, linerpc.Options{
		Log:             log,
		Handler:         s.handleRequest,
		OnProtocolError: s.onProtocolError,
	})
	go s.route()

	hctx, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	if err := s.call(hctx, "initialize", map[string]any{
		"clientInfo": map[string]any{"name": clientName, "title": "OpenChef", "version": r.opts.ClientVersion},
	}, nil); err != nil {
		return nil, s.fail(ctx, err)
	}
	if err := s.peer.Notify("initialized", nil); err != nil {
		return nil, s.fail(ctx, err)
	}
	var models modelListResponse
	if err := s.call(hctx, "model/list", map[string]any{}, &models); err != nil {
		return nil, s.fail(ctx, err)
	}
	s.efforts = models.supportedEfforts(spec.Model)
	var limits rateLimitsRead
	if err := s.call(hctx, "account/rateLimits/read", nil, &limits); err == nil {
		s.setLimits(limits.RateLimits)
	} else {
		// Rate limits are informative; an account without them must not block work.
		log.Notef(rawlog.Note, "codex: account/rateLimits/read failed: %v", err)
	}
	return s, nil
}

// threadParams builds thread/start and thread/resume parameters from the spec.
func (s *session) threadParams(extra map[string]any) map[string]any {
	p := map[string]any{
		"cwd":            s.spec.Dir,
		"approvalPolicy": "untrusted",
		"sandbox":        sandboxFor(s.spec.Mode),
		"ephemeral":      false,
	}
	if s.spec.Model != "" {
		p["model"] = s.spec.Model
	}
	if s.spec.AskUser != nil {
		p["config"] = map[string]any{"mcp_servers": map[string]any{
			s.spec.AskUser.Name: mcpServerConfig(s.spec.AskUser),
		}}
	}
	for k, v := range extra {
		p[k] = v
	}
	return p
}

// sandboxFor maps the permission mode. Every mode except plan uses workspace-write with
// approvals routed to OpenChef ("untrusted"): the approver applies the mode's semantics
// and the deny rules, so deny rules hold even in bypassPermissions (§9.3).
func sandboxFor(mode runtime.Mode) string {
	if mode == runtime.ModePlan {
		return "read-only"
	}
	return "workspace-write"
}

func mcpServerConfig(m *runtime.MCPServer) map[string]any {
	cfg := map[string]any{"command": m.Command, "args": m.Args}
	if m.ToolTimeout > 0 {
		cfg["tool_timeout_sec"] = m.ToolTimeout.Seconds()
	}
	if len(m.Env) > 0 {
		env := map[string]string{}
		for _, kv := range m.Env {
			if k, v, ok := strings.Cut(kv, "="); ok {
				env[k] = v
			}
		}
		cfg["env"] = env
	}
	return cfg
}
