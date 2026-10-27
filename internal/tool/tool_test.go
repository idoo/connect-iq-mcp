package tool_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/idoo/garmin-mcp/internal/tool"
	"github.com/idoo/garmin-mcp/internal/tool/tooltest"
)

type filter struct {
	State string   `json:"state,omitempty"`
	Tags  []string `json:"tags,omitempty"`
}

type exampleIn struct {
	tool.ConfirmFlag
	App    string `json:"app" jsonschema:"app id"`
	Filter filter `json:"filter,omitzero"`
	Limit  int    `json:"limit,omitempty"`
}

var exampleMeta = tool.Meta{
	Name: "example_delete", Toolset: tool.Store, Title: "Example",
	Description: "Deletes things for tests.", Class: tool.Destructive,
	Confirm: tool.ConfirmByFlag,
	Enums:   map[string][]any{"filter.state": {"A", "B"}},
}

// mutate issues the DELETE; it records whether write capability was present.
func exampleHandler(ctx context.Context, env *tool.Env, in exampleIn) (*tool.Result, error) {
	wctx, res := tool.RequireConfirm(ctx, env, in.Confirm, tool.PlanPreview{
		Summary: "delete " + in.App, Intended: in.Filter, Affected: []string{in.App},
	})
	if res != nil {
		return res, nil
	}
	if err := tooltest.Delete(wctx, env, "/v1/things/"+in.App); err != nil {
		return nil, err
	}
	return tool.OK(in), nil
}

func newKit(t *testing.T) *tooltest.Kit {
	k := tooltest.New(t)
	return k
}

func TestSchemaInference(t *testing.T) {
	tl := tool.New(exampleMeta, exampleHandler)
	s := tl.InputSchema()
	if !slices.Equal(s.Required, []string{"app"}) {
		t.Errorf("required = %v", s.Required)
	}
	if got := s.Properties["confirm"]; got == nil || got.Type != "boolean" {
		t.Errorf("confirm property = %+v", got)
	}
	if got := s.Properties["filter"].Properties["state"].Enum; !slices.Equal(got, []any{"A", "B"}) {
		t.Errorf("enum = %v", got)
	}
	if s.Properties["app"].Description != "app id" {
		t.Errorf("description = %q", s.Properties["app"].Description)
	}
}

func TestCoercionAndValidation(t *testing.T) {
	k := newKit(t)
	tl := tool.New(exampleMeta, exampleHandler)
	tooltest.MustOK(t, tooltest.Call(t, tl, k.Env, `{"app":"1","filter":{"state":"A","tags":["x"]}}`))
	res := tooltest.MustOK(t, tooltest.Call(t, tl, k.Env, `{"app":"1","filter":"{\"state\":\"A\",\"tags\":\"[\\\"x\\\"]\"}","confirm":true}`))
	got := tooltest.Decode[exampleIn](t, res)
	if got.Filter.State != "A" || !slices.Equal(got.Filter.Tags, []string{"x"}) {
		t.Errorf("coerced = %+v", got)
	}
	tooltest.MustFail(t, tooltest.Call(t, tl, k.Env, `{"app":"1","filter":{"state":"Z"}}`), "invalid_arguments")
	tooltest.MustFail(t, tooltest.Call(t, tl, k.Env, `{"app":"1","bogus":1}`), "invalid_arguments")
	tooltest.MustFail(t, tooltest.Call(t, tl, k.Env, `{}`), "invalid_arguments")
}

func TestFlagConfirm(t *testing.T) {
	k := newKit(t)
	tl := tool.New(exampleMeta, exampleHandler)

	res := tooltest.MustOK(t, tooltest.Call(t, tl, k.Env, `{"app":"1"}`))
	if body := tooltest.Decode[map[string]any](t, res); body["status"] != "needs_confirm" {
		t.Errorf("preview = %v", body)
	}
	if n := len(k.HTTP.Mutations()); n != 0 {
		t.Fatalf("preview mutated: %d requests", n)
	}

	asked := k.Elicit(false)
	tooltest.MustFail(t, tooltest.Call(t, tl, k.Env, `{"app":"1","confirm":true}`), "declined")
	if len(*asked) != 1 || len(k.HTTP.Mutations()) != 0 {
		t.Fatalf("decline: asked=%v mutations=%d", *asked, len(k.HTTP.Mutations()))
	}

	k.Elicit(true)
	res = tooltest.MustOK(t, tooltest.Call(t, tl, k.Env, `{"app":"1","confirm":true}`))
	if n := len(k.HTTP.Mutations()); n != 1 {
		t.Fatalf("confirmed call: %d mutations", n)
	}
	if res.Meta.ApprovedBy != tool.ApprovedHuman {
		t.Errorf("approved_by = %q, want human", res.Meta.ApprovedBy)
	}
}

