package rpc

import (
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Transcript records protocol traffic as JSONL with personal data removed, so recordings
// can be committed as test fixtures.
type Transcript struct {
	mu    sync.Mutex
	f     *os.File
	start time.Time
	repl  *strings.Replacer
}

// Keys whose values are replaced wherever they appear in a JSON object.
var redactKeys = map[string]bool{
	"installationId": true, "accountId": true, "account_id": true, "userId": true,
	"user_id": true, "email": true, "token": true, "access_token": true,
	"refresh_token": true, "id_token": true, "userAgent": true,
	"organizationId": true, "orgId": true,
}

var (
	emailRE = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	// Claude encodes paths into directory names, e.g. "-private-var-folders-...".
	encodedTempRE = regexp.MustCompile(`-(private-)?var-folders-[A-Za-z0-9_-]+`)
)

// NewTranscript creates path. Absolute machine paths are rewritten: home → $HOME,
// the per-run work directory → $WORK.
func NewTranscript(path, workDir string) (*Transcript, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("create transcript: %w", err)
	}
	home, _ := os.UserHomeDir()
	host, _ := os.Hostname()
	pairs := []string{}
	if workDir != "" {
		pairs = append(pairs, workDir, "$WORK")
		if real, err := evalSymlinks(workDir); err == nil && real != workDir {
			pairs = append(pairs, real, "$WORK")
		}
	}
	if exe, err := os.Executable(); err == nil {
		pairs = append(pairs, filepath.Dir(exe), "$BIN")
	}
	if home != "" {
		pairs = append(pairs, home, "$HOME")
	}
	if u, err := user.Current(); err == nil && len(u.Username) >= 4 {
		pairs = append(pairs, u.Username, "USER")
	}
	if host != "" {
		pairs = append(pairs, host, "HOST")
	}
	return &Transcript{f: f, start: time.Now(), repl: strings.NewReplacer(pairs...)}, nil
}

// Line records one raw protocol line.
func (t *Transcript) Line(dir string, raw []byte) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.write(dir, nil, t.scrubString(string(raw)))
		return
	}
	t.write(dir, scrubValue(v, t), "")
}

// Note records a free-text event (stderr line, observation).
func (t *Transcript) Note(kind, text string) {
	t.write(kind, nil, t.scrubString(text))
}

func (t *Transcript) write(dir string, msg any, text string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rec := map[string]any{"ms": time.Since(t.start).Milliseconds(), "dir": dir}
	if msg != nil {
		rec["msg"] = msg
	}
	if text != "" {
		rec["text"] = text
	}
	raw, _ := json.Marshal(rec)
	_, _ = t.f.Write(append(raw, '\n'))
}

// ScrubJSON applies the transcript's redaction rules to an arbitrary JSON document.
func (t *Transcript) ScrubJSON(raw []byte) []byte {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return []byte(t.scrubString(string(raw)))
	}
	out, err := json.MarshalIndent(scrubValue(v, t), "", "  ")
	if err != nil {
		return []byte(t.scrubString(string(raw)))
	}
	return out
}

// Close flushes the transcript.
func (t *Transcript) Close() error { return t.f.Close() }

func (t *Transcript) scrubString(s string) string {
	s = encodedTempRE.ReplaceAllString(t.repl.Replace(s), "-WORK")
	return emailRE.ReplaceAllString(s, "user@example.com")
}

func scrubValue(v any, t *Transcript) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if redactKeys[k] {
				if val != nil {
					x[k] = "REDACTED"
				}
				continue
			}
			x[k] = scrubValue(val, t)
		}
		return x
	case []any:
		for i := range x {
			x[i] = scrubValue(x[i], t)
		}
		return x
	case string:
		return t.scrubString(x)
	default:
		return v
	}
}
