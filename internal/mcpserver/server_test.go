package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/idoo/garmin-mcp/internal/tool"
)

type stubIn struct {
	tool.ConfirmFlag
	App string `json:"app,omitempty" jsonschema:"app name or id"`
}

func okHandler(context.Context, *tool.Env, stubIn) (*tool.Result, error) {
	return tool.OK(map[string]any{"ok": true}), nil
}

func stub(name string, ts tool.Toolset, c tool.Class, mod func(*tool.Meta)) tool.Tool {
	m := tool.Meta{Name: name, Toolset: ts, Title: name, Description: "stub " + name, Class: c}
	if c == tool.Destructive {
		m.Confirm = tool.ConfirmByFlag
	}
	if mod != nil {
		mod(&m)
	}
	return tool.New(m, okHandler)
}

type noteIn struct {
	tool.ConfirmFlag
	Value string `json:"value" jsonschema:"new value"`
}

// fixture is a small registry: reads and writes across toolsets, an opt-in
// toolset, a read-only-available tool, a confirm:true tool and a panic.
type fixture struct {
	writes atomic.Int32
}

func (f *fixture) tools() []tool.Tool {
	return []tool.Tool{
		stub("apps_list", tool.Core, tool.Read, nil),
		stub("api_request", tool.Core, tool.Destructive, func(m *tool.Meta) { m.AvailableReadOnly = true; m.Confirm = tool.ConfirmNone }),
		stub("builds_list", tool.Store, tool.Read, nil),
		stub("release_submit", tool.Store, tool.Destructive, nil),
		stub("users_list", tool.Connect, tool.Read, nil),
		stub("signing_bundle_ids_list", tool.Connect, tool.Read, nil),
		tool.New(tool.Meta{Name: "boom", Toolset: tool.Core, Description: "panics", Class: tool.Read},
			func(context.Context, *tool.Env, stubIn) (*tool.Result, error) { panic("kaboom") }),
		tool.New(tool.Meta{Name: "note_set", Toolset: tool.Store, Description: "set note", Class: tool.Write, Confirm: tool.ConfirmByFlag},
			func(ctx context.Context, env *tool.Env, in noteIn) (*tool.Result, error) {
				wctx, res := tool.RequireConfirm(ctx, env, in.Confirm, tool.PlanPreview{Summary: "set note", Intended: in.Value})
				if res != nil {
					return res, nil
				}
				if !tool.IsWrite(wctx) {
					return nil, fmt.Errorf("no write capability")
				}
				f.writes.Add(1)
				return tool.OK(map[string]any{"value": in.Value}), nil
			}),
	}
}

type harness struct {
	cs      *mcp.ClientSession
	s       *session
	changed chan struct{}
}

// connect links a client to a fresh session. proto overrides the negotiated
// protocol version (default 2025-11-25, which allows in-request elicitation).
func connect(t *testing.T, deps Deps, client string, elicit func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error), proto ...string) *harness {
	t.Helper()
	version := "2025-11-25"
	if len(proto) > 0 {
		version = proto[0]
	}
	return connectOpts(t, deps, client, &mcp.ClientOptions{ElicitationHandler: elicit}, version)
}

// connectOpts is connect with explicit client options (e.g. declared
// capabilities); the list_changed handler is always installed.
func connectOpts(t *testing.T, deps Deps, client string, opts *mcp.ClientOptions, version string) *harness {
	t.Helper()
	ctx := context.Background()
	s, err := newSession(deps)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{s: s, changed: make(chan struct{}, 64)}
	ct, st := mcp.NewInMemoryTransports()
	ss, err := s.srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	opts.ToolListChangedHandler = func(context.Context, *mcp.ToolListChangedRequest) { h.changed <- struct{}{} }
	h.cs, err = mcp.NewClient(&mcp.Implementation{Name: client, Version: "1"}, opts).Connect(ctx, ct, &mcp.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.cs.Close(); _ = ss.Close() })
	return h
}

func (h *harness) names(t *testing.T) []string {
	t.Helper()
	var out []string
	for tl, err := range h.cs.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, tl.Name)
	}
	slices.Sort(out)
	return out
}

func (h *harness) call(t *testing.T, name string, args any) (*mcp.CallToolResult, map[string]any) {
	t.Helper()
	res, err := h.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	var body map[string]any
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &body); err != nil {
		t.Fatal(err)
	}
	return res, body
}

func dataOf(body map[string]any) map[string]any {
	d, _ := body["data"].(map[string]any)
	return d
}

func team(writes bool) tool.ProfileInfo {
	return tool.ProfileInfo{Name: "work", AllowWrites: writes}
}

