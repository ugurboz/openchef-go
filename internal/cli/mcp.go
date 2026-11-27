package cli

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ugurboz/openchef/internal/api"
	"github.com/ugurboz/openchef/internal/buildinfo"
	"github.com/ugurboz/openchef/internal/mcp"
)

// newMCPCommand is the stdio MCP server agents' CLIs start (SessionSpec.AskUser). It is
// hidden: OpenChef runs it, people do not.
func newMCPCommand(deps Deps) *cobra.Command {
	mcpCmd := &cobra.Command{Use: "mcp", Short: "MCP servers OpenChef gives its agents", Hidden: true}
	var taskID, agent string
	ask := &cobra.Command{
		Use:   "ask-user",
		Short: "Serve the ask_user tool on stdin/stdout for one task's agent",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a := &daemonAsker{deps: deps, task: taskID, agent: agent}
			return mcp.Serve(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), a, buildinfo.Get().Version)
		},
	}
	ask.Flags().StringVar(&taskID, "task", "", "the task the agent works on")
	ask.Flags().StringVar(&agent, "agent", "", "the agent's name")
	_ = ask.MarkFlagRequired("task")  // static flag names; cannot fail
	_ = ask.MarkFlagRequired("agent") // static flag names; cannot fail
	mcpCmd.AddCommand(ask)
	return mcpCmd
}

// daemonAsker sends each question to the daemon's queue on its own connection, so a
// daemon restart between questions does not break the server.
type daemonAsker struct {
	deps        Deps
	task, agent string
}

func (a *daemonAsker) Ask(ctx context.Context, question string, options []string) (string, error) {
	c, err := connect(ctx, a.deps, false)
	if err != nil {
		return "", fmt.Errorf("reach the daemon: %w", err)
	}
	defer func() { _ = c.Close() }()
	res, err := c.Ask(ctx, api.AskParams{Task: a.task, Agent: a.agent, Question: question, Options: options})
	if err != nil {
		return "", fmt.Errorf("ask: %w", err)
	}
	return res.Answer, nil
}
