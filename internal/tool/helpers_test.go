package tool_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/idoo/garmin-mcp/internal/tool"
	"github.com/idoo/garmin-mcp/internal/tool/tooltest"
)

func TestSharedErrorHelpers(t *testing.T) {
	api := func(s int) error { return fmt.Errorf("wrapped: %w", &tool.APIError{Status: s}) }
	tests := []struct {
		name               string
		err                error
		status             int
		notFound, conflict bool
	}{
		{"nil", nil, 0, false, false},
		{"plain", errors.New("x"), 0, false, false},
		{"api404", api(404), 404, true, false},
		{"api409", api(409), 409, false, true},
		{"api500", api(500), 500, false, false},
	}
	for _, tc := range tests {
		if got := tool.APIStatus(tc.err); got != tc.status {
			t.Errorf("%s: APIStatus = %d", tc.name, got)
		}
		if got := tool.IsNotFound(tc.err); got != tc.notFound {
			t.Errorf("%s: IsNotFound = %v", tc.name, got)
		}
		if got := tool.IsConflict(tc.err); got != tc.conflict {
			t.Errorf("%s: IsConflict = %v", tc.name, got)
		}
	}
	inner := errors.New("inner")
	err := tool.InvalidArgs("bad %s: %w", "limit", inner)
	if !errors.Is(err, tool.ErrInvalidArgs) || !errors.Is(err, inner) || err.Error() != "invalid arguments: bad limit: inner" {
		t.Fatalf("InvalidArgs = %q", err)
	}
	if b := tool.BodyFor(err); b.Error != "invalid_arguments" {
		t.Fatalf("code = %q", b.Error)
	}
}

func TestWritesDisabledHintPointsAtEnableWrites(t *testing.T) {
	k := tooltest.New(t).ReadOnly()
	tl := tool.New(exampleMeta, exampleHandler)
	b := tooltest.MustFail(t, tooltest.Call(t, tl, k.Env, `{"app":"1"}`), "writes_disabled")
	if b.Hint != tool.EnableWritesHint || !strings.Contains(b.Hint, `"enable_writes"`) || strings.Contains(b.Hint, "--allow-writes") {
		t.Fatalf("hint = %q", b.Hint)
	}
}

// gateTool builds a tool whose handler runs gate and then mutates once.
func gateTool(m tool.Meta, gate func(ctx context.Context, env *tool.Env, in textIn) (context.Context, *tool.Result)) tool.Tool {
	return tool.New(m, func(ctx context.Context, env *tool.Env, in textIn) (*tool.Result, error) {
		wctx, res := gate(ctx, env, in)
		if res != nil {
			return res, nil
		}
		err := tooltest.Delete(wctx, env, "/v1/things/1")
		return nil, err
	})
}

type textIn struct {
	Text string `json:"text"`
}

func TestGateMisuseFailsClosed(t *testing.T) {
	base := tool.Meta{Toolset: tool.Store, Description: "Gate test.", Class: tool.Destructive}
	ungated := base
	ungated.Name, ungated.Class = "gate_none", tool.Write
	humanFlag := base
	humanFlag.Name, humanFlag.Confirm, humanFlag.RequireHuman = "gate_human_flag", tool.ConfirmByFlag, true

	tests := []struct {
		name  string
		tool  func() tool.Tool
		setup func(k *tooltest.Kit)
		args  string
		code  string
	}{
		{"unconfirmed_on_require_human", func() tool.Tool {
			m := humanFlag
			return tool.New(m, func(ctx context.Context, env *tool.Env, _ exampleIn) (*tool.Result, error) {
				wctx, res := tool.Unconfirmed(ctx, env)
				if res != nil {
					return res, nil
				}
				err := tooltest.Delete(wctx, env, "/v1/things/1")
				return nil, err
			})
		}, nil, `{"app":"1"}`, "internal_error"},
		{"flag_gate_in_ungated_tool", func() tool.Tool {
			return gateTool(ungated, func(ctx context.Context, env *tool.Env, _ textIn) (context.Context, *tool.Result) {
				return tool.RequireConfirm(ctx, env, true, tool.PlanPreview{Summary: "x"})
			})
		}, nil, `{"text":"a"}`, "internal_error"},
		{"noconfirm_require_human_flag", func() tool.Tool {
			return tool.New(humanFlag, exampleHandler)
		}, func(k *tooltest.Kit) { k.Env.NoConfirm = true }, `{"app":"1"}`, "human_approval_required"},
		{"elicit_error_is_not_approval", func() tool.Tool {
			return tool.New(humanFlag, exampleHandler)
		}, func(k *tooltest.Kit) {
			k.Env.Elicit = func(context.Context, string) (bool, error) { return false, errors.New("transport closed") }
		}, `{"app":"1","confirm":true}`, "error"},
		{"elicit_error_non_human", func() tool.Tool {
			return tool.New(exampleMeta, exampleHandler)
		}, func(k *tooltest.Kit) {
			k.Env.Elicit = func(context.Context, string) (bool, error) { return false, errors.New("transport closed") }
		}, `{"app":"1","confirm":true}`, "error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			k := newKit(t)
			if tc.setup != nil {
				tc.setup(k)
			}
			tooltest.MustFail(t, tooltest.Call(t, tc.tool(), k.Env, tc.args), tc.code)
			if n := len(k.HTTP.Mutations()); n != 0 {
				t.Fatalf("mutations = %d", n)
			}
		})
	}
}

