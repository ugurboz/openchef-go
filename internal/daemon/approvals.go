package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"maps"

	"github.com/ugurboz/openchef/internal/api"
	"github.com/ugurboz/openchef/internal/service"
	"github.com/ugurboz/openchef/internal/store"
	"github.com/ugurboz/openchef/internal/task"
)

// ApprovalMethods exposes svc's approval queue (§9.2).
func ApprovalMethods(svc *service.Service) map[string]Method {
	h := approvalHandlers{svc: svc}
	return map[string]Method{
		api.MethodApprovalList:   h.list,
		api.MethodApprovalDecide: h.decide,
		api.MethodApprovalAsk:    h.ask,
	}
}

type approvalHandlers struct {
	svc *service.Service
}

func (h approvalHandlers) list(ctx context.Context, _ *Conn, params json.RawMessage) (any, error) {
	p, err := decode[api.ApprovalListParams](params)
	if err != nil {
		return nil, err
	}
	f := store.ApprovalFilter{Pending: !p.All, Limit: p.Limit}
	if p.Task != "" {
		if f.Task, err = taskID(p.Task); err != nil {
			return nil, err
		}
	}
	out, err := h.svc.Approvals(ctx, f)
	if err != nil {
		return nil, rpcErr(err)
	}
	if out == nil {
		out = []task.Approval{}
	}
	return out, nil
}

func (h approvalHandlers) decide(ctx context.Context, _ *Conn, params json.RawMessage) (any, error) {
	p, err := decode[api.DecideParams](params)
	if err != nil {
		return nil, err
	}
	var d task.Decision
	switch p.Verdict {
	case api.VerdictAllow:
		d = task.Decision{Status: task.ApprovalAllowed}
	case api.VerdictDeny:
		d = task.Decision{Status: task.ApprovalDenied, Answer: p.Text}
	case api.VerdictAnswer:
		d = task.Decision{Status: task.ApprovalAnswered, Answer: p.Text}
	default:
		return nil, api.RPCError(api.CodeInvalidParams, "verdict must be allow, deny or answer, not %q", p.Verdict)
	}
	a, err := h.svc.Decide(ctx, p.ID, d)
	switch {
	case errors.Is(err, service.ErrApprovalSettled):
		return nil, api.RPCError(api.CodeApprovalSettled, "%v", err)
	case errors.Is(err, store.ErrNotFound):
		return nil, api.RPCError(api.CodeTaskInvalid, "no such request: %d", p.ID)
	case err != nil:
		return nil, rpcErr(err)
	}
	return a, nil
}

// ServiceMethods is the whole service API: tasks and approvals.
func ServiceMethods(svc *service.Service) map[string]Method {
	m := TaskMethods(svc)
	maps.Copy(m, ApprovalMethods(svc))
	return m
}

func (h approvalHandlers) ask(ctx context.Context, _ *Conn, params json.RawMessage) (any, error) {
	p, err := decode[api.AskParams](params)
	if err != nil {
		return nil, err
	}
	id, err := taskID(p.Task)
	if err != nil {
		return nil, err
	}
	if p.Agent == "" || p.Question == "" {
		return nil, api.RPCError(api.CodeInvalidParams, "agent and question are required")
	}
	a, err := h.svc.Ask(ctx, service.Request{Task: id, Agent: p.Agent, Kind: task.ApprovalQuestion,
		Summary: p.Question, Options: p.Options})
	if err != nil {
		return nil, rpcErr(err)
	}
	res := api.AskResult{Status: string(a.Status)}
	if a.Status == task.ApprovalAnswered {
		res.Answer = a.Answer
	}
	return res, nil
}
