package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ugurboz/openchef/internal/buildinfo"
)

func run(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	cmd := NewRootCommand(IO{Out: &out, Err: &errOut})
	cmd.SetArgs(args)
	err = cmd.Execute()
	return out.String(), errOut.String(), err
}

func TestVersionText(t *testing.T) {
	t.Parallel()
	out, errOut, err := run(t, "version")
	if err != nil {
		t.Fatalf("version: %v", err)
	}
	if !strings.HasPrefix(out, "openchef ") {
		t.Fatalf("unexpected output %q", out)
	}
	if errOut != "" {
		t.Fatalf("version must not write to stderr, got %q", errOut)
	}
}

func TestVersionJSON(t *testing.T) {
	t.Parallel()
	out, _, err := run(t, "version", "--json")
	if err != nil {
		t.Fatalf("version --json: %v", err)
	}
	var info buildinfo.Info
	if err := json.Unmarshal([]byte(out), &info); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if info.Version == "" || info.GoVersion == "" || info.Platform == "" {
		t.Fatalf("missing fields: %+v", info)
	}
}

func TestVersionRejectsArgs(t *testing.T) {
	t.Parallel()
	if _, _, err := run(t, "version", "extra"); err == nil {
		t.Fatal("expected error for unexpected argument")
	}
}

func TestRootWithoutArgsPrintsHelp(t *testing.T) {
	t.Parallel()
	out, _, err := run(t)
	if err != nil {
		t.Fatalf("root: %v", err)
	}
	if !strings.Contains(out, "Usage:") {
		t.Fatalf("expected help output, got %q", out)
	}
}

func TestUnknownCommandFails(t *testing.T) {
	t.Parallel()
	if _, _, err := run(t, "no-such-command"); err == nil {
		t.Fatal("expected error for unknown command")
	}
}
