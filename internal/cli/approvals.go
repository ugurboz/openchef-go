package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/ugurboz/openchef/internal/api"
	"github.com/ugurboz/openchef/internal/runtime/linerpc"
	"github.com/ugurboz/openchef/internal/task"
)

func newApprovalsCommand(deps Deps) *cobra.Command {
	var (
		asJSON, all bool
		taskID      string
		limit       int
	)
	cmd := &cobra.Command{
		Use:   "approvals",
		Short: "List requests and questions waiting for you",
		Long: "Lists what agents are waiting for: permission requests (answer with `openchef approve <id>` " +
			"or `openchef deny <id>`) and questions (`openchef answer <id> <text>`). Unanswered requests " +
			"are denied after 15 minutes.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := connect(ctx, deps, false)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			as, err := c.Approvals(ctx, api.ApprovalListParams{Task: taskID, All: all, Limit: limit})
			if err != nil {
				return friendly(err)
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return report(out, true, as, "")
			}
			if len(as) == 0 {
				msg := "nothing is waiting for you"
				if all {
					msg = "no requests yet"
				}
				return report(out, false, nil, msg)
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "ID\tTASK\tAGENT\tSTATUS\tASKED\tREQUEST") // tabwriter buffers; Flush reports errors
			now := time.Now()
			for i := range as {
				a := &as[i]
				_, _ = fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\n", a.ID, a.TaskID, a.Agent, a.Status, since(a.CreatedAt, now), firstLine(what(a), 70))
			}
			if err := tw.Flush(); err != nil {
				return fmt.Errorf("write output: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	cmd.Flags().BoolVar(&all, "all", false, "include settled requests")
	cmd.Flags().StringVar(&taskID, "task", "", "only this task's requests")
	cmd.Flags().IntVar(&limit, "limit", 50, "show at most this many requests")
	return cmd
}

func newApproveCommand(deps Deps) *cobra.Command {
	return newDecideCommand(deps, "approve <id>", "Allow a waiting request", api.VerdictAllow, cobra.ExactArgs(1))
}

func newDenyCommand(deps Deps) *cobra.Command {
	cmd := newDecideCommand(deps, "deny <id> [message]", "Deny a waiting request, optionally telling the agent why",
		api.VerdictDeny, cobra.MinimumNArgs(1))
	return cmd
}

func newAnswerCommand(deps Deps) *cobra.Command {
	return newDecideCommand(deps, "answer <id> <text>", "Answer an agent's question", api.VerdictAnswer, cobra.MinimumNArgs(2))
}

func newDecideCommand(deps Deps, use, short, verdict string, args cobra.PositionalArgs) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  args,
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(strings.TrimPrefix(args[0], "#"), 10, 64)
			if err != nil || id <= 0 {
				return fmt.Errorf("request id %q: want the number shown by `openchef approvals`", args[0])
			}
			ctx := cmd.Context()
			c, err := connect(ctx, deps, false)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			a, err := c.Decide(ctx, api.DecideParams{ID: id, Verdict: verdict, Text: strings.Join(args[1:], " ")})
			if err != nil {
				return decideError(err)
			}
			return report(cmd.OutOrStdout(), asJSON, a, fmt.Sprintf("request %d %s", a.ID, a.Status))
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the request as JSON")
	return cmd
}

func decideError(err error) error {
	var rpc *linerpc.Error
	if errors.As(err, &rpc) && rpc.Code == api.CodeApprovalSettled {
		return errors.New(rpc.Message)
	}
	return friendly(err)
}

// what is a request as one reads it: the tool and its argument, or the question.
func what(a *task.Approval) string {
	if a.Kind == task.ApprovalQuestion {
		s := "asks: " + a.Summary
		if len(a.Options) > 0 {
			s += " (" + strings.Join(a.Options, ", ") + ")"
		}
		return s
	}
	if a.Tool == "" {
		return a.Summary
	}
	return a.Tool + ": " + a.Summary
}

// approval renders a request or its decision in the live view.
func (r *renderer) approval(kind string, a *task.Approval) {
	if a == nil {
		return
	}
	if kind == "requested" {
		r.printf("  ? #%d %s %s", a.ID, a.Agent, indentRest(sanitize(what(a)), "      "))
		if a.Kind == task.ApprovalQuestion {
			r.printf("    answer: openchef answer %d \"…\"", a.ID)
		} else {
			r.printf("    allow: openchef approve %d   deny: openchef deny %d", a.ID, a.ID)
		}
		return
	}
	line := fmt.Sprintf("  · #%d %s", a.ID, a.Status)
	if a.DecidedBy != "" && a.DecidedBy != task.DecidedByUser {
		line += " (" + string(a.DecidedBy) + ")"
	}
	if a.Kind == task.ApprovalQuestion && a.Status == task.ApprovalAnswered {
		line += ": " + firstLine(a.Answer, 80)
	}
	r.printf("%s", line)
}

// indentRest keeps every line of a multi-line command visible, indenting all but the
// first: a person approves exactly what runs.
func indentRest(s, indent string) string {
	return strings.ReplaceAll(strings.TrimRight(s, "\n"), "\n", "\n"+indent)
}
