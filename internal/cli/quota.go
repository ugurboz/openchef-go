package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/ugurboz/openchef/internal/api"
)

func newQuotaCommand(deps Deps) *cobra.Command {
	var refresh, asJSON bool
	cmd := &cobra.Command{
		Use:   "quota",
		Short: "Show each pool's quota windows, health and resets",
		Long: "Shows what OpenChef knows about each pool's rate-limit windows, from the agents' own streams " +
			"and from usage probes. --refresh probes now (at most every 30 seconds). Probes read the CLIs' " +
			"stored logins and call undocumented provider endpoints; `openchef quota probes off` stops them.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			c, err := connect(ctx, deps, false)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			r, err := c.Quota(ctx, api.QuotaParams{Refresh: refresh})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				return report(out, true, r, "")
			}
			return printQuota(out, &r, time.Now())
		},
	}
	cmd.Flags().BoolVar(&refresh, "refresh", false, "probe the providers now")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	cmd.AddCommand(newProbesCommand(deps))
	return cmd
}

func newProbesCommand(deps Deps) *cobra.Command {
	return &cobra.Command{
		Use:       "probes on|off",
		Short:     "Turn the usage probes on or off (kept across restarts)",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"on", "off"},
		RunE: func(cmd *cobra.Command, args []string) error {
			on := args[0] == "on"
			if !on && args[0] != "off" {
				return fmt.Errorf("want on or off, not %q", args[0])
			}
			ctx := cmd.Context()
			c, err := connect(ctx, deps, false)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			if err := c.SetProbes(ctx, on); err != nil {
				return err
			}
			return report(cmd.OutOrStdout(), false, nil, "usage probes "+args[0])
		},
	}
}

func printQuota(w interface{ Write([]byte) (int, error) }, r *api.QuotaResult, now time.Time) error {
	var b strings.Builder
	if len(r.Pools) == 0 {
		b.WriteString("no quota data yet: it arrives with the first agent turn or probe (openchef quota --refresh)\n")
	} else {
		tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
		_, _ = fmt.Fprintln(tw, "POOL\tHEALTH\tWINDOW\tUSED\tRESETS\tUPDATED") // strings.Builder never fails
		for i := range r.Pools {
			p := &r.Pools[i]
			health := p.Health
			if p.Exhausted && !p.ResetAt.IsZero() {
				health += " until " + p.ResetAt.Local().Format("15:04 Jan 2")
			}
			if len(p.Windows) == 0 {
				_, _ = fmt.Fprintf(tw, "%s\t%s\t-\t-\t-\t%s\n", p.Pool, health, since(p.Updated, now))
			}
			for j, win := range p.Windows {
				pool, h := p.Pool, health
				if j > 0 {
					pool, h = "", ""
				}
				resets := "-"
				if !win.ResetsAt.IsZero() {
					resets = "in " + win.ResetsAt.Sub(now).Round(time.Minute).String()
				}
				_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%.0f%%\t%s\t%s\n", pool, h, win.Name, win.Utilization*100, resets, since(p.Updated, now))
			}
		}
		_ = tw.Flush() // into a strings.Builder
	}
	for _, p := range r.Probes {
		state := "ok"
		if p.Problem != "" {
			state = p.Problem
		}
		fmt.Fprintf(&b, "probe %s (%s): %s\n", p.Name, p.Pool, state)
	}
	if _, err := w.Write([]byte(b.String())); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}
