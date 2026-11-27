package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/api"
	"github.com/ugurboz/openchef/internal/quota"
)

func TestQuotaCommand(t *testing.T) {
	e := newCLIEnv(t)
	if err := e.deps.StartDaemon(t.Context(), e.paths); err != nil { // quota does not start one itself
		t.Fatal(err)
	}
	out, err := e.cli("quota")
	if err != nil || !strings.Contains(out, "no quota data yet") {
		t.Fatalf("empty quota: %v\n%s", err, out)
	}
	now := time.Now()
	e.quota.Observe(quota.Observation{Pool: "claude-max", Source: quota.SourceStream, At: now,
		Windows: []quota.Window{{Name: "five_hour", Utilization: 0.72, ResetsAt: now.Add(2 * time.Hour)}, {Name: "weekly", Utilization: 0.1}}})
	out, err = e.cli("quota")
	for _, want := range []string{"claude-max", "yellow", "five_hour", "72%", "in 2h0m0s", "weekly", "10%"} {
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("quota lacks %q: %v\n%s", want, err, out)
		}
	}
	out, err = e.cli("quota", "--json")
	var r api.QuotaResult
	if err != nil || json.Unmarshal([]byte(out), &r) != nil || len(r.Pools) != 1 || r.Pools[0].Health != "yellow" {
		t.Fatalf("quota --json: %v\n%s", err, out)
	}
	if out, err := e.cli("quota", "probes", "off"); err != nil || !strings.Contains(out, "usage probes off") {
		t.Fatalf("probes off: %v\n%s", err, out)
	}
	if _, err := e.cli("quota", "probes", "maybe"); err == nil {
		t.Fatal("bad switch accepted")
	}
}

func TestDoctorShowsDisabledProbes(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	r := doctorReport{Config: configReport{Status: configValid}, Healthy: true,
		Probes: []api.ProbeStatus{{Name: "claude", Pool: "claude-max", Problem: "quota: probe response changed shape; probe disabled"}, {Name: "chatgpt", Pool: "chatgpt"}}}
	if err := renderDoctor(&b, r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "changed shape") || !strings.Contains(b.String(), "chatgpt") {
		t.Fatalf("doctor:\n%s", b.String())
	}
}
