package codex

import (
	"testing"
	"time"

	"github.com/ugurboz/openchef/internal/runtime"
)

func TestMCPServerToolTimeout(t *testing.T) {
	t.Parallel()
	cfg := mcpServerConfig(&runtime.MCPServer{Command: "openchef", Args: []string{"mcp"}, ToolTimeout: 16 * time.Minute})
	if cfg["tool_timeout_sec"] != 960.0 {
		t.Fatalf("config %v", cfg)
	}
	if _, ok := mcpServerConfig(&runtime.MCPServer{Command: "x"})["tool_timeout_sec"]; ok {
		t.Fatal("timeout set without one")
	}
}
