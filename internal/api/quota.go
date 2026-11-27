package api

import (
	"context"
	"time"

	"github.com/ugurboz/openchef/internal/quota"
)

// Quota methods (§10, §12.1 `openchef quota`).
const (
	MethodQuotaGet    = "quota/get"
	MethodQuotaProbes = "quota/probes"
)

// QuotaParams asks for the quota report.
type QuotaParams struct {
	// Refresh runs the L2 probes first (each at most every 30 s).
	Refresh bool `json:"refresh,omitempty"`
}

// PoolQuota is one pool's merged quota.
type PoolQuota struct {
	Pool      string         `json:"pool"`
	Health    string         `json:"health"`
	Windows   []quota.Window `json:"windows"`
	Exhausted bool           `json:"exhausted,omitempty"`
	// ResetAt is when an exhausted pool takes work again; zero if unknown.
	ResetAt time.Time      `json:"resetAt,omitzero"`
	Updated time.Time      `json:"updated"`
	Sources []quota.Source `json:"sources"`
}

// ProbeStatus is one L2 probe; Problem is empty when it can run.
type ProbeStatus struct {
	Name    string `json:"name"`
	Pool    string `json:"pool"`
	Problem string `json:"problem,omitempty"`
}

// QuotaResult is the quota report.
type QuotaResult struct {
	Pools  []PoolQuota   `json:"pools"`
	Probes []ProbeStatus `json:"probes"`
}

// ProbesParams flips the L2 probe kill switch.
type ProbesParams struct {
	On bool `json:"on"`
}

// Quota returns the quota report.
func (c *Client) Quota(ctx context.Context, p QuotaParams) (QuotaResult, error) {
	var out QuotaResult
	err := c.Call(ctx, MethodQuotaGet, p, &out)
	return out, err
}

// SetProbes turns L2 probes on or off.
func (c *Client) SetProbes(ctx context.Context, on bool) error {
	return c.Call(ctx, MethodQuotaProbes, ProbesParams{On: on}, nil)
}
