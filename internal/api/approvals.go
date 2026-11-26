package api

import (
	"context"

	"github.com/ugurboz/openchef/internal/task"
)

// Approval queue methods (§9.2). Requests reach viewers as task events
// (engine.EventApprovalRequested / EventApprovalDecided on NotifyEvent).
const (
	MethodApprovalList   = "approval/list"
	MethodApprovalDecide = "approval/decide"
)

// CodeApprovalSettled is returned when a request was already settled.
const CodeApprovalSettled = -32011

// ApprovalListParams filters approval/list.
type ApprovalListParams struct {
	// Task limits the list to one task.
	Task string `json:"task,omitempty"`
	// All includes settled requests; by default only pending ones are listed.
	All   bool `json:"all,omitempty"`
	Limit int  `json:"limit,omitempty"`
}

// Verdicts for approval/decide.
const (
	VerdictAllow  = "allow"
	VerdictDeny   = "deny"
	VerdictAnswer = "answer"
)

// DecideParams settles one request.
type DecideParams struct {
	ID      int64  `json:"id"`
	Verdict string `json:"verdict"`
	// Text is the answer to a question, or the message a denied agent sees.
	Text string `json:"text,omitempty"`
}

// Approvals lists queued requests, oldest first.
func (c *Client) Approvals(ctx context.Context, p ApprovalListParams) ([]task.Approval, error) {
	var out []task.Approval
	err := c.Call(ctx, MethodApprovalList, p, &out)
	return out, err
}

// Decide settles a request.
func (c *Client) Decide(ctx context.Context, p DecideParams) (task.Approval, error) {
	var out task.Approval
	err := c.Call(ctx, MethodApprovalDecide, p, &out)
	return out, err
}
