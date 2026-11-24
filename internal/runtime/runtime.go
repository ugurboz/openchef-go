// Package runtime defines the contract between the engine and the agent CLIs it drives
// (docs/architecture.md §6.1). Each CLI has an adapter in a sub-package that implements
// Runtime and Session; the engine only ever sees the normalized types declared here.
package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
)

// Runtime starts and resumes sessions for one agent CLI.
type Runtime interface {
	// Name is the runtime identifier used in configuration ("claude", "codex", "agy").
	Name() string
	// Open starts a new session.
	Open(ctx context.Context, spec SessionSpec) (Session, error)
	// Resume continues a session started earlier, possibly by another process.
	Resume(ctx context.Context, ref SessionRef, spec SessionSpec) (Session, error)
}

// Session is one live conversation with an agent. It is not safe for concurrent turns:
// a second Turn while one is running returns ErrTurnInProgress.
type Session interface {
	// Ref identifies the session for persistence and Resume.
	Ref() SessionRef
	// Turn sends one user message and streams this turn's events. The channel delivers
	// exactly one terminal event (EventTurnCompleted or EventTurnFailed) and is then
	// closed. Events from earlier turns or earlier processes are never replayed.
	Turn(ctx context.Context, in TurnInput) (<-chan Event, error)
	// Interrupt cancels the turn in progress; the session stays usable.
	Interrupt(ctx context.Context) error
	// Close ends the session and its process. It is idempotent.
	Close(ctx context.Context) error
}

// SessionRef is the CLI-native identity of a session.
type SessionRef struct {
	Runtime string `json:"runtime"`
	// ID is the Claude session id, the Codex thread id or the ACP session id.
	ID string `json:"id"`
}

// SessionSpec describes how to run a session.
type SessionSpec struct {
	// Dir is the working directory (the task's worktree).
	Dir string
	// Model is passed to the CLI as-is; empty means the CLI's default.
	Model string
	// Effort is the normalized reasoning effort; adapters map it to the CLI's values.
	Effort Effort
	// Mode is the permission mode (Claude Code vocabulary, §9).
	Mode Mode
	// DenyRules are permission rules in Claude Code syntax that must never be allowed.
	// Adapters pass them natively where the CLI supports it (Claude) and the approver
	// enforces them for every request it sees.
	DenyRules []string
	// Env is the complete child environment (never inherited implicitly).
	Env []string
	// Approver decides permission requests and answers questions. Required.
	Approver Approver
	// AskUser, if set, is the MCP server that provides OpenChef's ask_user tool.
	AskUser *MCPServer
	// LogPath, if set, receives every raw protocol line (see package rawlog).
	LogPath string
}

// MCPServer is a stdio MCP server command.
type MCPServer struct {
	Name    string
	Command string
	Args    []string
	Env     []string
}

// TurnInput is one user message.
type TurnInput struct {
	Text string
	// OutputSchema, if set, is a JSON Schema the final answer must satisfy.
	OutputSchema json.RawMessage
}

// Effort is the normalized reasoning effort scale (§7.4).
type Effort string

// Effort levels, lowest first.
const (
	EffortDefault Effort = "" // let the CLI decide
	EffortLow     Effort = "low"
	EffortMedium  Effort = "medium"
	EffortHigh    Effort = "high"
	EffortMax     Effort = "max"
)

var effortOrder = []Effort{EffortLow, EffortMedium, EffortHigh, EffortMax}

// Valid reports whether e is a known effort level or the default.
func (e Effort) Valid() bool { return e == EffortDefault || slices.Contains(effortOrder, e) }

// Nearest returns the highest level in supported that does not exceed e. If every
// supported level is above e, the lowest supported level is returned. It returns ""
// when supported is empty or e is the default.
func (e Effort) Nearest(supported []Effort) Effort {
	if e == EffortDefault || len(supported) == 0 {
		return EffortDefault
	}
	rank := slices.Index(effortOrder, e)
	best, bestRank := EffortDefault, -1
	lowest, lowestRank := EffortDefault, len(effortOrder)
	for _, s := range supported {
		r := slices.Index(effortOrder, s)
		if r < 0 {
			continue
		}
		if r <= rank && r > bestRank {
			best, bestRank = s, r
		}
		if r < lowestRank {
			lowest, lowestRank = s, r
		}
	}
	if best != EffortDefault {
		return best
	}
	return lowest
}

// Mode is a permission mode, identical to Claude Code's (§9.1).
type Mode string

// Permission modes.
const (
	ModeManual            Mode = "manual"
	ModeAcceptEdits       Mode = "acceptEdits"
	ModePlan              Mode = "plan"
	ModeAuto              Mode = "auto"
	ModeDontAsk           Mode = "dontAsk"
	ModeBypassPermissions Mode = "bypassPermissions"
)

// Modes lists every permission mode.
var Modes = []Mode{ModeManual, ModeAcceptEdits, ModePlan, ModeAuto, ModeDontAsk, ModeBypassPermissions}

// ParseMode accepts the canonical names and Claude Code's legacy alias "default".
func ParseMode(s string) (Mode, error) {
	if s == "default" {
		return ModeManual, nil
	}
	m := Mode(s)
	if slices.Contains(Modes, m) {
		return m, nil
	}
	return "", fmt.Errorf("runtime: unknown permission mode %q", s)
}
