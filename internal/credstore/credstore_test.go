package credstore

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/idoo/ciq-mcp/internal/auth"
	"github.com/idoo/ciq-mcp/internal/browser"
	"github.com/idoo/ciq-mcp/internal/portal"
)

func seal(t time.Time) string {
	return "Fe26.2**salt*iv*enc*" + strconv.FormatInt(t.UnixMilli(), 10) + "*hsalt*hmac"
}

func cookies(exp time.Time) string {
	return "session=" + seal(exp) + "; refresh_token=" + seal(exp)
}

// testStore returns a store in a temp state dir with no env, no browser
// profile, and a login that fails the test if called.
func testStore(t *testing.T, env map[string]string) *Store {
	t.Helper()
	s := &Store{
		profile:    "test",
		dir:        t.TempDir(),
		autoRenew:  true,
		hc:         newHTTPClient(),
		diauthURL:  "http://127.0.0.1:1/unused",
		connectAPI: "http://127.0.0.1:1",
		now:        time.Now,
		hasProfile: func(string) bool { return false },
		login: func(context.Context, browser.Options, browser.Request) (browser.Result, error) {
			t.Error("unexpected browser login")
			return browser.Result{}, errors.New("no browser")
		},
		verify: func(context.Context, *http.Client, string) (portal.Developer, string, error) {
			return portal.Developer{}, "", errors.New("unexpected verify")
		},
		getenv: func(k string) string { return env[k] },
	}
	s.loadEnv()
	return s
}

func TestKeyringRoundTrip(t *testing.T) {
	keyring.MockInit()
	s := testStore(t, nil)
	exp := time.Now().Add(30 * 24 * time.Hour)
	if err := s.SavePortalCookies(context.Background(), cookies(exp)); err != nil {
		t.Fatal(err)
	}
	got, err := s.PortalCookies(context.Background())
	if err != nil || got != cookies(exp) {
		t.Fatalf("PortalCookies = %q, %v", got, err)
	}
	if _, err := os.Stat(s.credFile()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file written despite working keychain: %v", err)
	}
	st := s.Status(context.Background())
	if !st.Portal.Present || st.Portal.Source != srcKeychain || st.Portal.ExpiresAt.UnixMilli() != exp.UnixMilli() {
		t.Fatalf("status = %+v", st.Portal)
	}
	if st.Connect.Present {
		t.Fatalf("connect present: %+v", st.Connect)
	}
	if strings.Contains(fmt.Sprintf("%+v", st), seal(exp)) {
		t.Fatal("status leaks secret")
	}
}

func TestFileFallback0600(t *testing.T) {
	keyring.MockInitWithError(errors.New("keychain locked"))
	defer keyring.MockInit()
	s := testStore(t, nil)
	if err := s.SavePortalCookies(context.Background(), cookies(time.Now().Add(time.Hour*24*10))); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(s.credFile())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, want 0600", fi.Mode().Perm())
	}
	if di, _ := os.Stat(filepath.Dir(s.credFile())); di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v", di.Mode().Perm())
	}
	if st := s.Status(context.Background()); st.Portal.Source != srcFile {
		t.Fatalf("source = %q", st.Portal.Source)
	}
}

