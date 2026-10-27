package login

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/idoo/garmin-mcp/internal/auth"
	"github.com/idoo/garmin-mcp/internal/browser"
	"github.com/idoo/garmin-mcp/internal/credstore"
)

// fakeCreds records the calls login makes.
type fakeCreds struct {
	refuse  error
	valid   bool
	signIn  func(credstore.SignInRequest) error
	calls   []string
	request credstore.SignInRequest
}

func (f *fakeCreds) RefuseEnv(bool) error { f.calls = append(f.calls, "refuse"); return f.refuse }
func (f *fakeCreds) LockSignIn(context.Context) (func(), error) {
	f.calls = append(f.calls, "lock-login")
	return func() { f.calls = append(f.calls, "unlock-login") }, nil
}

func (f *fakeCreds) Lock(context.Context) (func(), error) {
	f.calls = append(f.calls, "lock")
	return func() { f.calls = append(f.calls, "unlock") }, nil
}

func (f *fakeCreds) ValidLocked(context.Context, bool) (auth.LoginResult, bool) {
	f.calls = append(f.calls, "valid")
	return auth.LoginResult{AlreadyValid: f.valid}, f.valid
}

func (f *fakeCreds) SignIn(_ context.Context, r credstore.SignInRequest) (*credstore.SignedIn, error) {
	f.calls = append(f.calls, "signin")
	f.request = r
	if f.signIn != nil {
		if err := f.signIn(r); err != nil {
			return nil, err
		}
	}
	return &credstore.SignedIn{}, nil
}

func (f *fakeCreds) CommitLocked(*credstore.SignedIn) (auth.LoginResult, error) {
	f.calls = append(f.calls, "commit")
	return auth.LoginResult{DeveloperID: "42"}, nil
}

func (f *fakeCreds) sequence() string { return strings.Join(f.calls, ",") }

func TestLoginRefusesEnvOverride(t *testing.T) {
	f := &fakeCreds{refuse: credstore.ErrPortalFromEnv}
	if _, err := login(context.Background(), f, auth.LoginOptions{}); !errors.Is(err, credstore.ErrPortalFromEnv) {
		t.Fatalf("err = %v", err)
	}
	if f.sequence() != "refuse" {
		t.Fatalf("calls = %s", f.sequence())
	}
}

func TestLoginSkipsBrowserWhenValid(t *testing.T) {
	f := &fakeCreds{valid: true}
	r, err := login(context.Background(), f, auth.LoginOptions{Connect: true, SkipIfValid: true})
	if err != nil || !r.AlreadyValid {
		t.Fatalf("Login = %+v, %v", r, err)
	}
	if want := "refuse,lock-login,lock,valid,unlock,unlock-login"; f.sequence() != want {
		t.Fatalf("calls = %s, want %s", f.sequence(), want)
	}
}

// LC-3: the window runs under the login lock only; the profile lock is taken
// for the save.
func TestLoginSuccessCommits(t *testing.T) {
	f := &fakeCreds{}
	var log strings.Builder
	r, err := login(context.Background(), f, auth.LoginOptions{Connect: true, BrowserPath: "/b", Timeout: 3 * time.Minute, SkipIfValid: true, Log: &log})
	if err != nil || r.DeveloperID != "42" {
		t.Fatalf("Login = %+v, %v", r, err)
	}
	if want := "refuse,lock-login,lock,valid,unlock,signin,lock,commit,unlock,unlock-login"; f.sequence() != want {
		t.Fatalf("calls = %s, want %s", f.sequence(), want)
	}
	q := f.request
	if q.Headless || !q.Portal || !q.Connect || q.BrowserPath != "/b" || q.Timeout != 3*time.Minute || q.Log != &log {
		t.Fatalf("request = %+v", q)
	}
}

func TestLoginFailureSavesNothing(t *testing.T) {
	cases := map[string]struct {
		err   error
		check func(error) bool
	}{
		"timeout": {errors.New("timed out waiting for Garmin sign-in"), func(err error) bool {
			var te timeoutError
			return errors.Is(err, ErrTimedOut) && errors.As(err, &te) && te.after == 2*time.Minute
		}},
		"no browser": {browser.ErrNoBrowser, func(err error) bool {
			return errors.Is(err, browser.ErrNoBrowser) && strings.Contains(err.Error(), browser.EnvBrowser)
		}},
		"rate limited": {auth.ErrRateLimited, func(err error) bool {
			return errors.Is(err, auth.ErrRateLimited) && !errors.Is(err, auth.ErrAuthRequired)
		}},
		"other": {errors.New("boom"), func(err error) bool { return err != nil && !errors.Is(err, ErrTimedOut) }},
	}
	for name, tc := range cases {
		f := &fakeCreds{signIn: func(credstore.SignInRequest) error { return tc.err }}
		_, err := login(context.Background(), f, auth.LoginOptions{Timeout: 2 * time.Minute})
		if !tc.check(err) {
			t.Errorf("%s: err = %v", name, err)
		}
		if strings.Contains(f.sequence(), "commit") {
			t.Errorf("%s: committed after failure: %s", name, f.sequence())
		}
	}
}

func TestCookieHeader(t *testing.T) {
	got := cookieHeader([]browser.Cookie{{Name: "session", Value: "S"}, {Value: "x"}, {Name: "refresh_token", Value: "R"}})
	if got != "session=S; refresh_token=R" {
		t.Fatalf("cookieHeader = %q", got)
	}
}
