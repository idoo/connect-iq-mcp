package credstore

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/idoo/garmin-mcp/internal/auth"
	"github.com/idoo/garmin-mcp/internal/browser"
)

// A portal-only sign-in must never let ConnectToken mint a Connect token from
// the browser profile's SSO session.
func TestConnectTokenWithoutConsentNoSilentMint(t *testing.T) {
	keyring.MockInit()
	srv, n := diauth(t, 0)
	s := testStore(t, nil) // its login fails the test if called
	s.diauthURL = srv.URL
	s.hasProfile = func(string) bool { return true }
	if err := s.writeStored(blob{Portal: &portalCred{Cookies: cookies(time.Now().Add(30 * 24 * time.Hour))}}); err != nil {
		t.Fatal(err)
	}
	_, err := s.ConnectToken(context.Background())
	if !errors.Is(err, auth.ErrAuthRequired) {
		t.Fatalf("err = %v", err)
	}
	if n.Load() != 0 {
		t.Fatalf("token endpoint called %d times", n.Load())
	}
	if _, err := os.Stat(s.stampFile()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("renewal attempted: %v", err)
	}
	if b, _, _ := s.readStored(); b.Connect != nil {
		t.Fatalf("connect stored: %+v", b.Connect)
	}
}

// With consent (a stored Connect credential) a dead refresh token still
// renews silently through the browser profile.
func TestConnectTokenWithConsentRenews(t *testing.T) {
	keyring.MockInit()
	srv, _ := diauth(t, 0)
	s := testStore(t, nil)
	s.diauthURL = srv.URL
	s.hasProfile = func(string) bool { return true }
	seedConnect(t, s, "revoked")
	var logins int
	s.login = func(_ context.Context, o browser.Options, r browser.Request) (browser.Result, error) {
		logins++
		if !o.Headless || !r.Connect || r.Portal {
			t.Errorf("options = %+v %+v", o, r)
		}
		return browser.Result{ConnectTicket: "ST-1"}, nil
	}
	tok, err := s.ConnectToken(context.Background())
	if err != nil || tok != "at-st" || logins != 1 {
		t.Fatalf("ConnectToken = %q, %v, logins %d", tok, err, logins)
	}
}

func TestLogoutWaitsForLock(t *testing.T) {
	keyring.MockInit()
	s := testStore(t, nil)
	if err := s.writeStored(blob{Portal: &portalCred{Cookies: cookies(time.Now().Add(time.Hour))}}); err != nil {
		t.Fatal(err)
	}
	unlock, err := lockPath(context.Background(), s.lockFile())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.logout(context.Background()) }()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("logout finished while the lock was held: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	// A login holding the lock writes the session; logout must still win.
	if err := s.writeStored(blob{Portal: &portalCred{Cookies: cookies(time.Now().Add(time.Hour))}}); err != nil {
		t.Fatal(err)
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("logout did not finish after unlock")
	}
	if b, _, _ := s.readStored(); b.Portal != nil {
		t.Fatalf("session survived logout: %+v", b.Portal)
	}
}
