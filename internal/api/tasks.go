package api

import (
	"context"
	"time"

	"github.com/ugurboz/openchef/internal/engine"
	"github.com/ugurboz/openchef/internal/task"
)

// Task methods and notifications.
const (
	MethodTaskRun    = "task/run"
	MethodTaskList   = "task/list"
	MethodTaskGet    = "task/get"
	MethodTaskCancel = "task/cancel"
	MethodTaskRetry  = "task/retry"
	// MethodTaskWatch returns a task's past events and then streams new ones as
	// NotifyEvent notifications until MethodTaskUnwatch or the connection closes.
	MethodTaskWatch   = "task/watch"
	MethodTaskUnwatch = "task/unwatch"

	// NotifyEvent carries one engine.Event.
	NotifyEvent = "event/appended"
	// NotifyWatchEnded carries WatchEnded when the daemon ends a watch (viewer fell
	// behind, daemon stopping).
	NotifyWatchEnded = "watch/ended"
)

// CodeTaskInvalid is returned for requests the daemon refuses (bad prompt, not a
// repository, cancelling a finished task).
const CodeTaskInvalid = -32010

// RunParams starts a single-step task.
type RunParams struct {
	Prompt string `json:"prompt"`
	// Dir is the caller's working directory; the task runs on its repository.
	Dir    string `json:"dir"`
	Verify string `json:"verify,omitempty"`
	Agent  string `json:"agent,omitempty"`
}

// TaskParams names a task.
type TaskParams struct {
	ID string `json:"id"`
}

// ListParams filters task/list.
type ListParams struct {
	Active bool `json:"active,omitempty"`
	Limit  int  `json:"limit,omitempty"`
}

// WatchParams starts a watch.
type WatchParams struct {
	ID string `json:"id"`
	// Replay includes the task's past events in the result.
	Replay bool `json:"replay,omitempty"`
}

// WatchResult holds the replayed events; live ones follow as notifications.
type WatchResult struct {
	Events []engine.Event `json:"events"`
	// Finished is true when the task is no longer active: no live events will follow.
	Finished bool `json:"finished"`
}

// WatchEnded tells a client its watch was ended by the daemon.
type WatchEnded struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

// Task is the wire form of task.Task.
type Task struct {
	ID         string    `json:"id"`
	Kind       string    `json:"kind"`
	Status     string    `json:"status"`
	Prompt     string    `json:"prompt"`
	Repo       string    `json:"repo"`
	Branch     string    `json:"branch,omitempty"`
	Worktree   string    `json:"worktree,omitempty"`
	BaseCommit string    `json:"baseCommit,omitempty"`
	Agent      string    `json:"agent,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	ResumeAt   time.Time `json:"resumeAt,omitzero"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

// Step is the wire form of task.Step.
type Step struct {
	Index   int    `json:"index"`
	Title   string `json:"title"`
	Status  string `json:"status"`
	Verify  string `json:"verify,omitempty"`
	Retries int    `json:"retries"`
	Commit  string `json:"commit,omitempty"`
}

// Attempt is the wire form of task.Attempt.
type Attempt struct {
	ID        int64     `json:"id"`
	Step      int       `json:"step"`
	Agent     string    `json:"agent"`
	Pool      string    `json:"pool"`
	Status    string    `json:"status"`
	Outcome   string    `json:"outcome,omitempty"`
	StartedAt time.Time `json:"startedAt"`
	EndedAt   time.Time `json:"endedAt,omitzero"`
}

// TaskDetail is the result of task/get.
type TaskDetail struct {
	Task     Task      `json:"task"`
	Steps    []Step    `json:"steps"`
	Attempts []Attempt `json:"attempts"`
	Driving  bool      `json:"driving"`
}

// FromTask converts a task for the wire.
func FromTask(t *task.Task) Task {
	return Task{
		ID: string(t.ID), Kind: string(t.Kind), Status: string(t.Status), Prompt: t.Prompt,
		Repo: t.RepoRoot, Branch: t.Branch, Worktree: t.Worktree, BaseCommit: t.BaseCommit,
		Agent: t.AgentPin, Reason: t.Reason, ResumeAt: t.ResumeAt, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt,
	}
}

// FromSteps converts steps for the wire.
func FromSteps(steps []task.Step) []Step {
	out := make([]Step, 0, len(steps))
	for i := range steps {
		s := &steps[i]
		out = append(out, Step{Index: s.Index, Title: s.Title, Status: string(s.Status), Verify: s.Verify,
			Retries: s.Retries, Commit: s.VerifiedCommit})
	}
	return out
}

// FromAttempts converts attempts for the wire.
func FromAttempts(attempts []task.Attempt) []Attempt {
	out := make([]Attempt, 0, len(attempts))
	for i := range attempts {
		a := &attempts[i]
		out = append(out, Attempt{ID: a.ID, Step: a.StepIndex, Agent: a.Agent, Pool: a.Pool, Status: string(a.Status),
			Outcome: a.Outcome, StartedAt: a.StartedAt, EndedAt: a.EndedAt})
	}
	return out
}

// Finished reports whether a task in status no longer produces events by itself: it
// ended or waits for the user. A watch of a waiting_quota task also stops here; the
// task wakes later and can be watched again.
func Finished(status string) bool {
	switch task.Status(status) {
	case task.StatusRunning, task.StatusReviewing, task.StatusPlanning:
		return false
	default:
		return true
	}
}

// Run starts a task.
func (c *Client) Run(ctx context.Context, p RunParams) (Task, error) {
	var t Task
	err := c.Call(ctx, MethodTaskRun, p, &t)
	return t, err
}

// List returns tasks, newest first.
func (c *Client) List(ctx context.Context, p ListParams) ([]Task, error) {
	var ts []Task
	err := c.Call(ctx, MethodTaskList, p, &ts)
	return ts, err
}

// Get returns one task.
func (c *Client) Get(ctx context.Context, id string) (TaskDetail, error) {
	var d TaskDetail
	err := c.Call(ctx, MethodTaskGet, TaskParams{ID: id}, &d)
	return d, err
}

// Cancel cancels a task.
func (c *Client) Cancel(ctx context.Context, id string) (Task, error) {
	var t Task
	err := c.Call(ctx, MethodTaskCancel, TaskParams{ID: id}, &t)
	return t, err
}

// Retry restarts a task that needs attention.
func (c *Client) Retry(ctx context.Context, id string) (Task, error) {
	var t Task
	err := c.Call(ctx, MethodTaskRetry, TaskParams{ID: id}, &t)
	return t, err
}

// Watch starts watching a task; live events arrive on Notifications.
func (c *Client) Watch(ctx context.Context, p WatchParams) (WatchResult, error) {
	var r WatchResult
	err := c.Call(ctx, MethodTaskWatch, p, &r)
	return r, err
}
