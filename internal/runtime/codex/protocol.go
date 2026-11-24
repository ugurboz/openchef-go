package codex

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/ugurboz/openchef/internal/runtime"
)

// Wire types of the `codex app-server` protocol subset OpenChef uses (verified against
// codex-cli 0.172.0, docs/research/m1-findings.md §1). Unknown fields are ignored;
// fields OpenChef depends on are checked where they are read.

type threadResponse struct {
	Thread struct {
		ID string `json:"id"`
	} `json:"thread"`
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoningEffort"`
}

type turnStartResponse struct {
	Turn struct {
		ID string `json:"id"`
	} `json:"turn"`
}

type modelListResponse struct {
	Data []struct {
		ID                        string `json:"id"`
		Model                     string `json:"model"`
		IsDefault                 bool   `json:"isDefault"`
		SupportedReasoningEfforts []struct {
			ReasoningEffort string `json:"reasoningEffort"`
		} `json:"supportedReasoningEfforts"`
	} `json:"data"`
}

// supportedEfforts returns the normalized efforts the model accepts. model "" selects
// the default model.
func (r modelListResponse) supportedEfforts(model string) []runtime.Effort {
	for _, m := range r.Data {
		if (model == "" && m.IsDefault) || (model != "" && (m.ID == model || m.Model == model)) {
			var out []runtime.Effort
			for _, e := range m.SupportedReasoningEfforts {
				if eff := runtime.Effort(e.ReasoningEffort); eff.Valid() && eff != runtime.EffortDefault {
					out = append(out, eff)
				}
			}
			return out
		}
	}
	return nil
}

type threadItem struct {
	ID               string          `json:"id"`
	Type             string          `json:"type"`
	Text             string          `json:"text"`
	Phase            string          `json:"phase"`
	Command          string          `json:"command"`
	Cwd              string          `json:"cwd"`
	Status           string          `json:"status"`
	ExitCode         *int            `json:"exitCode"`
	AggregatedOutput string          `json:"aggregatedOutput"`
	Changes          []fileChange    `json:"changes"`
	Server           string          `json:"server"`
	Tool             string          `json:"tool"`
	Arguments        json.RawMessage `json:"arguments"`
	Error            json.RawMessage `json:"error"`
}

type fileChange struct {
	Path string `json:"path"`
	Kind any    `json:"kind"`
}

type itemNotification struct {
	ThreadID string     `json:"threadId"`
	TurnID   string     `json:"turnId"`
	Item     threadItem `json:"item"`
}

type deltaNotification struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	Delta    string `json:"delta"`
}

type tokenBreakdown struct {
	InputTokens           int `json:"inputTokens"`
	CachedInputTokens     int `json:"cachedInputTokens"`
	CacheWriteInputTokens int `json:"cacheWriteInputTokens"`
	OutputTokens          int `json:"outputTokens"`
	ReasoningOutputTokens int `json:"reasoningOutputTokens"`
}

func (b tokenBreakdown) minus(o tokenBreakdown) tokenBreakdown {
	return tokenBreakdown{
		InputTokens:           b.InputTokens - o.InputTokens,
		CachedInputTokens:     b.CachedInputTokens - o.CachedInputTokens,
		CacheWriteInputTokens: b.CacheWriteInputTokens - o.CacheWriteInputTokens,
		OutputTokens:          b.OutputTokens - o.OutputTokens,
		ReasoningOutputTokens: b.ReasoningOutputTokens - o.ReasoningOutputTokens,
	}
}

// usage converts to the normalized form. Codex reports cached tokens as part of
// inputTokens; OpenChef counts them separately so the fields are additive.
func (b tokenBreakdown) usage() runtime.Usage {
	return runtime.Usage{
		InputTokens:      max(0, b.InputTokens-b.CachedInputTokens),
		OutputTokens:     b.OutputTokens,
		CacheReadTokens:  b.CachedInputTokens,
		CacheWriteTokens: b.CacheWriteInputTokens,
	}
}

type tokenUsageNotification struct {
	ThreadID   string `json:"threadId"`
	TurnID     string `json:"turnId"`
	TokenUsage struct {
		Last  tokenBreakdown `json:"last"`
		Total tokenBreakdown `json:"total"`
	} `json:"tokenUsage"`
}

type turnCompletedNotification struct {
	ThreadID string `json:"threadId"`
	Turn     struct {
		ID         string     `json:"id"`
		Status     string     `json:"status"`
		Error      *turnError `json:"error"`
		DurationMs *int64     `json:"durationMs"`
	} `json:"turn"`
}

type turnError struct {
	Message           string          `json:"message"`
	AdditionalDetails string          `json:"additionalDetails"`
	CodexErrorInfo    json.RawMessage `json:"codexErrorInfo"`
}

type errorNotification struct {
	ThreadID  string    `json:"threadId"`
	TurnID    string    `json:"turnId"`
	Error     turnError `json:"error"`
	WillRetry bool      `json:"willRetry"`
}

