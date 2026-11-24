// Package cli wires the openchef command tree. It contains no business logic: commands
// parse flags, call into internal packages and render results.
package cli

import (
	"io"

	"github.com/spf13/cobra"
)

// IO holds the streams commands write to. Tests pass buffers; main passes os.Stdout/Stderr.
type IO struct {
	Out io.Writer
	Err io.Writer
}

// NewRootCommand builds the full command tree.
func NewRootCommand(streams IO) *cobra.Command {
	root := &cobra.Command{
		Use:   "openchef",
		Short: "Orchestrate AI coding agents across your subscriptions",
		Long: "OpenChef plans work, hands each step to the best available coding agent " +
			"(Claude Code, Codex CLI, Antigravity), verifies the result itself, and fails " +
			"over to another subscription when one runs out of quota.",
		SilenceUsage:  true,
		SilenceErrors: true,
		// Until the TUI exists (milestone M6), running without a subcommand prints help.
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	root.SetOut(streams.Out)
	root.SetErr(streams.Err)
	root.CompletionOptions.HiddenDefaultCmd = true

	root.AddCommand(newVersionCommand())
	return root
}
