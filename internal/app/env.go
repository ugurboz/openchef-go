package app

import (
	"strings"

	"github.com/ugurboz/openchef/internal/config"
)

// apiKeyVars are removed from subscription-backed sessions so a CLI cannot silently
// bill an API account (§13).
var apiKeyVars = []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "CODEX_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY"}

// sessionEnv is the agent CLI environment for pool: the daemon's environment without
// API keys, except the key an API pool names explicitly.
func sessionEnv(from []string, pool config.Pool) []string {
	out := make([]string, 0, len(from))
	for _, kv := range from {
		k, _, _ := strings.Cut(kv, "=")
		up := strings.ToUpper(k)
		keep := true
		for _, v := range apiKeyVars {
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
