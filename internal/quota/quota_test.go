package quota

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/runtime"
)

var t0 = time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func obs(at time.Time, src Source, ws ...Window) Observation {
	return Observation{Pool: "claude-max", Source: src, At: at, Windows: ws}
}

func TestMergeWithinAndAcrossWindows(t *testing.T) {
	t.Parallel()
	c := &clock{t: t0}
	tr := NewTracker(c.now)
	reset := t0.Add(3 * time.Hour)
	tr.Observe(obs(t0, SourceStream, Window{Name: "five_hour", Utilization: 0.50, ResetsAt: reset}))
	// A later but lower reading of the same window (another source, rounding) does not
	// lower utilization; the reset differs by seconds only.
	tr.Observe(obs(t0.Add(time.Minute), SourceProbe, Window{Name: "five_hour", Utilization: 0.40, ResetsAt: reset.Add(30 * time.Second)}))
	s, _ := tr.State("claude-max")
	if len(s.Windows) != 1 || s.Windows[0].Utilization != 0.50 || len(s.Sources) != 2 {
		t.Fatalf("same window: %+v", s)
	}
	// A later reset is a new window: it replaces the old one.
	tr.Observe(obs(t0.Add(2*time.Minute), SourceStream, Window{Name: "five_hour", Utilization: 0.05, ResetsAt: reset.Add(5 * time.Hour)}))
	s, _ = tr.State("claude-max")
	if s.Windows[0].Utilization != 0.05 {
		t.Fatalf("new window: %+v", s)
	}
	// A stale observation of the replaced window is ignored.
	tr.Observe(obs(t0.Add(3*time.Minute), SourceProbe, Window{Name: "five_hour", Utilization: 0.90, ResetsAt: reset}))
	s, _ = tr.State("claude-max")
	if s.Windows[0].Utilization != 0.05 {
		t.Fatalf("old window came back: %+v", s)
	}
	// Past its reset a window no longer counts.
	c.add(9 * time.Hour)
	if s, _ = tr.State("claude-max"); len(s.Windows) != 0 {
		t.Fatalf("expired window kept: %+v", s)
	}
}

func TestFromSignal(t *testing.T) {
	t.Parallel()
	o := FromSignal("p", t0, &runtime.QuotaSignal{Exhausted: true, Windows: []runtime.QuotaWindow{
		{Name: "seven_day", Utilization: 1.3, ResetsAt: t0.Add(time.Hour)}, {Name: "five_hour", Utilization: 0.2},
	}})
	if o.Source != SourceStream || !o.Exhausted || o.Windows[0].Name != "weekly" || o.Windows[0].Utilization != 1 {
		t.Fatalf("%+v", o)
	}
}

func TestHealth(t *testing.T) {
	t.Parallel()
	at := func(u float64, age time.Duration) Health {
		s := State{Windows: []Window{{Name: "weekly", Utilization: 0.1}, {Name: "five_hour", Utilization: u}}, Updated: t0.Add(-age)}
		return HealthOf(&s, t0)
	}
	cases := []struct {
		u    float64
		age  time.Duration
		want Health
	}{
		{0.10, 0, Green}, {0.599, 0, Green}, {0.60, 0, Yellow}, {0.849, 0, Yellow},
		{0.85, 0, Red}, {0.989, 0, Red}, {0.99, 0, Exhausted},
		{0.10, 20 * time.Minute, Yellow}, {0.70, 20 * time.Minute, Red}, {0.90, 20 * time.Minute, Red},
		{1.0, 48 * time.Hour, Exhausted},
	}
	for _, c := range cases {
		if got := at(c.u, c.age); got != c.want {
			t.Errorf("u=%.3f age=%s: %s; want %s", c.u, c.age, got, c.want)
		}
	}
	if h := HealthOf(&State{}, t0); h != Unknown {
		t.Errorf("no data = %s", h)
	}
	if h := HealthOf(&State{Exhausted: true, Windows: []Window{{Utilization: 0.2}}}, t0); h != Exhausted {
		t.Errorf("reported exhaustion = %s", h)
	}
}

func TestExhaustionEndsAtReset(t *testing.T) {
	t.Parallel()
	c := &clock{t: t0}
	tr := NewTracker(c.now)
	reset := t0.Add(time.Hour)
	o := obs(t0, SourceStream, Window{Name: "five_hour", Utilization: 1, ResetsAt: reset})
	o.Exhausted = true
	tr.Observe(o)
	s, _ := tr.State("claude-max")
	if !s.Exhausted || !ResetAt(&s).Equal(reset) {
		t.Fatalf("%+v", s)
	}
	c.add(2 * time.Hour)
	if s, _ = tr.State("claude-max"); s.Exhausted {
		t.Fatalf("still exhausted after the reset: %+v", s)
	}
}

