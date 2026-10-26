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
)

const (
	defaultDIAuthURL   = "https://diauth.garmin.com/di-oauth2-service/oauth/token"
	defaultConnectAPI  = "https://connectapi.garmin.com"
	grantServiceTicket = "https://connectapi.garmin.com/di-oauth2-service/oauth/grant/service_ticket"

	portalRenewWindow  = 3 * 24 * time.Hour
	accessRefreshSlack = 15 * time.Minute
	renewInterval      = 10 * time.Minute
	loginHint          = `call the auth_login tool (it opens a browser window for the user to sign in), or run "garmin-mcp auth login" in a terminal`
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
)

func authRequired(what string, cause error) error {
	if cause != nil {
		return fmt.Errorf("%w: %s (%v); %s", auth.ErrAuthRequired, what, cause, loginHint)
	}
	return fmt.Errorf("%w: %s; %s", auth.ErrAuthRequired, what, loginHint)
}

// renewRateLimited reports a renewal Garmin's sign-in server refused with its
// rate limit. It is not an auth failure: signing in again right away hits the
// same limit, so the error says to wait rather than to call auth_login.
func renewRateLimited(what string, err error) error {
	return fmt.Errorf("%s and silent renewal was refused: %w", what, err)
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
		if b.Portal != nil && !s.portalDead(b.Portal) {
			return b.Portal.Cookies, nil
		}
		return "", authRequired("GARMIN_MCP_PORTAL_COOKIES holds an expired session", nil)
	}
	unlock, err := lockPath(ctx, s.lockFile())
	if err != nil {
		return "", fmt.Errorf("lock credentials: %w", err)
	}
	defer unlock()
	if b, _, _, err = s.view(); err != nil { // another process may have renewed
		return "", err
	}
	if h, ok := s.portalUsable(b.Portal); ok {
		return h, nil
	}
	rerr := s.renewLocked(ctx, true, false, false)
	if rerr == nil {
		if b, _, _, err = s.view(); err == nil && b.Portal != nil {
			return b.Portal.Cookies, nil
		}
	}
	if b.Portal != nil && !s.portalDead(b.Portal) {
		return b.Portal.Cookies, nil // still valid, just close to expiry
	}
	if errors.Is(rerr, browser.ErrRateLimited) {
		return "", renewRateLimited("developer portal session expired", rerr)
	}
	if b.Portal == nil {
		return "", authRequired("not signed in to the Connect IQ developer portal", nil)
	}
	return "", authRequired("developer portal session expired", nil)
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

func (s *Store) portalDead(p *portalCred) bool {
	exp, err := portal.SealExpiry(p.Cookies)
	return err == nil && !exp.After(s.now())
}

// SavePortalCookies applies setCookie to the header stored now, under the
// profile lock, only if that header is still sent (what the request carried).
// A header renewed or removed by another process meanwhile is left alone.
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
	merged := portal.MergeSetCookie(sent, setCookie)
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
		return "", errNoConnectConsent()
	}
	if tok, ok := s.accessUsable(b.Connect); ok {
		return tok, nil
	}
	unlock, err := lockPath(ctx, s.lockFile())
	if err != nil {
		return "", fmt.Errorf("lock credentials: %w", err)
	}
	defer unlock()
	if b, _, _, err = s.view(); err != nil { // another process may have refreshed
		return "", err
	}
	if b.Connect == nil { // signed out meanwhile
		return "", errNoConnectConsent()
	}
	if tok, ok := s.accessUsable(b.Connect); ok {
		return tok, nil
	}
	var cause error
	if c := b.Connect; c != nil && c.RefreshToken != "" &&
		(c.RefreshExpiresAt == 0 || time.Unix(c.RefreshExpiresAt, 0).After(s.now())) {
		nc, err := s.diRefresh(ctx, c)
		if err == nil {
			if err := s.saveConnect(nc); err != nil {
				return "", err
			}
			return nc.AccessToken, nil
		}
		if !errors.Is(err, errTokenRejected) {
			return "", err // transient: keep the refresh token
		}
		cause = err
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
	return "", authRequired("Garmin Connect sign-in expired", cause)
}

