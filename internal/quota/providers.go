package quota

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"time"
)

// Probe endpoints (§10.1). Both are undocumented; see §10.2 for the risk policy.
const (
	claudeUsageURL  = "https://api.anthropic.com/api/oauth/usage"
	claudeBeta      = "oauth-2025-04-20"
	chatgptUsageURL = "https://chatgpt.com/backend-api/wham/usage"
)

// Credentials reads a CLI's stored login. Implementations must only read: OpenChef
// never refreshes, stores, logs or forwards tokens.
type Credentials func(ctx context.Context) (token, account string, err error)

// ClaudeProbe probes Claude's subscription usage with Claude Code's OAuth login.
func ClaudeProbe(pool string, creds Credentials, client *http.Client, now func() time.Time) *Prober {
	if now == nil {
		now = time.Now
	}
	fetch := httpGet(client, claudeUsageURL, func() (http.Header, error) {
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		defer cancel()
		token, _, err := creds(ctx)
		if err != nil {
			return nil, err
		}
		h := http.Header{}
		h.Set("Authorization", "Bearer "+token)
		h.Set("anthropic-beta", claudeBeta)
		return h, nil
	})
	return &Prober{Pool: pool, name: "claude", fetch: fetch, parse: parseClaude, now: now, enabled: true}
}

// parseClaude reads five_hour and seven_day: utilization as a percentage, resets as
// RFC 3339 (phase-0 findings §1). Every other field is ignored; a missing window
// object, or one without the two fields, is a shape change.
func parseClaude(body []byte, at time.Time) (Observation, error) {
	var raw struct {
		FiveHour *claudeWindow `json:"five_hour"`
		SevenDay *claudeWindow `json:"seven_day"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Observation{}, fmt.Errorf("decode: %w", err)
	}
	if raw.FiveHour == nil && raw.SevenDay == nil {
		return Observation{}, errors.New("neither five_hour nor seven_day present")
	}
	obs := Observation{At: at}
	for _, w := range []struct {
		name     string
		duration time.Duration
		v        *claudeWindow
	}{{"five_hour", 5 * time.Hour, raw.FiveHour}, {"weekly", 7 * 24 * time.Hour, raw.SevenDay}} {
		if w.v == nil {
			continue
		}
		win, err := w.v.window(w.name, w.duration)
		if err != nil {
			return Observation{}, err
		}
		obs.Windows = append(obs.Windows, win)
	}
	return obs, nil
}

type claudeWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

func (c *claudeWindow) window(name string, d time.Duration) (Window, error) {
	if c.Utilization == nil {
		return Window{}, fmt.Errorf("%s.utilization missing", name)
	}
	w := Window{Name: name, Utilization: clamp(*c.Utilization / 100), Duration: d}
	if c.ResetsAt != nil && *c.ResetsAt != "" {
		t, err := time.Parse(time.RFC3339Nano, *c.ResetsAt)
		if err != nil {
			return Window{}, fmt.Errorf("%s.resets_at: %w", name, err)
		}
		w.ResetsAt = t
	}
	return w, nil
}

// ChatGPTProbe probes ChatGPT plan usage with Codex's stored login. The Codex
// app-server's account/rateLimits/read is preferred where a session is running; this
// is the fallback (§10.2).
func ChatGPTProbe(pool string, creds Credentials, client *http.Client, now func() time.Time) *Prober {
	if now == nil {
		now = time.Now
	}
	fetch := httpGet(client, chatgptUsageURL, func() (http.Header, error) {
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		defer cancel()
		token, account, err := creds(ctx)
		if err != nil {
			return nil, err
		}
		h := http.Header{}
		h.Set("Authorization", "Bearer "+token)
		if account != "" {
			h.Set("ChatGPT-Account-Id", account)
		}
		return h, nil
	})
	return &Prober{Pool: pool, name: "chatgpt", fetch: fetch, parse: parseChatGPT, now: now, enabled: true}
}

// parseChatGPT reads rate_limit.{limit_reached, primary_window, secondary_window}; each
// window has used_percent, limit_window_seconds and reset_at (epoch seconds).
func parseChatGPT(body []byte, at time.Time) (Observation, error) {
	var raw struct {
		RateLimit *struct {
			LimitReached *bool          `json:"limit_reached"`
			Primary      *chatgptWindow `json:"primary_window"`
			Secondary    *chatgptWindow `json:"secondary_window"`
		} `json:"rate_limit"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return Observation{}, fmt.Errorf("decode: %w", err)
	}
	rl := raw.RateLimit
	if rl == nil || rl.LimitReached == nil || rl.Primary == nil {
		return Observation{}, errors.New("rate_limit.limit_reached or primary_window missing")
	}
	obs := Observation{At: at, Exhausted: *rl.LimitReached}
	for _, w := range []struct {
		name string
		v    *chatgptWindow
	}{{"primary", rl.Primary}, {"secondary", rl.Secondary}} {
		if w.v == nil {
			continue // free plans have no secondary window
		}
		win, err := w.v.window(w.name)
		if err != nil {
			return Observation{}, err
		}
		obs.Windows = append(obs.Windows, win)
	}
	return obs, nil
}

