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
	resetLoginGate(t)
}

// resetLoginGate clears the process-wide window slot and cooldown.
func resetLoginGate(t *testing.T) {
	t.Helper()
	reset := func() {
		loginGate.mu.Lock()
		loginGate.busy, loginGate.failedAt = false, time.Time{}
		loginGate.mu.Unlock()
	}
	reset()
	t.Cleanup(reset)
}

// TestAuthLoginCooldownAndSingleWindow: an agent can't loop sign-in windows.
// A second call while a window is open, or within 10 minutes of a window
// closing without sign-in, is refused without calling Login.
func TestAuthLoginCooldownAndSingleWindow(t *testing.T) {
	setEnv(t, nil)
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	oldNow := loginNow
	loginNow = func() time.Time { return now }
	t.Cleanup(func() { loginNow = oldNow })

	k := tooltest.New(t)
	k.Creds.Cookie, k.Creds.Token = "", ""
	var progress []float64
	var totals []float64
	k.Env.Progress = func(done, total float64, _ string) {
		progress = append(progress, done)
		totals = append(totals, total)
	}
	inside := make(chan struct{})
	release := make(chan struct{})
	lc := &loginCreds{FakeCredentials: k.Creds, login: func(o auth.LoginOptions) (auth.LoginResult, error) {
		_, _ = o.Log.Write([]byte("opened\nstill waiting\n"))
		close(inside)
		<-release
		return auth.LoginResult{}, errors.New("browser closed before sign-in finished")
	}}
	k.Env.Credentials = lc
	first := make(chan *tool.Result, 1)
	tl := authLoginTool(t)
	go func() { first <- tooltest.Call(t, tl, k.Env, `{}`) }()
	<-inside
	// Second window while the first is open.
	body := tooltest.MustFail(t, tooltest.Call(t, authLoginTool(t), k.Env, `{}`), "login_unavailable")
	if !strings.Contains(body.Message, "already open") {
		t.Fatalf("concurrent: %+v", body)
	}
	close(release)
	<-first
	if lc.calls != 1 {
		t.Fatalf("Login called %d times", lc.calls)
	}
	for i, p := range progress {
		if totals[i] != 0 || (i > 0 && p <= progress[i-1]) {
			t.Fatalf("progress not strictly increasing without total: %v / %v", progress, totals)
		}
	}
	if len(progress) < 2 {
		t.Fatalf("progress = %v", progress)
	}

	// Window ended without sign-in: cooldown.
	now = now.Add(9 * time.Minute)
	body = tooltest.MustFail(t, tooltest.Call(t, authLoginTool(t), k.Env, `{}`), "login_unavailable")
	if !strings.Contains(body.Message, "1m0s") || lc.calls != 1 {
		t.Fatalf("cooldown: %+v calls %d", body, lc.calls)
	}
	// After the cooldown a new window may open.
	now = now.Add(time.Minute)
	lc.login = func(auth.LoginOptions) (auth.LoginResult, error) { return auth.LoginResult{AlreadyValid: true}, nil }
	tooltest.MustOK(t, tooltest.Call(t, authLoginTool(t), k.Env, `{}`))
	if lc.calls != 2 {
		t.Fatalf("calls = %d", lc.calls)
	}
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
