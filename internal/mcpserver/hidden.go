package mcpserver

import (
	"context"
	"fmt"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/idoo/connect-iq-mcp/internal/tool"
)

// errToolsetDisabled is the result of calling a tool whose toolset is off in
// this session.
var errToolsetDisabled = &tool.Error{
	Code:    "toolset_disabled",
	Message: "this tool's toolset is off in this session",
	Hint:    `call toolsets with action "enable" and this toolset; the tool appears after list_changed`,
}

// hiddenToolMiddleware answers a tools/call for a registered but unlisted
// tool with a tool error naming the unlock call, instead of the SDK's
// "unknown tool". Listed and truly unknown names pass through.
func (s *session) hiddenToolMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method != "tools/call" {
			return next(ctx, method, req)
		}
		r, ok := req.(*mcp.CallToolRequest)
		if !ok || r.Params == nil {
			return next(ctx, method, req)
		}
		if res := s.hiddenResult(r.Params.Name); res != nil {
			// Same _meta.garmin as a real call: the agent still learns which
			// profile it is acting on.
			p := s.profile()
			res.Meta.Profile = p.Name
			res.Meta.PinSource = p.PinSource
			res.Meta.ApprovedBy = tool.ApprovedNone
			return s.render(res), nil
		}
		return next(ctx, method, req)
	}
}

// hiddenResult returns the error result for a hidden tool, or nil when name
// is listed, unknown or no longer hidden.
func (s *session) hiddenResult(name string) *tool.Result {
	if t, ok := s.offPlatform[name]; ok {
		label := tool.PlatformLabel(t.Meta().Platforms)
		return tool.Fail(&tool.Error{
			Code:    tool.ErrUnavailable.Code,
			Message: fmt.Sprintf("%s is %s; this server runs on %s", name, label, s.deps.Platform),
			Hint:    "run connect-iq-mcp on a supported OS for this tool",
		})
	}
	s.mu.Lock()
	listed := s.listed[name]
	s.mu.Unlock()
	if listed {
		return nil
	}
	i := slices.IndexFunc(s.tools, func(t tool.Tool) bool { return t.Name() == name })
	if i < 0 {
		return nil
	}
	m := s.tools[i].Meta()
	writeLocked := s.writeLocked()[name]
	s.mu.Lock()
	on := s.on[m.Toolset]
	s.mu.Unlock()
	switch {
	case !on:
		r := tool.Fail(errToolsetDisabled).Then(tool.NextStep{Tool: toolsetsName, Args: map[string]any{"action": "enable", "toolsets": []string{string(m.Toolset)}}})
		if writeLocked {
			r.Then(unlockWrites)
		}
		return r
	case writeLocked:
		return tool.Fail(fmt.Errorf("%s: %w", name, tool.ErrWritesDisabled))
	}
	return nil
}
