package mcpserver

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/idoo/garmin-mcp/internal/config"
	"github.com/idoo/garmin-mcp/internal/tool"
)

type toolsetsIn struct {
	Action   string   `json:"action" jsonschema:"what to do"`
	Toolsets []string `json:"toolsets,omitempty" jsonschema:"toolsets for enable/disable"`
}

type lockedTool struct {
	Tool   string `json:"tool"`
	Reason string `json:"reason"`
}

type toolsetEntry struct {
	tool.ToolsetInfo
	Locked []lockedTool `json:"locked,omitempty"`
}

// toolsetsTool builds the session's toolsets tool (core, local). Its
// enable_writes action changes the session's security mode, so it is a Write
// tool (no readOnlyHint) that stays listed in read-only mode.
func (s *session) toolsetsTool() tool.Tool {
	names := make([]any, len(tool.AllToolsets))
	for i, ts := range tool.AllToolsets {
		names[i] = string(ts)
	}
	return tool.New(tool.Meta{
		Name:              toolsetsName,
		Toolset:           tool.Core,
		Title:             "Toolsets",
		Description:       "List toolsets, enable or disable them for this session (tool list refreshes), or ask the user to allow Connect IQ writes for this session.",
		Class:             tool.Write,
		AvailableReadOnly: true,
		Local:             true,
		Enums: map[string][]any{
			"action":   {"list", "enable", "disable", "enable_writes"},
			"toolsets": names,
		},
	}, s.toolsetsHandler)
}

func (s *session) toolsetsHandler(ctx context.Context, env *tool.Env, in toolsetsIn) (*tool.Result, error) {
	switch in.Action {
	case "list":
		return tool.OK(map[string]any{"writes": env.Profile.AllowWrites, "toolsets": s.listDetailed()}), nil
	case "enable", "disable":
		ts := make([]tool.Toolset, len(in.Toolsets))
		for i, n := range in.Toolsets {
			ts[i] = tool.Toolset(n)
		}
		before := map[tool.Toolset]bool{}
		for _, info := range s.List() {
			before[info.Name] = info.Enabled
		}
		var err error
		if in.Action == "enable" {
			err = s.Enable(ctx, ts...)
		} else {
			err = s.Disable(ctx, ts...)
		}
		if err != nil {
			return nil, err
		}
		return tool.OK(map[string]any{"changed": s.changedSince(before)}), nil
	default:
		return s.enableWritesAction(ctx, env)
	}
}

func (s *session) listDetailed() []toolsetEntry {
	p := s.profile()
	infos := s.List()
	out := make([]toolsetEntry, len(infos))
	for i, info := range infos {
		out[i].ToolsetInfo = info
		for _, t := range s.tools {
			if t.Meta().Toolset != info.Name || t.ListedIn(p) {
				continue
			}
			out[i].Locked = append(out[i].Locked, lockedTool{Tool: t.Name(), Reason: "writes_disabled"})
		}
	}
	return out
}

// changedToolset is one toolset whose enabled state an enable/disable call
// flipped; an enabled one lists the tools it added and those still locked.
type changedToolset struct {
	Name    tool.Toolset `json:"name"`
	Enabled bool         `json:"enabled"`
	Tools   []string     `json:"tools,omitempty"`
	Locked  []lockedTool `json:"locked,omitempty"`
}

// changedSince reports only the toolsets whose enabled state differs from
// before; the client re-reads the full list via list_changed.
func (s *session) changedSince(before map[tool.Toolset]bool) []changedToolset {
	p := s.profile()
	out := []changedToolset{}
	for _, e := range s.listDetailed() {
		if before[e.Name] == e.Enabled {
			continue
		}
		c := changedToolset{Name: e.Name, Enabled: e.Enabled}
		if e.Enabled {
			c.Locked = e.Locked
			for _, t := range s.tools {
				if t.Meta().Toolset == e.Name && t.ListedIn(p) {
					c.Tools = append(c.Tools, t.Name())
				}
			}
		}
		out = append(out, c)
	}
	return out
}

