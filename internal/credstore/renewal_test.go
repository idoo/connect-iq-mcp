package credstore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/idoo/garmin-mcp/internal/auth"
	"github.com/idoo/garmin-mcp/internal/flock"
	"github.com/idoo/garmin-mcp/internal/tool"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// renewStore is a store with a browser profile, a fake clock and a counted
// fake browser that answers with fn.
func renewStore(t *testing.T, env map[string]string, fn func(SignInRequest) (Capture, error)) (*Store, *fakeClock, *atomic.Int32) {
	t.Helper()
	keyring.MockInit()
	s := testStore(t, env)
	clk := &fakeClock{t: time.Now()}
	s.now = clk.now
	s.verify = okVerify
	s.hasProfile = func(string) bool { return true }
	var logins atomic.Int32
	s.login = func(_ context.Context, r SignInRequest) (Capture, error) {
		logins.Add(1)
		return fn(r)
	}
	return s, clk, &logins
}

func readStamp(t *testing.T, s *Store, kind string) renewStamp {
	t.Helper()
	raw, err := os.ReadFile(s.stampFile(kind))
	if err != nil {
		t.Fatal(err)
	}
	var st renewStamp
	if err := json.Unmarshal(raw, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// LC-2: after a 1015 every call for 90 minutes reports the rate limit (never
// auth_required) without starting the browser again.
func TestRateLimitHoldsRenewalBack90m(t *testing.T) {
	s, clk, logins := renewStore(t, nil, func(SignInRequest) (Capture, error) {
		return Capture{}, auth.ErrRateLimited
	})
	if err := s.writeStored(blob{Portal: &portalCred{Cookies: cookies(clk.now().Add(-time.Hour))}}); err != nil {
		t.Fatal(err)
	}
	check := func(what string) {
		t.Helper()
		_, err := s.PortalCookies(context.Background())
		var held *rateLimitedError
		if !errors.Is(err, auth.ErrRateLimited) || errors.Is(err, auth.ErrAuthRequired) ||
			strings.Contains(err.Error(), tool.LoginCommand(s.profile)) || !errors.As(err, &held) {
			t.Fatalf("%s: err = %v", what, err)
		}
	}
	check("first")
	if st := readStamp(t, s, stampPortal); st.Outcome != outcomeRateLimited || st.Until.Sub(st.At) != rateLimitBackoff {
		t.Fatalf("stamp = %+v", st)
	}
	clk.add(89 * time.Minute)
	check("inside 90 min")
	if logins.Load() != 1 {
		t.Fatalf("logins = %d, want 1", logins.Load())
	}
	clk.add(2 * time.Minute)
	check("after 90 min")
	if logins.Load() != 2 {
		t.Fatalf("logins = %d, want 2", logins.Load())
	}
}

// LC-4: a renewal that hands back a seal expiring no later than the stored
// one keeps the stored session and holds portal renewal back for 12 hours.
func TestSealNotExtendedBacksOff12h(t *testing.T) {
	var same string
	s, clk, logins := renewStore(t, nil, func(SignInRequest) (Capture, error) {
		return Capture{PortalCookies: "session=" + same}, nil
	})
	exp := clk.now().Add(2 * 24 * time.Hour)
	same = seal(exp)
	stored := &portalCred{Cookies: cookies(exp), DeveloperID: "42", SavedAt: 7}
	if err := s.writeStored(blob{Portal: stored}); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		got, err := s.PortalCookies(context.Background())
		if err != nil || got != cookies(exp) {
			t.Fatalf("call %d: %q, %v", i, got, err)
		}
		clk.add(11 * time.Hour)
	}
	if logins.Load() != 1 {
		t.Fatalf("logins = %d, want 1", logins.Load())
	}
	if b, _, _ := s.readStored(); b.Portal.SavedAt != 7 {
		t.Fatalf("not-extended seal was saved: %+v", b.Portal)
	}
	if st := readStamp(t, s, stampPortal); st.Outcome != outcomeNotExtended || st.Until.Sub(st.At) != notExtendedBackoff {
		t.Fatalf("stamp = %+v", st)
	}
	clk.add(2 * time.Hour) // 24 h after the attempt
	if _, err := s.PortalCookies(context.Background()); err != nil || logins.Load() != 2 {
		t.Fatalf("after 12 h: %v, logins %d", err, logins.Load())
	}
}

// r4: auth status and the auth_status tool (both read Status notes) show an
// active rate-limit or not-extended pause until it ends.
func TestStatusShowsRenewalPause(t *testing.T) {
	s, clk, _ := renewStore(t, nil, func(SignInRequest) (Capture, error) {
		return Capture{}, errors.New("unused")
	})
	if err := s.writeStored(blob{Portal: &portalCred{Cookies: cookies(clk.now().Add(48 * time.Hour))}}); err != nil {
		t.Fatal(err)
	}
	if err := s.writeStamp(stampPortal, outcomeRateLimited, rateLimitBackoff); err != nil {
		t.Fatal(err)
	}
	if err := s.writeStamp(stampConnect, outcomeNotExtended, notExtendedBackoff); err != nil {
		t.Fatal(err)
	}
	until := clk.now().Add(rateLimitBackoff).Local().Format("15:04")
	next := clk.now().Add(notExtendedBackoff).Local().Format("Jan 2 15:04")
	if st := s.Status(context.Background()); !strings.Contains(st.Portal.Note, until) || !strings.Contains(st.Connect.Note, next) {
		t.Fatalf("status lacks the pauses: %q / %q", st.Portal.Note, st.Connect.Note)
	}
	clk.add(rateLimitBackoff + time.Minute)
	st := s.Status(context.Background())
	if cn := s.pauseNote(stampConnect); s.pauseNote(stampPortal) != "" || cn == "" || !strings.HasSuffix(st.Connect.Note, cn) {
		t.Fatalf("after 91 min: %q / %q", st.Portal.Note, st.Connect.Note)
	}
	clk.add(notExtendedBackoff)
	if s.pauseNote(stampConnect) != "" {
		t.Fatalf("after 12 h: %q", s.Status(context.Background()).Connect.Note)
	}
}

// r4: `auth renew` while a sign-in window is open says so instead of
// auth_required (which would send agents to auth_login).
func TestRenewDuringOpenSignIn(t *testing.T) {
	s, clk, logins := renewStore(t, nil, func(SignInRequest) (Capture, error) {
		return Capture{}, errors.New("browser must not start")
	})
	if err := s.writeStored(blob{Portal: &portalCred{Cookies: cookies(clk.now().Add(time.Hour))}}); err != nil {
		t.Fatal(err)
	}
	unlockLogin, err := flock.Lock(context.Background(), s.loginLockFile())
	if err != nil {
		t.Fatal(err)
	}
	defer unlockLogin()
	_, err = s.Renew(context.Background())
	if !errors.Is(err, ErrRenewSignInOpen) || errors.Is(err, auth.ErrAuthRequired) {
		t.Fatalf("err = %v", err)
	}
	if logins.Load() != 0 {
		t.Fatalf("logins = %d", logins.Load())
	}
}

// LC-9: a portal renewal does not hold Connect renewal back.
func TestRenewStampsIndependent(t *testing.T) {
	srv, _ := diauth(t, 0)
	var fresh string
	s, clk, logins := renewStore(t, nil, func(r SignInRequest) (Capture, error) {
		if r.Portal {
			return Capture{PortalCookies: "session=" + fresh}, nil
		}
		return Capture{ConnectTicket: "ST-1"}, nil
	})
	s.diauthURL = srv.URL
	fresh = seal(clk.now().Add(60 * 24 * time.Hour))
	if err := s.writeStored(blob{
		Portal: &portalCred{Cookies: cookies(clk.now().Add(24 * time.Hour))},
		Connect: &connectCred{
			ClientID: diClientIDs[0], AccessToken: "old", AccessExpiresAt: clk.now().Add(-time.Minute).Unix(),
			RefreshToken: "revoked", RefreshExpiresAt: clk.now().Add(24 * time.Hour).Unix(),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.PortalCookies(context.Background()); err != nil || got != "session="+fresh {
		t.Fatalf("PortalCookies = %q, %v", got, err)
	}
	clk.add(time.Minute)
	if tok, err := s.ConnectToken(context.Background()); err != nil || tok != "at-st" {
		t.Fatalf("ConnectToken = %q, %v", tok, err)
	}
	if logins.Load() != 2 {
		t.Fatalf("logins = %d, want 2", logins.Load())
	}
}

// LC-9: a stamp from the future (the clock went back) never blocks renewal.
func TestStampFromFutureDoesNotBlock(t *testing.T) {
	var fresh string
	s, clk, logins := renewStore(t, nil, func(SignInRequest) (Capture, error) {
		return Capture{PortalCookies: "session=" + fresh}, nil
	})
	fresh = seal(clk.now().Add(60 * 24 * time.Hour))
	if err := s.writeStored(blob{Portal: &portalCred{Cookies: cookies(clk.now().Add(-time.Hour))}}); err != nil {
		t.Fatal(err)
	}
	clk.add(2 * time.Hour)
	if err := s.writeStamp(stampPortal, outcomeRateLimited, rateLimitBackoff); err != nil {
		t.Fatal(err)
	}
	clk.add(-2 * time.Hour) // clock stepped back
	if got, err := s.PortalCookies(context.Background()); err != nil || got != "session="+fresh || logins.Load() != 1 {
		t.Fatalf("PortalCookies = %q, %v, logins %d", got, err, logins.Load())
	}
}

// LC-3: an interactive sign-in (login lock) or another process (profile
// lock) never blocks a call while the current credential still works.
func TestHeldLocksDoNotBlockValidCredential(t *testing.T) {
	s, clk, logins := renewStore(t, nil, func(SignInRequest) (Capture, error) {
		return Capture{}, errors.New("browser must not start")
	})
	valid := cookies(clk.now().Add(2 * 24 * time.Hour)) // inside the renew window
	if err := s.writeStored(blob{
		Portal: &portalCred{Cookies: valid},
		Connect: &connectCred{
			ClientID: diClientIDs[0], AccessToken: "live", AccessExpiresAt: clk.now().Add(5 * time.Minute).Unix(),
			RefreshToken: "rt", RefreshExpiresAt: clk.now().Add(24 * time.Hour).Unix(),
		},
	}); err != nil {
		t.Fatal(err)
	}
	unlockLogin, err := flock.Lock(context.Background(), s.loginLockFile())
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if got, err := s.PortalCookies(context.Background()); err != nil || got != valid {
		t.Fatalf("login lock held: %q, %v", got, err)
	}
	if d := time.Since(start); d > 500*time.Millisecond || logins.Load() != 0 {
		t.Fatalf("login lock held: took %v, logins %d", d, logins.Load())
	}
	unlockLogin()

	unlock, err := flock.Lock(context.Background(), s.lockFile())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	start = time.Now()
	if got, err := s.PortalCookies(context.Background()); err != nil || got != valid {
		t.Fatalf("profile lock held: %q, %v", got, err)
	}
	if tok, err := s.ConnectToken(context.Background()); err != nil || tok != "live" {
		t.Fatalf("profile lock held: ConnectToken = %q, %v", tok, err)
	}
	if d := time.Since(start); d > 2*profileLockWait+time.Second {
		t.Fatalf("profile lock held: took %v", d)
	}
	// An expired credential still waits for the lock (and fails with ctx).
	if err := s.writeStored(blob{Portal: &portalCred{Cookies: cookies(clk.now().Add(-time.Hour))}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if _, err := s.PortalCookies(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired credential: %v", err)
	}
}

// LC-6: when saving a rotated Connect token fails, the new token stays in
// memory, is used instead of the old refresh token and is saved later.
func TestConnectSaveFailureKeepsRotation(t *testing.T) {
	useFakeKeyring(t)
	srv, n := diauth(t, 0)
	s := testStore(t, nil)
	clk := &fakeClock{t: time.Now()}
	s.now = clk.now
	s.diauthURL = srv.URL
	seedConnect(t, s, "rt-0")
	var failSet atomic.Bool
	set := keyringSet
	keyringSet = func(svc, user, v string) error {
		if failSet.Load() {
			return errors.New("access denied")
		}
		return set(svc, user, v)
	}
	failSet.Store(true)
	if tok, err := s.ConnectToken(context.Background()); err != nil || tok != "at-1" {
		t.Fatalf("save failing: %q, %v", tok, err)
	}
	if b, _, _ := s.readStored(); b.Connect.RefreshToken != "rt-0" {
		t.Fatalf("stored = %+v", b.Connect)
	}
	if tok, err := s.ConnectToken(context.Background()); err != nil || tok != "at-1" || n.Load() != 1 {
		t.Fatalf("second call: %q, %v, refreshes %d", tok, err, n.Load())
	}
	failSet.Store(false)
	clk.add(24 * time.Hour) // access token expired: next call locks, saves, refreshes with rt-1
	if tok, err := s.ConnectToken(context.Background()); err != nil || tok != "at-2" {
		t.Fatalf("after recovery: %q, %v", tok, err)
	}
	if b, _, _ := s.readStored(); b.Connect.RefreshToken != "rt-2" || s.pendingConnect != nil {
		t.Fatalf("stored = %+v, pending %+v", b.Connect, s.pendingConnect)
	}
}

// LC-5: a rejected env refresh token is auth_required naming the variable and
// never renews through the browser profile.
func TestEnvConnectRejectedNoBrowser(t *testing.T) {
	srv, _ := diauth(t, 0)
	s, _, logins := renewStore(t, map[string]string{"GARMIN_MCP_CONNECT_REFRESH_TOKEN": "revoked"},
		func(SignInRequest) (Capture, error) {
			return Capture{ConnectTicket: "ST-1"}, nil
		})
	s.diauthURL = srv.URL
	_, err := s.ConnectToken(context.Background())
	if !errors.Is(err, auth.ErrAuthRequired) || strings.Contains(err.Error(), s.loginHint()) ||
		!strings.Contains(err.Error(), tool.LoginCommand(s.profile, "--connect")) {
		t.Fatalf("err = %v", err)
	}
	if logins.Load() != 0 {
		t.Fatalf("browser renewal started %d times", logins.Load())
	}
	if _, err := os.Stat(s.stampFile(stampConnect)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("renewal attempted: %v", err)
	}
}
