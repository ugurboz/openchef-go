package daemon

import (
	"context"
	"encoding/json"

	"github.com/ugurboz/openchef/internal/api"
	"github.com/ugurboz/openchef/internal/service"
)

// QuotaMethods exposes the quota monitor (§10).
func QuotaMethods(q *service.Quota) map[string]Method {
	h := quotaHandlers{q: q}
	return map[string]Method{
		api.MethodQuotaGet:    h.get,
		api.MethodQuotaProbes: h.probes,
	}
}

type quotaHandlers struct {
	q *service.Quota
}

func (h quotaHandlers) get(ctx context.Context, _ *Conn, params json.RawMessage) (any, error) {
	p, err := decode[api.QuotaParams](params)
	if err != nil {
		return nil, err
	}
	r := h.q.Report(ctx, p.Refresh)
	out := api.QuotaResult{Pools: []api.PoolQuota{}, Probes: []api.ProbeStatus{}}
	for i := range r.Pools {
		pq := &r.Pools[i]
		out.Pools = append(out.Pools, api.PoolQuota{Pool: pq.State.Pool, Health: pq.Health.String(), Windows: pq.State.Windows,
			Exhausted: pq.State.Exhausted, ResetAt: pq.ResetAt, Updated: pq.State.Updated, Sources: pq.State.Sources})
	}
	for _, ps := range r.Probes {
		out.Probes = append(out.Probes, api.ProbeStatus{Name: ps.Name, Pool: ps.Pool, Problem: ps.Problem})
	}
	return out, nil
}

func (h quotaHandlers) probes(ctx context.Context, _ *Conn, params json.RawMessage) (any, error) {
	p, err := decode[api.ProbesParams](params)
	if err != nil {
		return nil, err
	}
	if err := h.q.SetProbes(ctx, p.On); err != nil {
		return nil, err
	}
	return struct{}{}, nil
}
