package credstore

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/idoo/garmin-mcp/internal/auth"
	"github.com/idoo/garmin-mcp/internal/browser"
	"github.com/idoo/garmin-mcp/internal/portal"
)

func okVerify(_ context.Context, _ *http.Client, h string) (portal.Developer, string, error) {
	return portal.Developer{ID: "42", Name: "Dev"}, h, nil
}

func TestLoginSkipsBrowserWhenValid(t *testing.T) {
	keyring.MockInit()
	srv, _ := diauth(t, 0)
	s := testStore(t, nil) // its login fails the test if called
	s.diauthURL = srv.URL
	s.verify = okVerify
	seedConnect(t, s, "rt-0") // access token near expiry, refresh token alive
	stored, _, _ := s.readStored()
	stored.Portal = &portalCred{Cookies: cookies(time.Now().Add(20 * 24 * time.Hour))}
	if err := s.writeStored(stored); err != nil {
		t.Fatal(err)
	}
	r, err := s.Login(context.Background(), auth.LoginOptions{Connect: true, SkipIfValid: true})
	if err != nil || !r.AlreadyValid || r.DeveloperID != "42" || !r.Connect {
		t.Fatalf("Login = %+v, %v", r, err)
	}
	if b, _, _ := s.readStored(); b.Connect.RefreshToken != "rt-1" {
		t.Fatalf("refresh during validity check not persisted: %q", b.Connect.RefreshToken)
	}
}

func TestLoginRefusesEnvOverride(t *testing.T) {
	keyring.MockInit()
	s := testStore(t, map[string]string{"GARMIN_MCP_PORTAL_COOKIES": cookies(time.Now().Add(time.Hour))})
	_, err := s.Login(context.Background(), auth.LoginOptions{})
	if err == nil || !strings.Contains(err.Error(), "GARMIN_MCP_PORTAL_COOKIES") {
		t.Fatalf("err = %v", err)
	}
	s = testStore(t, map[string]string{"GARMIN_MCP_CONNECT_REFRESH_TOKEN": "rt"})
	_, err = s.Login(context.Background(), auth.LoginOptions{Connect: true})
	if err == nil || !strings.Contains(err.Error(), "GARMIN_MCP_CONNECT_REFRESH_TOKEN") {
		t.Fatalf("err = %v", err)
	}
}

func TestLoginSuccessSaves(t *testing.T) {
	keyring.MockInit()
	srv, _ := diauth(t, 0)
	s := testStore(t, nil)
	s.diauthURL = srv.URL
	s.verify = okVerify
	exp := time.Now().Add(60 * 24 * time.Hour)
	s.login = func(_ context.Context, o browser.Options, r browser.Request) (browser.Result, error) {
		if o.Headless || o.Timeout != 3*time.Minute || !r.Portal || !r.Connect || o.ProfileDir != s.browserDir() {
			t.Errorf("options = %+v %+v", o, r)
		}
		return browser.Result{
			PortalCookies: []browser.Cookie{{Name: "session", Value: seal(exp)}, {Name: "refresh_token", Value: seal(exp)}},
			ConnectTicket: "ST-1",
		}, nil
	}
	r, err := s.Login(context.Background(), auth.LoginOptions{Connect: true, Timeout: 3 * time.Minute, SkipIfValid: true})
	if err != nil || r.AlreadyValid || r.DeveloperName != "Dev" || r.PortalExpires.UnixMilli() != exp.UnixMilli() || !r.Connect {
		t.Fatalf("Login = %+v, %v", r, err)
	}
	b, _, _ := s.readStored()
	if b.Portal == nil || b.Portal.DeveloperID != "42" || b.Connect == nil || b.Connect.RefreshToken != "rt-0" {
		t.Fatalf("stored = %+v", b)
	}
}

func TestLoginFailureSavesNothing(t *testing.T) {
	keyring.MockInit()
	s := testStore(t, nil)
	s.verify = okVerify
	s.login = func(context.Context, browser.Options, browser.Request) (browser.Result, error) {
		return browser.Result{}, errors.New("timed out waiting for Garmin sign-in")
	}
	_, err := s.Login(context.Background(), auth.LoginOptions{Timeout: 2 * time.Minute})
	if err == nil || !strings.Contains(err.Error(), "timed out after 2m") || !strings.Contains(err.Error(), "nothing was saved") {
		t.Fatalf("err = %v", err)
	}
	// Portal OK but Connect ticket exchange fails: neither is saved.
	exp := time.Now().Add(60 * 24 * time.Hour)
	s.login = func(context.Context, browser.Options, browser.Request) (browser.Result, error) {
		return browser.Result{PortalCookies: []browser.Cookie{{Name: "session", Value: seal(exp)}, {Name: "refresh_token", Value: seal(exp)}}}, nil
	}
	if _, err := s.Login(context.Background(), auth.LoginOptions{Connect: true}); err == nil {
		t.Fatal("missing ticket accepted")
	}
	if b, src, _ := s.readStored(); src != "" || b.Portal != nil || b.Connect != nil {
		t.Fatalf("saved after failure: %q %+v", src, b)
	}
	s.login = func(context.Context, browser.Options, browser.Request) (browser.Result, error) {
		return browser.Result{}, browser.ErrNoBrowser
	}
	if _, err := s.Login(context.Background(), auth.LoginOptions{}); err == nil || !strings.Contains(err.Error(), "GARMIN_MCP_BROWSER") {
		t.Fatalf("no-browser err = %v", err)
	}
}