const claudeBody = `{
  "five_hour": {"utilization": 55.0, "resets_at": "2026-11-26T14:00:00.123456+00:00", "limit_dollars": null},
  "seven_day": {"utilization": 12.5, "resets_at": "2026-11-30T09:00:00+00:00"},
  "seven_day_opus": null, "tangelo": null, "extra_usage": {"is_enabled": false}
}`

const chatgptBody = `{
  "plan_type": "plus",
  "rate_limit": {
    "allowed": true, "limit_reached": false,
    "primary_window": {"used_percent": 42, "limit_window_seconds": 18000, "reset_after_seconds": 600, "reset_at": 1795701600},
    "secondary_window": {"used_percent": 7, "limit_window_seconds": 604800, "reset_after_seconds": 9000, "reset_at": 1796200000}
  },
  "credits": {"has_credits": false}
}`

func TestParseClaude(t *testing.T) {
	t.Parallel()
	o, err := parseClaude([]byte(claudeBody), t0)
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Windows) != 2 || o.Windows[0].Name != "five_hour" || o.Windows[0].Utilization != 0.55 ||
		o.Windows[0].ResetsAt.Hour() != 14 || o.Windows[1].Name != "weekly" || o.Windows[1].Utilization != 0.125 {
		t.Fatalf("%+v", o)
	}
	for name, body := range map[string]string{
		"not json":          `<html>`,
		"no windows":        `{"tangelo": null}`,
		"no utilization":    `{"five_hour": {"resets_at": "2026-11-26T14:00:00Z"}}`,
		"bad time":          `{"five_hour": {"utilization": 1, "resets_at": "tomorrow"}}`,
		"utilization a str": `{"five_hour": {"utilization": "55%"}}`,
	} {
		if _, err := parseClaude([]byte(body), t0); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestParseChatGPT(t *testing.T) {
	t.Parallel()
	o, err := parseChatGPT([]byte(chatgptBody), t0)
	if err != nil {
		t.Fatal(err)
	}
	if o.Exhausted || len(o.Windows) != 2 || o.Windows[0].Name != "five_hour" || o.Windows[0].Utilization != 0.42 ||
		o.Windows[1].Name != "weekly" || o.Windows[1].Duration != 7*24*time.Hour {
		t.Fatalf("%+v", o)
	}
	free := `{"rate_limit": {"limit_reached": true, "primary_window": {"used_percent": 100, "limit_window_seconds": 18000, "reset_at": 1795701600}, "secondary_window": null}}`
	if o, err := parseChatGPT([]byte(free), t0); err != nil || !o.Exhausted || len(o.Windows) != 1 {
		t.Fatalf("free plan: %+v, %v", o, err)
	}
	if _, err := parseChatGPT([]byte(`{"rate_limit": {"allowed": true}}`), t0); err == nil {
		t.Error("missing fields accepted")
	}
}

// server is an https test server answering with the given status and body.
func server(t *testing.T, status *atomic.Int64, body string, got *atomic.Value) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.Header.Clone())
		w.WriteHeader(int(status.Load()))
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func testProber(srv *httptest.Server, c *clock, token string) *Prober {
	fetch := httpGet(srv.Client(), srv.URL+"/api/oauth/usage", func() (http.Header, error) {
		h := http.Header{}
		h.Set("Authorization", "Bearer "+token)
		return h, nil
	})
	return &Prober{Pool: "claude-max", name: "claude", fetch: fetch, parse: parseClaude, now: c.now, enabled: true}
}

