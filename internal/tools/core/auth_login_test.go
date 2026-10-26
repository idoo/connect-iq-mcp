package core

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/idoo/garmin-mcp/internal/auth"
	"github.com/idoo/garmin-mcp/internal/tool"
	"github.com/idoo/garmin-mcp/internal/tool/tooltest"
)

// loginCreds is a fake store whose Login is scripted per test.
type loginCreds struct {
	*tooltest.FakeCredentials
	login func(auth.LoginOptions) (auth.LoginResult, error)
	calls int
}

func (c *loginCreds) Login(_ context.Context, o auth.LoginOptions) (auth.LoginResult, error) {
	c.calls++
	return c.login(o)
}

func authLoginTool(t *testing.T) tool.Tool {
	t.Helper()
	tl, ok := tool.Lookup("auth_login")
	if !ok {
		t.Fatal("auth_login not registered")
	}
	return tl
}

func setEnv(t *testing.T, vars map[string]string) {
	t.Helper()
	old := getenv
	getenv = func(k string) string { return vars[k] }
	t.Cleanup(func() { getenv = old })
}

func TestAuthLoginAlreadySignedIn(t *testing.T) {
	setEnv(t, nil)
	k := tooltest.New(t)
	lc := &loginCreds{FakeCredentials: k.Creds, login: func(o auth.LoginOptions) (auth.LoginResult, error) {
		if !o.SkipIfValid || o.Timeout != 10*time.Minute || o.Connect {
			return auth.LoginResult{}, fmt.Errorf("options = %+v", o)
		}
		return auth.LoginResult{AlreadyValid: true}, nil
	}}
	k.Env.Credentials = lc
	res := tooltest.MustOK(t, tooltest.Call(t, authLoginTool(t), k.Env, `{}`))
	out := res.Data.(authStatusOut)
	if !out.Configured || !out.Portal.Present || lc.calls != 1 {
		t.Fatalf("out = %+v, calls %d", out, lc.calls)
	}
}

func TestAuthLoginSuccessAndFailure(t *testing.T) {
	setEnv(t, nil)
	k := tooltest.New(t)
	k.Creds.Cookie, k.Creds.Token = "", ""
	lc := &loginCreds{FakeCredentials: k.Creds, login: func(o auth.LoginOptions) (auth.LoginResult, error) {
		if !o.Connect || o.Timeout != 3*time.Minute {
			return auth.LoginResult{}, fmt.Errorf("options = %+v", o)
		}
		_, _ = o.Log.Write([]byte("Sign in to Garmin in the window that just opened.\n"))
		k.Creds.Cookie, k.Creds.Token = "SESSION=new", "tok"
		return auth.LoginResult{DeveloperID: "42"}, nil
	}}
	k.Env.Credentials = lc
	asked := k.Elicit(true)
	res := tooltest.MustOK(t, tooltest.Call(t, authLoginTool(t), k.Env, `{"connect":true,"timeout_minutes":3}`))
	if len(*asked) != 1 || !strings.Contains((*asked)[0], "health") {
		t.Fatalf("asked = %q", *asked)
	}
	out := res.Data.(authStatusOut)
	if !out.Portal.Present || !out.Connect.Present || out.Hint != "" {
		t.Fatalf("out = %+v", out)
	}
	if strings.Contains(fmt.Sprintf("%+v", out), "SESSION=new") {
		t.Fatal("secret leaked")
	}

	k.Creds.Cookie, k.Creds.Token = "", ""
	lc.login = func(auth.LoginOptions) (auth.LoginResult, error) {
		return auth.LoginResult{}, errors.New("sign-in timed out after 10m0s without finishing in the browser window; nothing was saved")
	}
	body := tooltest.MustFail(t, tooltest.Call(t, authLoginTool(t), k.Env, `{}`), "login_failed")
	if !strings.Contains(body.Message, "nothing was saved") {
		t.Fatalf("body = %+v", body)
	}
	if k.Creds.Cookie != "" {
		t.Fatal("credentials saved after timeout")
	}
}

func TestAuthLoginRefusals(t *testing.T) {
	k := tooltest.New(t)
	lc := &loginCreds{FakeCredentials: k.Creds, login: func(auth.LoginOptions) (auth.LoginResult, error) {
		return auth.LoginResult{}, errors.New("developer-portal cookies come from GARMIN_MCP_PORTAL_COOKIES; unset it to sign in with the browser")
	}}
	k.Env.Credentials = lc
	setEnv(t, map[string]string{"GARMIN_MCP_CI": "1"})
	tooltest.MustFail(t, tooltest.Call(t, authLoginTool(t), k.Env, `{}`), "login_unavailable")
	if lc.calls != 0 {
		t.Fatalf("CI called Login %d times", lc.calls)
	}

	setEnv(t, nil)
	body := tooltest.MustFail(t, tooltest.Call(t, authLoginTool(t), k.Env, `{}`), "login_failed")
	if !strings.Contains(body.Message, "GARMIN_MCP_PORTAL_COOKIES") {
		t.Fatalf("body = %+v", body)
	}
	tooltest.MustFail(t, tooltest.Call(t, authLoginTool(t), k.Env, `{"timeout_minutes":16}`), "invalid_arguments")
}

func TestAuthLoginConnectNeedsHuman(t *testing.T) {
	setEnv(t, nil)
	k := tooltest.New(t)
	k.Creds.Token = "" // portal only: Connect never enabled
	k.Env.Profile.AllowNoElicit = true
	lc := &loginCreds{FakeCredentials: k.Creds, login: func(auth.LoginOptions) (auth.LoginResult, error) {
		return auth.LoginResult{}, nil
	}}
	k.Env.Credentials = lc
	k.Env.Elicit = func(context.Context, string) (bool, error) { return false, tool.ErrElicitationUnavailable }
	body := tooltest.MustFail(t, tooltest.Call(t, authLoginTool(t), k.Env, `{"connect":true}`), tool.ErrHumanRequired.Code)
	if !strings.Contains(body.Hint, "garmin-mcp auth login --connect") {
		t.Fatalf("body = %+v", body)
	}
	k.Elicit(false)
	tooltest.MustFail(t, tooltest.Call(t, authLoginTool(t), k.Env, `{"connect":true}`), tool.ErrDeclined.Code)
	if lc.calls != 0 {
		t.Fatalf("Login called %d times without consent", lc.calls)
	}

	// Already consented: re-login does not ask again.
	k.Creds.Token = "tok"
	k.Env.Elicit = nil
	tooltest.MustOK(t, tooltest.Call(t, authLoginTool(t), k.Env, `{"connect":true}`))
	if lc.calls != 1 {
		t.Fatalf("calls = %d", lc.calls)
	}
}
