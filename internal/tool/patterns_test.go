package tool_test

import (
	"context"
	"testing"

	"github.com/idoo/connect-iq-mcp/internal/tool"
	"github.com/idoo/connect-iq-mcp/internal/tool/tooltest"
)

func TestPatternsRejectBeforeHandler(t *testing.T) {
	type idIn struct {
		ReviewID string `json:"review_id"`
	}
	ran := false
	tl := tool.New(tool.Meta{
		Name: "example_get", Toolset: tool.Store, Description: "Gets a review.", Class: tool.Read,
		Patterns: map[string]string{"review_id": `^[A-Za-z0-9_-]{1,128}$`},
	}, func(context.Context, *tool.Env, idIn) (*tool.Result, error) {
		ran = true
		return nil, nil
	})
	k := tooltest.New(t)
	for _, bad := range []string{"..", "a/b", ""} {
		tooltest.MustFail(t, tooltest.Call(t, tl, k.Env, `{"review_id":"`+bad+`"}`), "invalid_arguments")
	}
	if ran {
		t.Fatal("handler ran for an id rejected by the pattern")
	}
	tooltest.MustOK(t, tooltest.Call(t, tl, k.Env, `{"review_id":"abc-1_2"}`))
	if !ran {
		t.Fatal("valid id did not reach the handler")
	}
}
