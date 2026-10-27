package credstore

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/idoo/garmin-mcp/internal/portal"
)

func okVerify(_ context.Context, _ *http.Client, h string) (portal.Developer, string, error) {
	return portal.Developer{ID: "42", Name: "Dev"}, h, nil
}

func TestValidLockedSkipsBrowser(t *testing.T) {
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
	unlock, err := s.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r, ok := s.ValidLocked(context.Background(), true)
	unlock()
	if !ok || !r.AlreadyValid || r.DeveloperID != "42" || !r.Connect {
		t.Fatalf("ValidLocked = %+v, %v", r, ok)
	}
	if b, _, _ := s.readStored(); b.Connect.RefreshToken != "rt-1" {
		t.Fatalf("refresh during validity check not persisted: %q", b.Connect.RefreshToken)
	}
}

func TestRefuseEnv(t *testing.T) {
	keyring.MockInit()
	s := testStore(t, map[string]string{"GARMIN_MCP_PORTAL_COOKIES": cookies(time.Now().Add(time.Hour))})
	if err := s.RefuseEnv(false); !errors.Is(err, ErrPortalFromEnv) {
		t.Fatalf("err = %v", err)
	}
	s = testStore(t, map[string]string{"GARMIN_MCP_CONNECT_REFRESH_TOKEN": "rt"})
	if err := s.RefuseEnv(false); err != nil {
		t.Fatalf("portal-only err = %v", err)
	}
	if err := s.RefuseEnv(true); !errors.Is(err, ErrConnectFromEnv) {
		t.Fatalf("err = %v", err)
	}
}

func TestSignInCommitSaves(t *testing.T) {
	keyring.MockInit()
	srv, _ := diauth(t, 0)
	s := testStore(t, nil)
	s.diauthURL = srv.URL
	s.verify = okVerify
	exp := time.Now().Add(60 * 24 * time.Hour)
	s.login = func(_ context.Context, r SignInRequest) (Capture, error) {
		if r.Headless || r.Timeout != 3*time.Minute || !r.Portal || !r.Connect || r.ProfileDir != s.browserDir() || r.VerifyPortal == nil {
			t.Errorf("request = %+v", r)
		}
		return Capture{PortalCookies: "session=" + seal(exp) + "; refresh_token=" + seal(exp), ConnectTicket: "ST-1"}, nil
	}
	res, err := s.SignIn(context.Background(), SignInRequest{Timeout: 3 * time.Minute, Portal: true, Connect: true})
	if err != nil {
		t.Fatal(err)
	}
	if b, src, _ := s.readStored(); src != "" || b.Portal != nil {
		t.Fatalf("SignIn saved: %q %+v", src, b)
	}
	unlock, err := s.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.CommitLocked(res)
	unlock()
	if err != nil || r.AlreadyValid || r.DeveloperName != "Dev" || r.PortalExpires.UnixMilli() != exp.UnixMilli() || !r.Connect {
		t.Fatalf("CommitLocked = %+v, %v", r, err)
	}
	b, _, _ := s.readStored()
	if b.Portal == nil || b.Portal.DeveloperID != "42" || b.Connect == nil || b.Connect.RefreshToken != "rt-0" {
		t.Fatalf("stored = %+v", b)
	}
}

func TestSignInFailureSavesNothing(t *testing.T) {
	keyring.MockInit()
	s := testStore(t, nil)
	s.verify = okVerify
	browserErr := errors.New("timed out waiting for Garmin sign-in")
	s.login = func(context.Context, SignInRequest) (Capture, error) { return Capture{}, browserErr }
	if _, err := s.SignIn(context.Background(), SignInRequest{Portal: true}); !errors.Is(err, browserErr) {
		t.Fatalf("err = %v", err)
	}
	// Portal OK but Connect ticket missing: neither is saved.
	exp := time.Now().Add(60 * 24 * time.Hour)
	s.login = func(context.Context, SignInRequest) (Capture, error) {
		return Capture{PortalCookies: "session=" + seal(exp) + "; refresh_token=" + seal(exp)}, nil
	}
	if _, err := s.SignIn(context.Background(), SignInRequest{Portal: true, Connect: true}); err == nil {
		t.Fatal("missing ticket accepted")
	}
	if b, src, _ := s.readStored(); src != "" || b.Portal != nil || b.Connect != nil {
		t.Fatalf("saved after failure: %q %+v", src, b)
	}
}
