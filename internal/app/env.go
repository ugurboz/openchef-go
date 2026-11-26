package app

import (
	"strings"

	"github.com/ugurboz/openchef/internal/config"
)

// billingVars are removed from subscription-backed sessions so a CLI cannot silently
// bill an API account or be redirected to another provider (§13): keys and tokens,
// base-URL overrides, and the switches that send Claude Code to Bedrock or Vertex or
// Gemini to Vertex.
var billingVars = []string{
	"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
	"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
	"OPENAI_API_KEY", "OPENAI_BASE_URL", "CODEX_API_KEY",
	"AZURE_OPENAI_API_KEY", "AZURE_OPENAI_ENDPOINT",
	"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_GENAI_USE_VERTEXAI", "GOOGLE_APPLICATION_CREDENTIALS",
}

// sessionEnv is the agent CLI environment for pool: the daemon's environment without
// API keys, except the key an API pool names explicitly.
func sessionEnv(from []string, pool config.Pool) []string {
	out := make([]string, 0, len(from))
	for _, kv := range from {
		k, _, _ := strings.Cut(kv, "=")
		up := strings.ToUpper(k)
		keep := true
		for _, v := range billingVars {
			if up == v {
				keep = pool.Billing == "api" && strings.EqualFold(pool.APIKeyEnv, k)
			}
		}
		if keep {
			out = append(out, kv)
		}
	}
	return out
}