func TestGateOutsideCallFailsClosed(t *testing.T) {
	k := newKit(t)
	ctx, res := tool.RequireConfirm(context.Background(), k.Env, true, tool.PlanPreview{Summary: "x"})
	if ctx != nil || res == nil || !res.IsError || tool.IsWrite(context.Background()) {
		t.Fatalf("gate outside Tool.Call granted writes: %+v", res)
	}
	if _, res := tool.Unconfirmed(context.Background(), k.Env); res == nil || !res.IsError {
		t.Fatal("Unconfirmed outside Tool.Call granted writes")
	}
}

func TestPreviewExpiresWithKitClock(t *testing.T) {
	k := newKit(t)
	tl := tool.New(exampleMeta, exampleHandler)
	tooltest.MustOK(t, tooltest.Call(t, tl, k.Env, `{"app":"1"}`))
	k.Clock.Set(k.Clock.Now().Add(tool.PreviewTTL))
	tooltest.MustFail(t, tooltest.Call(t, tl, k.Env, `{"app":"1","confirm":true}`), "preview_required")
	if n := len(k.HTTP.Mutations()); n != 0 {
		t.Fatalf("mutations = %d", n)
	}
}

func TestMetaCheckPanics(t *testing.T) {
	ok := tool.Meta{Name: "meta_x", Toolset: tool.Store, Description: "d", Class: tool.Destructive, Confirm: tool.ConfirmByFlag}
	tests := map[string]func(m *tool.Meta){
		"destructive_without_confirm": func(m *tool.Meta) { m.Confirm = tool.ConfirmNone },
		"human_without_confirm":       func(m *tool.Meta) { m.Class, m.Confirm, m.RequireHuman = tool.Write, tool.ConfirmNone, true },
		"read_with_confirm":           func(m *tool.Meta) { m.Class = tool.Read },
		"flag_without_field":          nil,
	}
	for name, mut := range tests {
		t.Run(name, func(t *testing.T) {
			m := ok
			defer func() {
				if recover() == nil {
					t.Fatal("no panic")
				}
			}()
			if mut == nil {
				tool.New(m, func(context.Context, *tool.Env, struct{}) (*tool.Result, error) { return nil, nil })
				return
			}
			mut(&m)
			tool.New(m, func(context.Context, *tool.Env, exampleIn) (*tool.Result, error) { return nil, nil })
		})
	}
}

// rootedFiles records the roots it was asked for.
type rootedFiles struct {
	tooltest.FakeFiles
	got   [][]string
	err   error
	calls int
}

func (r *rootedFiles) ForRoots(roots []string) (tool.Files, error) {
	r.calls++
	r.got = append(r.got, roots)
	if r.err != nil {
		return nil, r.err
	}
	return &r.FakeFiles, nil
}

func TestCallPassesClientRootsLazily(t *testing.T) {
	k := newKit(t)
	rf := &rootedFiles{FakeFiles: *k.Files}
	if err := os.WriteFile(k.Files.Root+"/a.txt", []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	listed := 0
	k.Env.Root = rf
	k.Env.Roots = func(context.Context) ([]string, error) {
		listed++
		return []string{"file:///proj"}, nil
	}
	read := tool.New(tool.Meta{Name: "roots_read", Toolset: tool.Store, Description: "Reads.", Class: tool.Read},
		func(_ context.Context, env *tool.Env, in struct {
			Read bool `json:"read,omitempty"`
		},
		) (*tool.Result, error) {
			if !in.Read {
				return nil, nil
			}
			if _, err := env.Root.ReadFile("a.txt"); err != nil {
				return nil, err
			}
			_, err := env.Root.Stat("a.txt")
			return nil, err
		})
	tooltest.MustOK(t, tooltest.Call(t, read, k.Env, `{}`))
	if listed != 0 {
		t.Fatal("roots listed by a call that touched no files")
	}
	tooltest.MustOK(t, tooltest.Call(t, read, k.Env, `{"read":true}`))
	if listed != 1 || rf.calls != 1 || rf.got[0][0] != "file:///proj" {
		t.Fatalf("listed=%d calls=%d got=%v", listed, rf.calls, rf.got)
	}
	// A listing failure still goes through ForRoots (which applies the safety
	// policy to the fallback root) rather than bypassing it.
	k.Env.Roots = func(context.Context) ([]string, error) { return nil, errors.New("no roots") }
	rf.err = fmt.Errorf("%w: refused", tool.ErrUnavailable)
	b := tooltest.MustFail(t, tooltest.Call(t, read, k.Env, `{"read":true}`), "unavailable")
	if rf.got[len(rf.got)-1] != nil || !strings.Contains(b.Message, "refused") {
		t.Fatalf("fallback roots = %v, %+v", rf.got, b)
	}
}
