// Package cli wires the openchef command tree. It contains no business logic: commands
// parse flags, call into internal packages and render results.
package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/ugurboz/openchef/internal/config"
	"github.com/ugurboz/openchef/internal/doctor"
)

// ExitError ends the program with Code without printing anything more: the command has
// already told the user what happened.
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

// IO holds the streams commands write to. Tests pass buffers; main passes os.Stdout/Stderr.
type IO struct {
	Out io.Writer
	Err io.Writer
}

// Deps are the environment dependencies of commands, replaceable in tests.
type Deps struct {
	Detector doctor.Detector
	Paths    func() (config.Paths, error)
	// StartDaemon starts a background daemon when a command needs one; nil launches
	// this executable's `daemon run` detached.
	StartDaemon func(ctx context.Context, paths config.Paths) error
}

// DefaultDeps uses the real environment.
func DefaultDeps() Deps {
	return Deps{Detector: doctor.New(), Paths: config.ResolvePaths}
}

// NewRootCommand builds the full command tree against the real environment.
func NewRootCommand(streams IO) *cobra.Command {
	return NewRootCommandWith(streams, DefaultDeps())
}

// NewRootCommandWith builds the command tree with explicit dependencies.
func NewRootCommandWith(streams IO, deps Deps) *cobra.Command {
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

	root.AddCommand(
		newVersionCommand(),
		newDoctorCommand(deps),
		newInitCommand(deps),
		newDaemonCommand(deps),
		newRunCommand(deps),
		newStatusCommand(deps),
		newLogsCommand(deps),
		newCancelCommand(deps),
		newRetryCommand(deps),
		newApprovalsCommand(deps),
		newApproveCommand(deps),
		newDenyCommand(deps),
		newAnswerCommand(deps),
	)
	return root
}
