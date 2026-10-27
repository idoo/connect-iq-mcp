package credstore

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/idoo/garmin-mcp/internal/auth"
	"github.com/idoo/garmin-mcp/internal/portal"
	"github.com/idoo/garmin-mcp/internal/tool"
)

func seal(t time.Time) string {
	return "Fe26.2**salt*iv*enc*" + strconv.FormatInt(t.UnixMilli(), 10) + "*hsalt*hmac"
}

func cookies(exp time.Time) string {
	return "session=" + seal(exp)
}

// jar is a full apps.garmin.com cookie jar as older versions stored it.
func jar(exp time.Time) string {
	return "notice_behavior=implied,eu; TAsessionID=t; " + cookies(exp) + "; refresh_token=" + seal(exp) +
		"; CIQ_DI_session=" + seal(exp) + "; ADRUM_BTa=R:0; ADRUM_BT1=R:0; SameSite=None; cmapi_cookie_privacy=permit; notice_preferences=2; notice_gdpr_prefs=0"
}

// Only the `session` cookie is sent and stored: an old stored jar or a jar in
// GARMIN_MCP_PORTAL_COOKIES is trimmed on read, and the next save persists
// the trimmed form.
func TestPortalJarTrimmedToSession(t *testing.T) {
	keyring.MockInit()
	exp := time.Now().Add(30 * 24 * time.Hour)
	s := testStore(t, nil)
	if err := s.writeStored(blob{Portal: &portalCred{Cookies: jar(exp)}}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.PortalCookies(context.Background()); err != nil || got != cookies(exp) {
		t.Fatalf("PortalCookies = %q, %v", got, err)
	}
	if err := s.saveConnect(&connectCred{RefreshToken: "rt"}); err != nil {
		t.Fatal(err)
	}
	if b, _, err := s.readStored(); err != nil || b.Portal.Cookies != cookies(exp) {
		t.Fatalf("stored = %+v, %v", b.Portal, err)
	}
	env := testStore(t, map[string]string{"GARMIN_MCP_PORTAL_COOKIES": jar(exp)})
	if got, err := env.PortalCookies(context.Background()); err != nil || got != cookies(exp) {
		t.Fatalf("env PortalCookies = %q, %v", got, err)
	}
	if st := env.Status(context.Background()).Portal; !st.ExpiresAt.Equal(time.UnixMilli(exp.UnixMilli())) {
		t.Fatalf("status expiry = %v, want %v", st.ExpiresAt, exp)
	}
	env = testStore(t, map[string]string{"GARMIN_MCP_PORTAL_COOKIES": "refresh_token=" + seal(exp)})
	if _, err := env.PortalCookies(context.Background()); !errors.Is(err, auth.ErrAuthRequired) {
		t.Fatalf("env without session: %v", err)
	}
}

// testStore returns a store in a temp state dir with no env, no browser
// profile, and a login that fails the test if called.
func testStore(t *testing.T, env map[string]string) *Store {
	t.Helper()
	// Tests never reach a real keychain; skip the D-Bus session check so
	// Linux CI without a bus behaves like the desktop.
	precheck, delay := keyringPrecheck, keyringRetryDelay
	keyringPrecheck, keyringRetryDelay = func() error { return nil }, time.Millisecond
	t.Cleanup(func() { keyringPrecheck, keyringRetryDelay = precheck, delay })
	s := &Store{
		profile:       "test",
		dir:           t.TempDir(),
		autoRenew:     true,
		hc:            newHTTPClient(),
		diauthURL:     "http://127.0.0.1:1/unused",
		connectAPI:    "http://127.0.0.1:1",
		now:           time.Now,
		hasProfile:    func(string) bool { return false },
		removeProfile: os.RemoveAll,
		login: func(context.Context, SignInRequest) (Capture, error) {
			t.Error("unexpected browser login")
			return Capture{}, errors.New("no browser")
		},
		verify: func(context.Context, *http.Client, string) (portal.Developer, string, error) {
			return portal.Developer{}, "", errors.New("unexpected verify")
		},
		getenv: func(k string) string { return env[k] },
	}
	s.loadEnv()
	return s
}

// noBrowser is a Browser for stores whose sign-ins the test never runs.
type noBrowser struct{}

func (noBrowser) SignIn(context.Context, SignInRequest) (Capture, error) {
	return Capture{}, errors.New("no browser")
}
func (noBrowser) ProfileExists(string) bool      { return false }
func (noBrowser) RemoveProfile(dir string) error { return os.RemoveAll(dir) }

func TestKeyringRoundTrip(t *testing.T) {
	keyring.MockInit()
	s := testStore(t, nil)
	exp := time.Now().Add(30 * 24 * time.Hour)
	if err := s.savePortal(&portalCred{Cookies: cookies(exp)}); err != nil {
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
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no POSIX modes; the file is DPAPI-protected instead")
	}
	keyring.MockInitWithError(keyring.ErrUnsupportedPlatform)
	defer keyring.MockInit()
	s := testStore(t, nil)
	if err := s.savePortal(&portalCred{Cookies: cookies(time.Now().Add(time.Hour * 24 * 10))}); err != nil {
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
	if st := s.Status(context.Background()); !strings.HasPrefix(st.Portal.Source, srcFile+" (keychain unavailable") {
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
	err := s.writeStored(blob{Connect: &connectCred{
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
	if !errors.Is(err, auth.ErrAuthRequired) || !strings.Contains(err.Error(), tool.LoginCommand(s.profile)) {
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
	if err := s.writeStored(blob{Portal: &portalCred{Cookies: cookies(time.Now().Add(-time.Hour))}}); err != nil {
		t.Fatal(err)
	}
	_, err := s.PortalCookies(context.Background())
	if !errors.Is(err, auth.ErrAuthRequired) || !strings.Contains(err.Error(), tool.LoginCommand(s.profile)) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "Fe26") {
		t.Fatal("error leaks cookie")
	}
}

// N4: a non-default profile's sign-in hint names --profile, so the human signs
// in the account the server runs as. This is the one golden test of the
// human text of the main sign-in hints; other tests compare error classes
// and tool.LoginCommand.
func TestLoginHintGolden(t *testing.T) {
	keyring.MockInit()
	s := testStore(t, nil)
	s.profile = "work"
	_, err := s.PortalCookies(context.Background())
	const portalHint = `auth_required: not signed in to the Connect IQ developer portal; call the auth_login tool (it opens a browser window for the user to sign in), or run "garmin-mcp auth login --profile work" in a terminal`
	if !errors.Is(err, auth.ErrAuthRequired) || err.Error() != portalHint {
		t.Fatalf("portal err =\n%v\nwant\n%s", err, portalHint)
	}
	_, err = s.ConnectToken(context.Background())
	const connectHint = `auth_required: Garmin Connect is not enabled; a human must run "garmin-mcp auth login --connect --profile work" (or approve auth_login with connect=true); call the auth_login tool (it opens a browser window for the user to sign in), or run "garmin-mcp auth login --profile work" in a terminal`
	if !errors.Is(err, auth.ErrAuthRequired) || err.Error() != connectHint {
		t.Fatalf("connect err =\n%v\nwant\n%s", err, connectHint)
	}
	st := s.Status(context.Background())
	if want := `not signed in; auth_login tool or "garmin-mcp auth login --profile work"`; st.Portal.Note != want {
		t.Fatalf("portal note = %q, want %q", st.Portal.Note, want)
	}
	if want := `not enabled (optional; health data); a human enables it with "garmin-mcp auth login --connect --profile work" or by approving auth_login with connect=true`; st.Connect.Note != want {
		t.Fatalf("connect note = %q, want %q", st.Connect.Note, want)
	}
}

func TestExpiringPortalSilentRenewal(t *testing.T) {
	keyring.MockInit()
	s := testStore(t, nil)
	_ = s.writeStored(blob{Portal: &portalCred{Cookies: cookies(time.Now().Add(24 * time.Hour))}})
	freshExp := time.Now().Add(60 * 24 * time.Hour)
	fresh := cookies(freshExp)
	s.hasProfile = func(string) bool { return true }
	var logins int
	s.login = func(_ context.Context, r SignInRequest) (Capture, error) {
		logins++
		if !r.Headless || !r.Portal {
			t.Errorf("request = %+v", r)
		}
		return Capture{PortalCookies: "session=" + seal(freshExp) + "; refresh_token=" + seal(freshExp)}, nil
	}
	s.verify = func(_ context.Context, _ *http.Client, h string) (portal.Developer, string, error) {
		return portal.Developer{ID: "42", Name: "Dev"}, h, nil
	}
	got, err := s.PortalCookies(context.Background())
	if err != nil || got != fresh || logins != 1 {
		t.Fatalf("got %q, %v, logins %d", got, err, logins)
	}
	// Rate limit: an immediate second attempt does not launch the browser.
	_ = s.writeStored(blob{Portal: &portalCred{Cookies: cookies(time.Now().Add(time.Hour))}})
	if _, err := s.PortalCookies(context.Background()); err != nil || logins != 1 {
		t.Fatalf("second: %v, logins %d", err, logins)
	}
}

// A Cloudflare 1015 during renewal or login is reported as a rate limit to
// wait out, never as "sign in again" (which would hit the same limit).
func TestRenewalRateLimited(t *testing.T) {
	keyring.MockInit()
	srv, _ := diauth(t, 0)
	s := testStore(t, nil)
	s.diauthURL = srv.URL
	s.verify = okVerify
	s.hasProfile = func(string) bool { return true }
	var logins int
	s.login = func(context.Context, SignInRequest) (Capture, error) {
		logins++
		return Capture{}, auth.ErrRateLimited
	}
	if err := s.writeStored(blob{
		Portal: &portalCred{Cookies: cookies(time.Now().Add(-time.Hour))},
		Connect: &connectCred{
			ClientID: diClientIDs[0], AccessToken: "old", AccessExpiresAt: time.Now().Add(5 * time.Minute).Unix(),
			RefreshToken: "revoked", RefreshExpiresAt: time.Now().Add(24 * time.Hour).Unix(),
		},
	}); err != nil {
		t.Fatal(err)
	}
	check := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, auth.ErrRateLimited) || errors.Is(err, auth.ErrAuthRequired) || strings.Contains(err.Error(), tool.LoginCommand(s.profile)) {
			t.Errorf("%s: err = %v", what, err)
		}
	}
	_, err := s.PortalCookies(context.Background())
	check("PortalCookies", err)
	// Connect has its own stamp: the portal's 1015 does not hold it back.
	_, err = s.ConnectToken(context.Background())
	check("ConnectToken", err)
	_, err = s.Renew(context.Background())
	check("auth renew", err)
	_, err = s.SignIn(context.Background(), SignInRequest{Portal: true})
	check("SignIn", err)
	if logins != 4 {
		t.Fatalf("logins = %d, want 4", logins)
	}
}

func TestEnvOverridePrecedence(t *testing.T) {
	keyring.MockInit()
	srv, _ := diauth(t, 0)
	envCookies := cookies(time.Now().Add(20 * 24 * time.Hour))
	s := testStore(t, map[string]string{
		"GARMIN_MCP_PORTAL_COOKIES":        envCookies,
		"GARMIN_MCP_CONNECT_REFRESH_TOKEN": "rt-0",
	})
	s.diauthURL = srv.URL
	_ = s.writeStored(blob{Portal: &portalCred{Cookies: cookies(time.Now().Add(40 * 24 * time.Hour))}})
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