func TestApprovedByAgentAndNone(t *testing.T) {
	k := newKit(t)
	tl := tool.New(exampleMeta, exampleHandler)
	if res := tooltest.MustOK(t, tooltest.Call(t, tl, k.Env, `{"app":"1"}`)); res.Meta.ApprovedBy != tool.ApprovedNone {
		t.Errorf("preview approved_by = %q", res.Meta.ApprovedBy)
	}
	// No elicitation: a flag tool proceeds on the agent's confirm.
	if res := tooltest.MustOK(t, tooltest.Call(t, tl, k.Env, `{"app":"1","confirm":true}`)); res.Meta.ApprovedBy != tool.ApprovedAgent {
		t.Errorf("confirmed approved_by = %q", res.Meta.ApprovedBy)
	}
}

func TestConfirmFlagNeedsPreview(t *testing.T) {
	tl := tool.New(exampleMeta, exampleHandler)
	const preview, confirm = `{"app":"1","limit":5}`, `{"app":"1","limit":5,"confirm":true}`

	t.Run("a_confirm_first_refused", func(t *testing.T) {
		k := newKit(t)
		tooltest.MustFail(t, tooltest.Call(t, tl, k.Env, confirm), "preview_required")
		if n := len(k.HTTP.Mutations()); n != 0 {
			t.Fatalf("mutations = %d", n)
		}
	})
	t.Run("b_preview_then_confirm", func(t *testing.T) {
		k := newKit(t)
		tooltest.MustOK(t, tooltest.Call(t, tl, k.Env, preview))
		res := tooltest.MustOK(t, tooltest.Call(t, tl, k.Env, confirm))
		if res.Meta.ApprovedBy != tool.ApprovedAgent || len(k.HTTP.Mutations()) != 1 {
			t.Fatalf("approved_by = %q, mutations = %d", res.Meta.ApprovedBy, len(k.HTTP.Mutations()))
		}
	})
	t.Run("c_changed_args_refused", func(t *testing.T) {
		k := newKit(t)
		tooltest.MustOK(t, tooltest.Call(t, tl, k.Env, preview))
		tooltest.MustFail(t, tooltest.Call(t, tl, k.Env, `{"app":"1","limit":6,"confirm":true}`), "preview_required")
		if n := len(k.HTTP.Mutations()); n != 0 {
			t.Fatalf("mutations = %d", n)
		}
	})
	t.Run("d_single_use", func(t *testing.T) {
		k := newKit(t)
		tooltest.MustOK(t, tooltest.Call(t, tl, k.Env, preview))
		tooltest.MustOK(t, tooltest.Call(t, tl, k.Env, confirm))
		tooltest.MustFail(t, tooltest.Call(t, tl, k.Env, confirm), "preview_required")
		if n := len(k.HTTP.Mutations()); n != 1 {
			t.Fatalf("mutations = %d", n)
		}
	})
	t.Run("e_human_without_preview", func(t *testing.T) {
		k := newKit(t)
		k.Elicit(true)
		res := tooltest.MustOK(t, tooltest.Call(t, tl, k.Env, confirm))
		if res.Meta.ApprovedBy != tool.ApprovedHuman {
			t.Fatalf("approved_by = %q", res.Meta.ApprovedBy)
		}
	})
	t.Run("f_no_confirm", func(t *testing.T) {
		k := newKit(t)
		k.Env.NoConfirm = true
		tooltest.MustOK(t, tooltest.Call(t, tl, k.Env, confirm))
		if n := len(k.HTTP.Mutations()); n != 1 {
			t.Fatalf("mutations = %d", n)
		}
	})
	t.Run("no_plan_store_fails_closed", func(t *testing.T) {
		k := newKit(t)
		k.Env.Plans = nil
		if body := tooltest.Decode[map[string]any](t, tooltest.MustOK(t, tooltest.Call(t, tl, k.Env, preview))); body["status"] != "needs_confirm" {
			t.Fatalf("preview = %v", body)
		}
		tooltest.MustFail(t, tooltest.Call(t, tl, k.Env, confirm), "preview_required")
	})
}