func (s *session) enableWritesAction(ctx context.Context, env *tool.Env) (*tool.Result, error) {
	if env.Profile.AllowWrites {
		return tool.Already(map[string]any{"writes": true}), nil
	}
	if s.deps.WritesLocked {
		return nil, errWritesLocked
	}
	ok, err := env.Elicit(ctx, tool.EscapeHidden(fmt.Sprintf("Allow Connect IQ writes for profile \"%s\" for this session?", env.Profile.Name)))
	if isElicitUnavailable(err) {
		return tool.OK(map[string]any{
			"writes":       false,
			"instructions": writesInstructions(clientName(ctx), env.Profile.Name),
		}), nil
	}
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("enable_writes: %w", tool.ErrDeclined)
	}
	s.enableWrites()
	env.SetWrites(true)
	return tool.OK(map[string]any{"writes": true, "toolsets": s.listDetailed()}), nil
}

func clientName(ctx context.Context) string {
	ss := sessionFrom(ctx)
	if ss == nil {
		return ""
	}
	if p := ss.InitializeParams(); p != nil && p.ClientInfo != nil {
		return p.ClientInfo.Name
	}
	return ""
}

// writesInstructions returns copy-paste steps to restart garmin-mcp with writes on
// for the detected MCP client. They only run for clients that cannot elicit,
// so they also name allow_no_elicit: without it every publish, listing edit and
// review reply fails with human_approval_required and needs a second restart.
func writesInstructions(client, profile string) string {
	cfgPath, err := config.DefaultPath(os.Getenv)
	if err != nil {
		cfgPath = "the garmin-mcp config.toml"
	}
	if profile == "" {
		profile = config.DefaultProfileName
	}
	tail := " Publishing, listing edits and review replies also need allow_no_elicit = true under [profiles." + profile + "] in " + cfgPath +
		", because this client can't show the approval; set both " + config.EnvAllowWrites + "=1 and allow_no_elicit before one restart." +
		" With allow_no_elicit on, those public actions go through on the agent's confirm alone, without a human yes." +
		" Then restart the client. Alternatively set allow_writes = true in that same section instead of the env variable."
	c := strings.ToLower(client)
	switch {
	case strings.Contains(c, "claude-code") || strings.Contains(c, "claude code"):
		return "Ask the user to run: claude mcp remove garmin-mcp -s user && claude mcp add garmin-mcp -s user -e GARMIN_MCP_ALLOW_WRITES=1 <existing -e flags> -- garmin-mcp." + tail
	case strings.Contains(c, "claude"):
		return `Ask the user to add "GARMIN_MCP_ALLOW_WRITES": "1" to mcpServers.garmin-mcp.env in claude_desktop_config.json.` + tail
	case strings.Contains(c, "cursor"):
		return `Ask the user to add "GARMIN_MCP_ALLOW_WRITES": "1" to mcpServers.garmin-mcp.env in ~/.cursor/mcp.json.` + tail
	case strings.Contains(c, "codex"):
		return `Ask the user to add GARMIN_MCP_ALLOW_WRITES = "1" to env of [mcp_servers.garmin-mcp] in ~/.codex/config.toml.` + tail
	case strings.Contains(c, "visual studio code") || strings.Contains(c, "vscode"):
		where := `the user mcp.json (run "MCP: Open User Configuration")`
		if paths := vscodeUserConfigs(); len(paths) > 0 {
			where = `the user mcp.json that "garmin-mcp config install vscode" writes, ` + strings.Join(paths, " or ") + ` (run "MCP: Open User Configuration")`
		}
		return `Ask the user to add "GARMIN_MCP_ALLOW_WRITES": "1" to servers.garmin-mcp.env in ` + where + `.` + tail
	case strings.Contains(c, "windsurf"):
		return `Ask the user to add "GARMIN_MCP_ALLOW_WRITES": "1" to mcpServers.garmin-mcp.env in ~/.codeium/windsurf/mcp_config.json.` + tail
	default:
		return "Ask the user to set the environment variable GARMIN_MCP_ALLOW_WRITES=1 in this client's garmin-mcp server config (or start garmin-mcp with --allow-writes)." + tail
	}
}

// vscodeUserConfigs lists the VS Code user mcp.json files `garmin-mcp config
// install vscode` writes on this machine (nil when the dirs are unknown);
// tests replace it.
var vscodeUserConfigs = func() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	cfg, err := os.UserConfigDir()
	if err != nil {
		return nil
	}
	return config.VSCodeUserConfigs(runtime.GOOS, home, cfg)
}