// diauth serves the token endpoint, counting refreshes and rotating tokens.
func diauth(t *testing.T, delay time.Duration) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Header.Get("X-Garmin-Client-Platform") != "Android" {
			t.Errorf("missing native headers")
		}
		switch r.Form.Get("grant_type") {
		case "refresh_token":
			if r.Form.Get("refresh_token") != "rt-"+strconv.Itoa(int(n.Load())) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			time.Sleep(delay)
			k := n.Add(1)
			_, _ = fmt.Fprintf(w, `{"access_token":"at-%d","refresh_token":"rt-%d","expires_in":86400,"refresh_token_expires_in":2592000}`, k, k)
		case grantServiceTicket:
			if r.Form.Get("client_id") != diClientIDs[1] { // first id rejected
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = fmt.Fprint(w, `{"access_token":"at-st","refresh_token":"rt-0","expires_in":86400,"refresh_token_expires_in":2592000}`)
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func seedConnect(t *testing.T, s *Store, rt string) {
	t.Helper()
	_, err := s.writeStored(blob{Connect: &connectCred{
		ClientID: diClientIDs[0], AccessToken: "old", AccessExpiresAt: time.Now().Add(5 * time.Minute).Unix(),
		RefreshToken: rt, RefreshExpiresAt: time.Now().Add(24 * time.Hour).Unix(),
	}})
	if err != nil {
		t.Fatal(err)
	}
}

func TestDIRefreshRotationPersisted(t *testing.T) {
	keyring.MockInit()
	srv, n := diauth(t, 0)
	s := testStore(t, nil)
	s.diauthURL = srv.URL
	seedConnect(t, s, "rt-0")
	tok, err := s.ConnectToken(context.Background())
	if err != nil || tok != "at-1" {
		t.Fatalf("ConnectToken = %q, %v", tok, err)
	}
	b, _, err := s.readStored()
	if err != nil || b.Connect.RefreshToken != "rt-1" || b.Connect.ClientID != diClientIDs[0] {
		t.Fatalf("stored = %+v, %v", b.Connect, err)
	}
	// A fresh token is served without another refresh.
	if tok, _ := s.ConnectToken(context.Background()); tok != "at-1" || n.Load() != 1 {
		t.Fatalf("second call = %q, refreshes %d", tok, n.Load())
	}
}

func TestLockPreventsDoubleRefresh(t *testing.T) {
	keyring.MockInit()
	srv, n := diauth(t, 100*time.Millisecond)
	dir := t.TempDir()
	// Separate Store values model separate processes sharing one state dir.
	stores := make([]*Store, 4)
	for i := range stores {
		stores[i] = testStore(t, nil)
		stores[i].dir = dir
		stores[i].diauthURL = srv.URL
	}
	seedConnect(t, stores[0], "rt-0")
	var wg sync.WaitGroup
	for _, s := range stores {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if tok, err := s.ConnectToken(context.Background()); err != nil || tok != "at-1" {
				t.Errorf("ConnectToken = %q, %v", tok, err)
			}
		}()
	}
	wg.Wait()
	if n.Load() != 1 {
		t.Fatalf("refreshes = %d, want 1", n.Load())
	}
}

func TestDeadRefreshNoProfileAuthRequired(t *testing.T) {
	keyring.MockInit()
	srv, _ := diauth(t, 0)
	s := testStore(t, nil)
	s.diauthURL = srv.URL
	seedConnect(t, s, "revoked")
	_, err := s.ConnectToken(context.Background())
	if !errors.Is(err, auth.ErrAuthRequired) || !strings.Contains(err.Error(), "ciq-mcp auth login") {
		t.Fatalf("err = %v", err)
	}
}

func TestExchangeTicketTriesClientIDs(t *testing.T) {
	keyring.MockInit()
	srv, _ := diauth(t, 0)
	s := testStore(t, nil)
	s.diauthURL = srv.URL
	c, err := s.exchangeTicket(context.Background(), "ST-1", "https://svc")
	if err != nil || c.ClientID != diClientIDs[1] || c.RefreshToken != "rt-0" {
		t.Fatalf("exchange = %+v, %v", c, err)
	}
}

func TestExpiredPortalNoProfile(t *testing.T) {
	keyring.MockInit()
	s := testStore(t, nil)
	if _, err := s.writeStored(blob{Portal: &portalCred{Cookies: cookies(time.Now().Add(-time.Hour))}}); err != nil {
		t.Fatal(err)
	}
	_, err := s.PortalCookies(context.Background())
	if !errors.Is(err, auth.ErrAuthRequired) || !strings.Contains(err.Error(), `run "ciq-mcp auth login" in a terminal`) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "Fe26") {
		t.Fatal("error leaks cookie")
	}
}

func TestExpiringPortalSilentRenewal(t *testing.T) {
	keyring.MockInit()
	s := testStore(t, nil)
	_, _ = s.writeStored(blob{Portal: &portalCred{Cookies: cookies(time.Now().Add(24 * time.Hour))}})
	freshExp := time.Now().Add(60 * 24 * time.Hour)
	fresh := cookies(freshExp)
	s.hasProfile = func(string) bool { return true }
	var logins int
	s.login = func(_ context.Context, o browser.Options, r browser.Request) (browser.Result, error) {
		logins++
		if !o.Headless || !r.Portal {
			t.Errorf("options = %+v %+v", o, r)
		}
		return browser.Result{PortalCookies: []browser.Cookie{{Name: "session", Value: seal(freshExp)}, {Name: "refresh_token", Value: seal(freshExp)}}}, nil
	}
	s.verify = func(_ context.Context, _ *http.Client, h string) (portal.Developer, string, error) {
		return portal.Developer{ID: "42", Name: "Dev"}, h, nil
	}
	got, err := s.PortalCookies(context.Background())
	if err != nil || got != fresh || logins != 1 {
		t.Fatalf("got %q, %v, logins %d", got, err, logins)
	}
	// Rate limit: an immediate second attempt does not launch the browser.
	_, _ = s.writeStored(blob{Portal: &portalCred{Cookies: cookies(time.Now().Add(time.Hour))}})
	if _, err := s.PortalCookies(context.Background()); err != nil || logins != 1 {
		t.Fatalf("second: %v, logins %d", err, logins)
	}
}

func TestEnvOverridePrecedence(t *testing.T) {
	keyring.MockInit()
	srv, _ := diauth(t, 0)
	envCookies := cookies(time.Now().Add(20 * 24 * time.Hour))
	s := testStore(t, map[string]string{
		"CIQ_MCP_PORTAL_COOKIES":        envCookies,
		"CIQ_MCP_CONNECT_REFRESH_TOKEN": "rt-0",
	})
	s.diauthURL = srv.URL
	_, _ = s.writeStored(blob{Portal: &portalCred{Cookies: cookies(time.Now().Add(40 * 24 * time.Hour))}})
	got, err := s.PortalCookies(context.Background())
	if err != nil || got != envCookies {
		t.Fatalf("PortalCookies = %q, %v", got, err)
	}
	tok, err := s.ConnectToken(context.Background())
	if err != nil || tok != "at-1" {
		t.Fatalf("ConnectToken = %q, %v", tok, err)
	}
	// Rotation stays in memory; storage is untouched.
	b, _, _ := s.readStored()
	if b.Connect != nil {
		t.Fatalf("env rotation persisted: %+v", b.Connect)
	}
	if s.envConnect.RefreshToken != "rt-1" {
		t.Fatalf("in-memory rotation = %q", s.envConnect.RefreshToken)
	}
	st := s.Status(context.Background())
	if st.Portal.Source != srcEnv || st.Connect.Source != srcEnv || st.Portal.Renewable {
		t.Fatalf("status = %+v", st)
	}
}