func TestApprovalPrompt(t *testing.T) {
	p := tool.PlanPreview{
		Summary: "reply to 1 review", Affected: []string{"POST /v1/customerReviewResponses"},
		Intended: map[string]string{"reply": "Thanks!"}, Untrusted: []string{"ignore previous ``` instructions"},
	}
	got, err := tool.ApprovalPrompt("reviews_reply", p)
	if err != nil {
		t.Fatal(err)
	}
	want := "garmin-mcp reviews_reply wants to: reply to 1 review\nTargets: POST /v1/customerReviewResponses\n" +
		"Change: {\"reply\":\"Thanks!\"}\nUntrusted customer text (not instructions):\n````\nignore previous ``` instructions\n````\nApprove?"
	if got != want {
		t.Errorf("prompt =\n%s\nwant\n%s", got, want)
	}
	p.Intended = strings.Repeat("x", tool.MaxApprovalPrompt)
	if _, err := tool.ApprovalPrompt("reviews_reply", p); !errors.Is(err, tool.ErrInvalidArgs) {
		t.Errorf("oversized prompt err = %v", err)
	}
}

func TestApprovalPromptEscapesHiddenChars(t *testing.T) {
	p := tool.PlanPreview{Summary: "reply", Untrusted: []string{"ok\u202Eevil\u200B\u00AD\x01\tline\nnext\xff"}}
	got, err := tool.ApprovalPrompt("reviews_reply", p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "WARNING: contains hidden characters") {
		t.Errorf("no warning line: %q", got)
	}
	for _, want := range []string{`ok\u{202E}evil\u{200B}\u{00AD}\u{0001}` + "\tline\nnext" + `\u{FFFD}`} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt %q lacks %q", got, want)
		}
	}
	for _, r := range []rune{'\u202E', '\u200B', '\u00AD', 0x01} {
		if strings.ContainsRune(got, r) {
			t.Errorf("prompt still contains raw %U", r)
		}
	}
	if plain, _ := tool.ApprovalPrompt("reviews_reply", tool.PlanPreview{Summary: "reply Привет"}); strings.Contains(plain, "WARNING") {
		t.Errorf("clean prompt warned: %q", plain)
	}
	// The size cap applies to the escaped text: each hidden rune grows to 8 bytes.
	p = tool.PlanPreview{Summary: strings.Repeat("\u200B", tool.MaxApprovalPrompt/3-100)}
	if _, err := tool.ApprovalPrompt("reviews_reply", p); !errors.Is(err, tool.ErrInvalidArgs) {
		t.Errorf("escaped oversized prompt err = %v", err)
	}
}

// HA-2: invisible characters that round 1 missed are escaped by category;
// \n, \t, CJK and plain emoji pass through untouched.
func TestEscapeHiddenByCategory(t *testing.T) {
	for _, r := range []rune{0xE0041, 0xE0001, 0x061C, 0xFE0F, 0xE0100, 0x180E, 0x180B, 0xFFFB, 0x034F, 0x115F, 0x3164, 0xFFA0, 0xE000, 0x0378} {
		got := tool.EscapeHidden("Thanks!" + string(r))
		want := fmt.Sprintf(`Thanks!\u{%04X}`, r)
		if !strings.HasPrefix(got, "WARNING: contains hidden characters") || !strings.HasSuffix(got, want) || strings.ContainsRune(got, r) {
			t.Errorf("%U: got %q, want warning + %q", r, got, want)
		}
	}
	for _, s := range []string{"line\n\tnext", "你好，世界 こんにちは 안녕", "great app 😀🚀👍", "Привет & <b>"} {
		if got := tool.EscapeHidden(s); got != s {
			t.Errorf("EscapeHidden(%q) = %q, want unchanged", s, got)
		}
	}
}

