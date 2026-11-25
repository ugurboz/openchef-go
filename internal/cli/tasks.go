package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/ugurboz/openchef/internal/api"
	"github.com/ugurboz/openchef/internal/config"
	"github.com/ugurboz/openchef/internal/daemon"
	"github.com/ugurboz/openchef/internal/engine"
	"github.com/ugurboz/openchef/internal/runtime/linerpc"
	"github.com/ugurboz/openchef/internal/task"
)

// startDaemon launches the background daemon (the default for Deps.StartDaemon).
func startDaemon(ctx context.Context, paths config.Paths) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate openchef executable: %w", err)
	}
	_, err = daemon.StartDetached(ctx, daemon.Detached{
		Exe: exe, Args: []string{"daemon", "run"}, LogPath: filepath.Join(paths.Logs, "daemon.log"),
		Socket: paths.Socket, Timeout: startTimeout,
	})
	return err
}

// connect dials the daemon, starting it first when autostart is set and none runs.
func connect(ctx context.Context, deps Deps, autostart bool) (*api.Client, error) {
	paths, err := deps.Paths()
	if err != nil {
		return nil, err
	}
	c, err := api.Dial(ctx, paths.Socket, clientName)
	if !errors.Is(err, api.ErrNotRunning) || !autostart {
		return c, err
	}
	start := deps.StartDaemon
	if start == nil {
		start = startDaemon
	}
	if err := start(ctx, paths); err != nil {
		return nil, err
	}
	return api.Dial(ctx, paths.Socket, clientName)
}

// errTaskUnsuccessful makes `run`/`logs -f` exit 1 when the task did not finish
// successfully; the renderer has already said why.
var errTaskUnsuccessful = &ExitError{Code: 1}

func newRunCommand(deps Deps) *cobra.Command {
	var (
		verifyCmd, agent      string
		quiet, detach, asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "run <prompt>",
		Short: "Run one step in a new worktree, verify it and commit it",
		Long: "Runs the prompt as a single step on a routed agent in a worktree of the current " +
			"repository, then verifies it with --verify and commits the result on branch " +
			"openchef/<task>. Output streams live; Ctrl-C detaches without cancelling " +
			"(use `openchef cancel`).",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			dir, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("current directory: %w", err)
			}
			c, err := connect(ctx, deps, true)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			t, err := c.Run(ctx, api.RunParams{Prompt: strings.Join(args, " "), Dir: dir, Verify: verifyCmd, Agent: agent})
			if err != nil {
				return friendly(err)
			}
			out := cmd.OutOrStdout()
			if detach {
				return report(out, asJSON, t, fmt.Sprintf("task %s started; follow it with: openchef logs %s -f", t.ID, t.ID))
			}
			if !asJSON {
				if _, err := fmt.Fprintf(out, "task %s on branch openchef/%s\n", t.ID, t.ID); err != nil {
					return fmt.Errorf("write output: %w", err)
				}
			}
			return follow(ctx, c, t.ID, &renderer{w: out, asJSON: asJSON, quiet: quiet}, true)
		},
	}
	cmd.Flags().StringVar(&verifyCmd, "verify", "", "command that must pass in the workspace, e.g. \"go test ./...\"")
	cmd.Flags().StringVar(&agent, "agent", "", "pin an agent from config.toml instead of routing")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "print only the final result")
	cmd.Flags().BoolVar(&detach, "detach", false, "return immediately; follow with `openchef logs <task> -f`")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print events as JSON lines")
	return cmd
}

// follow renders a task's events until it finishes, parks, or ctx ends (Ctrl-C detaches).
func follow(ctx context.Context, c *api.Client, id string, r *renderer, replay bool) error {
	res, err := c.Watch(ctx, api.WatchParams{ID: id, Replay: replay})
	if err != nil {
		return friendly(err)
	}
	last := ""
	for i := range res.Events {
		if err := r.event(&res.Events[i]); err != nil {
			return err
		}
		if res.Events[i].Kind == engine.EventTaskStatus {
			last = res.Events[i].Status
		}
	}
	if res.Finished {
		return outcome(last)
	}
	for {
		select {
		case n, ok := <-c.Notifications():
			if !ok {
				return errors.New("the daemon closed the connection (it may have stopped); the task resumes when it restarts")
			}
			switch n.Method {
			case api.NotifyEvent:
				var ev engine.Event
				if err := json.Unmarshal(n.Params, &ev); err != nil {
					return fmt.Errorf("decode event: %w", err)
				}
				if err := r.event(&ev); err != nil {
					return err
				}
				if ev.Kind == engine.EventTaskStatus && api.Finished(ev.Status) {
					return outcome(ev.Status)
				}
			case api.NotifyWatchEnded:
				var we api.WatchEnded
				_ = json.Unmarshal(n.Params, &we) // the reason is informative
				return fmt.Errorf("stopped following task %s: %s", id, we.Reason)
			}
		case <-ctx.Done():
			r.printf("detached; task %s keeps running (follow: openchef logs %s -f, stop: openchef cancel %s)", id, id, id)
			return r.err
		}
	}
}

func outcome(status string) error {
	switch task.Status(status) {
	case task.StatusDone, task.StatusWaitingQuota, task.StatusRunning:
		return nil
	default:
		return errTaskUnsuccessful
	}
}

