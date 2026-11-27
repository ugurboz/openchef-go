package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/config"
	"github.com/ugurboz/openchef/internal/doctor"
)

// fakeEnv simulates installed CLIs. loggedIn lists runtimes that report a login.
func fakeEnv(t *testing.T, installed, loggedIn []string) (Deps, config.Paths) {
	t.Helper()
	paths, err := config.PathsIn(filepath.Join(t.TempDir(), "home"))
	if err != nil {
		t.Fatal(err)
	}
	has := func(list []string, s string) bool {
		for _, x := range list {
			if x == s {
				return true
			}
		}
		return false
	}
	det := doctor.Detector{
		Timeout: time.Second,
		LookPath: func(bin string) (string, error) {
			if has(installed, bin) {
				return "/fake/" + bin, nil
			}
			return "", exec.ErrNotFound
		},
		Run: func(_ context.Context, path string, args ...string) (doctor.Result, error) {
			bin := filepath.Base(path)
			switch strings.Join(args, " ") {
			case "--version":
				return doctor.Result{Stdout: []byte("1.0.0")}, nil
			case "auth status":
				if has(loggedIn, bin) {
					return doctor.Result{Stdout: []byte(`{"loggedIn":true,"authMethod":"claude.ai"}`)}, nil
				}
				return doctor.Result{Stdout: []byte(`{"loggedIn":false}`), ExitCode: 1}, nil
			case "login status":
				if has(loggedIn, bin) {
					return doctor.Result{Stdout: []byte("Logged in using ChatGPT")}, nil
				}
				return doctor.Result{Stdout: []byte("Not logged in"), ExitCode: 1}, nil
			}
			return doctor.Result{}, errors.New("unexpected command")
		},
	}
	return Deps{Detector: det, Paths: func() (config.Paths, error) { return paths, nil }}, paths
}

func runWith(t *testing.T, deps Deps, args ...string) (stdout string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := NewRootCommandWith(IO{Out: &out, Err: &errOut}, deps)
	cmd.SetArgs(args)
	err = cmd.ExecuteContext(context.Background())
	return out.String(), err
}

func TestDoctorWithoutConfig(t *testing.T) {
	t.Parallel()
	deps, _ := fakeEnv(t, []string{"claude", "codex"}, []string{"claude"})
	out, err := runWith(t, deps, "doctor")
	if err != nil {
		t.Fatalf("doctor: %v\n%s", err, out)
	}
	for _, want := range []string{"not found — run `openchef init`", "claude", "logged in (claude.ai)", "NOT logged in", "not installed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}

func TestDoctorUnhealthyWhenNothingUsable(t *testing.T) {
	t.Parallel()
	deps, _ := fakeEnv(t, nil, nil)
	out, err := runWith(t, deps, "doctor")
	if !errors.Is(err, ErrUnhealthy) {
		t.Fatalf("got %v, want ErrUnhealthy", err)
	}
	if !strings.Contains(out, "install and log in") {
		t.Errorf("missing hint:\n%s", out)
	}
}

func TestDoctorReportsInvalidConfig(t *testing.T) {
	t.Parallel()
	deps, paths := fakeEnv(t, []string{"claude"}, []string{"claude"})
	if err := os.MkdirAll(paths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.Config, []byte("[pools.x]\nprovider = \"acme\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runWith(t, deps, "doctor", "--json")
	if !errors.Is(err, ErrUnhealthy) {
		t.Fatalf("got %v, want ErrUnhealthy", err)
	}
	var report doctorReport
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out)
	}
	if report.Config.Status != configInvalid || len(report.Config.Problems) == 0 || report.Healthy {
		t.Fatalf("unexpected report: %+v", report)
	}
}

func TestInitWritesValidConfigAndDoctorAccepts(t *testing.T) {
	t.Parallel()
	deps, paths := fakeEnv(t, []string{"claude", "codex", "agy"}, []string{"claude", "codex"})
	out, err := runWith(t, deps, "init", "--plan", "claude=max")
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Wrote "+paths.Config) || !strings.Contains(out, "claude=max") {
		t.Errorf("unexpected output:\n%s", out)
	}
	cfg, err := config.Load(paths.Config)
	if err != nil {
		t.Fatalf("generated config does not load: %v", err)
	}
	if len(cfg.Pools) != 3 {
		t.Errorf("want pools for claude, codex and agy (login unknown is usable), got %v", cfg.Pools)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(paths.Config)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Errorf("config mode = %v, %v; want 0600", info.Mode().Perm(), err)
		}
	}
	if out, err := runWith(t, deps, "doctor"); err != nil || !strings.Contains(out, "valid ("+paths.Config+")") {
		t.Errorf("doctor after init: %v\n%s", err, out)
	}
}

