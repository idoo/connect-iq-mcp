package credstore

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/idoo/garmin-mcp/internal/auth"
	"github.com/idoo/garmin-mcp/internal/browser"
	"github.com/idoo/garmin-mcp/internal/portal"
	"github.com/idoo/garmin-mcp/internal/tool"
)

const (
	defaultDIAuthURL   = "https://diauth.garmin.com/di-oauth2-service/oauth/token"
	defaultConnectAPI  = "https://connectapi.garmin.com"
	grantServiceTicket = "https://connectapi.garmin.com/di-oauth2-service/oauth/grant/service_ticket"

	portalRenewWindow  = 3 * 24 * time.Hour
	accessRefreshSlack = 15 * time.Minute

	// profileLockWait bounds the wait for the profile lock while the current
	// credential still works; past it that credential is returned.
	profileLockWait = time.Second

	// Renewal stamps (see renewLocked).
	stampPortal        = "portal"
	stampConnect       = "connect"
	renewInterval      = 10 * time.Minute
	rateLimitBackoff   = 90 * time.Minute // a 1015 lasted over an hour live
	notExtendedBackoff = 12 * time.Hour

	outcomeStarted     = "started"
	outcomeRenewed     = "renewed"
	outcomeFailed      = "failed"
	outcomeRateLimited = "rate_limited"
	outcomeNotExtended = "not_extended"
)

// diClientIDs are tried in order when exchanging a service ticket.
var diClientIDs = []string{
	"GARMIN_CONNECT_MOBILE_ANDROID_DI_2025Q2",
	"GARMIN_CONNECT_MOBILE_ANDROID_DI_2024Q4",
	"GARMIN_CONNECT_MOBILE_ANDROID_DI",
}

// nativeHeaders are the Garmin Connect Android app headers.
var nativeHeaders = map[string]string{
	"User-Agent":                  "GCM-Android-5.23",
	"X-Garmin-User-Agent":         "com.garmin.android.apps.connectmobile/5.23; ; Google/sdk_gphone64_arm64/google; Android/33; Dalvik/2.1.0",
	"X-Garmin-Paired-App-Version": "10861",
	"X-Garmin-Client-Platform":    "Android",
	"X-App-Ver":                   "10861",
	"X-Lang":                      "en",
	"X-GCExperience":              "GC5",
	"Accept-Language":             "en-US,en;q=0.9",
	"Accept":                      "application/json",
}

var (
	errTokenRejected = errors.New("garmin rejected the token")
	errNoRenewal     = errors.New("silent renewal unavailable")
	// errSealNotExtended: a portal renewal returned a seal that expires no
	// later than the stored one; the stored session is kept.
	errSealNotExtended = fmt.Errorf("%w: the renewed developer-portal session expires no later than the current one", errNoRenewal)
	// errSignInOpen: an interactive sign-in window holds the browser profile.
	errSignInOpen = fmt.Errorf("%w: an interactive sign-in is using the browser profile", errNoRenewal)
)

// loginHint tells the agent or human how to sign this profile in.
func (s *Store) loginHint() string {
	return "call the auth_login tool (it opens a browser window for the user to sign in), or run \"" + tool.LoginCommand(s.profile) + "\" in a terminal"
}

func (s *Store) authRequired(what string, cause error) error {
	if cause != nil {
		return fmt.Errorf("%w: %s (%v); %s", auth.ErrAuthRequired, what, cause, s.loginHint())
	}
	return fmt.Errorf("%w: %s; %s", auth.ErrAuthRequired, what, s.loginHint())
}

// renewRateLimited reports a renewal Garmin's sign-in server refused with its
// rate limit. It is not an auth failure: signing in again right away hits the
// same limit, so the error says to wait rather than to call auth_login.
func renewRateLimited(what string, err error) error {
	return fmt.Errorf("%s and silent renewal was refused: %w", what, err)
}

// rateLimitedError is a renewal refused by (or held back because of) a
// Cloudflare 1015. It wraps browser.ErrRateLimited and names when to retry.
type rateLimitedError struct{ until time.Time }

