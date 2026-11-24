package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/ugurboz/openchef/internal/config"
	"github.com/ugurboz/openchef/internal/doctor"
)

// ErrUnhealthy is returned by `openchef doctor` when OpenChef cannot work as configured.
var ErrUnhealthy = errors.New("doctor found problems")

// Config states reported by doctor.
const (
	configMissing = "missing"
	configValid   = "valid"
	configInvalid = "invalid"
)

type doctorReport struct {
	Paths    config.Paths           `json:"paths"`
	Config   configReport           `json:"config"`
	Runtimes []doctor.RuntimeStatus `json:"runtimes"`
	Healthy  bool                   `json:"healthy"`
}

type configReport struct {
	Status   string   `json:"status"`
	Problems []string `json:"problems,omitempty"`
}

func newDoctorCommand(deps Deps) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check installed agent CLIs, logins and configuration",
		Long: "doctor reports which agent CLIs (claude, codex, agy) are installed, their " +
			"versions and login state, and whether the OpenChef configuration is valid. " +
			"It exits non-zero when no agent CLI is usable or the configuration is invalid.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			report, err := buildDoctorReport(cmd, deps)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if asJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(report); err != nil {
					return fmt.Errorf("encode report: %w", err)
				}
			} else if err := renderDoctor(out, report); err != nil {
				return fmt.Errorf("write report: %w", err)
			}
			if !report.Healthy {
				return ErrUnhealthy
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}

func buildDoctorReport(cmd *cobra.Command, deps Deps) (doctorReport, error) {
	paths, err := deps.Paths()
	if err != nil {
		return doctorReport{}, fmt.Errorf("resolve paths: %w", err)
	}
	report := doctorReport{Paths: paths, Runtimes: deps.Detector.Detect(cmd.Context())}

	_, err = config.Load(paths.Config)
	var ve *config.ValidationError
	switch {
	case err == nil:
		report.Config.Status = configValid
	case errors.Is(err, config.ErrNotFound):
		report.Config.Status = configMissing
	case errors.As(err, &ve):
		report.Config = configReport{Status: configInvalid, Problems: ve.Problems}
	default:
		report.Config = configReport{Status: configInvalid, Problems: []string{err.Error()}}
	}

	usable := false
	for _, r := range report.Runtimes {
		usable = usable || r.Usable()
	}
	report.Healthy = usable && report.Config.Status != configInvalid
	return report, nil
}

func renderDoctor(w io.Writer, r doctorReport) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	p := printer{w: tw}
	p.printf("OpenChef home\t%s\n", r.Paths.Home)
	switch r.Config.Status {
	case configValid:
		p.printf("Config\tvalid (%s)\n", r.Paths.Config)
	case configMissing:
		p.printf("Config\tnot found — run `openchef init`\n")
	default:
		p.printf("Config\tinvalid (%s)\n", r.Paths.Config)
		for _, prob := range r.Config.Problems {
			p.printf("\t  - %s\n", prob)
		}
	}
	p.printf("\nRuntime\tVersion\tStatus\tPath\n")
	for _, rt := range r.Runtimes {
		p.printf("%s\t%s\t%s\t%s\n", rt.Runtime, orDash(rt.Version), runtimeState(rt), orDash(rt.Path))
	}
	if !r.Healthy {
		p.printf("\nOpenChef cannot run yet: ")
		if r.Config.Status == configInvalid {
			p.printf("fix the configuration above.\n")
		} else {
			p.printf("install and log in to at least one of claude, codex or agy.\n")
		}
	}
	if p.err != nil {
		return p.err
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("flush: %w", err)
	}
	return nil
}

func runtimeState(s doctor.RuntimeStatus) string {
	switch {
	case s.Problem != "":
		return s.Problem
	case s.Login == doctor.LoginOK:
		return withDetail("logged in", s.LoginDetail)
	case s.Login == doctor.LoginMissing:
		return withDetail("NOT logged in", s.LoginDetail)
	default:
		return withDetail("login unknown", s.LoginDetail)
	}
}

func withDetail(s, detail string) string {
	if detail == "" {
		return s
	}
	return s + " (" + detail + ")"
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// printer remembers the first write error so rendering code stays readable.
type printer struct {
	w   io.Writer
	err error
}

func (p *printer) printf(format string, args ...any) {
	if p.err != nil {
		return
	}
	if _, err := fmt.Fprintf(p.w, format, args...); err != nil {
		p.err = fmt.Errorf("write: %w", err)
	}
}
