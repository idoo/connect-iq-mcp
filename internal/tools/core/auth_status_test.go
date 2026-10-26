package core

import (
	"testing"

	"github.com/idoo/garmin-mcp/internal/tool"
	"github.com/idoo/garmin-mcp/internal/tool/tooltest"
)

func authStatusTool(t *testing.T) tool.Tool {
	t.Helper()
	tl, ok := tool.Lookup("auth_status")
	if !ok {
		t.Fatal("auth_status not registered")
	}
	return tl
}

func TestAuthStatus(t *testing.T) {
	k := tooltest.New(t)
	k.Creds.Token = ""
	res := tooltest.MustOK(t, tooltest.Call(t, authStatusTool(t), k.Env, `{}`))
	out := res.Data.(authStatusOut)
	if !out.Configured || !out.Portal.Present || out.Connect.Present || out.Hint == "" {
		t.Fatalf("out = %+v", out)
	}

	k.Env.Credentials = nil
	res = tooltest.MustOK(t, tooltest.Call(t, authStatusTool(t), k.Env, `{}`))
	if out := res.Data.(authStatusOut); out.Configured || out.Portal.Note != "not configured" {
		t.Fatalf("unconfigured = %+v", out)
	}
}
