package mcpserver

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/idoo/garmin-mcp/internal/tool"
)

func TestEnableWritesPromptEscapesHidden(t *testing.T) {
	var prompts []string
	elicit := func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		prompts = append(prompts, req.Params.Message)
		return &mcp.ElicitResult{Action: "decline"}, nil
	}
	h := connect(t, Deps{Profile: tool.ProfileInfo{Name: "work\u202Eevil"}}, "test", elicit)
	h.call(t, "toolsets", map[string]any{"action": "enable_writes"})
	if len(prompts) != 1 {
		t.Fatalf("prompts = %q", prompts)
	}
	p := prompts[0]
	if strings.ContainsRune(p, '\u202E') || !strings.Contains(p, `\u{202E}`) || !strings.HasPrefix(p, "WARNING: contains hidden characters") {
		t.Fatalf("hidden character reached the human unescaped: %q", p)
	}
}
