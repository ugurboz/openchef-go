// Package daemon is the long-running OpenChef process (docs/architecture.md §5, §12.3):
// it holds the single-instance lock, serves the JSON-RPC API on a local socket and owns
// every agent process. CLIs and the TUI are clients; they come and go without stopping
// it.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	goruntime "runtime"
	"sync"
	"time"

	"github.com/ugurboz/openchef/internal/api"
	"github.com/ugurboz/openchef/internal/config"
	"github.com/ugurboz/openchef/internal/runtime/linerpc"
)

// StopTimeout bounds the shutdown work of OnStop (§5.4: graceful interrupt, then 10 s).
const StopTimeout = 10 * time.Second

// maxSocketPath is the portable limit of a Unix socket path (sun_path is 104 bytes on
// macOS, 108 on Linux, including the terminating NUL).
const maxSocketPath = 103

// Method handles one API method on one connection.
type Method func(ctx context.Context, c *Conn, params json.RawMessage) (any, error)

// Options configure Run.
type Options struct {
	Paths config.Paths
	// Version is the daemon's build version, reported to clients.
	Version string
	// Methods adds API methods beyond initialize, daemon/status and daemon/stop.
	Methods map[string]Method
	// OnStop runs once on shutdown, before connections are closed, with StopTimeout to
	// stop sessions gracefully (§5.4).
	OnStop func(ctx context.Context) error
	// Log receives operational messages; nil discards them.
	Log *slog.Logger
}

// Conn is one client connection.
type Conn struct {
	id   int
	peer *linerpc.Peer
	net  net.Conn
	// ready is closed once peer is set; handlers wait for it, because the peer starts
	// reading (and dispatching) before linerpc.New returns.
	ready chan struct{}

	mu          sync.Mutex
	initialized bool
}

// Notify sends a server notification (e.g. event/appended) to this client.
func (c *Conn) Notify(method string, params any) error {
	if err := c.peer.Notify(method, params); err != nil {
		return fmt.Errorf("daemon: notify connection %d: %w", c.id, err)
	}
	return nil
}

type server struct {
	opts    Options
	log     *slog.Logger
	started time.Time
	stop    chan struct{}
	once    sync.Once

	mu     sync.Mutex
	conns  map[int]*Conn
	nextID int
	wg     sync.WaitGroup
}

// Run starts the daemon and serves until ctx is cancelled or a client calls
// daemon/stop. It fails with ErrRunning if another daemon holds the lock.
func Run(ctx context.Context, opts Options) error {
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	p := opts.Paths
	if err := os.MkdirAll(p.Home, 0o700); err != nil {
		return fmt.Errorf("daemon: create %s: %w", p.Home, err)
	}
	if len(p.Socket) > maxSocketPath {
		return fmt.Errorf("daemon: socket path %s is %d bytes; the limit is %d (set %s to a shorter directory)",
			p.Socket, len(p.Socket), maxSocketPath, config.HomeEnv)
	}
	lock, err := Acquire(p.Lock)
	if err != nil {
		return err
	}
	defer func() {
		if err := lock.Release(); err != nil {
			log.Error("release lock", "err", err)
		}
	}()

	// Holding the lock, any socket file left behind belongs to a dead daemon.
	if err := os.Remove(p.Socket); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("daemon: remove stale socket: %w", err)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", p.Socket)
	if err != nil {
		return fmt.Errorf("daemon: listen on %s: %w", p.Socket, err)
	}
	if goruntime.GOOS != "windows" {
		// On Windows the socket inherits the home directory's user-only ACL.
		if err := os.Chmod(p.Socket, 0o600); err != nil {
			return errors.Join(fmt.Errorf("daemon: restrict socket: %w", err), ln.Close())
		}
	}

	s := &server{opts: opts, log: log, started: time.Now(), stop: make(chan struct{}), conns: map[int]*Conn{}}
	log.Info("daemon started", "pid", os.Getpid(), "socket", p.Socket, "version", opts.Version)
	go s.accept(ctx, ln)

	select {
	case <-ctx.Done():
	case <-s.stop:
	}
	return s.shutdown(ctx, ln)
}