// failureKind maps codexErrorInfo (a string enum, or an object for parameterized
// variants such as {"httpConnectionFailed": {...}}) to a normalized failure kind.
func failureKind(info json.RawMessage) runtime.FailureKind {
	name := errorInfoName(info)
	switch name {
	case "usageLimitExceeded", "sessionBudgetExceeded", "apiKeySpendLimitExceeded",
		"projectSpendLimitExceeded", "organizationSpendLimitExceeded":
		return runtime.FailQuotaExhausted
	case "rateLimitExceeded", "serverOverloaded", "httpConnectionFailed", "flexUnavailable",
		"policyCapacityUnavailable", "internalServerError":
		return runtime.FailRateLimited
	case "unauthorized":
		return runtime.FailAuth
	case "contextWindowExceeded":
		return runtime.FailContextOverflow
	default:
		return runtime.FailUnknown
	}
}

func errorInfoName(info json.RawMessage) string {
	if len(info) == 0 || string(info) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(info, &s) == nil {
		return s
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(info, &obj) == nil {
		for k := range obj {
			return k
		}
	}
	return ""
}

func (e *turnError) failure(kind runtime.FailureKind) *runtime.Failure {
	msg := e.Message
	if e.AdditionalDetails != "" {
		msg += ": " + e.AdditionalDetails
	}
	if name := errorInfoName(e.CodexErrorInfo); name != "" {
		msg = fmt.Sprintf("%s [%s]", msg, name)
	}
	return &runtime.Failure{Kind: kind, Message: msg}
}

type rateLimitWindow struct {
	UsedPercent        float64 `json:"usedPercent"`
	WindowDurationMins *int64  `json:"windowDurationMins"`
	ResetsAt           *int64  `json:"resetsAt"`
}

type rateLimitSnapshot struct {
	LimitID              string           `json:"limitId"`
	PlanType             string           `json:"planType"`
	Primary              *rateLimitWindow `json:"primary"`
	Secondary            *rateLimitWindow `json:"secondary"`
	RateLimitReachedType json.RawMessage  `json:"rateLimitReachedType"`
}

type rateLimitsRead struct {
	RateLimits rateLimitSnapshot `json:"rateLimits"`
}

type rateLimitsUpdated struct {
	RateLimits rateLimitSnapshot `json:"rateLimits"`
}

// merge applies a sparse update (nil windows keep their previous value).
func (s rateLimitSnapshot) merge(update rateLimitSnapshot) rateLimitSnapshot {
	out := s
	if update.Primary != nil {
		out.Primary = update.Primary
	}
	if update.Secondary != nil {
		out.Secondary = update.Secondary
	}
	if update.PlanType != "" {
		out.PlanType = update.PlanType
	}
	if len(update.RateLimitReachedType) > 0 {
		out.RateLimitReachedType = update.RateLimitReachedType
	}
	return out
}

func (s rateLimitSnapshot) signal() *runtime.QuotaSignal {
	sig := &runtime.QuotaSignal{}
	for _, w := range []*rateLimitWindow{s.Primary, s.Secondary} {
		if w == nil {
			continue
		}
		qw := runtime.QuotaWindow{Utilization: clamp01(w.UsedPercent / 100)}
		if w.WindowDurationMins != nil {
			qw.Duration = time.Duration(*w.WindowDurationMins) * time.Minute
		}
		qw.Name = windowName(qw.Duration)
		if w.ResetsAt != nil && *w.ResetsAt > 0 {
			qw.ResetsAt = time.Unix(*w.ResetsAt, 0).UTC()
		}
		if qw.Utilization >= 1 {
			sig.Exhausted = true
		}
		sig.Windows = append(sig.Windows, qw)
	}
	if reached := strings.TrimSpace(string(s.RateLimitReachedType)); reached != "" && reached != "null" {
		sig.Exhausted = true
	}
	if len(sig.Windows) == 0 && !sig.Exhausted {
		return nil
	}
	return sig
}

func windowName(d time.Duration) string {
	switch d {
	case 5 * time.Hour:
		return "five_hour"
	case 7 * 24 * time.Hour:
		return "weekly"
	case 0:
		return "unknown"
	default:
		return fmt.Sprintf("window_%dm", int(d.Minutes()))
	}
}

func clamp01(f float64) float64 { return max(0, min(1, f)) }

// Server-request parameter shapes.

type commandApprovalParams struct {
	ThreadID string `json:"threadId"`
	TurnID   string `json:"turnId"`
	ItemID   string `json:"itemId"`
	Command  string `json:"command"`
	Cwd      string `json:"cwd"`
	Reason   string `json:"reason"`
}

type fileChangeApprovalParams struct {
	ThreadID  string `json:"threadId"`
	TurnID    string `json:"turnId"`
	ItemID    string `json:"itemId"`
	Reason    string `json:"reason"`
	GrantRoot string `json:"grantRoot"`
}

type elicitationParams struct {
	ThreadID   string `json:"threadId"`
	ServerName string `json:"serverName"`
	Message    string `json:"message"`
	Meta       struct {
		ApprovalKind string          `json:"codex_approval_kind"`
		ToolName     string          `json:"tool_name"`
		ToolParams   json.RawMessage `json:"tool_params"`
	} `json:"_meta"`
}

type userInputParams struct {
	ThreadID  string `json:"threadId"`
	Questions []struct {
		ID       string `json:"id"`
		Question string `json:"question"`
		Options  []struct {
			Label string `json:"label"`
		} `json:"options"`
	} `json:"questions"`
}
