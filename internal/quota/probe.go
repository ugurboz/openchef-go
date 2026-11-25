package quota

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Probe errors.
var (
	// ErrShapeChanged means a probe response lacks the fields OpenChef relies on. The
	// probe disables itself (§10.2: fail closed) until the daemon restarts.
	ErrShapeChanged = errors.New("quota: probe response changed shape; probe disabled")
	// ErrNoCredentials means the CLI's stored login was not found or has expired.
	ErrNoCredentials = errors.New("quota: no usable CLI login for the probe")
	// ErrMuted means the probe is backing off after errors or rate limiting.
	ErrMuted = errors.New("quota: probe is backing off")
	// ErrDisabled means the probe was turned off (kill switch or shape change).
	ErrDisabled = errors.New("quota: probe is disabled")
)

// Probe limits of §10.2.
const (
	probeTimeout    = 10 * time.Second
	maxBody         = 1 << 20
	minInterval     = 30 * time.Second
	backoffOn429    = 5 * time.Minute
	muteAfter5xx    = 3
	muteFor5xx      = 30 * time.Minute
	maxBackoffOn429 = time.Hour
)

// fetcher performs one probe request: it returns the body of a 200 response.
type fetcher func(ctx context.Context) ([]byte, error)

// statusError is a non-200 probe response.
type statusError struct {
	host string
	code int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("quota: %s answered HTTP %d", e.host, e.code)
}

// httpGet returns a fetcher for a fixed URL on an allow-listed host. Redirects are
// refused, so the token can never be sent anywhere but the provider's own host. Errors
// never contain the request headers.
func httpGet(client *http.Client, rawURL string, headers func() (http.Header, error)) fetcher {
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return func(context.Context) ([]byte, error) {
			return nil, fmt.Errorf("quota: probe URL %q must be an https URL", rawURL)
		}
	}
	if client == nil {
		client = &http.Client{Timeout: probeTimeout}
	}
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return func(ctx context.Context) ([]byte, error) {
		h, err := headers()
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
		if err != nil {
			return nil, fmt.Errorf("quota: build request for %s: %w", u.Host, err)
		}
		req.Header = h
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "openchef")
		resp, err := c.Do(req)
		if err != nil {
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err // drop the URL wrapper; it is in the message below
			}
			return nil, fmt.Errorf("quota: request to %s: %w", u.Host, err)
		}
		defer func() { _ = resp.Body.Close() }() // read-only response
		if resp.StatusCode != http.StatusOK {
			return nil, &statusError{host: u.Host, code: resp.StatusCode}
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		if err != nil {
			return nil, fmt.Errorf("quota: read response from %s: %w", u.Host, err)
		}
		return body, nil
	}
}

// Prober runs one L2 probe under §10.2's rules: a minimum interval, backoff on 429,
// muting after repeated server errors, a kill switch, and self-disabling when the
// response changes shape.
type Prober struct {
	// Pool receives the observations.
	Pool  string
	name  string
	fetch fetcher
	parse func(body []byte, at time.Time) (Observation, error)
	now   func() time.Time

	mu       sync.Mutex
	enabled  bool
	disabled error // why the probe is off, if it is
	last     time.Time
	until    time.Time // muted until
	fails5xx int
	backoff  time.Duration
}

// Name identifies the probe ("claude", "chatgpt").
func (p *Prober) Name() string { return p.name }

// SetEnabled is the kill switch (`openchef quota probes off`, config quota.probes).
func (p *Prober) SetEnabled(on bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.enabled = on
}

// Status reports whether the probe can run and, if not, why (for `openchef doctor`).
func (p *Prober) Status() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch {
	case p.disabled != nil:
		return p.disabled
	case !p.enabled:
		return ErrDisabled
	case p.now().Before(p.until):
		return fmt.Errorf("%w until %s", ErrMuted, p.until.Format(time.RFC3339))
	}
	return nil
}

// Run performs the probe unless it is off, muted, or ran less than 30 s ago.
func (p *Prober) Run(ctx context.Context) (Observation, error) {
	p.mu.Lock()
	now := p.now()
	switch {
	case p.disabled != nil:
		p.mu.Unlock()
		return Observation{}, p.disabled
	case !p.enabled:
		p.mu.Unlock()
		return Observation{}, ErrDisabled
	case now.Before(p.until):
		p.mu.Unlock()
		return Observation{}, ErrMuted
	case !p.last.IsZero() && now.Sub(p.last) < minInterval:
		p.mu.Unlock()
		return Observation{}, fmt.Errorf("%w: last run %s ago", ErrMuted, now.Sub(p.last).Round(time.Second))
	}
	p.last = now
	p.mu.Unlock()

	body, err := p.fetch(ctx)
	if err != nil {
		p.failed(err)
		return Observation{}, err
	}
	obs, err := p.parse(body, now)
	if err != nil {
		p.mu.Lock()
		p.disabled = fmt.Errorf("%s probe: %w: %w", p.name, ErrShapeChanged, err)
		p.mu.Unlock()
		return Observation{}, p.disabled
	}
	p.mu.Lock()
	p.fails5xx, p.backoff = 0, 0
	p.mu.Unlock()
	obs.Pool, obs.Source = p.Pool, SourceProbe
	return obs, nil
}

func (p *Prober) failed(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var se *statusError
	if !errors.As(err, &se) {
		return
	}
	switch {
	case se.code == http.StatusTooManyRequests:
		if p.backoff == 0 {
			p.backoff = backoffOn429
		} else {
			p.backoff = min(2*p.backoff, maxBackoffOn429)
		}
		p.until = p.now().Add(p.backoff)
	case se.code >= 500:
		p.fails5xx++
		if p.fails5xx >= muteAfter5xx {
			p.until = p.now().Add(muteFor5xx)
			p.fails5xx = 0
		}
	case se.code == http.StatusUnauthorized || se.code == http.StatusForbidden:
		// The stored login expired or was revoked; OpenChef never refreshes tokens.
		p.until = p.now().Add(muteFor5xx)
	}
}
