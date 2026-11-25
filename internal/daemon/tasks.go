package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/ugurboz/openchef/internal/api"
	"github.com/ugurboz/openchef/internal/service"
	"github.com/ugurboz/openchef/internal/store"
	"github.com/ugurboz/openchef/internal/task"
)

// TaskMethods exposes svc as the task API (§12.3).
func TaskMethods(svc *service.Service) map[string]Method {
	h := &taskHandlers{svc: svc, watches: map[watchKey]*service.Subscription{}}
	return map[string]Method{
		api.MethodTaskRun:     h.run,
		api.MethodTaskList:    h.list,
		api.MethodTaskGet:     h.get,
		api.MethodTaskCancel:  h.cancel,
		api.MethodTaskRetry:   h.retry,
		api.MethodTaskWatch:   h.watch,
		api.MethodTaskUnwatch: h.unwatch,
	}
}

type watchKey struct {
	conn int
	task task.ID
}

type taskHandlers struct {
	svc *service.Service

	mu      sync.Mutex
	watches map[watchKey]*service.Subscription
}

func decode[T any](params json.RawMessage) (T, error) {
	var v T
	if len(params) == 0 {
		return v, nil
	}
	if err := json.Unmarshal(params, &v); err != nil {
		return v, api.RPCError(api.CodeInvalidParams, "invalid params: %v", err)
	}
	return v, nil
}

func taskID(s string) (task.ID, error) {
	id, err := task.ParseID(s)
	if err != nil {
		return "", api.RPCError(api.CodeInvalidParams, "%v", err)
	}
	return id, nil
}

// rpcErr turns service errors into JSON-RPC errors the CLI can show as they are.
func rpcErr(err error) error {
	switch {
	case errors.Is(err, service.ErrInvalidRequest):
		return api.RPCError(api.CodeTaskInvalid, "%v", err)
	case errors.Is(err, store.ErrNotFound):
		return api.RPCError(api.CodeTaskInvalid, "no such task")
	}
	return err
}

func (h *taskHandlers) run(ctx context.Context, _ *Conn, params json.RawMessage) (any, error) {
	p, err := decode[api.RunParams](params)
	if err != nil {
		return nil, err
	}
	t, err := h.svc.Run(ctx, service.RunRequest{Prompt: p.Prompt, Dir: p.Dir, Verify: p.Verify, Agent: p.Agent})
	if err != nil {
		return nil, rpcErr(err)
	}
	return api.FromTask(&t), nil
}

func (h *taskHandlers) list(ctx context.Context, _ *Conn, params json.RawMessage) (any, error) {
	p, err := decode[api.ListParams](params)
	if err != nil {
		return nil, err
	}
	ts, err := h.svc.List(ctx, store.TaskFilter{Active: p.Active, Limit: p.Limit})
	if err != nil {
		return nil, rpcErr(err)
	}
	out := make([]api.Task, 0, len(ts))
	for i := range ts {
		out = append(out, api.FromTask(&ts[i]))
	}
	return out, nil
}

func (h *taskHandlers) get(ctx context.Context, _ *Conn, params json.RawMessage) (any, error) {
	p, err := decode[api.TaskParams](params)
	if err != nil {
		return nil, err
	}
	id, err := taskID(p.ID)
	if err != nil {
		return nil, err
	}
	d, err := h.svc.Get(ctx, id)
	if err != nil {
		return nil, rpcErr(err)
	}
	return api.TaskDetail{Task: api.FromTask(&d.Task), Steps: api.FromSteps(d.Steps),
		Attempts: api.FromAttempts(d.Attempts), Driving: d.Driving}, nil
}

func (h *taskHandlers) cancel(ctx context.Context, _ *Conn, params json.RawMessage) (any, error) {
	return h.change(ctx, params, h.svc.Cancel)
}

func (h *taskHandlers) retry(ctx context.Context, _ *Conn, params json.RawMessage) (any, error) {
	return h.change(ctx, params, h.svc.Retry)
}

func (h *taskHandlers) change(ctx context.Context, params json.RawMessage, fn func(context.Context, task.ID) (task.Task, error)) (any, error) {
	p, err := decode[api.TaskParams](params)
	if err != nil {
		return nil, err
	}
	id, err := taskID(p.ID)
	if err != nil {
		return nil, err
	}
	t, err := fn(ctx, id)
	if err != nil {
		return nil, rpcErr(err)
	}
	return api.FromTask(&t), nil
}

// watch subscribes the connection to a task's events. Live events are forwarded as
// notifications by a goroutine that ends with the subscription or the connection.
func (h *taskHandlers) watch(ctx context.Context, c *Conn, params json.RawMessage) (any, error) {
	p, err := decode[api.WatchParams](params)
	if err != nil {
		return nil, err
	}
	id, err := taskID(p.ID)
	if err != nil {
		return nil, err
	}
	past, sub, err := h.svc.Watch(ctx, id, p.Replay)
	if err != nil {
		return nil, rpcErr(err)
	}
	d, err := h.svc.Get(ctx, id)
	if err != nil {
		h.svc.Unwatch(id, sub)
		return nil, rpcErr(err)
	}
	key := watchKey{conn: c.ID(), task: id}
	h.mu.Lock()
	if old := h.watches[key]; old != nil {
		h.svc.Unwatch(id, old)
	}
	h.watches[key] = sub
	h.mu.Unlock()
	go h.forward(c, key, sub)
	return api.WatchResult{Events: past, Finished: api.Finished(string(d.Task.Status)) && !d.Driving}, nil
}

func (h *taskHandlers) forward(c *Conn, key watchKey, sub *service.Subscription) {
	defer h.drop(key, sub)
	for {
		select {
		case ev, ok := <-sub.C:
			if !ok {
				if err := sub.Err(); err != nil {
					_ = c.Notify(api.NotifyWatchEnded, api.WatchEnded{ID: string(key.task), Reason: err.Error()}) // best effort: the client may be gone
				}
				return
			}
			if err := c.Notify(api.NotifyEvent, ev); err != nil {
				return
			}
		case <-c.Done():
			return
		}
	}
}

func (h *taskHandlers) drop(key watchKey, sub *service.Subscription) {
	h.mu.Lock()
	if h.watches[key] == sub {
		delete(h.watches, key)
	}
	h.mu.Unlock()
	h.svc.Unwatch(key.task, sub)
}

func (h *taskHandlers) unwatch(_ context.Context, c *Conn, params json.RawMessage) (any, error) {
	p, err := decode[api.TaskParams](params)
	if err != nil {
		return nil, err
	}
	id, err := taskID(p.ID)
	if err != nil {
		return nil, err
	}
	key := watchKey{conn: c.ID(), task: id}
	h.mu.Lock()
	sub := h.watches[key]
	delete(h.watches, key)
	h.mu.Unlock()
	if sub != nil {
		h.svc.Unwatch(id, sub)
	}
	return struct{}{}, nil
}
