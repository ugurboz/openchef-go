// Package doctor inspects the local environment: which agent CLIs are installed, their
// versions and whether they are logged in. It only runs read-only status commands.
package doctor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Login states.
const (
	LoginOK      = "logged_in"
	LoginMissing = "logged_out"
	LoginUnknown = "unknown"
)

// RuntimeStatus describes one agent CLI.
type RuntimeStatus struct {
	Runtime     string `json:"runtime"`
	Binary      string `json:"binary"`
	Found       bool   `json:"found"`
	Path        string `json:"path,omitempty"`
	Version     string `json:"version,omitempty"`
	Login       string `json:"login"`
	LoginDetail string `json:"loginDetail,omitempty"`
	Problem     string `json:"problem,omitempty"`
}

// Usable reports whether OpenChef can drive this runtime.
func (s RuntimeStatus) Usable() bool {
	return s.Found && s.Problem == "" && s.Login != LoginMissing
}

// Result is the outcome of one command.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Runner executes a command and returns its output. A non-zero exit is not an error;
// err is reserved for failures to run the command at all (missing binary, timeout).
type Runner func(ctx context.Context, path string, args ...string) (Result, error)

// Detector finds and inspects agent CLIs. The zero value is not usable; use New.
type Detector struct {
	LookPath func(string) (string, error)
	Run      Runner
	Timeout  time.Duration
}

// New returns a Detector that uses the real PATH and processes.
func New() Detector {
	return Detector{LookPath: exec.LookPath, Run: execRunner, Timeout: 15 * time.Second}
}

type probe struct {
	runtime, binary string
	versionArgs     []string
	parseVersion    func(string) string
	login           func(ctx context.Context, d Detector, path string) (state, detail string, err error)
}

var probes = []probe{
	{
		runtime: "claude", binary: "claude",
		versionArgs:  []string{"--version"},
		parseVersion: firstField,
		login:        claudeLogin,
	},
	{
		runtime: "codex", binary: "codex",
		versionArgs:  []string{"--version"},
		parseVersion: lastField,
		login:        codexLogin,
	},
	{
		runtime: "agy", binary: "agy",
		versionArgs:  []string{"--version"},
		parseVersion: firstField,
		// agy exposes no login status command (checked on agy 1.5.16); report it as
		// unknown rather than guess.
		login: func(context.Context, Detector, string) (string, string, error) {
			return LoginUnknown, "agy has no login status command", nil
		},
	},
}

// Detect inspects every supported runtime. It never fails as a whole; problems are
// reported per runtime.
func (d Detector) Detect(ctx context.Context) []RuntimeStatus {
	out := make([]RuntimeStatus, 0, len(probes))
	for _, p := range probes {
		out = append(out, d.inspect(ctx, p))
	}
	return out
}

func (d Detector) inspect(ctx context.Context, p probe) RuntimeStatus {
	st := RuntimeStatus{Runtime: p.runtime, Binary: p.binary, Login: LoginUnknown}
	path, err := d.LookPath(p.binary)
	if err != nil {
		st.Problem = "not installed (not found on PATH)"
		return st
	}
	st.Found = true
	st.Path = path

	res, err := d.run(ctx, path, p.versionArgs...)
	switch {
	case err != nil:
		st.Problem = fmt.Sprintf("version check failed: %v", err)
		return st
	case res.ExitCode != 0:
		st.Problem = fmt.Sprintf("version check exited with code %d", res.ExitCode)
		return st
	}
	st.Version = p.parseVersion(string(res.Stdout))

	state, detail, err := p.login(ctx, d, path)
	if err != nil {
		st.Login = LoginUnknown
		st.LoginDetail = err.Error()
		return st
	}
	st.Login, st.LoginDetail = state, detail
	return st
}

func (d Detector) run(ctx context.Context, path string, args ...string) (Result, error) {
	ctx, cancel := context.WithTimeout(ctx, d.Timeout)
	defer cancel()
	return d.Run(ctx, path, args...)
}

func claudeLogin(ctx context.Context, d Detector, path string) (state, detail string, err error) {
	res, err := d.run(ctx, path, "auth", "status")
	if err != nil {
		return "", "", fmt.Errorf("claude auth status: %w", err)
	}
	var status struct {
		LoggedIn   *bool  `json:"loggedIn"`
		AuthMethod string `json:"authMethod"`
	}
	if jerr := json.Unmarshal(res.Stdout, &status); jerr != nil || status.LoggedIn == nil {
		return "", "", fmt.Errorf("claude auth status: unexpected output (exit %d)", res.ExitCode)
	}
	if !*status.LoggedIn {
		return LoginMissing, "run `claude auth login`", nil
	}
	return LoginOK, status.AuthMethod, nil
}

func codexLogin(ctx context.Context, d Detector, path string) (state, detail string, err error) {
	res, err := d.run(ctx, path, "login", "status")
	if err != nil {
		return "", "", fmt.Errorf("codex login status: %w", err)
	}
	// codex prints its status ("Logged in using ChatGPT") on stderr.
	text := strings.TrimSpace(string(res.Stdout))
	if text == "" {
		text = strings.TrimSpace(string(res.Stderr))
	}
	if res.ExitCode != 0 {
		return LoginMissing, "run `codex login`", nil
	}
	return LoginOK, text, nil
}

func firstField(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[0]
	}
	return ""
}

func lastField(s string) string {
	if f := strings.Fields(s); len(f) > 0 {
		return f[len(f)-1]
	}
	return ""
}

func execRunner(ctx context.Context, path string, args ...string) (Result, error) {
	//nolint:gosec // G204: path comes from LookPath of a fixed binary name; args are constants.
	cmd := exec.CommandContext(ctx, path, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, nil
	case ctx.Err() != nil:
		return Result{}, fmt.Errorf("timed out: %w", ctx.Err())
	case errors.As(err, &exitErr):
		return Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: exitErr.ExitCode()}, nil
	default:
		return Result{}, fmt.Errorf("run %s: %w", path, err)
	}
}
