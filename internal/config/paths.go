// Package config resolves OpenChef's file locations and loads, validates and generates
// its TOML configuration (docs/architecture.md §13).
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// HomeEnv overrides the OpenChef home directory (default ~/.openchef).
const HomeEnv = "OPENCHEF_HOME"

// Paths lists every location OpenChef reads or writes. All paths are absolute.
type Paths struct {
	Home      string `json:"home"`
	Config    string `json:"config"`
	Settings  string `json:"settings"`
	State     string `json:"state"`
	Logs      string `json:"logs"`
	Sessions  string `json:"sessions"`
	Worktrees string `json:"worktrees"`
	Socket    string `json:"socket"`
	Lock      string `json:"lock"`
}

// ResolvePaths returns the paths for the current user, honouring OPENCHEF_HOME.
func ResolvePaths() (Paths, error) {
	home := os.Getenv(HomeEnv)
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return Paths{}, fmt.Errorf("config: locate user home directory: %w", err)
		}
		home = filepath.Join(userHome, ".openchef")
	}
	return PathsIn(home)
}

// PathsIn returns the paths rooted at home.
func PathsIn(home string) (Paths, error) {
	if home == "" {
		return Paths{}, errors.New("config: empty home directory")
	}
	abs, err := filepath.Abs(home)
	if err != nil {
		return Paths{}, fmt.Errorf("config: resolve %q: %w", home, err)
	}
	return Paths{
		Home:      abs,
		Config:    filepath.Join(abs, "config.toml"),
		Settings:  filepath.Join(abs, "settings.toml"),
		State:     filepath.Join(abs, "state.db"),
		Logs:      filepath.Join(abs, "logs"),
		Sessions:  filepath.Join(abs, "sessions"),
		Worktrees: filepath.Join(abs, "worktrees"),
		Socket:    filepath.Join(abs, "daemon.sock"),
		Lock:      filepath.Join(abs, "daemon.lock"),
	}, nil
}