func (e *rateLimitedError) Error() string {
	return "Garmin is rate-limiting sign-ins (Cloudflare 1015); wait until " + e.until.Local().Format("15:04") +
		", do not call auth_login (a new sign-in hits the same limit)"
}

func (e *rateLimitedError) Unwrap() error { return browser.ErrRateLimited }

// lockProfile takes the profile lock. When the caller still holds a usable
// credential (live), it waits at most profileLockWait and reports busy
// instead of an error, so the caller can return that credential while
// another process holds the lock (LC-3).
func (s *Store) lockProfile(ctx context.Context, live bool) (unlock func(), busy bool, err error) {
	lctx := ctx
	if live {
		var cancel context.CancelFunc
		lctx, cancel = context.WithTimeout(ctx, profileLockWait)
		defer cancel()
	}
	unlock, err = lockPath(lctx, s.lockFile())
	if err != nil {
		if live && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("lock credentials: %w", err)
	}
	return unlock, false, nil
}

// tryLoginLock takes the browser sign-in lock without waiting; busy means an
// interactive sign-in holds it.
func (s *Store) tryLoginLock() (unlock func(), busy bool, err error) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // lockPath tries once before it looks at ctx
	unlock, err = lockPath(ctx, s.loginLockFile())
	if errors.Is(err, context.Canceled) {
		return nil, true, nil
	}
	return unlock, false, err
}

// PortalCookies returns a Cookie header for apps.garmin.com, renewing it
// silently when its seal expires within three days.
func (s *Store) PortalCookies(ctx context.Context) (string, error) {
	b, src, _, err := s.view()
	if err != nil {
		return "", err
	}
	if h, ok := s.portalUsable(b.Portal); ok {
		return h, nil
	}
	if src == srcEnv { // read-only: cannot renew
		if b.Portal == nil || b.Portal.Cookies == "" {
			return "", s.authRequired("GARMIN_MCP_PORTAL_COOKIES has no `session` cookie", nil)
		}
		if !s.portalDead(b.Portal) {
			return b.Portal.Cookies, nil
		}
		return "", s.authRequired("GARMIN_MCP_PORTAL_COOKIES holds an expired session", nil)
	}
	rerr := fmt.Errorf("%w: auto_renew is off", errNoRenewal)
	if s.autoRenew { // auto_renew off: nothing to decide under the lock
		unlock, busy, err := s.lockProfile(ctx, !s.portalDead(b.Portal))
		if err != nil {
			return "", err
		}
		if busy {
			return b.Portal.Cookies, nil // still valid; another process is busy with it
		}
		defer unlock()
		if b, _, _, err = s.view(); err != nil { // another process may have renewed
			return "", err
		}
		if h, ok := s.portalUsable(b.Portal); ok {
			return h, nil
		}
		if rerr = s.renewLocked(ctx, true, false, false); rerr == nil {
			if b, _, _, err = s.view(); err == nil && b.Portal != nil {
				return b.Portal.Cookies, nil
			}
		}
	}
	if b.Portal != nil && !s.portalDead(b.Portal) {
		return b.Portal.Cookies, nil // still valid, just close to expiry
	}
	if errors.Is(rerr, browser.ErrRateLimited) {
		return "", renewRateLimited("developer portal session expired", rerr)
	}
	if b.Portal == nil {
		return "", s.authRequired("not signed in to the Connect IQ developer portal", nil)
	}
	return "", s.authRequired("developer portal session expired", nil)
}

// portalUsable reports p's header when its expiry is beyond the renew window
// (or unknown).
func (s *Store) portalUsable(p *portalCred) (string, bool) {
	if p == nil || p.Cookies == "" {
		return "", false
	}
	exp, err := portal.SealExpiry(p.Cookies)
	if err != nil || exp.Sub(s.now()) > portalRenewWindow {
		return p.Cookies, true
	}
	return "", false
}

// portalDead reports a missing header or one whose seal has expired.
func (s *Store) portalDead(p *portalCred) bool {
	if p == nil || p.Cookies == "" {
		return true
	}
	exp, err := portal.SealExpiry(p.Cookies)
	return err == nil && !exp.After(s.now())
}