// errNoConnectConsent is returned when Connect was never explicitly enabled.
func errNoConnectConsent() error {
	return authRequired(`Garmin Connect is not enabled; a human must run "garmin-mcp auth login --connect" (or approve auth_login with connect=true)`, nil)
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

// renewLocked silently signs in again with the dedicated browser profile.
// force skips the auto_renew switch and the 10-minute rate limit (`auth renew`).
// Callers hold the profile lock.
func (s *Store) renewLocked(ctx context.Context, wantPortal, wantConnect, force bool) error {
	if !force && !s.autoRenew {
		return fmt.Errorf("%w: auto_renew is off", errNoRenewal)
	}
	if !s.hasProfile(s.browserDir()) {
		return fmt.Errorf("%w: no browser profile (sign in once with the auth_login tool or \"garmin-mcp auth login\")", errNoRenewal)
	}
	if !force {
		if fi, err := os.Stat(s.stampFile()); err == nil && s.now().Sub(fi.ModTime()) < renewInterval {
			return fmt.Errorf("%w: renewal attempted less than 10 minutes ago", errNoRenewal)
		}
	}
	if err := writeFile0600(s.stampFile(), []byte(s.now().UTC().Format(time.RFC3339))); err != nil {
		return err
	}
	_ = os.Chtimes(s.stampFile(), s.now(), s.now())
	_, err := s.signIn(ctx, browser.Options{ProfileDir: s.browserDir(), Headless: true}, wantPortal, wantConnect)
	return err
}

// signInResult describes what a browser sign-in stored.
type signInResult struct {
	Portal  *portalCred
	Connect *connectCred
	Expires time.Time
}

// signIn runs the browser, verifies what it captured and stores it only once
// everything requested succeeded. Callers hold the profile lock.
func (s *Store) signIn(ctx context.Context, o browser.Options, wantPortal, wantConnect bool) (signInResult, error) {
	var out signInResult
	req := browser.Request{
		Portal:  wantPortal,
		Connect: wantConnect,
		VerifyPortal: func(ctx context.Context, h string) error {
			_, _, err := s.verify(ctx, s.hc, h)
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
		dev, rotated, err := s.verify(ctx, s.hc, header)
		if err != nil {
			return out, err
		}
		if rotated == "" {
			rotated = header
		}
		out.Portal = &portalCred{Cookies: rotated, DeveloperID: dev.ID, DeveloperName: dev.Name, SavedAt: s.now().Unix()}
		out.Expires, _ = portal.SealExpiry(rotated)
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
	if out.Portal != nil {
		if err := s.savePortal(out.Portal); err != nil {
			return signInResult{}, err
		}
	}
	if out.Connect != nil {
		if err := s.saveConnect(out.Connect); err != nil {
			return signInResult{}, err
		}
	}
	return out, nil
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
		note := "unreadable: " + err.Error()
		return auth.Status{Portal: auth.CredStatus{Note: note}, Connect: auth.CredStatus{Note: note}}
	}
	if b.Portal != nil && b.Portal.Cookies != "" {
		exp, _ := portal.SealExpiry(b.Portal.Cookies)
		st.Portal = s.credStatus(ps, exp, renewable && ps != srcEnv)
	} else {
		st.Portal = auth.CredStatus{Renewable: renewable, Note: `not signed in; auth_login tool or "garmin-mcp auth login"`}
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
		st.Connect = auth.CredStatus{Renewable: renewable, Note: `not enabled (optional; health data); a human enables it with "garmin-mcp auth login --connect" or by approving auth_login with connect=true`}
	}
	return st
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
			cs.Note += `; auth_login tool or "garmin-mcp auth login"`
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