// friendly unwraps daemon refusals into their message.
func friendly(err error) error {
	var rpc *linerpc.Error
	if errors.As(err, &rpc) && rpc.Code == api.CodeTaskInvalid {
		return errors.New(rpc.Message)
	}
	return err
}

func newLogsCommand(deps Deps) *cobra.Command {
	var followFlag, asJSON bool
	cmd := &cobra.Command{
		Use:   "logs <task>",
		Short: "Show a task's events; -f follows them live",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := connect(ctx, deps, false)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			r := &renderer{w: cmd.OutOrStdout(), asJSON: asJSON}
			if followFlag {
				return follow(ctx, c, args[0], r, true)
			}
			res, err := c.Watch(ctx, api.WatchParams{ID: args[0], Replay: true})
			if err != nil {
				return friendly(err)
			}
			if err := c.Call(ctx, api.MethodTaskUnwatch, api.TaskParams{ID: args[0]}, nil); err != nil {
				return err
			}
			for i := range res.Events {
				if err := r.event(&res.Events[i]); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&followFlag, "follow", "f", false, "keep following new events")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print events as JSON lines")
	return cmd
}

func newStatusCommand(deps Deps) *cobra.Command {
	var (
		asJSON, all bool
		limit       int
	)
	cmd := &cobra.Command{
		Use:   "status [task]",
		Short: "List tasks, or show one task's steps and attempts",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := connect(ctx, deps, false)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			out := cmd.OutOrStdout()
			if len(args) == 1 {
				d, err := c.Get(ctx, args[0])
				if err != nil {
					return friendly(err)
				}
				if asJSON {
					return report(out, true, d, "")
				}
				return printDetail(out, &d)
			}
			ts, err := c.List(ctx, api.ListParams{Active: !all, Limit: limit})
			if err != nil {
				return err
			}
			if asJSON {
				return report(out, true, ts, "")
			}
			if len(ts) == 0 {
				msg := "no active tasks (openchef status --all shows finished ones)"
				if all {
					msg = "no tasks yet (start one with: openchef run \"<prompt>\")"
				}
				return report(out, false, nil, msg)
			}
			tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
			_, _ = fmt.Fprintln(tw, "TASK\tSTATUS\tSTARTED\tPROMPT") // tabwriter buffers; Flush reports errors
			now := time.Now()
			for i := range ts {
				t := &ts[i]
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", t.ID, t.Status, since(t.CreatedAt, now), firstLine(t.Prompt, 60))
			}
			if err := tw.Flush(); err != nil {
				return fmt.Errorf("write output: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	cmd.Flags().BoolVar(&all, "all", false, "include finished tasks")
	cmd.Flags().IntVar(&limit, "limit", 20, "show at most this many tasks")
	return cmd
}

func printDetail(w interface{ Write([]byte) (int, error) }, d *api.TaskDetail) error {
	var b strings.Builder
	t := d.Task
	fmt.Fprintf(&b, "task     %s (%s)\nstatus   %s", t.ID, t.Kind, t.Status)
	if t.Reason != "" {
		fmt.Fprintf(&b, ": %s", t.Reason)
	}
	if !t.ResumeAt.IsZero() {
		fmt.Fprintf(&b, " (resumes %s)", t.ResumeAt.Local().Format("15:04 Jan 2"))
	}
	fmt.Fprintf(&b, "\nrepo     %s\n", t.Repo)
	if t.Branch != "" {
		fmt.Fprintf(&b, "branch   %s\nworktree %s\n", t.Branch, t.Worktree)
	}
	fmt.Fprintf(&b, "prompt   %s\n\n", firstLine(t.Prompt, 100))
	for i := range d.Steps {
		s := &d.Steps[i]
		fmt.Fprintf(&b, "step %d  %-8s %s", s.Index, s.Status, s.Title)
		if s.Commit != "" {
			fmt.Fprintf(&b, "  (%s)", shortSHA(s.Commit))
		}
		b.WriteString("\n")
		for j := range d.Attempts {
			if a := &d.Attempts[j]; a.Step == s.Index {
				fmt.Fprintf(&b, "  attempt %d  %s on %s: %s", a.ID, a.Agent, a.Pool, a.Status)
				if a.Outcome != "" && a.Status != string(task.AttemptSucceeded) {
					fmt.Fprintf(&b, " (%s)", firstLine(a.Outcome, 80))
				}
				b.WriteString("\n")
			}
		}
	}
	if _, err := w.Write([]byte(b.String())); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}

func newCancelCommand(deps Deps) *cobra.Command {
	return newTaskActionCommand(deps, "cancel <task>", "Cancel a task and stop its agent",
		func(ctx context.Context, c *api.Client, id string) (api.Task, error) { return c.Cancel(ctx, id) },
		"task %s cancelled")
}

func newRetryCommand(deps Deps) *cobra.Command {
	return newTaskActionCommand(deps, "retry <task>", "Retry a task that needs attention or waits for quota",
		func(ctx context.Context, c *api.Client, id string) (api.Task, error) { return c.Retry(ctx, id) },
		"task %s restarted; follow it with: openchef logs %[1]s -f")
}

func newTaskActionCommand(deps Deps, use, short string, act func(context.Context, *api.Client, string) (api.Task, error), done string) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			c, err := connect(ctx, deps, false)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			t, err := act(ctx, c, args[0])
			if err != nil {
				return friendly(err)
			}
			return report(cmd.OutOrStdout(), asJSON, t, fmt.Sprintf(done, t.ID))
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the task as JSON")
	return cmd
}