// HA-11: the approval prompt shows payload text as posted, without HTML escaping.
func TestApprovalPromptKeepsAmpersand(t *testing.T) {
	got, err := tool.ApprovalPrompt("reviews_reply", tool.PlanPreview{Summary: "reply", Intended: map[string]string{"reply": "see https://x.com/?a=1&b=2 <b>"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `"see https://x.com/?a=1&b=2 <b>"`) || strings.Contains(got, `\u0026`) {
		t.Errorf("prompt = %q", got)
	}
}

func TestOversizedPromptRefusesApproval(t *testing.T) {
	k := newKit(t)
	k.Elicit(true)
	big := `{"app":"` + strings.Repeat("a", tool.MaxApprovalPrompt) + `","confirm":true}`
	tooltest.MustFail(t, tooltest.Call(t, tool.New(exampleMeta, exampleHandler), k.Env, big), "invalid_arguments")
	if n := len(k.HTTP.Mutations()); n != 0 {
		t.Fatalf("oversized prompt mutated: %d", n)
	}
}

func TestAlreadyKeepsShape(t *testing.T) {
	type out struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	b, _ := json.Marshal(tool.Already(out{ID: "1", Name: "n"}).Data)
	if string(b) != `{"already":true,"id":"1","name":"n"}` {
		t.Errorf("already = %s", b)
	}
	b, _ = json.Marshal(tool.Already([]int{1}).Data)
	if string(b) != `{"already":true,"result":[1]}` {
		t.Errorf("already non-object = %s", b)
	}
}

func TestHumanConfirmNeedsElicitation(t *testing.T) {
	k := newKit(t)
	m := exampleMeta
	m.Name, m.Confirm, m.RequireHuman = "example_human", tool.ConfirmByFlag, true
	tl := tool.New(m, exampleHandler)
	tooltest.MustFail(t, tooltest.Call(t, tl, k.Env, `{"app":"1","confirm":true}`), "human_approval_required")
	k.Env.Profile.AllowNoElicit = true
	tooltest.MustOK(t, tooltest.Call(t, tl, k.Env, `{"app":"1"}`))
	tooltest.MustOK(t, tooltest.Call(t, tl, k.Env, `{"app":"1","confirm":true}`))
}

func TestRequireHumanConfirmForcesHuman(t *testing.T) {
	k := newKit(t)
	m := exampleMeta
	m.Name = "example_forced"
	tl := tool.New(m, func(ctx context.Context, env *tool.Env, in exampleIn) (*tool.Result, error) {
		wctx, res := tool.RequireHumanConfirm(ctx, env, in.Confirm, tool.PlanPreview{Summary: "delete " + in.App})
		if res != nil {
			return res, nil
		}
		if err := tooltest.Delete(wctx, env, "/v1/things/"+in.App); err != nil {
			return nil, err
		}
		return tool.OK(in), nil
	})
	// Meta.RequireHuman is false, yet the agent's confirm alone is refused.
	tooltest.MustFail(t, tooltest.Call(t, tl, k.Env, `{"app":"1","confirm":true}`), "human_approval_required")
	k.Elicit(false)
	tooltest.MustFail(t, tooltest.Call(t, tl, k.Env, `{"app":"1","confirm":true}`), "declined")
	k.Elicit(true)
	if res := tooltest.MustOK(t, tooltest.Call(t, tl, k.Env, `{"app":"1","confirm":true}`)); res.Meta.ApprovedBy != tool.ApprovedHuman {
		t.Errorf("approved_by = %q, want human", res.Meta.ApprovedBy)
	}
	k.Env.Elicit = nil
	k.Env.Profile.AllowNoElicit = true
	tooltest.MustOK(t, tooltest.Call(t, tl, k.Env, `{"app":"1"}`))
	tooltest.MustOK(t, tooltest.Call(t, tl, k.Env, `{"app":"1","confirm":true}`))
}

func TestAnnotations(t *testing.T) {
	cases := []struct {
		class                 tool.Class
		readOnly, destructive bool
		destructiveSet        bool
	}{
		{tool.Read, true, false, false},
		{tool.Write, false, false, true},
		{tool.Destructive, false, true, true},
	}
	for _, c := range cases {
		a := tool.Meta{Class: c.class, Idempotent: true}.Annotations()
		if a.ReadOnlyHint != c.readOnly || (a.DestructiveHint != nil) != c.destructiveSet ||
			(a.DestructiveHint != nil && *a.DestructiveHint != c.destructive) {
			t.Errorf("%v: %+v", c.class, a)
		}
		if !a.IdempotentHint || a.OpenWorldHint == nil || !*a.OpenWorldHint {
			t.Errorf("%v: idempotent/openWorld = %+v", c.class, a)
		}
	}
	if a := (tool.Meta{Class: tool.Read, Local: true}).Annotations(); *a.OpenWorldHint {
		t.Error("local tool must not be open world")
	}
}

func TestGuards(t *testing.T) {
	tl := tool.New(exampleMeta, exampleHandler)
	tooltest.MustFail(t, tooltest.Call(t, tl, newKit(t).ReadOnly().Env, `{"app":"1"}`), "writes_disabled")

	type noIn struct{}
	boom := tool.New(tool.Meta{Name: "boom", Toolset: tool.Core, Description: "Panics.", Class: tool.Read},
		func(context.Context, *tool.Env, noIn) (*tool.Result, error) { panic("kaboom") })
	tooltest.MustFail(t, tooltest.Call(t, boom, &tool.Env{}, ``), "internal_error")

	// Nil Env members are replaced with stand-ins returning ErrUnavailable.
	var gotErr error
	probe := tool.New(tool.Meta{Name: "probe", Toolset: tool.Core, Description: "Probes.", Class: tool.Read},
		func(_ context.Context, env *tool.Env, _ noIn) (*tool.Result, error) {
			_, gotErr = env.Root.ReadFile("x")
			return nil, nil
		})
	tooltest.MustOK(t, tooltest.Call(t, probe, &tool.Env{}, ``))
	if !errors.Is(gotErr, tool.ErrUnavailable) {
		t.Errorf("stand-in error = %v", gotErr)
	}
}

func TestReadOnlyEnums(t *testing.T) {
	type reqIn struct {
		tool.ConfirmFlag
		Method string `json:"method"`
	}
	tl := tool.New(tool.Meta{
		Name: "example_request", Toolset: tool.Core, Description: "Any call.", Class: tool.Destructive,
		Confirm: tool.ConfirmByFlag, AvailableReadOnly: true,
		Enums:         map[string][]any{"method": {"GET", "POST", "DELETE"}},
		ReadOnlyEnums: map[string][]any{"method": {"GET"}},
	}, func(context.Context, *tool.Env, reqIn) (*tool.Result, error) { return tool.OK(map[string]any{}), nil })
	if got := tl.InputSchemaFor(false).Properties["method"].Enum; !slices.Equal(got, []any{"GET"}) {
		t.Errorf("read-only enum = %v", got)
	}
	if got := tl.InputSchemaFor(true).Properties["method"].Enum; len(got) != 3 {
		t.Errorf("write enum = %v", got)
	}
	k := tooltest.New(t).ReadOnly()
	tooltest.MustOK(t, tooltest.Call(t, tl, k.Env, `{"method":"GET"}`))
	tooltest.MustFail(t, tooltest.Call(t, tl, k.Env, `{"method":"DELETE"}`), "invalid_arguments")
}

func TestReadOnlyTransportGuard(t *testing.T) {
	// A ConfirmNone write tool gets write capability; a read tool does not.
	k := tooltest.New(t)
	type noIn struct{}
	post := func(ctx context.Context, env *tool.Env, _ noIn) (*tool.Result, error) {
		return nil, tooltest.Delete(ctx, env, "/things")
	}
	w := tool.New(tool.Meta{Name: "w", Toolset: tool.Core, Description: "W.", Class: tool.Write}, post)
	r := tool.New(tool.Meta{Name: "r", Toolset: tool.Core, Description: "R.", Class: tool.Read}, post)
	tooltest.MustOK(t, tooltest.Call(t, w, k.Env, ``))
	tooltest.MustFail(t, tooltest.Call(t, r, k.Env, ``), "write_guard")
}

func TestRegisterDuplicatePanics(t *testing.T) {
	type noIn struct{}
	tl := tool.New(tool.Meta{Name: "dup_test", Toolset: tool.Core, Description: "Dup.", Class: tool.Read},
		func(context.Context, *tool.Env, noIn) (*tool.Result, error) { return nil, nil })
	tool.Register(tl)
	defer func() {
		if recover() == nil {
			t.Error("duplicate Register did not panic")
		}
	}()
	tool.Register(tl)
}

func TestFailMapsNextSteps(t *testing.T) {
	err := tool.WithNext(&tool.APIError{Status: 404}, tool.NextStep{Tool: "apps_list"})
	res := tool.Fail(err)
	body := res.Data.(tool.ErrorBody)
	if body.Error != "api_error" || len(res.Next) != 1 || res.Next[0].Tool != "apps_list" {
		t.Errorf("body = %+v next = %+v", body, res.Next)
	}
}

// N4: an auth_required hint for a server pinned to "work" names --profile work;
// "default" keeps the bare command.
func TestAuthRequiredHintNamesProfile(t *testing.T) {
	type noIn struct{}
	tl := tool.New(tool.Meta{Name: "needs_auth", Toolset: tool.Core, Description: "Fails auth.", Class: tool.Read},
		func(context.Context, *tool.Env, noIn) (*tool.Result, error) {
			return nil, fmt.Errorf("%w: session expired", tool.ErrAuthRequired)
		})
	for profile, want := range map[string]string{"work": "`garmin-mcp auth login --profile work`", "default": "`garmin-mcp auth login`"} {
		k := newKit(t)
		k.Env.Profile.Name = profile
		b := tooltest.MustFail(t, tooltest.Call(t, tl, k.Env, ``), "auth_required")
		if !strings.Contains(b.Hint, want) {
			t.Errorf("%s: hint = %q", profile, b.Hint)
		}
	}
	if got := tool.LoginCommand("work", "--connect"); got != "garmin-mcp auth login --connect --profile work" {
		t.Errorf("LoginCommand = %q", got)
	}
}
