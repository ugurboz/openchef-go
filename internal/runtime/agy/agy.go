// Package agy drives the Antigravity CLI over the Agent Client Protocol (`agy acp`), per
// docs/architecture.md §6.4 and decisions D-015, D-016.
package agy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/runtime/rawlog"
)

// Name is the runtime identifier.
const Name = "agy"

// Options configure the runtime.
type Options struct {
	// Binary is the agy executable (default "agy", resolved via PATH).
	Binary string
	// Args start the ACP server before OpenChef's flags (default ["acp"]). Tests replace
	// Binary and Args to run a scripted fake.
	Args []string
	// CLIDirName is agy's --cli_dir_name (default "openchef"): OpenChef's own settings
	// and logs, separate from the user's agy configuration.
	CLIDirName string
	// SettingsDir is where OpenChef writes agy's settings.json (default
	// <HOME>/.gemini/antigravity-cli/<CLIDirName>, HOME taken from SessionSpec.Env).
	SettingsDir string
	// AllowTempDir permits workspaces under the temp tree, where agy's sandbox does not
	// contain writes (D-015). Only tests set it.
	AllowTempDir bool
	// ClientVersion is reported to agy in initialize.
	ClientVersion string
}

// Runtime implements runtime.Runtime for agy.
type Runtime struct{ opts Options }

// New returns an agy runtime.
func New(opts Options) *Runtime {
	if opts.Binary == "" {
		opts.Binary = "agy"
	}
	if opts.Args == nil {
		opts.Args = []string{"acp"}
	}
	if opts.CLIDirName == "" {
		opts.CLIDirName = "openchef"
	}
	if opts.ClientVersion == "" {
		opts.ClientVersion = "0.0.0"
	}
	return &Runtime{opts: opts}
}

// Name implements runtime.Runtime.
func (*Runtime) Name() string { return Name }

// Open starts a new ACP session.
func (r *Runtime) Open(ctx context.Context, spec runtime.SessionSpec) (runtime.Session, error) {
	return r.begin(ctx, spec, "")
}

// Resume loads a session in a new process (session/load).
func (r *Runtime) Resume(ctx context.Context, ref runtime.SessionRef, spec runtime.SessionSpec) (runtime.Session, error) {
	if ref.Runtime != Name || ref.ID == "" {
		return nil, fmt.Errorf("agy: cannot resume %+v", ref)
	}
	return r.begin(ctx, spec, ref.ID)
}

func (r *Runtime) begin(ctx context.Context, spec runtime.SessionSpec, id string) (runtime.Session, error) {
	if spec.Approver == nil {
		return nil, errors.New("agy: SessionSpec.Approver is required")
	}
	if !spec.Effort.Valid() {
		return nil, fmt.Errorf("agy: invalid effort %q", spec.Effort)
	}
	if spec.Mode != "" && !validMode(spec.Mode) {
		return nil, fmt.Errorf("agy: unsupported permission mode %q", spec.Mode)
	}
	if spec.Dir == "" {
		return nil, errors.New("agy: SessionSpec.Dir is required")
	}
	if !r.opts.AllowTempDir {
		if err := refuseTempDir(spec.Dir); err != nil {
			return nil, err
		}
	}
	if err := r.writeSettings(spec.Env); err != nil {
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
	s := newSession(r, spec, log, id)
	if err := s.start(ctx); err != nil {
		s.abort(ctx)
		return nil, err
	}
	return s, nil
}

func validMode(m runtime.Mode) bool {
	for _, x := range runtime.Modes {
		if x == m {
			return true
		}
	}
	return false
}

// settings is what OpenChef writes to its own agy CLI directory (D-015): commands run
// unasked only inside the read-only, offline sandbox; every escape reaches the host.
var settings = map[string]any{
	"toolPermission":        "proceed-in-sandbox",
	"enableTerminalSandbox": true,
}

func (r *Runtime) settingsDir(env []string) (string, error) {
	if r.opts.SettingsDir != "" {
		return r.opts.SettingsDir, nil
	}
	home := ""
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "HOME="); ok {
			home = v
		}
	}
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("agy: home directory: %w", err)
		}
		home = h
	}
	return filepath.Join(home, ".gemini", "antigravity-cli", r.opts.CLIDirName), nil
}

// writeSettings writes settings.json atomically, and only when its content differs,
// so concurrent sessions never see a partial file.
func (r *Runtime) writeSettings(env []string) error {
	dir, err := r.settingsDir(env)
	if err != nil {
		return err
	}
	want, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("agy: encode settings: %w", err)
	}
	path := filepath.Join(dir, "settings.json")
	//nolint:gosec // G304: OpenChef's own agy settings file.
	if have, err := os.ReadFile(path); err == nil && bytes.Equal(have, want) {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("agy: settings directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".settings-*.json")
	if err != nil {
		return fmt.Errorf("agy: write settings: %w", err)
	}
	_, werr := tmp.Write(want)
	cerr := tmp.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(tmp.Name()) // best effort: the write error is what matters
		return fmt.Errorf("agy: write settings: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name()) // best effort, as above
		return fmt.Errorf("agy: write settings: %w", err)
	}
	return nil
}

// refuseTempDir rejects workspaces inside the temp tree: agy's sandbox allows writes
// there without asking (m1-findings §11), so containment would not hold.
func refuseTempDir(dir string) error {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("agy: workspace: %w", err)
	}
	if r, err := filepath.EvalSymlinks(abs); err == nil {
		abs = r
	}
	roots := []string{os.TempDir(), "/tmp", "/var/tmp", "/var/folders"}
	for _, root := range roots {
		cands := []string{filepath.Clean(root)}
		if r, err := filepath.EvalSymlinks(root); err == nil {
			cands = append(cands, r)
		}
		for _, c := range cands {
			if abs == c || strings.HasPrefix(abs, c+string(filepath.Separator)) {
				return fmt.Errorf("agy: workspace %s is inside the temp tree (%s), where agy's sandbox does not contain writes; use a worktree elsewhere", dir, c)
			}
		}
	}
	return nil
}
