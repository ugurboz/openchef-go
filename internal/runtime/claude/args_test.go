package claude

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/ugurboz/openchef/internal/runtime"
)

// flag returns the value following name in args, and whether name occurs.
func flag(args []string, name string) (string, bool) {
	i := slices.Index(args, name)
	if i < 0 {
		return "", false
	}
	if i+1 < len(args) {
		return args[i+1], true
	}
	return "", true
}

func TestModeMapping(t *testing.T) {
	t.Parallel()
	all := strings.Join(defaultTools, ",")
	cases := []struct {
		mode  runtime.Mode
		want  string
		tools string
	}{
		{"", "manual", all},
		{runtime.ModeManual, "manual", all},
		{runtime.ModeAcceptEdits, "acceptEdits", all},
		// D-008: --restricted refuses bypassPermissions; the approver allows instead.
		{runtime.ModeBypassPermissions, "acceptEdits", all},
		{runtime.ModeAuto, "auto", all},
		{runtime.ModeDontAsk, "dontAsk", all},
		// D-008: Claude's plan mode switches models; OpenChef's is read-only dontAsk.
		{runtime.ModePlan, "dontAsk", "Read,Glob,Grep"},
	}
	for _, c := range cases {
		args, err := buildArgs(runtime.SessionSpec{Mode: c.mode}, launch{sessionID: "id"})
		if err != nil {
			t.Fatalf("%q: %v", c.mode, err)
		}
		if got, _ := flag(args, "--permission-mode"); got != c.want {
			t.Errorf("%q: --permission-mode %q, want %q", c.mode, got, c.want)
		}
		if got, _ := flag(args, "--tools"); got != c.tools {
			t.Errorf("%q: --tools %q, want %q", c.mode, got, c.tools)
		}
	}
	if _, err := buildArgs(runtime.SessionSpec{Mode: "yolo"}, launch{}); err == nil {
		t.Fatal("unknown mode accepted")
	}
}

func TestIsolationFlagsAlwaysPresent(t *testing.T) {
	t.Parallel()
	for _, m := range runtime.Modes {
		args, err := buildArgs(runtime.SessionSpec{Mode: m}, launch{sessionID: "id"})
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"--restricted", "--strict-mcp-config", "-p", "--verbose"} {
			if !slices.Contains(args, f) {
				t.Errorf("%s: missing %s", m, f)
			}
		}
		if v, _ := flag(args, "--permission-prompt-tool"); v != "stdio" {
			t.Errorf("%s: --permission-prompt-tool %q", m, v)
		}
		if v, _ := flag(args, "--disallowedTools"); v != "AskUserQuestion" {
			t.Errorf("%s: --disallowedTools %q", m, v)
		}
		tools, _ := flag(args, "--tools")
		for _, bad := range []string{"Task", "WebFetch", "WebSearch", "CronCreate", "Monitor", "RemoteTrigger"} {
			if slices.Contains(strings.Split(tools, ","), bad) {
				t.Errorf("%s: %s enabled by default", m, bad)
			}
		}
	}
}

func TestWebAccessAddsWebTools(t *testing.T) {
	t.Parallel()
	args, err := buildArgs(runtime.SessionSpec{WebAccess: true}, launch{sessionID: "id"})
	if err != nil {
		t.Fatal(err)
	}
	tools, _ := flag(args, "--tools")
	if !strings.Contains(tools, "WebFetch") || !strings.Contains(tools, "WebSearch") {
		t.Fatalf("--tools %q", tools)
	}
}

func TestIdentityModelEffortSchema(t *testing.T) {
	t.Parallel()
	spec := runtime.SessionSpec{Model: "haiku", Effort: runtime.EffortLow}
	args, err := buildArgs(spec, launch{sessionID: "abc", schema: json.RawMessage(`{"type":"object"}`), mcpConfig: "/tmp/m.json"})
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{
		"--session-id": "abc", "--model": "haiku", "--effort": "low",
		"--json-schema": `{"type":"object"}`, "--mcp-config": "/tmp/m.json",
	} {
		if got, _ := flag(args, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if _, ok := flag(args, "--resume"); ok {
		t.Error("--resume on a new session")
	}

	args, err = buildArgs(runtime.SessionSpec{}, launch{sessionID: "abc", resume: true})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := flag(args, "--resume"); got != "abc" {
		t.Errorf("--resume %q", got)
	}
	for _, f := range []string{"--session-id", "--model", "--effort", "--json-schema", "--mcp-config"} {
		if _, ok := flag(args, f); ok {
			t.Errorf("unexpected %s", f)
		}
	}
}

func TestSettingsRules(t *testing.T) {
	t.Parallel()
	spec := runtime.SessionSpec{
		DenyRules: []string{"Bash(rm:*)", "Edit(**/.git/**)", ""},
		AskRules:  []string{"Bash(npm publish:*)", "Bash(npm publish:*)"},
		AskUser:   &runtime.MCPServer{Name: "openchef", Command: "openchef"},
	}
	raw, err := settingsJSON(spec)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Permissions struct {
			Deny  []string `json:"deny"`
			Ask   []string `json:"ask"`
			Allow []string `json:"allow"`
		} `json:"permissions"`
	}
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	p := got.Permissions
	for _, r := range runtime.InvariantDenyRules {
		if !slices.Contains(p.Deny, r) {
			t.Errorf("invariant %s missing", r)
		}
	}
	if !slices.Contains(p.Deny, "Bash(rm:*)") || slices.Contains(p.Deny, "") {
		t.Errorf("deny %v", p.Deny)
	}
	if n := len(p.Deny); n != len(runtime.InvariantDenyRules)+1 {
		t.Errorf("deny not deduplicated: %d rules", n)
	}
	if !slices.Equal(p.Ask, []string{"Bash(npm publish:*)"}) {
		t.Errorf("ask %v", p.Ask)
	}
	if !slices.Equal(p.Allow, []string{"mcp__openchef__ask_user"}) {
		t.Errorf("allow %v", p.Allow)
	}

	raw, err = settingsJSON(runtime.SessionSpec{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, `"ask"`) || strings.Contains(raw, `"allow"`) {
		t.Errorf("empty spec settings: %s", raw)
	}
}

func TestMCPConfig(t *testing.T) {
	t.Parallel()
	raw, err := mcpConfigJSON(&runtime.MCPServer{Name: "openchef", Command: "/bin/oc", Args: []string{"mcp"}, Env: []string{"A=1", "B=x=y", "bad"}})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	srv := got.MCPServers["openchef"]
	if srv.Command != "/bin/oc" || !slices.Equal(srv.Args, []string{"mcp"}) || srv.Env["A"] != "1" || srv.Env["B"] != "x=y" || len(srv.Env) != 2 {
		t.Fatalf("%+v", srv)
	}
}

func TestNewUUID(t *testing.T) {
	t.Parallel()
	a, err := newUUID()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := newUUID()
	if a == b || len(a) != 36 || a[14] != '4' || !strings.ContainsAny(a[19:20], "89ab") {
		t.Fatalf("bad uuid %s / %s", a, b)
	}
}