func TestInitSkipsUnusableRuntimes(t *testing.T) {
	t.Parallel()
	deps, paths := fakeEnv(t, []string{"claude", "codex"}, []string{"codex"})
	out, err := runWith(t, deps, "init")
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	if !strings.Contains(out, "Skipped:  claude") {
		t.Errorf("claude should be reported as skipped:\n%s", out)
	}
	cfg, err := config.Load(paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Agents["opus"]; ok {
		t.Error("agents generated for a logged-out runtime")
	}
}

func TestInitRefusesOverwriteUnlessForced(t *testing.T) {
	t.Parallel()
	deps, paths := fakeEnv(t, []string{"claude"}, []string{"claude"})
	if _, err := runWith(t, deps, "init"); err != nil {
		t.Fatal(err)
	}
	if _, err := runWith(t, deps, "init"); !errors.Is(err, ErrConfigExists) {
		t.Fatalf("second init: got %v, want ErrConfigExists", err)
	}
	if _, err := runWith(t, deps, "init", "--force", "--plan", "claude=ultra"); err != nil {
		t.Fatalf("forced init: %v", err)
	}
	cfg, err := config.Load(paths.Config)
	if err != nil || cfg.Pools["claude"].PlanTier != "ultra" {
		t.Fatalf("forced overwrite not applied: %+v, %v", cfg, err)
	}
}

func TestInitDryRunWritesNothing(t *testing.T) {
	t.Parallel()
	deps, paths := fakeEnv(t, []string{"claude"}, []string{"claude"})
	out, err := runWith(t, deps, "init", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[pools.claude]") {
		t.Errorf("dry run should print the config:\n%s", out)
	}
	if _, err := os.Stat(paths.Config); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry run wrote a file (stat err=%v)", err)
	}
}

func TestInitErrors(t *testing.T) {
	t.Parallel()
	none, _ := fakeEnv(t, nil, nil)
	if _, err := runWith(t, none, "init"); !errors.Is(err, ErrNoUsableRuntimes) {
		t.Errorf("no runtimes: got %v", err)
	}
	deps, _ := fakeEnv(t, []string{"claude"}, []string{"claude"})
	for _, bad := range []string{"cursor=pro", "claude=gold"} {
		if _, err := runWith(t, deps, "init", "--plan", bad); err == nil {
			t.Errorf("--plan %s: expected error", bad)
		}
	}
}

func TestInitRecordsProbeConsent(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{nil, true}, // not a terminal: the default, written down
		{[]string{"--quota-probes", "off"}, false},
		{[]string{"--quota-probes", "on"}, true},
	} {
		deps, paths := fakeEnv(t, []string{"claude"}, []string{"claude"})
		out, err := runWith(t, deps, append([]string{"init"}, tc.args...)...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", tc.args, err, out)
		}
		cfg, err := config.Load(paths.Config)
		if err != nil || cfg.Quota.Probes == nil || *cfg.Quota.Probes != tc.want || cfg.Quota.ProbesEnabled("anthropic") != tc.want {
			t.Errorf("%v: probes %v, %v", tc.args, cfg.Quota.Probes, err)
		}
		if !strings.Contains(out, "Probes:") {
			t.Errorf("%v: output does not say:\n%s", tc.args, out)
		}
	}
	deps, _ := fakeEnv(t, []string{"claude"}, []string{"claude"})
	if _, err := runWith(t, deps, "init", "--quota-probes", "maybe"); err == nil {
		t.Error("bad --quota-probes accepted")
	}
}
