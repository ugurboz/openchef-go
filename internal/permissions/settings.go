package permissions

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/pelletier/go-toml/v2"

	"github.com/ugurboz/openchef/internal/runtime"
)

// Settings file names inside a repository (§9.1).
const (
	ProjectSettings = ".openchef/settings.toml"
	LocalSettings   = ".openchef/settings.local.toml"
)

// fileSettings is one settings file: the same keys as Claude Code's JSON settings.
type fileSettings struct {
	Permissions struct {
		DefaultMode string   `toml:"defaultMode"`
		Allow       []string `toml:"allow"`
		Ask         []string `toml:"ask"`
		Deny        []string `toml:"deny"`
	} `toml:"permissions"`
}

// SettingsError lists the problems of one settings file.
type SettingsError struct {
	Path     string
	Problems []error
}

func (e *SettingsError) Error() string {
	return fmt.Sprintf("permissions: %s: %v", e.Path, errors.Join(e.Problems...))
}

func (e *SettingsError) Unwrap() []error { return e.Problems }

// Load merges the user settings file and the repository's project and local files, in
// that order: rules accumulate, and the most specific file's defaultMode wins. Missing
// files are skipped. Any invalid rule fails the load: a typo in a deny rule must never
// silently allow.
func Load(userFile, repoRoot string) (Policy, error) {
	files := []string{userFile}
	if repoRoot != "" {
		files = append(files, filepath.Join(repoRoot, ProjectSettings), filepath.Join(repoRoot, LocalSettings))
	}
	var p Policy
	for _, f := range files {
		if f == "" {
			continue
		}
		settings, err := readSettings(f)
		if errors.Is(err, errMissing) {
			continue
		}
		if err != nil {
			return Policy{}, err
		}
		if err := merge(&p, f, settings); err != nil {
			return Policy{}, err
		}
	}
	if p.Mode == "" {
		p.Mode = runtime.ModeManual
	}
	return p, nil
}

var errMissing = errors.New("missing")

func readSettings(path string) (*fileSettings, error) {
	//nolint:gosec // G304: settings files at their documented locations.
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errMissing
	}
	if err != nil {
		return nil, fmt.Errorf("permissions: read %s: %w", path, err)
	}
	var s fileSettings
	dec := toml.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, &SettingsError{Path: path, Problems: []error{err}}
	}
	return &s, nil
}

func merge(p *Policy, path string, s *fileSettings) error {
	var problems []error
	if m := s.Permissions.DefaultMode; m != "" {
		mode, err := runtime.ParseMode(m)
		if err != nil {
			problems = append(problems, err)
		} else {
			p.Mode = mode
		}
	}
	add := func(dst *[]Rule, list []string, kind string) {
		for _, raw := range list {
			r, err := ParseRule(raw)
			if err != nil {
				problems = append(problems, fmt.Errorf("%s: %w", kind, err))
				continue
			}
			*dst = append(*dst, r)
		}
	}
	add(&p.Allow, s.Permissions.Allow, "allow")
	add(&p.Ask, s.Permissions.Ask, "ask")
	add(&p.Deny, s.Permissions.Deny, "deny")
	if len(problems) > 0 {
		return &SettingsError{Path: path, Problems: problems}
	}
	return nil
}
