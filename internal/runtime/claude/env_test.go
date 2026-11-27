package claude

import (
	"slices"
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/runtime"
)

func TestProcessEnvSetsMCPToolTimeout(t *testing.T) {
	t.Parallel()
	spec := runtime.SessionSpec{Env: []string{"PATH=/bin", "MCP_TOOL_TIMEOUT=1"}}
	if got := processEnv(&spec); !slices.Equal(got, spec.Env) {
		t.Fatalf("without ask_user: %v", got)
	}
	spec.AskUser = &runtime.MCPServer{Name: "openchef", Command: "openchef", ToolTimeout: 16 * time.Minute}
	got := processEnv(&spec)
	if !slices.Equal(got, []string{"PATH=/bin", "MCP_TOOL_TIMEOUT=960000"}) {
		t.Fatalf("with ask_user: %v", got)
	}
	if spec.Env[1] != "MCP_TOOL_TIMEOUT=1" {
		t.Fatal("spec env modified")
	}
}
