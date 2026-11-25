package runtime

import (
	"encoding/json"
	"time"
)

// EventKind identifies a normalized event.
type EventKind string

// Event kinds. EventTurnCompleted and EventTurnFailed are terminal.
const (
	EventText          EventKind = "text"
	EventToolCall      EventKind = "tool_call"
	EventToolResult    EventKind = "tool_result"
	EventQuota         EventKind = "quota"
	EventTurnCompleted EventKind = "turn_completed"
	EventTurnFailed    EventKind = "turn_failed"
)

// Terminal reports whether the event ends a turn.
func (k EventKind) Terminal() bool { return k == EventTurnCompleted || k == EventTurnFailed }

// Event is one normalized occurrence in a turn. Exactly one of the pointer fields is set,
// matching Kind (Text uses the Text field).
type Event struct {
	Kind EventKind `json:"kind"`
	At   time.Time `json:"at"`

	Text       string       `json:"text,omitempty"`
	ToolCall   *ToolCall    `json:"toolCall,omitempty"`
	ToolResult *ToolResult  `json:"toolResult,omitempty"`
	Quota      *QuotaSignal `json:"quota,omitempty"`
	Result     *TurnResult  `json:"result,omitempty"`
	Failure    *Failure     `json:"failure,omitempty"`
}

// ToolCall is the agent invoking a tool.
type ToolCall struct {
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input,omitempty"`
	// Signature identifies "the same call" for loop detection: name plus a canonical
	// form of the input.
	Signature string `json:"signature"`
}

// ToolResult reports a finished tool call.
type ToolResult struct {
	ID      string `json:"id,omitempty"`
	IsError bool   `json:"isError"`
	Output  string `json:"output,omitempty"`
}

// QuotaSignal is quota information observed in-stream (L1, §10.1).
type QuotaSignal struct {
	Windows []QuotaWindow `json:"windows"`
	// Exhausted is true when the CLI reports that the limit is reached and cannot be
	// extended.
	Exhausted bool `json:"exhausted"`
}

// QuotaWindow is one rate-limit window, normalized.
type QuotaWindow struct {
	// Name is "five_hour", "weekly" or a CLI-specific name.
	Name string `json:"name"`
	// Utilization is in [0, 1].
	Utilization float64 `json:"utilization"`
	// ResetsAt is the absolute reset time; zero if unknown.
	ResetsAt time.Time `json:"resetsAt"`
	// Duration of the window; zero if unknown.
	Duration time.Duration `json:"duration"`
}

// Usage is the token and cost accounting of one turn, taken from the CLI's turn-level
// totals only.
type Usage struct {
	InputTokens      int     `json:"inputTokens"`
	OutputTokens     int     `json:"outputTokens"`
	CacheReadTokens  int     `json:"cacheReadTokens"`
	CacheWriteTokens int     `json:"cacheWriteTokens"`
	CostUSD          float64 `json:"costUSD,omitempty"`
}

// TurnResult is the outcome of a completed turn.
type TurnResult struct {
	// FinalText is the agent's final answer, from a single source (never the
	// concatenation of streamed text and the result).
	FinalText string `json:"finalText"`
	// Structured holds the schema-validated answer when TurnInput.OutputSchema was set.
	Structured json.RawMessage `json:"structured,omitempty"`
	Usage      Usage           `json:"usage"`
	// Denials counts permission requests refused during the turn.
	Denials int `json:"denials"`
	// Models lists the models that actually served the turn, where the CLI reports it;
	// it can differ from the requested model (e.g. Claude's plan mode, D-008).
	Models   []string      `json:"models,omitempty"`
	Duration time.Duration `json:"duration"`
}

// FailureKind classifies a failed turn.
type FailureKind string

// Failure kinds (§6.1).
const (
	FailQuotaExhausted  FailureKind = "quota_exhausted"
	FailRateLimited     FailureKind = "rate_limited"
	FailAuth            FailureKind = "auth"
	FailContextOverflow FailureKind = "context_overflow"
	FailCrashed         FailureKind = "crashed"
	FailTimeout         FailureKind = "timeout"
	FailInterrupted     FailureKind = "interrupted"
	FailProtocol        FailureKind = "protocol"
	FailUnknown         FailureKind = "unknown"
)

// Failure describes why a turn did not complete.
type Failure struct {
	Kind    FailureKind `json:"kind"`
	Message string      `json:"message"`
	// ResetsAt, for quota and rate-limit failures, is when retrying makes sense.
	ResetsAt time.Time `json:"resetsAt,omitzero"`
	// Usage spent before the failure, if the CLI reported it.
	Usage Usage `json:"usage"`
}