func (s *server) accept(ctx context.Context, ln net.Listener) {
	for {
		nc, err := ln.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				s.log.Error("accept", "err", err)
				s.requestStop()
			}
			return
		}
		s.mu.Lock()
		s.nextID++
		c := &Conn{id: s.nextID, net: nc, ready: make(chan struct{})}
		s.conns[c.id] = c
		s.mu.Unlock()
		c.peer = linerpc.New(nc, nc, linerpc.Options{Handler: func(hctx context.Context, method string, params json.RawMessage) (any, error) {
			<-c.ready
			return s.handle(hctx, c, method, params)
		}})
		close(c.ready)
		s.wg.Go(func() {
			select {
			case <-c.peer.Done():
			case <-ctx.Done():
			}
			s.drop(c)
		})
	}
}

func (s *server) drop(c *Conn) {
	s.mu.Lock()
	delete(s.conns, c.id)
	s.mu.Unlock()
	if err := c.net.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		s.log.Debug("close connection", "id", c.id, "err", err)
	}
}

func (s *server) requestStop() { s.once.Do(func() { close(s.stop) }) }

func (s *server) handle(ctx context.Context, c *Conn, method string, params json.RawMessage) (any, error) {
	if method == api.MethodInitialize {
		return s.initialize(c, params)
	}
	c.mu.Lock()
	ok := c.initialized
	c.mu.Unlock()
	if !ok {
		return nil, api.RPCError(api.CodeNotInitialized, "call %s first", api.MethodInitialize)
	}
	switch method {
	case api.MethodStatus:
		return s.status(), nil
	case api.MethodStop:
		s.log.Info("stop requested", "connection", c.id)
		s.requestStop()
		return struct{}{}, nil
	}
	if m, ok := s.opts.Methods[method]; ok {
		return m(ctx, c, params)
	}
	return nil, api.RPCError(linerpc.CodeMethodNotFound, "unknown method %q", method)
}

func (s *server) initialize(c *Conn, params json.RawMessage) (any, error) {
	var p api.InitializeParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, api.RPCError(api.CodeInvalidParams, "initialize: %v", err)
	}
	if p.Version != api.Version {
		return nil, api.RPCError(api.CodeVersionMismatch, "daemon speaks %s, client %s", api.Version, p.Version)
	}
	c.mu.Lock()
	c.initialized = true
	c.mu.Unlock()
	return api.InitializeResult{Version: api.Version, DaemonVersion: s.opts.Version, PID: os.Getpid()}, nil
}

func (s *server) status() api.Status {
	s.mu.Lock()
	n := len(s.conns)
	s.mu.Unlock()
	return api.Status{
		PID: os.Getpid(), DaemonVersion: s.opts.Version, StartedAt: s.started,
		Socket: s.opts.Paths.Socket, Home: s.opts.Paths.Home, Connections: n,
	}
}

// shutdown stops accepting, lets OnStop stop the sessions, closes every connection and
// removes the socket (§5.4).
func (s *server) shutdown(ctx context.Context, ln net.Listener) error {
	s.log.Info("daemon stopping")
	var errs []error
	if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		errs = append(errs, fmt.Errorf("daemon: close listener: %w", err))
	}
	if s.opts.OnStop != nil {
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), StopTimeout)
		if err := s.opts.OnStop(sctx); err != nil {
			errs = append(errs, fmt.Errorf("daemon: stop: %w", err))
		}
		cancel()
	}
	s.mu.Lock()
	conns := make([]*Conn, 0, len(s.conns))
	for _, c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	for _, c := range conns {
		s.drop(c)
	}
	s.wg.Wait()
	if err := os.Remove(s.opts.Paths.Socket); err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, fmt.Errorf("daemon: remove socket: %w", err))
	}
	s.log.Info("daemon stopped")
	return errors.Join(errs...)
}