func TestProberPolicy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	c := &clock{t: t0}
	var status atomic.Int64
	status.Store(200)
	var hdr atomic.Value
	srv := server(t, &status, claudeBody, &hdr)
	p := testProber(srv, c, "sekret-token")

	o, err := p.Run(ctx)
	if err != nil || o.Pool != "claude-max" || o.Source != SourceProbe || len(o.Windows) != 2 {
		t.Fatalf("first run: %+v, %v", o, err)
	}
	if h, _ := hdr.Load().(http.Header); h.Get("Authorization") != "Bearer sekret-token" || h.Get("User-Agent") != "openchef" {
		t.Errorf("headers %v", h)
	}
	if _, err := p.Run(ctx); !errors.Is(err, ErrMuted) {
		t.Errorf("second run within 30 s = %v", err)
	}

	// 429: back off for 5 min, then double.
	status.Store(429)
	c.add(time.Minute)
	if _, err := p.Run(ctx); err == nil || strings.Contains(err.Error(), "sekret") {
		t.Fatalf("429 = %v (must not leak the token)", err)
	}
	c.add(4 * time.Minute)
	if err := p.Status(); !errors.Is(err, ErrMuted) {
		t.Errorf("4 min after 429 = %v", err)
	}
	c.add(2 * time.Minute)
	if err := p.Status(); err != nil {
		t.Errorf("after the backoff = %v", err)
	}

	// Three server errors mute it for 30 min.
	status.Store(503)
	for range 3 {
		c.add(time.Minute)
		_, _ = p.Run(ctx) // errors expected
	}
	if err := p.Status(); !errors.Is(err, ErrMuted) {
		t.Errorf("after three 5xx = %v", err)
	}
	c.add(31 * time.Minute)

	// The kill switch.
	status.Store(200)
	p.SetEnabled(false)
	if _, err := p.Run(ctx); !errors.Is(err, ErrDisabled) {
		t.Errorf("disabled = %v", err)
	}
	p.SetEnabled(true)
	if _, err := p.Run(ctx); err != nil {
		t.Errorf("re-enabled = %v", err)
	}
}

func TestProberDisablesOnShapeChange(t *testing.T) {
	t.Parallel()
	c := &clock{t: t0}
	var status atomic.Int64
	status.Store(200)
	var hdr atomic.Value
	srv := server(t, &status, `{"five_hour": {"utilization": "a lot"}}`, &hdr)
	p := testProber(srv, c, "t")
	if _, err := p.Run(context.Background()); !errors.Is(err, ErrShapeChanged) {
		t.Fatalf("Run = %v; want ErrShapeChanged", err)
	}
	c.add(time.Hour)
	if _, err := p.Run(context.Background()); !errors.Is(err, ErrShapeChanged) {
		t.Fatalf("still disabled = %v", err)
	}
}

// A redirect is never followed, so the token cannot leave the provider's host.
func TestProberRefusesRedirects(t *testing.T) {
	t.Parallel()
	var leaked atomic.Bool
	other := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { leaked.Store(true) }))
	defer other.Close()
	first := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/steal", http.StatusFound)
	}))
	defer first.Close()
	c := &clock{t: t0}
	p := testProber(first, c, "t")
	if _, err := p.Run(context.Background()); err == nil {
		t.Fatal("redirect accepted")
	}
	if leaked.Load() {
		t.Fatal("request reached the redirect target")
	}
}

func TestCredentials(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	home := t.TempDir()
	env := func(string) string { return "" }
	write := func(p, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	codex := CodexCredentials(home, env)
	if _, _, err := codex(ctx); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("missing auth.json = %v", err)
	}
	write(filepath.Join(home, ".codex", "auth.json"), `{"tokens": {"access_token": "tok", "account_id": "acc"}}`)
	if tok, acc, err := codex(ctx); err != nil || tok != "tok" || acc != "acc" {
		t.Errorf("codex = %q %q %v", tok, acc, err)
	}
	write(filepath.Join(home, ".codex", "auth.json"), `{"OPENAI_API_KEY": "sk-x", "tokens": null}`)
	if _, _, err := codex(ctx); !errors.Is(err, ErrNoCredentials) {
		t.Errorf("API-key login = %v", err)
	}

	// Claude: the credentials file (the keychain is only consulted on macOS, so the
	// test reads the file directly to stay independent of the machine).
	dir := filepath.Join(home, "claude")
	now := func() time.Time { return t0 }
	write(filepath.Join(dir, ".credentials.json"), `{"claudeAiOauth": {"accessToken": "c-tok", "expiresAt": `+itoa(t0.Add(time.Hour).UnixMilli())+`}}`)
	claude := claudeFileCredentials(dir, now)
	if tok, _, err := claude(ctx); err != nil || tok != "c-tok" {
		t.Errorf("claude = %q %v", tok, err)
	}
	write(filepath.Join(dir, ".credentials.json"), `{"claudeAiOauth": {"accessToken": "c-tok", "expiresAt": `+itoa(t0.Add(-time.Hour).UnixMilli())+`}}`)
	if _, _, err := claude(ctx); !errors.Is(err, ErrNoCredentials) || !strings.Contains(err.Error(), "expired") {
		t.Errorf("expired = %v", err)
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
