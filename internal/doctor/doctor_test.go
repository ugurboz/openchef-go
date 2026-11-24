package doctor

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"
)

type fakeCmd struct {
	out    string
	stderr string
	code   int
	err    error
}

// fakeDetector answers LookPath from installed and Run from a "path args..." table.
func fakeDetector(installed map[string]string, cmds map[string]fakeCmd) Detector {
	return Detector{
		LookPath: func(bin string) (string, error) {
			if p, ok := installed[bin]; ok {
				return p, nil
			}
			return "", exec.ErrNotFound
		},
		Run: func(_ context.Context, path string, args ...string) (Result, error) {
			key := path + " " + strings.Join(args, " ")
			c, ok := cmds[key]
			if !ok {
				return Result{}, errors.New("unexpected command: " + key)
			}
			return Result{Stdout: []byte(c.out), Stderr: []byte(c.stderr), ExitCode: c.code}, c.err
		},
		Timeout: time.Second,
	}
}

func byRuntime(statuses []RuntimeStatus) map[string]RuntimeStatus {
	m := map[string]RuntimeStatus{}
	for _, s := range statuses {
		m[s.Runtime] = s
	}
	return m
}

func TestDetectAllInstalledAndLoggedIn(t *testing.T) {
	t.Parallel()
	d := fakeDetector(
		map[string]string{"claude": "/bin/claude", "codex": "/bin/codex", "agy": "/bin/agy"},
		map[string]fakeCmd{
			"/bin/claude --version":   {out: "2.1.325 (Claude Code)\n"},
			"/bin/claude auth status": {out: `{"loggedIn": true, "authMethod": "claude.ai"}`},
			"/bin/codex --version":    {out: "codex-cli 0.172.0\n"},
			"/bin/codex login status": {stderr: "Logged in using ChatGPT\n"},
			"/bin/agy --version":      {out: "1.5.16\n"},
		},
	)
	got := byRuntime(d.Detect(context.Background()))

	want := map[string]RuntimeStatus{
		"claude": {Runtime: "claude", Binary: "claude", Found: true, Path: "/bin/claude", Version: "2.1.325", Login: LoginOK, LoginDetail: "claude.ai"},
		"codex":  {Runtime: "codex", Binary: "codex", Found: true, Path: "/bin/codex", Version: "0.172.0", Login: LoginOK, LoginDetail: "Logged in using ChatGPT"},
		"agy":    {Runtime: "agy", Binary: "agy", Found: true, Path: "/bin/agy", Version: "1.5.16", Login: LoginUnknown, LoginDetail: "agy has no login status command"},
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s:\n got  %+v\n want %+v", name, got[name], w)
		}
		if !got[name].Usable() {
			t.Errorf("%s should be usable", name)
		}
	}
}

func TestDetectProblems(t *testing.T) {
	t.Parallel()
	d := fakeDetector(
		map[string]string{"claude": "/bin/claude", "codex": "/bin/codex"},
		map[string]fakeCmd{
			"/bin/claude --version":   {out: "2.1.325 (Claude Code)\n"},
			"/bin/claude auth status": {out: `{"loggedIn": false}`, code: 1},
			"/bin/codex --version":    {code: 127},
		},
	)
	got := byRuntime(d.Detect(context.Background()))

	if s := got["claude"]; s.Login != LoginMissing || s.Usable() {
		t.Errorf("claude logged out: got %+v", s)
	}
	if s := got["codex"]; s.Problem == "" || s.Usable() {
		t.Errorf("codex broken version: got %+v", s)
	}
	if s := got["agy"]; s.Found || s.Problem == "" || s.Usable() {
		t.Errorf("agy missing: got %+v", s)
	}
}

func TestClaudeStatusUnexpectedOutput(t *testing.T) {
	t.Parallel()
	d := fakeDetector(
		map[string]string{"claude": "/bin/claude"},
		map[string]fakeCmd{
			"/bin/claude --version":   {out: "2.1.325 (Claude Code)"},
			"/bin/claude auth status": {out: "not json"},
		},
	)
	s := byRuntime(d.Detect(context.Background()))["claude"]
	if s.Login != LoginUnknown || !strings.Contains(s.LoginDetail, "unexpected output") {
		t.Fatalf("got %+v", s)
	}
	if !s.Usable() {
		t.Fatal("unknown login state must not make the runtime unusable")
	}
}

func TestCommandFailureIsReported(t *testing.T) {
	t.Parallel()
	d := fakeDetector(
		map[string]string{"claude": "/bin/claude"},
		map[string]fakeCmd{"/bin/claude --version": {err: errors.New("timed out")}},
	)
	s := byRuntime(d.Detect(context.Background()))["claude"]
	if !s.Found || !strings.Contains(s.Problem, "timed out") {
		t.Fatalf("got %+v", s)
	}
}

func TestExecRunner(t *testing.T) {
	t.Parallel()
	self, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go binary not on PATH")
	}
	res, err := execRunner(context.Background(), self, "version")
	if err != nil || res.ExitCode != 0 || !strings.HasPrefix(string(res.Stdout), "go version") {
		t.Fatalf("go version: %+v, %v", res, err)
	}
	res, err = execRunner(context.Background(), self, "no-such-subcommand")
	if err != nil || res.ExitCode == 0 {
		t.Fatalf("non-zero exit should be a result, not an error: %+v, %v", res, err)
	}
	if _, err := execRunner(context.Background(), "/definitely/not/here"); err == nil {
		t.Fatal("missing binary should be an error")
	}
}
