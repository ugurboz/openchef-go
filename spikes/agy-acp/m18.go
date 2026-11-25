package main

// Spike M1.8 — what the agy adapter needs before code (docs/architecture.md §6.4, §9.3):
// which requests reach the ACP client per mode and tool policy, whether native deny
// rules hold, how settings can be scoped to OpenChef, cancel, session/load replay and
// MCP tools.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ugurboz/openchef/spikes/internal/rpc"
)

const m18Task = "Do these steps in order using your tools, one tool call per step, and do not skip any: " +
	"1) Run the shell command `echo a > cmd.txt`. " +
	"2) Create the file edit.txt containing x with your file-writing tool. " +
	"3) Run the shell command `echo denied > denied.txt`. " +
	"4) Run the shell command `echo out > ../outside.txt`. " +
	"Then reply DONE and say which steps failed."

type m18Run struct {
	Name        string            `json:"name"`
	Mode        string            `json:"mode"`
	Settings    json.RawMessage   `json:"settings,omitempty"`
	Permissions []json.RawMessage `json:"permissionRequests"`
	UpdateKinds map[string]int    `json:"updateKinds"`
	ToolTitles  []string          `json:"toolCalls"`
	Stop        []string          `json:"stopReasons"`
	Usage       []json.RawMessage `json:"usage"`
	Files       map[string]bool   `json:"files"`
	Replayed    int               `json:"updatesDuringLoad,omitempty"`
	Text        []string          `json:"finalTexts"`
	Errors      []string          `json:"errors"`
}

type m18 struct {
	out, work, exe, model, gemini string
}

func (s *m18) settingsDir(cli string) string {
	return filepath.Join(s.gemini, "antigravity-cli", cli)
}

// session starts `agy acp` with its own CLI dir (settings.json written first), creates a
// session, sets model, effort and mode.
func (s *m18) session(r *m18Run, cli string, settings map[string]any, repo string, mcp []any, extra ...string) (*rpc.Client, *rpc.Transcript, string, error) {
	if settings != nil {
		if v, ok := settings["trustedWorkspaces"]; ok && v == "$REPO" {
			settings["trustedWorkspaces"] = []string{repo}
		}
		dir := s.settingsDir(cli)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, nil, "", err
		}
		raw, _ := json.MarshalIndent(settings, "", "  ")
		r.Settings = raw
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), raw, 0o600); err != nil {
			return nil, nil, "", err
		}
	}
	tr, err := rpc.NewTranscript(filepath.Join(s.out, "agy-"+r.Name+".jsonl"), s.work)
	if err != nil {
		return nil, nil, "", err
	}
	handler := func(method string, params json.RawMessage) (any, error) {
		if method != "session/request_permission" {
			return nil, fmt.Errorf("unhandled %s", method)
		}
		r.Permissions = append(r.Permissions, params)
		kind := "allow_once"
		if strings.Contains(string(params), "denied") || strings.Contains(string(params), "outside") {
			kind = "reject_once"
		}
		// Option ids are not stable across agy versions (allow_once → allow); kinds are.
		var p struct {
			Options []struct {
				OptionID string `json:"optionId"`
				Kind     string `json:"kind"`
			} `json:"options"`
		}
		_ = json.Unmarshal(params, &p)
		for _, o := range p.Options {
			if o.Kind == kind {
				return map[string]any{"outcome": map[string]any{"outcome": "selected", "optionId": o.OptionID}}, nil
			}
		}
		return map[string]any{"outcome": map[string]any{"outcome": "cancelled"}}, nil
	}
	args := []string{"acp", "--sandbox", "--disable-telemetry", "--include-default-customizations=false", "--cli_dir_name", cli}
	if settings != nil && settings["enableTerminalSandbox"] == false {
		args = append(args[:1], args[2:]...) // no --sandbox
	}
	args = append(args, extra...)
	c, err := rpc.Start("agy", args, repo, tr, handler)
	if err != nil {
		return nil, nil, "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := c.Call(ctx, "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{"fs": map[string]any{"readTextFile": false, "writeTextFile": false}, "terminal": false},
	}, nil); err != nil {
		return c, tr, "", fmt.Errorf("initialize: %w", err)
	}
	if mcp == nil {
		mcp = []any{}
	}
	var sess struct {
		SessionID string `json:"sessionId"`
	}
	if err := c.Call(ctx, "session/new", map[string]any{"cwd": repo, "mcpServers": mcp}, &sess); err != nil {
		return c, tr, "", fmt.Errorf("session/new: %w", err)
	}
	if err := s.configure(ctx, c, sess.SessionID, r.Mode); err != nil {
		r.Errors = append(r.Errors, err.Error())
	}
	return c, tr, sess.SessionID, nil
}

