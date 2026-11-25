// Command openchef is the OpenChef CLI, TUI and daemon entry point.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"

	"github.com/ugurboz/openchef/internal/cli"
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	cmd := cli.NewRootCommand(cli.IO{Out: os.Stdout, Err: os.Stderr})
	if err := cmd.ExecuteContext(ctx); err != nil {
		if exit, ok := errors.AsType[*cli.ExitError](err); ok {
			return exit.Code
		}
		fmt.Fprintf(os.Stderr, "openchef: %v\n", err)
		return 1
	}
	return 0
}
