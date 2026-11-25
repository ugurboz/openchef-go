package agy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSettingsFile(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	r := New(Options{})
	if err := r.writeSettings([]string{"HOME=" + home, "PATH=/bin"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, ".gemini", "antigravity-cli", "openchef", "settings.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got["toolPermission"] != "proceed-in-sandbox" || got["enableTerminalSandbox"] != true || len(got) != 2 {
		t.Fatalf("settings %v", got)
	}
	info, _ := os.Stat(path)
	// Rewriting identical content is a no-op; a changed file is restored.
	if err := r.writeSettings([]string{"HOME=" + home}); err != nil {
		t.Fatal(err)
	}
	again, _ := os.Stat(path)
	if !again.ModTime().Equal(info.ModTime()) {
		t.Error("identical settings were rewritten")
	}
	if err := os.WriteFile(path, []byte(`{"toolPermission":"always-proceed"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := r.writeSettings([]string{"HOME=" + home}); err != nil {
		t.Fatal(err)
	}
	if raw, _ := os.ReadFile(path); !strings.Contains(string(raw), "proceed-in-sandbox") {
		t.Fatalf("tampered settings not restored: %s", raw)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("temp files left behind: %v", entries)
	}
	custom := filepath.Join(t.TempDir(), "agy-settings")
	if err := New(Options{SettingsDir: custom}).writeSettings(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(custom, "settings.json")); err != nil {
		t.Errorf("SettingsDir ignored: %v", err)
	}
}

func TestRefuseTempDir(t *testing.T) {
	t.Parallel()
	if err := refuseTempDir(t.TempDir()); err == nil {
		t.Error("a workspace in the temp tree was accepted")
	}
	if err := refuseTempDir(os.TempDir()); err == nil {
		t.Error("the temp root itself was accepted")
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := refuseTempDir(wd); err != nil {
		t.Errorf("the repository was refused: %v", err)
	}
	if err := refuseTempDir("/tmpfoo/x"); err != nil {
		t.Errorf("a sibling of /tmp was refused: %v", err)
	}
}

func TestDefaults(t *testing.T) {
	t.Parallel()
	r := New(Options{})
	if r.Name() != Name || r.opts.Binary != "agy" || r.opts.CLIDirName != "openchef" || len(r.opts.Args) != 1 || r.opts.Args[0] != "acp" {
		t.Fatalf("%+v", r.opts)
	}
}