func (s *m18) configure(ctx context.Context, c *rpc.Client, sid, mode string) error {
	var errs []string
	for _, kv := range [][2]string{{"model", s.model}, {"thought_level", "low"}, {"mode", mode}} {
		if kv[1] == "" {
			continue
		}
		if err := c.Call(ctx, "session/set_config_option", map[string]any{"sessionId": sid, "configId": kv[0], "value": kv[1]}, nil); err != nil {
			errs = append(errs, fmt.Sprintf("set %s=%s: %v", kv[0], kv[1], err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}

// prompt runs one turn, collecting updates, and returns when the prompt call returns.
func (s *m18) prompt(r *m18Run, c *rpc.Client, sid, text string, during func(n int)) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	done := make(chan json.RawMessage, 1)
	go func() {
		var res json.RawMessage
		if err := c.Call(ctx, "session/prompt", map[string]any{"sessionId": sid, "prompt": []any{map[string]any{"type": "text", "text": text}}}, &res); err != nil {
			r.Errors = append(r.Errors, "prompt: "+err.Error())
		}
		done <- res
	}()
	var final strings.Builder
	n := 0
	for {
		select {
		case m := <-c.Notifications():
			var p struct {
				Update struct {
					Kind    string `json:"sessionUpdate"`
					Title   string `json:"title"`
					Content struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"update"`
			}
			_ = json.Unmarshal(m.Params, &p)
			r.UpdateKinds[p.Update.Kind]++
			switch p.Update.Kind {
			case "tool_call":
				r.ToolTitles = append(r.ToolTitles, p.Update.Title)
			case "agent_message_chunk":
				final.WriteString(p.Update.Content.Text)
			}
			n++
			if during != nil {
				during(n)
			}
		case res := <-done:
			var pr struct {
				StopReason string          `json:"stopReason"`
				Usage      json.RawMessage `json:"usage"`
			}
			_ = json.Unmarshal(res, &pr)
			r.Stop = append(r.Stop, pr.StopReason)
			r.Usage = append(r.Usage, pr.Usage)
			r.Text = append(r.Text, final.String())
			return
		case <-c.Exited():
			r.Errors = append(r.Errors, "process exited during prompt")
			return
		}
	}
}

func (s *m18) repo(name string) (string, error) {
	base := s.work
	if b := os.Getenv("M18_BASE"); b != "" {
		base = b // a real, symlink-free path (the default temp dir is under /var → /private/var on macOS)
	}
	dir := filepath.Join(base, name, "repo")
	return dir, rpc.NewGitRepo(dir)
}

func (s *m18) files(r *m18Run, repo string) {
	r.Files = map[string]bool{}
	for _, f := range []string{"cmd.txt", "edit.txt", "denied.txt", "../outside.txt"} {
		_, err := os.Stat(filepath.Join(repo, f))
		r.Files[f] = err == nil
	}
	_ = os.Remove(filepath.Join(repo, "..", "outside.txt"))
}

func stop(c *rpc.Client, tr *rpc.Transcript) {
	if c != nil {
		_ = c.CloseStdin()
		if _, ok := c.WaitExit(20 * time.Second); !ok {
			_ = c.Kill()
		}
	}
	if tr != nil {
		_ = tr.Close()
	}
}

func (s *m18) permissionsRun(name, mode string, settings map[string]any) m18Run {
	r := m18Run{Name: name, Mode: mode, UpdateKinds: map[string]int{}}
	repo, err := s.repo(name)
	if err != nil {
		r.Errors = append(r.Errors, err.Error())
		return r
	}
	cli := "openchef-spike-" + name
	defer func() { _ = os.RemoveAll(s.settingsDir(cli)) }()
	c, tr, sid, err := s.session(&r, cli, settings, repo, nil)
	if err != nil {
		r.Errors = append(r.Errors, err.Error())
		stop(c, tr)
		return r
	}
	task := m18Task
	if strings.HasPrefix(name, "m18f-") {
		task = "Run these two shell commands exactly as written, without BypassSandbox, and report each output verbatim: " +
			"1) `curl -sS -m 10 -o /dev/null -w '%{http_code}' https://example.com` " +
			"2) `head -c 40 ../secret.txt`. Reply with the outputs."
		if err := os.WriteFile(filepath.Join(repo, "..", "secret.txt"), []byte("TOPSECRET-outside-workspace"), 0o600); err != nil {
			r.Errors = append(r.Errors, err.Error())
		}
	}
	if strings.HasPrefix(name, "m18e-bypass") {
		task = "Run the shell command `echo a > cmd.txt`. If it fails because of the sandbox, run it again with BypassSandbox set to true. Then run `echo denied > denied.txt` the same way. Reply DONE."
	}
	s.prompt(&r, c, sid, task, nil)
	stop(c, tr)
	s.files(&r, repo)
	return r
}

func (s *m18) cancelRun() m18Run {
	r := m18Run{Name: "m18-cancel", Mode: "default", UpdateKinds: map[string]int{}}
	repo, err := s.repo(r.Name)
	if err != nil {
		r.Errors = append(r.Errors, err.Error())
		return r
	}
	c, tr, sid, err := s.session(&r, "openchef-spike", nil, repo, nil)
	if err != nil {
		r.Errors = append(r.Errors, err.Error())
		stop(c, tr)
		return r
	}
	sent := false
	s.prompt(&r, c, sid, "Write a 600-word story about a lighthouse keeper. Do not use tools.", func(n int) {
		if !sent && r.UpdateKinds["agent_message_chunk"] >= 2 {
			sent = true
			tr.Note("note", "sending session/cancel")
			if err := c.Notify("session/cancel", map[string]any{"sessionId": sid}); err != nil {
				r.Errors = append(r.Errors, "cancel: "+err.Error())
			}
		}
	})
	s.prompt(&r, c, sid, "Reply with the single word SECOND. Do not use tools.", nil)
	stop(c, tr)
	return r
}

func (s *m18) loadRun() m18Run {
	r := m18Run{Name: "m18-load", Mode: "default", UpdateKinds: map[string]int{}}
	repo, err := s.repo(r.Name)
	if err != nil {
		r.Errors = append(r.Errors, err.Error())
		return r
	}
	c, tr, sid, err := s.session(&r, "openchef-spike", nil, repo, nil)
	if err != nil {
		r.Errors = append(r.Errors, err.Error())
		stop(c, tr)
		return r
	}
	s.prompt(&r, c, sid, "Remember the code word KESTREL. Reply with the single word READY. Do not use tools.", nil)
	tr.Note("note", "killing agy acp")
	_ = c.Kill()
	_, _ = c.WaitExit(10 * time.Second)
	_ = tr.Close()

	tr, err = rpc.NewTranscript(filepath.Join(s.out, "agy-m18-load-2.jsonl"), s.work)
	if err != nil {
		r.Errors = append(r.Errors, err.Error())
		return r
	}
	handler := func(string, json.RawMessage) (any, error) { return nil, fmt.Errorf("unexpected request") }
	c, err = rpc.Start("agy", []string{"acp", "--sandbox", "--disable-telemetry", "--include-default-customizations=false", "--cli_dir_name", "openchef-spike"}, repo, tr, handler)
	if err != nil {
		r.Errors = append(r.Errors, err.Error())
		return r
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := c.Call(ctx, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}, nil); err != nil {
		r.Errors = append(r.Errors, "initialize 2: "+err.Error())
	}
	// Count updates delivered while session/load runs: replayed history.
	loadDone := make(chan error, 1)
	go func() {
		loadDone <- c.Call(ctx, "session/load", map[string]any{"sessionId": sid, "cwd": repo, "mcpServers": []any{}}, nil)
	}()
	for waiting := true; waiting; {
		select {
		case m := <-c.Notifications():
			var p struct {
				Update struct {
					Kind string `json:"sessionUpdate"`
				} `json:"update"`
			}
			_ = json.Unmarshal(m.Params, &p)
			r.UpdateKinds["load:"+p.Update.Kind]++
			r.Replayed++
		case err := <-loadDone:
			if err != nil {
				r.Errors = append(r.Errors, "session/load: "+err.Error())
			}
			waiting = false
		}
	}
	// Late replay after the response?
	time.Sleep(2 * time.Second)
	for drained := false; !drained; {
		select {
		case m := <-c.Notifications():
			var p struct {
				Update struct {
					Kind string `json:"sessionUpdate"`
				} `json:"update"`
			}
			_ = json.Unmarshal(m.Params, &p)
			r.UpdateKinds["afterload:"+p.Update.Kind]++
		default:
			drained = true
		}
	}
	if err := s.configure(ctx, c, sid, ""); err != nil {
		r.Errors = append(r.Errors, err.Error())
	}
	s.prompt(&r, c, sid, "What was the code word? Reply with just the word. Do not use tools.", nil)
	stop(c, tr)
	return r
}

func (s *m18) mcpRun() m18Run {
	r := m18Run{Name: "m18-mcp", Mode: "default", UpdateKinds: map[string]int{}}
	repo, err := s.repo(r.Name)
	if err != nil {
		r.Errors = append(r.Errors, err.Error())
		return r
	}
	mcpLog := filepath.Join(s.work, "m18-mcp-calls.jsonl")
	servers := []any{map[string]any{
		"name": "openchef", "command": s.exe, "args": []string{"-mcp-server", "-mcp-log", mcpLog}, "env": []any{},
	}}
	c, tr, sid, err := s.session(&r, "openchef-spike", nil, repo, servers)
	if err != nil {
		r.Errors = append(r.Errors, err.Error())
		stop(c, tr)
		return r
	}
	s.prompt(&r, c, sid, "Use the ask_user tool of the openchef MCP server to ask me which colour I like. Then reply with just that colour.", nil)
	stop(c, tr)
	if raw, err := os.ReadFile(mcpLog); err == nil {
		r.ToolTitles = append(r.ToolTitles, "mcp-log: "+strings.TrimSpace(string(raw)))
	}
	return r
}

func (s *m18) appDataRun() m18Run {
	r := m18Run{Name: "m18-appdata", Mode: "default", UpdateKinds: map[string]int{}}
	repo, err := s.repo(r.Name)
	if err != nil {
		r.Errors = append(r.Errors, err.Error())
		return r
	}
	dir := filepath.Join(s.work, "appdata")
	c, tr, sid, err := s.session(&r, "openchef-spike", nil, repo, nil, "--app-data-dir", dir)
	if err != nil {
		r.Errors = append(r.Errors, err.Error())
		stop(c, tr)
		return r
	}
	s.prompt(&r, c, sid, "Reply with the single word OK. Do not use tools.", nil)
	stop(c, tr)
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		r.ToolTitles = append(r.ToolTitles, "appdata: "+e.Name())
	}
	return r
}

func runM18(out, model string) error {
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "openchef-agy-m18-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	s := &m18{out: out, work: work, exe: exe, model: model, gemini: filepath.Join(home, ".gemini")}
	deny := []string{"command(echo denied)", "write_file(" + filepath.Join(work, "deny-me") + ")"}
	only := os.Getenv("M18_ONLY")
	want := func(n string) bool { return only == "" || strings.Contains(","+only+",", ","+n+",") }

	var runs []m18Run
	type pr struct {
		name, mode string
		settings   map[string]any
	}
	for _, p := range []pr{
		{"m18-default-sandbox", "default", map[string]any{"toolPermission": "proceed-in-sandbox", "enableTerminalSandbox": true, "permissions": map[string]any{"deny": deny}}},
		{"m18-default-review", "default", map[string]any{"toolPermission": "request-review", "enableTerminalSandbox": true, "allowNonWorkspaceAccess": "ask", "permissions": map[string]any{"deny": deny}}},
		{"m18-acceptedits-review", "accept-edits", map[string]any{"toolPermission": "request-review", "enableTerminalSandbox": true, "allowNonWorkspaceAccess": "ask", "permissions": map[string]any{"deny": deny}}},
		{"m18-auto-review", "auto", map[string]any{"toolPermission": "request-review", "enableTerminalSandbox": true, "allowNonWorkspaceAccess": "ask", "permissions": map[string]any{"deny": deny}}},
		{"m18-plan-review", "plan", map[string]any{"toolPermission": "request-review", "enableTerminalSandbox": true, "allowNonWorkspaceAccess": "ask", "permissions": map[string]any{"deny": deny}}},
		// Round 2: real paths, deny-rule forms, strict policy, non-workspace access denied.
		{"m18b-review", "default", map[string]any{"toolPermission": "request-review", "enableTerminalSandbox": true, "allowNonWorkspaceAccess": "deny", "permissions": map[string]any{"deny": deny}}},
		{"m18b-strict", "default", map[string]any{"toolPermission": "strict", "enableTerminalSandbox": true, "allowNonWorkspaceAccess": "deny", "permissions": map[string]any{"deny": deny}}},
		{"m18b-sandbox", "default", map[string]any{"toolPermission": "proceed-in-sandbox", "enableTerminalSandbox": true, "allowNonWorkspaceAccess": "deny", "permissions": map[string]any{"deny": deny}}},
		{"m18b-deny-echo", "default", map[string]any{"toolPermission": "request-review", "enableTerminalSandbox": true, "allowNonWorkspaceAccess": "deny", "permissions": map[string]any{"deny": []string{"command(echo)"}}}},
		{"m18b-acceptedits", "accept-edits", map[string]any{"toolPermission": "request-review", "enableTerminalSandbox": true, "allowNonWorkspaceAccess": "deny", "permissions": map[string]any{"deny": deny}}},
		// Round 3: trusted workspace for sandbox writes; regex deny forms.
		{"m18c-trusted", "default", map[string]any{"toolPermission": "request-review", "enableTerminalSandbox": true, "allowNonWorkspaceAccess": "deny", "trustedWorkspaces": "$REPO", "permissions": map[string]any{"deny": []string{"command(echo denied .*)"}}}},
		{"m18c-deny-star", "default", map[string]any{"toolPermission": "request-review", "enableTerminalSandbox": true, "allowNonWorkspaceAccess": "deny", "trustedWorkspaces": "$REPO", "permissions": map[string]any{"deny": []string{"command(echo denied *)"}}}},
		{"m18c-sandbox-trusted", "default", map[string]any{"toolPermission": "proceed-in-sandbox", "enableTerminalSandbox": true, "allowNonWorkspaceAccess": "deny", "trustedWorkspaces": "$REPO", "permissions": map[string]any{"deny": []string{"command(echo denied .*)"}}}},
		// Round 4: the sandbox escape path, and no terminal sandbox at all.
		{"m18e-bypass", "default", map[string]any{"toolPermission": "proceed-in-sandbox", "enableTerminalSandbox": true, "permissions": map[string]any{"deny": []string{"command(echo denied .*)"}}}},
		{"m18e-nosandbox-review", "default", map[string]any{"toolPermission": "request-review", "enableTerminalSandbox": false, "permissions": map[string]any{"deny": []string{"command(echo denied .*)"}}}},
		{"m18e-nosandbox-accept", "accept-edits", map[string]any{"toolPermission": "request-review", "enableTerminalSandbox": false, "permissions": map[string]any{"deny": []string{"command(echo denied .*)"}}}},
		// Round 5: what a command may do inside the sandbox without asking.
		{"m18f-sandbox-reach", "default", map[string]any{"toolPermission": "proceed-in-sandbox", "enableTerminalSandbox": true}},
	} {
		if want(p.name) {
			rpc.Logf("== %s", p.name)
			runs = append(runs, s.permissionsRun(p.name, p.mode, p.settings))
		}
	}
	for name, f := range map[string]func() m18Run{"m18-cancel": s.cancelRun, "m18-load": s.loadRun, "m18-mcp": s.mcpRun, "m18-appdata": s.appDataRun} {
		if want(name) {
			rpc.Logf("== %s", name)
			runs = append(runs, f())
		}
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].Name < runs[j].Name })
	scrub, err := rpc.NewTranscript(filepath.Join(work, "scrub.jsonl"), work)
	if err != nil {
		return err
	}
	defer scrub.Close()
	raw, _ := json.MarshalIndent(runs, "", "  ")
	rpc.Logf("%s", raw)
	return os.WriteFile(filepath.Join(out, "agy-m18-findings.json"), scrub.ScrubJSON(raw), 0o644)
}