func TestListingPolicy(t *testing.T) {
	tests := []struct {
		name string
		deps func(*fixture) Deps
		want []string
	}{
		{
			"read-only", func(f *fixture) Deps { return Deps{Tools: f.tools(), Profile: team(false)} },
			[]string{"api_request", "apps_list", "boom", "builds_list", "toolsets"},
		},
		{
			"writes", func(f *fixture) Deps { return Deps{Tools: f.tools(), Profile: team(true)} },
			[]string{"api_request", "apps_list", "boom", "builds_list", "note_set", "release_submit", "toolsets"},
		},
		{
			"discovery", func(f *fixture) Deps { return Deps{Tools: f.tools(), Profile: team(true), Discovery: true} },
			[]string{"api_request", "apps_list", "boom", "toolsets"},
		},
		{"all toolsets", func(f *fixture) Deps {
			return Deps{Tools: f.tools(), Profile: team(false), Toolsets: tool.AllToolsets}
		}, []string{"api_request", "apps_list", "boom", "builds_list", "signing_bundle_ids_list", "toolsets", "users_list"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := connect(t, tt.deps(&fixture{}), "test", nil)
			if got := h.names(t); !slices.Equal(got, tt.want) {
				t.Fatalf("tools = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestEnableToolsetSendsListChanged(t *testing.T) {
	f := &fixture{}
	h := connect(t, Deps{Tools: f.tools(), Profile: team(false)}, "test", nil)
	_ = h.names(t)
	res, body := h.call(t, "toolsets", map[string]any{"action": "enable", "toolsets": []string{"connect"}})
	if res.IsError {
		t.Fatalf("enable failed: %+v", res.Content)
	}
	changed, _ := dataOf(body)["changed"].([]any)
	if dataOf(body)["toolsets"] != nil || len(changed) != 1 ||
		changed[0].(map[string]any)["name"] != "connect" ||
		!strings.Contains(fmt.Sprint(changed[0]), "signing_bundle_ids_list") {
		t.Fatalf("enable result = %v, want only the connect toolset", body)
	}
	if _, again := h.call(t, "toolsets", map[string]any{"action": "enable", "toolsets": []string{"connect"}}); len(dataOf(again)["changed"].([]any)) != 0 {
		t.Fatalf("re-enable result = %v, want no changes", again)
	}
	select {
	case <-h.changed:
	case <-time.After(5 * time.Second):
		t.Fatal("no tools/list_changed notification")
	}
	if got := h.names(t); !slices.Contains(got, "signing_bundle_ids_list") {
		t.Fatalf("signing tool not listed after enable: %v", got)
	}
	if res, _ := h.call(t, "toolsets", map[string]any{"action": "disable", "toolsets": []string{"core"}}); !res.IsError {
		t.Fatal("disabling core succeeded")
	}
	_, body = h.call(t, "toolsets", map[string]any{"action": "list"})
	sets, _ := dataOf(body)["toolsets"].([]any)
	var release map[string]any
	for _, s := range sets {
		if m := s.(map[string]any); m["name"] == "store" {
			release = m
		}
	}
	locked, _ := release["locked"].([]any)
	if len(locked) != 2 || !strings.Contains(fmt.Sprint(locked), "writes_disabled") {
		t.Fatalf("release locked = %v", release)
	}
}

func TestToolsetsToolNotReadOnlyHint(t *testing.T) {
	h := connect(t, Deps{Tools: (&fixture{}).tools(), Profile: team(false)}, "test", nil)
	res, err := h.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range res.Tools {
		if tl.Name == "toolsets" {
			if tl.Annotations == nil || tl.Annotations.ReadOnlyHint || tl.Annotations.DestructiveHint == nil || *tl.Annotations.DestructiveHint {
				t.Fatalf("toolsets annotations = %+v, want write (not read-only, not destructive)", tl.Annotations)
			}
			return
		}
	}
	t.Fatal("toolsets not listed in read-only mode")
}

func TestEnableWrites(t *testing.T) {
	answer := func(action string) func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		return func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			if props, _ := req.Params.RequestedSchema.(map[string]any)["properties"].(map[string]any); len(props) != 0 {
				return nil, fmt.Errorf("approval form has fields: %v", props)
			}
			return &mcp.ElicitResult{Action: action, Content: map[string]any{}}, nil
		}
	}
	accept := func(ok bool) func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		if ok {
			return answer("accept")
		}
		return answer("decline")
	}
	args := map[string]any{"action": "enable_writes"}

	t.Run("accept", func(t *testing.T) {
		var flipped []bool
		deps := Deps{Tools: (&fixture{}).tools(), Profile: team(false)}
		deps.Env.SetWrites = func(on bool) { flipped = append(flipped, on) }
		h := connect(t, deps, "test", accept(true))
		if res, _ := h.call(t, "toolsets", args); res.IsError {
			t.Fatalf("enable_writes failed: %+v", res.Content)
		}
		if got := h.names(t); !slices.Contains(got, "release_submit") {
			t.Fatalf("write tools not listed: %v", got)
		}
		if !slices.Equal(flipped, []bool{true}) {
			t.Fatalf("SetWrites calls = %v, want [true]", flipped)
		}
	})
	t.Run("decline", func(t *testing.T) {
		h := connect(t, Deps{Tools: (&fixture{}).tools(), Profile: team(false)}, "test", accept(false))
		res, body := h.call(t, "toolsets", args)
		if !res.IsError || dataOf(body)["error"] != "declined" {
			t.Fatalf("decline result = %v", body)
		}
		if got := h.names(t); slices.Contains(got, "release_submit") {
			t.Fatal("writes enabled after decline")
		}
	})
	t.Run("no elicitation", func(t *testing.T) {
		h := connect(t, Deps{Tools: (&fixture{}).tools(), Profile: team(false)}, "cursor", nil)
		res, body := h.call(t, "toolsets", args)
		instr, _ := dataOf(body)["instructions"].(string)
		if res.IsError || !strings.Contains(instr, "~/.cursor/mcp.json") || !strings.Contains(instr, "GARMIN_MCP_ALLOW_WRITES") {
			t.Fatalf("fallback = %v", body)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		h := connect(t, Deps{Tools: (&fixture{}).tools(), Profile: team(false)}, "test", answer("cancel"))
		if res, body := h.call(t, "toolsets", args); !res.IsError || dataOf(body)["error"] != "declined" {
			t.Fatalf("cancel result = %v", body)
		}
	})
	t.Run("url-only elicitation gets setup instructions", func(t *testing.T) {
		opts := &mcp.ClientOptions{
			ElicitationHandler: answer("accept"),
			Capabilities:       &mcp.ClientCapabilities{Elicitation: &mcp.ElicitationCapabilities{URL: &mcp.URLElicitationCapabilities{}}},
		}
		h := connectOpts(t, Deps{Tools: (&fixture{}).tools(), Profile: team(false)}, "cursor", opts, "2025-11-25")
		res, body := h.call(t, "toolsets", args)
		instr, _ := dataOf(body)["instructions"].(string)
		if res.IsError || !strings.Contains(instr, "GARMIN_MCP_ALLOW_WRITES") {
			t.Fatalf("url-only result = %v", body)
		}
		if got := h.names(t); slices.Contains(got, "release_submit") {
			t.Fatal("writes enabled without a form approval")
		}
	})
	t.Run("2026-07-28 client is capped and can approve", func(t *testing.T) {
		h := connect(t, Deps{Tools: (&fixture{}).tools(), Profile: team(false)}, "test", accept(true), mrtrProtocol)
		if got := h.cs.InitializeResult().ProtocolVersion; got != "2025-11-25" {
			t.Fatalf("negotiated %q, want 2025-11-25", got)
		}
		res, body := h.call(t, "toolsets", args)
		if res.IsError || dataOf(body)["instructions"] != nil {
			t.Fatalf("enable_writes on a 2026-07-28 client = %v", body)
		}
		if got := h.names(t); !slices.Contains(got, "release_submit") {
			t.Fatalf("write tools not listed after approval: %v", got)
		}
	})
	t.Run("locked", func(t *testing.T) {
		h := connect(t, Deps{Tools: (&fixture{}).tools(), Profile: team(false), WritesLocked: true}, "test", accept(true))
		res, body := h.call(t, "toolsets", args)
		if !res.IsError || dataOf(body)["error"] != "writes_locked" {
			t.Fatalf("locked result = %v", body)
		}
	})
}

// The human must approve exactly what is sent: a secret-shaped payload is
// refused (never shown as <redacted>) and hidden characters arrive escaped.
func TestApprovalPromptIntegrity(t *testing.T) {
	run := func(t *testing.T, value string) (*fixture, []string, map[string]any, bool) {
		f := &fixture{}
		var prompts []string
		elicit := func(_ context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
			prompts = append(prompts, req.Params.Message)
			return &mcp.ElicitResult{Action: "accept"}, nil
		}
		h := connect(t, Deps{Tools: f.tools(), Profile: team(true)}, "test", elicit)
		if res, body := h.call(t, "note_set", map[string]any{"value": value}); res.IsError || dataOf(body)["status"] != "needs_confirm" {
			t.Fatalf("preview = %v", body)
		}
		res, body := h.call(t, "note_set", map[string]any{"value": value, "confirm": true})
		return f, prompts, body, res.IsError
	}

	t.Run("secret refused", func(t *testing.T) {
		pem := "-----BEGIN NOTE-----\nhidden instructions\n-----END NOTE-----"
		f, prompts, body, isErr := run(t, "hi "+pem)
		if !isErr || dataOf(body)["error"] != "invalid_arguments" {
			t.Fatalf("secret-shaped payload approved: %v", body)
		}
		if len(prompts) != 0 {
			t.Fatalf("human was shown %q", prompts)
		}
		if f.writes.Load() != 0 {
			t.Fatalf("writes = %d, want 0", f.writes.Load())
		}
	})
	t.Run("bidi escaped", func(t *testing.T) {
		f, prompts, body, isErr := run(t, "pay \u202Eevil")
		if isErr || len(prompts) != 1 {
			t.Fatalf("confirm = %v, prompts = %q", body, prompts)
		}
		p := prompts[0]
		if !strings.HasPrefix(p, "WARNING: contains hidden characters") || !strings.Contains(p, `\u{202E}`) || strings.ContainsRune(p, '\u202E') {
			t.Fatalf("prompt = %q", p)
		}
		if f.writes.Load() != 1 {
			t.Fatalf("writes = %d, want 1", f.writes.Load())
		}
	})
}

func TestPanicBecomesErrorResult(t *testing.T) {
	h := connect(t, Deps{Tools: (&fixture{}).tools(), Profile: team(false)}, "test", nil)
	res, body := h.call(t, "boom", map[string]any{})
	if !res.IsError || dataOf(body)["error"] != "internal_error" {
		t.Fatalf("panic result = %v", body)
	}
	if res, _ := h.call(t, "apps_list", map[string]any{}); res.IsError {
		t.Fatal("server unusable after panic")
	}
}

func TestRenderRedactsAndCaps(t *testing.T) {
	s := &session{deps: Deps{MaxResultBytes: 2048}}
	items := make([]string, 200)
	for i := range items {
		items[i] = strings.Repeat("x", 40)
	}
	r := s.render(tool.OK(map[string]any{"items": items, "auth": "Authorization: Bearer eyJhbGciOiJFUzI1NiJ9.eyJpc3MiOiJ4In0.c2ln"}))
	text := r.Content[0].(*mcp.TextContent).Text
	if strings.Contains(text, "eyJhbGciOiJFUzI1NiJ9.eyJpc3MiOiJ4In0") {
		t.Fatal("JWT not redacted")
	}
	if len(text) > 4096 || !strings.Contains(text, `"truncated"`) {
		t.Fatalf("cap not applied (%d bytes)", len(text))
	}
}

func TestRenderMarksWriteLockedSteps(t *testing.T) {
	s, err := newSession(Deps{Tools: (&fixture{}).tools(), Profile: team(false)})
	if err != nil {
		t.Fatal(err)
	}
	r := tool.OK(map[string]any{
		"checks": []any{map[string]any{"id": "x", "fix_via": map[string]any{"tool": "release_submit"}}},
	}).Then(tool.NextStep{Tool: "release_submit"}, tool.NextStep{Tool: "builds_list"})
	text := s.render(r).Content[0].(*mcp.TextContent).Text
	var got struct {
		Data struct {
			Checks []struct {
				FixVia tool.NextStep `json:"fix_via"`
			} `json:"checks"`
		} `json:"data"`
		Next []tool.NextStep `json:"next"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatal(err)
	}
	unlock := func(st tool.NextStep) bool {
		return st.Locked == "writes_disabled" && st.Unlock != nil && st.Unlock.Tool == "toolsets" && st.Unlock.Args["action"] == "enable_writes"
	}
	if !unlock(got.Next[0]) || got.Next[1].Locked != "" || got.Next[1].Unlock != nil {
		t.Errorf("next = %+v", got.Next)
	}
	if len(got.Data.Checks) != 1 || !unlock(got.Data.Checks[0].FixVia) {
		t.Errorf("fix_via = %+v", got.Data.Checks)
	}
	// In write mode nothing is locked.
	s.enableWrites()
	text = s.render(tool.OK(nil).Then(tool.NextStep{Tool: "release_submit"})).Content[0].(*mcp.TextContent).Text
	if strings.Contains(text, "writes_disabled") {
		t.Errorf("write mode marked a step locked: %s", text)
	}
}

func TestInstructionsBudget(t *testing.T) {
	if len(Instructions) > 1536 {
		t.Fatalf("instructions = %d bytes, max 1536", len(Instructions))
	}
}

func TestWritesDisabledErrorPointsToEnableWrites(t *testing.T) {
	render := func(deps Deps) map[string]any {
		s, err := newSession(deps)
		if err != nil {
			t.Fatal(err)
		}
		out := s.render(tool.Fail(fmt.Errorf("analytics_setup: %w", tool.ErrWritesDisabled)))
		var body map[string]any
		if err := json.Unmarshal(out.StructuredContent.(json.RawMessage), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	body := render(Deps{Tools: (&fixture{}).tools(), Profile: team(false)})
	d := dataOf(body)
	if d["error"] != "writes_disabled" || d["hint"] != writesHint {
		t.Fatalf("data = %v", d)
	}
	next, _ := body["next"].([]any)
	if len(next) != 1 || next[0].(map[string]any)["tool"] != toolsetsName {
		t.Fatalf("next = %v, want the toolsets unlock", body["next"])
	}
	locked := dataOf(render(Deps{Tools: (&fixture{}).tools(), Profile: team(false), WritesLocked: true}))
	if locked["hint"] != errWritesLocked.Hint {
		t.Fatalf("pinned-off hint = %v", locked["hint"])
	}
}

func TestHiddenToolCall(t *testing.T) {
	var calls atomic.Int32
	counted := func(name string, ts tool.Toolset, c tool.Class) tool.Tool {
		m := tool.Meta{Name: name, Toolset: ts, Title: name, Description: "stub " + name, Class: c}
		if c == tool.Destructive {
			m.Confirm = tool.ConfirmByFlag
		}
		return tool.New(m, func(context.Context, *tool.Env, stubIn) (*tool.Result, error) {
			calls.Add(1)
			return tool.OK(map[string]any{"ok": true}), nil
		})
	}
	tools := []tool.Tool{
		counted("apps_list", tool.Core, tool.Read),
		counted("release_submit", tool.Store, tool.Destructive),
		counted("store_lookup", tool.Connect, tool.Read),
	}
	nextOf := func(body map[string]any) []map[string]any {
		var out []map[string]any
		for _, n := range body["next"].([]any) {
			out = append(out, n.(map[string]any))
		}
		return out
	}

	t.Run("writes_disabled", func(t *testing.T) {
		h := connect(t, Deps{Tools: tools, Profile: team(false)}, "test", nil)
		res, body := h.call(t, "release_submit", map[string]any{"confirm": true})
		d := dataOf(body)
		if !res.IsError || d["error"] != "writes_disabled" || d["hint"] != writesHint {
			t.Fatalf("result = %v", body)
		}
		next := nextOf(body)
		if len(next) == 0 || next[0]["tool"] != toolsetsName || next[0]["args"].(map[string]any)["action"] != "enable_writes" {
			t.Fatalf("next = %v", next)
		}
		if calls.Load() != 0 {
			t.Fatal("hidden tool handler ran")
		}
	})
	t.Run("toolset_disabled", func(t *testing.T) {
		h := connect(t, Deps{Tools: tools, Profile: team(false)}, "test", nil)
		res, body := h.call(t, "store_lookup", map[string]any{})
		if !res.IsError || dataOf(body)["error"] != "toolset_disabled" {
			t.Fatalf("result = %v", body)
		}
		next := nextOf(body)
		args, _ := json.Marshal(next[0]["args"])
		if next[0]["tool"] != toolsetsName || string(args) != `{"action":"enable","toolsets":["connect"]}` {
			t.Fatalf("next = %v", next)
		}
		if calls.Load() != 0 {
			t.Fatal("hidden tool handler ran")
		}
		garmin, _ := body["_meta"].(map[string]any)["garmin"].(map[string]any)
		if garmin["profile"] != "work" || garmin["approved_by"] != tool.ApprovedNone {
			t.Fatalf("_meta.garmin = %v, want the session profile", garmin)
		}
	})
	t.Run("unknown", func(t *testing.T) {
		h := connect(t, Deps{Tools: tools, Profile: team(false)}, "test", nil)
		if _, err := h.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: "no_such_tool"}); err == nil {
			t.Fatal("unknown tool did not return a JSON-RPC error")
		}
	})
	t.Run("writes_locked", func(t *testing.T) {
		h := connect(t, Deps{Tools: tools, Profile: team(false), WritesLocked: true}, "test", nil)
		res, body := h.call(t, "release_submit", map[string]any{"confirm": true})
		if d := dataOf(body); !res.IsError || d["error"] != "writes_disabled" || d["hint"] != errWritesLocked.Hint {
			t.Fatalf("result = %v", body)
		}
	})
}
