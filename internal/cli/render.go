package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/ugurboz/openchef/internal/engine"
	"github.com/ugurboz/openchef/internal/runtime"
	"github.com/ugurboz/openchef/internal/task"
)

// renderer prints engine events for people (D-014): streamed text as it arrives, one
// line per tool call, verification and step outcomes. `run` and `logs -f` share it, so
// both always show the same thing. With asJSON it prints one event per line instead.
type renderer struct {
	w      io.Writer
	asJSON bool
	quiet  bool
	// midText is true while agent text is being streamed without a trailing newline.
	midText bool
	err     error
}

func (r *renderer) printf(format string, args ...any) {
	if r.err != nil {
		return
	}
	if r.midText {
		if _, err := fmt.Fprintln(r.w); err != nil {
			r.err = err
			return
		}
		r.midText = false
	}
	if _, err := fmt.Fprintf(r.w, format+"\n", args...); err != nil {
		r.err = fmt.Errorf("write output: %w", err)
	}
}

// event renders one event and reports any write error so far.
func (r *renderer) event(ev *engine.Event) error {
	if r.asJSON {
		line, err := json.Marshal(ev)
		if err != nil {
			return fmt.Errorf("encode event: %w", err)
		}
		if _, err := fmt.Fprintln(r.w, string(line)); err != nil {
			return fmt.Errorf("write output: %w", err)
		}
		return nil
	}
	if r.quiet {
		if ev.Kind == engine.EventTaskStatus {
			r.status(ev)
		}
		return r.err
	}
	switch ev.Kind {
	case engine.EventTaskStatus:
		r.status(ev)
	case engine.EventStepStarted:
		r.printf("▸ Step %d: %s", ev.Step, ev.Message)
	case engine.EventAttemptStarted:
		r.printf("  agent %s", ev.Agent)
	case engine.EventAgent:
		r.agent(ev.Runtime)
	case engine.EventVerifyStarted:
		r.printf("  ▶ verify  %s", ev.Message)
	case engine.EventVerifyFinished:
		if v := ev.Verify; v != nil {
			mark := "✓"
			if !v.Passed {
				mark = "✗"
			}
			r.printf("  %s verification %s", mark, v.Summary())
		}
	case engine.EventAttemptEnded:
		if ev.Status != string(task.AttemptSucceeded) {
			r.printf("  agent %s stopped: %s", ev.Agent, ev.Message)
		}
	case engine.EventStepFinished:
		if ev.Status == string(task.StepVerified) {
			r.printf("  ✓ step %d done (%s)", ev.Step, shortSHA(ev.Message))
		} else {
			r.printf("  ✗ step %d failed: %s", ev.Step, ev.Message)
		}
	case engine.EventNotice:
		r.printf("  · %s", ev.Message)
	}
	return r.err
}

func (r *renderer) status(ev *engine.Event) {
	switch task.Status(ev.Status) {
	case task.StatusRunning:
		if ev.Message != "" && ev.Message != "created" {
			r.printf("◆ running (%s)", ev.Message)
		}
	case task.StatusDone:
		r.printf("✔ done")
	case task.StatusWaitingQuota:
		r.printf("⏸ waiting for quota: %s", ev.Message)
	case task.StatusNeedsAttention:
		r.printf("! needs attention: %s", ev.Message)
	case task.StatusFailed:
		r.printf("✖ failed: %s", ev.Message)
	case task.StatusCancelled:
		r.printf("✖ cancelled")
	default:
		r.printf("◆ %s", ev.Status)
	}
}

func (r *renderer) agent(ev *runtime.Event) {
	if ev == nil {
		return
	}
	switch ev.Kind {
	case runtime.EventText:
		if r.err != nil || ev.Text == "" {
			return
		}
		text := sanitize(ev.Text)
		if !r.midText {
			text = "  " + text
		}
		text = strings.ReplaceAll(text, "\n", "\n  ")
		if _, err := io.WriteString(r.w, text); err != nil {
			r.err = fmt.Errorf("write output: %w", err)
			return
		}
		r.midText = !strings.HasSuffix(ev.Text, "\n")
	case runtime.EventToolCall:
		if tc := ev.ToolCall; tc != nil {
			r.printf("  ● %s  %s", tc.Name, toolSummary(tc))
		}
	case runtime.EventToolResult:
		if tr := ev.ToolResult; tr != nil && tr.IsError {
			r.printf("    ⎿ error: %s", firstLine(tr.Output, 120))
		}
	case runtime.EventQuota:
		if q := ev.Quota; q != nil && q.Exhausted {
			r.printf("  · quota exhausted")
		}
	case runtime.EventTurnFailed:
		if f := ev.Failure; f != nil {
			r.printf("  · turn ended: %s %s", f.Kind, firstLine(f.Message, 120))
		}
	case runtime.EventTurnCompleted:
	}
}

// sanitize drops control characters other than newline and tab from agent text, so
// output prompted by repository content cannot drive the terminal (ANSI/OSC escapes:
// clipboard writes, title changes, hidden text).
func sanitize(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			return -1
		}
		return r
	}, s)
}

// toolSummary is the most telling argument of a tool call: the command for Bash, the
// path for file tools.
func toolSummary(tc *runtime.ToolCall) string {
	var in map[string]any
	if json.Unmarshal(tc.Input, &in) == nil {
		for _, key := range []string{"command", "file_path", "path", "pattern", "url", "query"} {
			if v, ok := in[key].(string); ok && v != "" {
				return firstLine(v, 100)
			}
		}
	}
	return ""
}

func firstLine(s string, limit int) string {
	line, _, _ := strings.Cut(strings.TrimSpace(sanitize(s)), "\n")
	if r := []rune(line); len(r) > limit {
		return string(r[:limit-1]) + "…"
	}
	return line
}

// shortSHA shortens the commit at the start of s and keeps any note after it.
func shortSHA(s string) string {
	sha, rest, found := strings.Cut(s, " ")
	if len(sha) > 12 {
		sha = sha[:12]
	}
	if found {
		return sha + " " + rest
	}
	return sha
}

// since renders an age for tables.
func since(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
