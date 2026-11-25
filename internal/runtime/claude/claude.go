// Package claude drives Claude Code headless (`claude -p` with stream-json I/O and the
// Agent SDK control protocol), per docs/architecture.md §6.2 and decisions D-006…D-011.
package claude

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/runtime/rawlog"
)

// Name is the runtime identifier.
const Name = "claude"

// Options configure the runtime.
type Options struct {
	// Binary is the claude executable (default "claude", resolved via PATH).
	Binary string
	// PrefixArgs are inserted before OpenChef's flags (tests run a scripted fake).
	PrefixArgs []string
}

// Runtime implements runtime.Runtime for Claude Code.
type Runtime struct{ opts Options }

// New returns a Claude runtime.
func New(opts Options) *Runtime {
	if opts.Binary == "" {
		opts.Binary = "claude"
	}
	return &Runtime{opts: opts}
}

// Name implements runtime.Runtime.
func (*Runtime) Name() string { return Name }

// Open starts a new session with an id chosen by OpenChef (--session-id).
func (r *Runtime) Open(ctx context.Context, spec runtime.SessionSpec) (runtime.Session, error) {
	id, err := newUUID()
	if err != nil {
		return nil, err
	}
	return r.begin(ctx, spec, id, false)
}

// Resume continues a session in a new process (--resume).
func (r *Runtime) Resume(ctx context.Context, ref runtime.SessionRef, spec runtime.SessionSpec) (runtime.Session, error) {
	if ref.Runtime != Name || ref.ID == "" {
		return nil, fmt.Errorf("claude: cannot resume %+v", ref)
	}
	return r.begin(ctx, spec, ref.ID, true)
}

func (r *Runtime) begin(ctx context.Context, spec runtime.SessionSpec, id string, resume bool) (runtime.Session, error) {
	if spec.Approver == nil {
		return nil, errors.New("claude: SessionSpec.Approver is required")
	}
	if !spec.Effort.Valid() {
		return nil, fmt.Errorf("claude: invalid effort %q", spec.Effort)
	}
	if _, _, err := modeAndTools(spec); err != nil {
		return nil, err
	}
	var log *rawlog.Log
	if spec.LogPath != "" {
		l, err := rawlog.Open(spec.LogPath)
		if err != nil {
			return nil, err
		}
		log = l
	}
	s := newSession(r, spec, log, runtime.SessionRef{Runtime: Name, ID: id}, resume)
	if spec.AskUser != nil {
		dir, err := os.MkdirTemp("", "openchef-claude-")
		if err != nil {
			s.abort()
			return nil, fmt.Errorf("claude: temp dir: %w", err)
		}
		s.tmpDir = dir
		raw, err := mcpConfigJSON(spec.AskUser)
		if err == nil {
			s.mcpConfig = filepath.Join(dir, "mcp.json")
			err = os.WriteFile(s.mcpConfig, raw, 0o600)
		}
		if err != nil {
			s.abort()
			return nil, fmt.Errorf("claude: write mcp config: %w", err)
		}
	}
	if err := s.spawn(ctx, nil, false); err != nil {
		s.abort()
		return nil, err
	}
	return s, nil
}

// newUUID returns a random RFC 4122 version 4 UUID.
func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("claude: session id: %w", err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