// SavePortalCookies applies setCookie to the header stored now, under the
// profile lock, only if that header is still sent (what the request carried).
// A header renewed or removed by another process meanwhile is left alone.
// Only the `session` cookie is kept.
func (s *Store) SavePortalCookies(ctx context.Context, sent string, setCookie []string) error {
	unlock, err := lockPath(ctx, s.lockFile())
	if err != nil {
		return fmt.Errorf("lock credentials: %w", err)
	}
	defer unlock()
	b, _, _, err := s.view()
	if err != nil {
		return err
	}
	if b.Portal == nil || b.Portal.Cookies != sent {
		return nil // no longer derives from the request's cookie
	}
	merged := portalSession(portal.MergeSetCookie(sent, setCookie))
	if merged == sent {
		return nil
	}
	p := *b.Portal
	p.Cookies = merged
	p.SavedAt = s.now().Unix()
	return s.savePortal(&p)
}

// ConnectToken returns a DI OAuth2 access token, refreshing it within 15
// minutes of expiry and renewing through the browser when the refresh token
// is dead.
//
// A stored Connect credential is the human's consent marker: only an explicit
// sign-in with connect=true (`garmin-mcp auth login --connect` or a
// human-approved auth_login) stores one, and logout deletes it. Without it
// ConnectToken never mints a token from the browser profile's SSO session.
func (s *Store) ConnectToken(ctx context.Context) (string, error) {
	b, _, _, err := s.view()
	if err != nil {
		return "", err
	}
	if b.Connect == nil {
		return "", s.errNoConnectConsent()
	}
	if tok, ok := s.accessUsable(b.Connect); ok {
		return tok, nil
	}
	live := b.Connect.AccessToken != "" && time.Unix(b.Connect.AccessExpiresAt, 0).After(s.now())
	unlock, busy, err := s.lockProfile(ctx, live)
	if err != nil {
		return "", err
	}
	if busy {
		return b.Connect.AccessToken, nil // not expired yet; another process is busy with it
	}
	defer unlock()
	s.retryPendingConnect()
	var cs string
	if b, _, cs, err = s.view(); err != nil { // another process may have refreshed
		return "", err
	}
	if b.Connect == nil { // signed out meanwhile
		return "", s.errNoConnectConsent()
	}
	if tok, ok := s.accessUsable(b.Connect); ok {
		return tok, nil
	}
	var cause error
	if c := b.Connect; c != nil && c.RefreshToken != "" &&
		(c.RefreshExpiresAt == 0 || time.Unix(c.RefreshExpiresAt, 0).After(s.now())) {
		nc, err := s.diRefresh(ctx, c)
		if err == nil {
			s.keepConnect(nc)
			return nc.AccessToken, nil
		}
		if !errors.Is(err, errTokenRejected) {
			return "", err // transient: keep the refresh token
		}
		cause = err
	}
	if cs == srcEnv { // never mint a token for whichever account the browser profile holds
		if cause == nil {
			cause = errors.New("refresh token expired")
		}
		return "", fmt.Errorf("%w: GARMIN_MCP_CONNECT_REFRESH_TOKEN was rejected (%v); put a fresh refresh token in it, or unset it and have a human run \"%s\"", auth.ErrAuthRequired, cause, tool.LoginCommand(s.profile, "--connect"))
	}
	rerr := s.renewLocked(ctx, false, true, false)
	if rerr == nil {
		if b, _, _, err = s.view(); err == nil && b.Connect != nil && b.Connect.AccessToken != "" {
			return b.Connect.AccessToken, nil
		}
	}
	if errors.Is(rerr, browser.ErrRateLimited) {
		return "", renewRateLimited("Garmin Connect sign-in expired", rerr)
	}
	return "", s.authRequired("Garmin Connect sign-in expired", cause)
}

