package core

import (
	"context"
	"testing"
	"time"

	"github.com/idoo/connect-iq-mcp/internal/auth"
	"github.com/idoo/connect-iq-mcp/internal/tool"
	"github.com/idoo/connect-iq-mcp/internal/tool/tooltest"
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
	if !out.Configured || !out.Portal.Present || out.Connect.Present || out.Hint != "" {
		t.Fatalf("out = %+v", out)
	}

	k.Env.Credentials = nil
	res = tooltest.MustOK(t, tooltest.Call(t, authStatusTool(t), k.Env, `{}`))
	if out := res.Data.(authStatusOut); out.Configured || out.Portal.Note != "not configured" {
		t.Fatalf("unconfigured = %+v", out)
	}
}

// statusCreds is a fake store with a scripted Status.
type statusCreds struct {
	*tooltest.FakeCredentials
	st auth.Status
}

func (c statusCreds) Status(context.Context) auth.Status { return c.st }

func TestAuthStatusHint(t *testing.T) {
	past, future := time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour)
	ok := auth.CredStatus{Present: true, ExpiresAt: future}
	expired := auth.CredStatus{Present: true, ExpiresAt: past}
	for _, tc := range []struct {
		name            string
		portal, connect auth.CredStatus
		hint            bool
	}{
		{"portal only", ok, auth.CredStatus{}, false},
		{"both valid", ok, ok, false},
		{"unknown expiry", auth.CredStatus{Present: true}, auth.CredStatus{}, false},
		{"portal missing", auth.CredStatus{}, auth.CredStatus{}, true},
		{"portal expired", expired, auth.CredStatus{}, true},
		{"connect consented but expired", ok, expired, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := tooltest.New(t)
			k.Env.Credentials = statusCreds{k.Creds, auth.Status{Portal: tc.portal, Connect: tc.connect}}
			out := tooltest.MustOK(t, tooltest.Call(t, authStatusTool(t), k.Env, `{}`)).Data.(authStatusOut)
			if (out.Hint != "") != tc.hint {
				t.Fatalf("hint = %q, want hint %v", out.Hint, tc.hint)
			}
		})
	}
}