type chatgptWindow struct {
	UsedPercent   *float64 `json:"used_percent"`
	WindowSeconds *int64   `json:"limit_window_seconds"`
	ResetAt       *int64   `json:"reset_at"`
}

func (c *chatgptWindow) window(name string) (Window, error) {
	if c.UsedPercent == nil || c.ResetAt == nil {
		return Window{}, fmt.Errorf("%s_window.used_percent or reset_at missing", name)
	}
	w := Window{Utilization: clamp(*c.UsedPercent / 100), ResetsAt: time.Unix(*c.ResetAt, 0).UTC()}
	if c.WindowSeconds != nil {
		w.Duration = time.Duration(*c.WindowSeconds) * time.Second
	}
	// Name by length where known: the 5 h window is "five_hour" however it is listed.
	switch {
	case w.Duration > 0 && w.Duration <= 6*time.Hour:
		w.Name = "five_hour"
	case w.Duration >= 6*24*time.Hour:
		w.Name = "weekly"
	default:
		w.Name = canonical(name)
	}
	return w, nil
}

// ClaudeCredentials reads Claude Code's OAuth login: the macOS keychain item
// "Claude Code-credentials", otherwise <CLAUDE_CONFIG_DIR or ~/.claude>/.credentials.json.
// An expired token is reported as missing; it is never refreshed.
func ClaudeCredentials(home string, env func(string) string, now func() time.Time) Credentials {
	dir := env("CLAUDE_CONFIG_DIR")
	if dir == "" {
		dir = filepath.Join(home, ".claude")
	}
	file := claudeFileCredentials(dir, now)
	return func(ctx context.Context) (string, string, error) {
		if goruntime.GOOS == "darwin" {
			out, err := exec.CommandContext(ctx, "/usr/bin/security", "find-generic-password", "-s", "Claude Code-credentials", "-w").Output()
			if err == nil {
				return claudeToken(out, now)
			}
		}
		return file(ctx)
	}
}

func claudeFileCredentials(dir string, now func() time.Time) Credentials {
	return func(context.Context) (string, string, error) {
		raw, err := readSecret(filepath.Join(dir, ".credentials.json"))
		if err != nil {
			return "", "", err
		}
		return claudeToken(raw, now)
	}
}

func claudeToken(raw []byte, now func() time.Time) (token, account string, err error) {
	var c struct {
		OAuth *struct {
			AccessToken string `json:"accessToken"`
			ExpiresAt   int64  `json:"expiresAt"` // epoch milliseconds
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(raw, &c); err != nil || c.OAuth == nil || c.OAuth.AccessToken == "" {
		return "", "", fmt.Errorf("%w: Claude Code login has no OAuth token", ErrNoCredentials)
	}
	if c.OAuth.ExpiresAt > 0 && now().After(time.UnixMilli(c.OAuth.ExpiresAt)) {
		return "", "", fmt.Errorf("%w: Claude Code login expired (run claude once to refresh it)", ErrNoCredentials)
	}
	return c.OAuth.AccessToken, "", nil
}

// CodexCredentials reads Codex's ChatGPT login from <CODEX_HOME or ~/.codex>/auth.json.
func CodexCredentials(home string, env func(string) string) Credentials {
	return func(context.Context) (string, string, error) {
		dir := env("CODEX_HOME")
		if dir == "" {
			dir = filepath.Join(home, ".codex")
		}
		raw, err := readSecret(filepath.Join(dir, "auth.json"))
		if err != nil {
			return "", "", err
		}
		var c struct {
			Tokens *struct {
				AccessToken string `json:"access_token"`
				AccountID   string `json:"account_id"`
			} `json:"tokens"`
		}
		if err := json.Unmarshal(raw, &c); err != nil || c.Tokens == nil || c.Tokens.AccessToken == "" {
			return "", "", fmt.Errorf("%w: Codex has no ChatGPT login (API-key logins have no plan usage)", ErrNoCredentials)
		}
		return c.Tokens.AccessToken, c.Tokens.AccountID, nil
	}
}

func readSecret(path string) ([]byte, error) {
	//nolint:gosec // G304: the CLIs' documented credential files, read-only.
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s not found", ErrNoCredentials, path)
	}
	if err != nil {
		return nil, fmt.Errorf("quota: read %s: %w", path, err)
	}
	return []byte(strings.TrimSpace(string(b))), nil
}