// keepConnect saves a refreshed Connect credential. When the save fails the
// credential stays in memory (view prefers it), so a rotated refresh token is
// not lost; the next locked ConnectToken retries the save (LC-6). Callers
// hold the profile lock.
func (s *Store) keepConnect(c *connectCred) {
	if err := s.saveConnect(c); err != nil {
		s.mu.Lock()
		s.pendingConnect = c
		s.mu.Unlock()
	}
}

// retryPendingConnect persists a credential keepConnect could not save. On
// success saveConnect clears it. Callers hold the profile lock.
func (s *Store) retryPendingConnect() {
	s.mu.Lock()
	c := s.pendingConnect
	s.mu.Unlock()
	if c != nil {
		_ = s.saveConnect(c)
	}
}

// errNoConnectConsent is returned when Connect was never explicitly enabled.
func (s *Store) errNoConnectConsent() error {
	return s.authRequired(`Garmin Connect is not enabled; a human must run "`+tool.LoginCommand(s.profile, "--connect")+`" (or approve auth_login with connect=true)`, nil)
}

func (s *Store) accessUsable(c *connectCred) (string, bool) {
	if c == nil || c.AccessToken == "" {
		return "", false
	}
	if time.Unix(c.AccessExpiresAt, 0).Sub(s.now()) > accessRefreshSlack {
		return c.AccessToken, true
	}
	return "", false
}

type tokenResponse struct {
	AccessToken           string `json:"access_token"`
	RefreshToken          string `json:"refresh_token"`
	ExpiresIn             int64  `json:"expires_in"`
	RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in"`
}

// diToken posts form to the DI token endpoint as clientID.
func (s *Store) diToken(ctx context.Context, clientID string, form url.Values) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.diauthURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	for k, v := range nativeHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(clientID+":")))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("garmin token endpoint: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden:
		return nil, fmt.Errorf("%w (HTTP %d)", errTokenRejected, resp.StatusCode)
	default:
		return nil, fmt.Errorf("garmin token endpoint: HTTP %d", resp.StatusCode)
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil || tr.AccessToken == "" {
		return nil, fmt.Errorf("garmin token endpoint: unexpected response")
	}
	return &tr, nil
}

func (s *Store) credFromToken(clientID string, tr *tokenResponse, prev *connectCred) *connectCred {
	now := s.now()
	c := &connectCred{ClientID: clientID, AccessToken: tr.AccessToken, RefreshToken: tr.RefreshToken}
	if prev != nil {
		c.DisplayName = prev.DisplayName
		if c.RefreshToken == "" {
			c.RefreshToken, c.RefreshExpiresAt = prev.RefreshToken, prev.RefreshExpiresAt
		}
	}
	if tr.ExpiresIn > 0 {
		c.AccessExpiresAt = now.Add(time.Duration(tr.ExpiresIn) * time.Second).Unix()
	}
	if tr.RefreshTokenExpiresIn > 0 {
		c.RefreshExpiresAt = now.Add(time.Duration(tr.RefreshTokenExpiresIn) * time.Second).Unix()
	}
	return c
}

// diRefresh exchanges c's refresh token. Without a known client id (env
// override) every DI client id is tried in order.
func (s *Store) diRefresh(ctx context.Context, c *connectCred) (*connectCred, error) {
	ids := diClientIDs
	if c.ClientID != "" {
		ids = []string{c.ClientID}
	}
	var last error
	for _, id := range ids {
		form := url.Values{"grant_type": {"refresh_token"}, "client_id": {id}, "refresh_token": {c.RefreshToken}}
		tr, err := s.diToken(ctx, id, form)
		if err == nil {
			return s.credFromToken(id, tr, c), nil
		}
		if !errors.Is(err, errTokenRejected) {
			return nil, err
		}
		last = err
	}
	return nil, last
}

// exchangeTicket trades a fresh service ticket for DI tokens.
func (s *Store) exchangeTicket(ctx context.Context, ticket, serviceURL string) (*connectCred, error) {
	var last error
	for _, id := range diClientIDs {
		form := url.Values{
			"client_id": {id}, "service_ticket": {ticket},
			"grant_type": {grantServiceTicket}, "service_url": {serviceURL},
		}
		tr, err := s.diToken(ctx, id, form)
		if err == nil {
			c := s.credFromToken(id, tr, nil)
			c.DisplayName = s.displayName(ctx, c.AccessToken)
			return c, nil
		}
		if !errors.Is(err, errTokenRejected) {
			return nil, err
		}
		last = err
	}
	return nil, last
}

// displayName fetches the Connect display name; failures yield "".
func (s *Store) displayName(ctx context.Context, token string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.connectAPI+"/userprofile-service/socialProfile", nil)
	if err != nil {
		return ""
	}
	for k, v := range nativeHeaders {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := s.hc.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	var p struct {
		DisplayName string `json:"displayName"`
		FullName    string `json:"fullName"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&p) != nil {
		return ""
	}
	if p.FullName != "" {
		return p.FullName
	}
	return p.DisplayName
}

// renewStamp records the last silent renewal of one credential kind in
// <profile>.renew-<kind>: when, how it ended and until when the next silent
// attempt is held back.
type renewStamp struct {
	At      time.Time `json:"at"`
	Outcome string    `json:"outcome"`
	Until   time.Time `json:"until"`
}

// stampBlocks reports the kind's stamp when it still holds renewal back. A
// stamp from the future (the clock went back) or a damaged one never blocks,
// and no stamp blocks longer than notExtendedBackoff.
func (s *Store) stampBlocks(kind string) (renewStamp, bool) {
	raw, err := os.ReadFile(s.stampFile(kind))
	if err != nil {
		return renewStamp{}, false
	}
	var st renewStamp
	if json.Unmarshal(raw, &st) != nil || st.At.IsZero() {
		return renewStamp{}, false
	}
	age := s.now().Sub(st.At)
	window := min(st.Until.Sub(st.At), notExtendedBackoff)
	return st, age >= 0 && age < window
}

// writeStamp records outcome for kind, holding renewal back for d.
func (s *Store) writeStamp(kind, outcome string, d time.Duration) error {
	now := s.now()
	raw, err := json.Marshal(renewStamp{At: now.UTC(), Outcome: outcome, Until: now.Add(d).UTC()})
	if err != nil {
		return err
	}
	return writeFile0600(s.stampFile(kind), raw)
}

// renewLocked silently signs in again with the dedicated browser profile.
// force skips the auto_renew switch and the stamps (`auth renew`). Each
// credential kind has its own stamp: a Cloudflare 1015 holds renewal back for
// rateLimitBackoff (and every call meanwhile gets the rate-limited error), a
// portal seal that did not get later keeps the old one and holds portal
// renewal back for notExtendedBackoff, anything else for renewInterval.
// Callers hold the profile lock.
func (s *Store) renewLocked(ctx context.Context, wantPortal, wantConnect, force bool) error {
	if !force && !s.autoRenew {
		return fmt.Errorf("%w: auto_renew is off", errNoRenewal)
	}
	if !s.hasProfile(s.browserDir()) {
		return fmt.Errorf("%w: no browser profile (sign in once with the auth_login tool or \"%s\")", errNoRenewal, tool.LoginCommand(s.profile))
	}
	var kinds []string
	if wantPortal {
		kinds = append(kinds, stampPortal)
	}
	if wantConnect {
		kinds = append(kinds, stampConnect)
	}
	if !force {
		for _, k := range kinds {
			st, blocked := s.stampBlocks(k)
			if !blocked {
				continue
			}
			if st.Outcome == outcomeRateLimited {
				return &rateLimitedError{until: st.Until}
			}
			return fmt.Errorf("%w: last %s renewal at %s (%s); the next attempt is after %s", errNoRenewal, k,
				st.At.Local().Format("15:04"), st.Outcome, st.Until.Local().Format("15:04"))
		}
	}
	unlockLogin, busy, err := s.tryLoginLock()
	if err != nil {
		return fmt.Errorf("lock browser profile: %w", err)
	}
	if busy {
		return errSignInOpen
	}
	defer unlockLogin()
	var oldSeal time.Time
	if wantPortal {
		if b, _, _, err := s.view(); err == nil && b.Portal != nil {
			oldSeal, _ = portal.SealExpiry(b.Portal.Cookies)
		}
	}
	for _, k := range kinds { // written first: a crash mid-renewal still backs off
		if err := s.writeStamp(k, outcomeStarted, renewInterval); err != nil {
			return err
		}
	}
	out, err := s.signIn(ctx, browser.Options{ProfileDir: s.browserDir(), Headless: true}, wantPortal, wantConnect, oldSeal)
	if err == nil {
		err = s.commitSignIn(&out)
	}
	outcome, backoff := outcomeRenewed, renewInterval
	switch {
	case errors.Is(err, browser.ErrRateLimited):
		outcome, backoff = outcomeRateLimited, rateLimitBackoff
	case err != nil:
		outcome = outcomeFailed
	}
	for _, k := range kinds {
		_ = s.writeStamp(k, outcome, backoff)
	}
	if outcome == outcomeRateLimited {
		return &rateLimitedError{until: s.now().Add(backoff)}
	}
	if err == nil && out.PortalNotExtended {
		_ = s.writeStamp(stampPortal, outcomeNotExtended, notExtendedBackoff)
		return errSealNotExtended
	}
	return err
}

// signInResult describes what a browser sign-in captured.
type signInResult struct {
	Portal  *portalCred
	Connect *connectCred
	Expires time.Time
	// PortalNotExtended: the renewed seal expires no later than the stored
	// one, so Portal is nil and the stored session is kept.
	PortalNotExtended bool
	// Warnings come from saves that replaced unreadable stored credentials.
	Warnings []string
}

// signIn runs the browser and verifies what it captured; commitSignIn stores
// it. It saves nothing and needs no profile lock. With a non-zero oldSeal a
// portal seal expiring no later than it is dropped (PortalNotExtended).
func (s *Store) signIn(ctx context.Context, o browser.Options, wantPortal, wantConnect bool, oldSeal time.Time) (signInResult, error) {
	var out signInResult
	req := browser.Request{
		Portal:  wantPortal,
		Connect: wantConnect,
		VerifyPortal: func(ctx context.Context, h string) error {
			_, _, err := s.verifyPortal(ctx, h)
			return err
		},
	}
	res, err := s.login(ctx, o, req)
	if err != nil {
		return out, err
	}
	if wantPortal {
		header := cookieHeader(res.PortalCookies)
		if header == "" {
			return out, errors.New("browser returned no developer-portal cookies")
		}
		dev, session, err := s.verifyPortal(ctx, header)
		if err != nil {
			return out, err
		}
		exp, expErr := portal.SealExpiry(session)
		if !oldSeal.IsZero() && expErr == nil && !exp.After(oldSeal) {
			out.PortalNotExtended = true
		} else {
			out.Portal = &portalCred{Cookies: session, DeveloperID: dev.ID, DeveloperName: dev.Name, SavedAt: s.now().Unix()}
			out.Expires = exp
		}
	}
	if wantConnect {
		if res.ConnectTicket == "" {
			return out, errors.New("browser returned no Garmin Connect service ticket")
		}
		svc := res.ConnectServiceURL
		if svc == "" {
			svc = "https://mobile.integration.garmin.com/gcm/android"
		}
		c, err := s.exchangeTicket(ctx, res.ConnectTicket, svc)
		if err != nil {
			return out, err
		}
		out.Connect = c
	}
	return out, nil
}

// commitSignIn stores what signIn captured. Callers hold the profile lock.
func (s *Store) commitSignIn(out *signInResult) error {
	if out.Portal != nil {
		if err := s.savePortal(out.Portal); err != nil {
			return err
		}
	}
	if out.Connect != nil {
		if err := s.saveConnect(out.Connect); err != nil {
			return err
		}
	}
	out.Warnings = s.takeSaveWarnings()
	return nil
}

// verifyPortal checks header's `session` cookie with the portal and returns
// the developer and the header to store: "session=<seal>", rotated if the
// portal set a new one.
func (s *Store) verifyPortal(ctx context.Context, header string) (portal.Developer, string, error) {
	header, err := portal.SessionOnly(header)
	if err != nil {
		return portal.Developer{}, "", err
	}
	dev, rotated, err := s.verify(ctx, s.hc, header)
	if err != nil {
		return portal.Developer{}, "", err
	}
	if r := portalSession(rotated); r != "" {
		header = r
	}
	return dev, header, nil
}

func cookieHeader(cs []browser.Cookie) string {
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		if c.Name != "" {
			parts = append(parts, c.Name+"="+c.Value)
		}
	}
	return strings.Join(parts, "; ")
}

// Status reports each credential without secrets or network calls.
func (s *Store) Status(context.Context) auth.Status {
	var st auth.Status
	b, ps, cs, err := s.view()
	renewable := s.hasProfile(s.browserDir())
	if err != nil {
		note := statusNote(err)
		return auth.Status{Portal: auth.CredStatus{Note: note}, Connect: auth.CredStatus{Note: note}}
	}
	if b.Portal != nil && b.Portal.Cookies != "" {
		exp, _ := portal.SealExpiry(b.Portal.Cookies)
		st.Portal = s.credStatus(ps, exp, renewable && ps != srcEnv)
	} else {
		st.Portal = auth.CredStatus{Renewable: renewable, Note: `not signed in; auth_login tool or "` + tool.LoginCommand(s.profile) + `"`}
	}
	if b.Connect != nil && b.Connect.RefreshToken != "" {
		var exp time.Time
		if b.Connect.RefreshExpiresAt > 0 {
			exp = time.Unix(b.Connect.RefreshExpiresAt, 0)
		}
		st.Connect = s.credStatus(cs, exp, renewable && cs != srcEnv)
		if cs == srcEnv {
			st.Connect.Note += "; rotated tokens are kept in memory only"
		}
	} else {
		st.Connect = auth.CredStatus{Renewable: renewable, Note: `not enabled (optional; health data); a human enables it with "` + tool.LoginCommand(s.profile, "--connect") + `" or by approving auth_login with connect=true`}
	}
	st.Portal.Note += s.pauseNote(stampPortal)
	st.Connect.Note += s.pauseNote(stampConnect)
	return st
}

// pauseNote describes an active silent-renewal pause of kind read from its
// stamp (the same window renewLocked honours), or "".
func (s *Store) pauseNote(kind string) string {
	st, blocked := s.stampBlocks(kind)
	if !blocked {
		return ""
	}
	switch st.Outcome {
	case outcomeRateLimited:
		return "; renewal paused: Garmin is rate-limiting sign-ins (Cloudflare 1015) until " + st.Until.Local().Format("15:04")
	case outcomeNotExtended:
		return "; renewal paused: the last renewal did not extend the session; next attempt after " + st.Until.Local().Format("Jan 2 15:04")
	}
	return ""
}

func (s *Store) credStatus(src string, exp time.Time, renewable bool) auth.CredStatus {
	cs := auth.CredStatus{Present: true, Source: src, ExpiresAt: exp, Renewable: renewable}
	auto := renewable && s.autoRenew
	switch {
	case exp.IsZero():
		cs.Note = "expiry unknown"
	case !exp.After(s.now()):
		cs.Note = "expired"
		if auto {
			cs.Note += ", renews automatically on next use"
		} else {
			cs.Note += `; auth_login tool or "` + tool.LoginCommand(s.profile) + `"`
		}
		return cs
	default:
		cs.Note = "expires in " + humanDuration(exp.Sub(s.now()))
	}
	if src == srcEnv {
		cs.Note += " (environment override, read-only)"
	} else if auto {
		cs.Note += ", renews automatically"
	}
	return cs
}

func humanDuration(d time.Duration) string {
	switch days := int(d.Hours() / 24); {
	case days >= 2:
		return fmt.Sprintf("%d days", days)
	case days == 1:
		return "1 day"
	case d >= 2*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	}
}
