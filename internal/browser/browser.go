package browser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// ErrInteractionRequired means Garmin showed a sign-in form, MFA prompt or
// Cloudflare challenge while running headless.
var ErrInteractionRequired = errors.New("garmin needs you to sign in")

// ErrRateLimited means Cloudflare in front of Garmin's sign-in server
// answered with error 1015. Live it was keyed on the signed-in profile, not
// the IP (a fresh profile still loaded), and lasted over an hour.
// Retrying right away makes it worse.
var ErrRateLimited = errors.New("garmin's sign-in server is rate limiting this sign-in (Cloudflare 1015); the current session keeps working until it expires, retry renewal in an hour or more")

var errClosed = errors.New("browser closed before sign-in finished")

// signInHint is printed when the interactive window opens; %s is the browser.
const signInHint = "Sign in to Garmin in the %s window that just opened (separate garmin-mcp profile). Keep “Remember me” ticked (garmin-mcp ticks it for you) so the session renews without you for up to 6 months."

// Options configures the browser launch.
type Options struct {
	ProfileDir  string        // dedicated user-data-dir; created 0700
	BrowserPath string        // "" = auto-detect Chrome, Brave, Edge, Chromium
	Headless    bool          // true for silent renewal
	Timeout     time.Duration // overall; default 10 min interactive, 60 s headless
	Log         io.Writer     // human-readable progress lines; never secrets
	BinDir      string        // Chrome for Testing download dir; "" = <state dir>/browser-bin
}

// Cookie is a browser cookie.
type Cookie struct {
	Name, Value, Domain, Path string
	Expires                   time.Time
	HTTPOnly, Secure          bool
}

// Request says what Login must capture.
type Request struct {
	Portal bool
	// VerifyPortal is called with a Cookie header built from apps.garmin.com
	// cookies whenever the browser is on apps.garmin.com; nil means logged in.
	VerifyPortal func(ctx context.Context, cookieHeader string) error
	Connect      bool // also capture a Garmin Connect mobile service ticket
}

// Result is what Login captured.
type Result struct {
	PortalCookies     []Cookie // only Domain == apps.garmin.com / .apps.garmin.com
	ConnectTicket     string   // "ST-...", empty if not requested
	ConnectServiceURL string
}

// pageState is what the poller reads from the current document.
type pageState struct {
	URL       string `json:"url"`
	Title     string `json:"title"`
	Form      bool   `json:"form"`
	MFA       bool   `json:"mfa"`
	Challenge bool   `json:"challenge"`
	Text      string `json:"text"` // start of the visible text, for block pages
}

const stateJS = `(() => {
  const q = s => { try { return !!document.querySelector(s); } catch (e) { return false; } };
  const text = document.body ? document.body.innerText.slice(0, 4000) : "";
  // Tick "Remember me" once per page; the long-lived SSO cookie it sets is
  // what silent renewal relies on. A user who unticks it is left alone.
  if (!window.__ciqRemember) {
    const rm = [...document.querySelectorAll('input[type=checkbox]')].find(b =>
      /remember/i.test([b.name, b.id, b.getAttribute('aria-label'), b.labels && b.labels[0] ? b.labels[0].innerText : ''].join(' ')));
    if (rm) { window.__ciqRemember = true; if (!rm.checked) rm.click(); }
  }
  return {
    url: location.href,
    title: document.title || "",
    form: q('input[type=password]') || q('input[type=email]') || q('input[name=username]') || q('iframe[src*="sso.garmin.com"]'),
    mfa: q('input[name=verificationCode]') || q('input[autocomplete="one-time-code"]') || /verification code|two-step verification|security code/i.test(text),
    challenge: typeof window._cf_chl_opt !== 'undefined' || q('#challenge-form') || q('#challenge-running') || q('#challenge-stage') || location.href.indexOf('__cf_chl') >= 0,
    text: text.slice(0, 1000)
  };
})()`

const pollEvery = 700 * time.Millisecond

// Login launches the browser with the dedicated profile and captures what r
// asks for. One launch handles the portal first, then Connect.
func Login(ctx context.Context, o Options, r Request) (Result, error) {
	if !r.Portal && !r.Connect {
		return Result{}, errors.New("browser: nothing requested")
	}
	if r.Portal && r.VerifyPortal == nil {
		return Result{}, errors.New("browser: VerifyPortal is required for a portal login")
	}
	if o.ProfileDir == "" {
		return Result{}, errors.New("browser: ProfileDir is required")
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Minute
		if o.Headless {
			o.Timeout = 60 * time.Second
		}
	}
	logw := o.Log
	if logw == nil {
		logw = io.Discard
	}
	if !o.Headless && !HasDisplay() {
		return Result{}, errors.New("browser: no display (DISPLAY and WAYLAND_DISPLAY are unset); sign in on a desktop session, or run `garmin-mcp auth login` on a machine with a screen and copy the profile")
	}
	if o.BinDir == "" {
		o.BinDir = defaultBinDir()
	}
	exe, name, note, err := resolveBrowser(ctx, o.BrowserPath, o.BinDir, logw)
	if err != nil {
		return Result{}, err
	}
	if note != "" {
		fmt.Fprintln(logw, note)
	}
	if err := os.MkdirAll(o.ProfileDir, 0o700); err != nil {
		return Result{}, fmt.Errorf("browser: create profile dir: %w", err)
	}
	_ = os.Chmod(o.ProfileDir, 0o700) //nolint:gosec // G302: a directory needs the x bit; 0700 is owner-only
	if err := writePreferences(o.ProfileDir); err != nil {
		return Result{}, err
	}
	excludeFromBackup(o.ProfileDir)
	// A headless renew orphaned by a killed server would hold the profile lock.
	killOrphans(o.ProfileDir)

	bctx, cleanup, err := start(ctx, o, exe)
	defer cleanup()
	if err != nil {
		return Result{}, err
	}
	if o.Headless {
		v, err := chromedp.CallBrowser(bctx, cdpbrowser.GetVersion, cdp.Empty{})
		if err != nil {
			return Result{}, fmt.Errorf("browser: read version: %w", err)
		}
		if _, err := chromedp.Call(bctx, emulation.SetUserAgentOverride, emulation.SetUserAgentOverrideParams{UserAgent: headfulUA(v.UserAgent)}); err != nil {
			return Result{}, fmt.Errorf("browser: set user agent: %w", err)
		}
	}

	wctx, cancelW := context.WithTimeout(bctx, o.Timeout)
	defer cancelW()
	s := &session{ctx: wctx, parent: ctx, headless: o.Headless, log: logw, name: name}
	if t := chromedp.FromContext(bctx).Target; t != nil {
		s.targetID = t.TargetID
	}

	var res Result
	if r.Portal {
		if o.Headless {
			// Silent renewal: drop the portal's own session first, so the
			// dashboard must mint a fresh seal through Garmin SSO instead of
			// re-verifying the one we already hold (which never extends it).
			if err := s.clearPortalSession(); err != nil {
				return Result{}, err
			}
		}
		cs, err := s.portal(r.VerifyPortal)
		if err != nil {
			return Result{}, err
		}
		res.PortalCookies = cs
	}
	if r.Connect {
		t, err := s.connect()
		if err != nil {
			return Result{}, err
		}
		res.ConnectTicket = t
		res.ConnectServiceURL = connectServiceURL
	}
	return res, nil
}

type session struct {
	ctx      context.Context // overall timeout, child of the browser context
	parent   context.Context // caller's context
	headless bool
	log      io.Writer
	targetID target.ID
	hinted   bool
	name     string // browser display name for progress lines
}

func (s *session) hint() {
	if s.headless || s.hinted {
		return
	}
	s.hinted = true
	fmt.Fprintf(s.log, signInHint+"\n", s.name)
}

func (s *session) navigate(u string) error {
	_, err := chromedp.Call(s.ctx, page.Navigate, page.NavigateParams{URL: u})
	if err != nil {
		return s.wrap(err)
	}
	return nil
}

// state reads the page; ok=false on transient errors (mid-navigation).
func (s *session) state() (pageState, bool, error) {
	st, err := chromedp.Run(s.ctx, chromedp.Evaluate[pageState](stateJS))
	if err == nil {
		return st, true, nil
	}
	if e := s.wrap(err); e != nil && (s.ctx.Err() != nil || s.parent.Err() != nil || errors.Is(e, errClosed)) {
		return pageState{}, false, e
	}
	if s.targetGone() {
		return pageState{}, false, errClosed
	}
	return pageState{}, false, nil
}

// targetGone reports whether our tab (or the whole browser) has gone away.
func (s *session) targetGone() bool {
	if s.ctx.Err() != nil {
		return false
	}
	res, err := chromedp.CallBrowser(s.ctx, target.GetTargets, target.GetTargetsParams{})
	if err != nil {
		return s.ctx.Err() == nil
	}
	for _, t := range res.TargetInfos {
		if t.TargetID == s.targetID {
			return false
		}
	}
	return s.targetID != ""
}

// wrap maps context/browser errors to the package's errors.
func (s *session) wrap(err error) error {
	if err == nil {
		return nil
	}
	if s.parent.Err() != nil {
		return s.parent.Err()
	}
	if errors.Is(s.ctx.Err(), context.DeadlineExceeded) {
		if s.headless {
			return fmt.Errorf("%w: timed out waiting for Garmin", ErrInteractionRequired)
		}
		return errors.New("timed out waiting for Garmin sign-in")
	}
	if s.ctx.Err() != nil {
		return errClosed
	}
	return err
}

// interaction decides whether the run must give up on this page: always on a
// Cloudflare block page (no sign-in can happen there), and headless whenever
// a person would have to act.
func (s *session) interaction(st pageState) error {
	switch cloudflareBlock(st.Title, st.Text) {
	case cfRateLimited:
		return ErrRateLimited
	case cfDenied:
		if s.headless {
			return fmt.Errorf("%w (Cloudflare blocked the headless browser: access denied)", ErrInteractionRequired)
		}
		return fmt.Errorf("%w (Cloudflare blocked this browser: access denied)", ErrInteractionRequired)
	}
	if !s.headless {
		return nil
	}
	switch {
	case st.Challenge || challengeTitle(st.Title) || classifyURL(st.URL) == pageChallenge:
		return fmt.Errorf("%w (Cloudflare challenge)", ErrInteractionRequired)
	case st.MFA:
		return fmt.Errorf("%w (verification code requested)", ErrInteractionRequired)
	case st.Form:
		return fmt.Errorf("%w (sign-in form shown)", ErrInteractionRequired)
	}
	return nil
}

func (s *session) sleep() error {
	t := time.NewTimer(pollEvery)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-s.ctx.Done():
		return s.wrap(s.ctx.Err())
	}
}

// portal waits until apps.garmin.com cookies verify.
func (s *session) portal(verify func(context.Context, string) error) ([]Cookie, error) {
	if err := s.navigate(dashboardURL); err != nil {
		return nil, err
	}
	s.hint()
	var lastHeader string
	var portalSince time.Time
	for {
		if err := s.sleep(); err != nil {
			return nil, err
		}
		st, ok, err := s.state()
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if err := s.interaction(st); err != nil {
			return nil, err
		}
		if classifyURL(st.URL) != pagePortal || challengeTitle(st.Title) {
			portalSince = time.Time{}
			continue
		}
		if portalSince.IsZero() {
			portalSince = time.Now()
		}
		cs, err := s.portalCookies()
		if err != nil {
			return nil, err
		}
		h := cookieHeader(cs)
		if h != "" && h != lastHeader {
			lastHeader = h
			if verr := verify(s.ctx, h); verr == nil {
				return cs, nil
			} else if s.parent.Err() != nil {
				return nil, s.parent.Err()
			}
		}
		// Headless on the portal but not signed in and nothing to click
		// through: a person must sign in.
		if s.headless && time.Since(portalSince) > 8*time.Second {
			return nil, fmt.Errorf("%w (portal session not accepted)", ErrInteractionRequired)
		}
	}
}

// portalSessionCookie selects the apps.garmin.com `session` cookie (host-only
// or domain) for Network.deleteCookies.
func portalSessionCookie() network.DeleteCookiesParams {
	return network.DeleteCookiesParams{Name: "session", URL: portalCookieURL}
}

// clearPortalSession deletes the profile's apps.garmin.com `session` cookie.
// Garmin SSO cookies stay, so a signed-in profile gets a new seal on the
// next dashboard load.
func (s *session) clearPortalSession() error {
	if _, err := chromedp.Call(s.ctx, network.DeleteCookies, portalSessionCookie()); err != nil {
		return fmt.Errorf("browser: clear old portal session: %w", s.wrap(err))
	}
	return nil
}

func (s *session) portalCookies() ([]Cookie, error) {
	res, err := chromedp.Call(s.ctx, network.GetCookies, network.GetCookiesParams{URLs: []string{portalCookieURL}})
	if err != nil {
		return nil, s.wrap(err)
	}
	var out []Cookie
	for _, c := range res.Cookies {
		if c == nil || !isPortalDomain(c.Domain) {
			continue
		}
		out = append(out, Cookie{
			Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path,
			Expires: cdpExpires(c.Expires), HTTPOnly: c.HTTPOnly, Secure: c.Secure,
		})
	}
	return out, nil
}

// connect captures a Connect mobile service ticket via the mobile SSO flow.
func (s *session) connect() (string, error) {
	ectx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	tickets := make(chan string, 4)
	send := func(t string) {
		if t == "" {
			return
		}
		select {
		case tickets <- t:
		default:
		}
	}

	if _, err := chromedp.Call(s.ctx, network.Enable, network.EnableParams{}); err != nil {
		return "", s.wrap(err)
	}
	// Subscriptions start before Fetch.enable/navigation so nothing is missed.
	paused := chromedp.Events(ectx, fetch.RequestPaused)
	responses := chromedp.Events(ectx, network.ResponseReceived)
	finished := chromedp.Events(ectx, network.LoadingFinished)

	var mu sync.Mutex
	loginReqs := map[network.RequestID]bool{}
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()
	wg.Add(3)
	go func() {
		defer wg.Done()
		for ev, err := range paused {
			if err != nil {
				return
			}
			if ev.Request != nil {
				send(ticketFromURL(ev.Request.URL))
			}
			// Never let the request reach the service: the ticket is single use.
			_, _ = chromedp.Call(ectx, fetch.FailRequest, fetch.FailRequestParams{RequestID: ev.RequestID, ErrorReason: network.ErrorReasonBlockedByClient})
		}
	}()
	go func() {
		defer wg.Done()
		for ev, err := range responses {
			if err != nil {
				return
			}
			if ev.Response != nil && isMobileLoginAPI(ev.Response.URL) {
				mu.Lock()
				loginReqs[ev.RequestID] = true
				mu.Unlock()
			}
			if ev.Response != nil {
				send(ticketFromURL(ev.Response.URL))
			}
		}
	}()
	go func() {
		defer wg.Done()
		for ev, err := range finished {
			if err != nil {
				return
			}
			mu.Lock()
			want := loginReqs[ev.RequestID]
			delete(loginReqs, ev.RequestID)
			mu.Unlock()
			if !want {
				continue
			}
			body, err := chromedp.Call(ectx, network.GetResponseBody, network.GetResponseBodyParams{RequestID: ev.RequestID})
			if err == nil {
				send(ticketFromJSON(body.Body))
			}
		}
	}()

	if _, err := chromedp.Call(s.ctx, fetch.Enable, fetch.EnableParams{Patterns: []*fetch.RequestPattern{
		{URLPattern: "https://" + connectHost + "/*", RequestStage: fetch.RequestStageRequest},
	}}); err != nil {
		return "", s.wrap(err)
	}
	if err := s.navigate(connectSignInURL); err != nil {
		return "", err
	}
	s.hint()
	if !s.headless {
		fmt.Fprintln(s.log, "Waiting for the Garmin Connect sign-in to finish…")
	}
	for {
		select {
		case t := <-tickets:
			_, _ = chromedp.Call(s.ctx, fetch.Disable, cdp.Empty{})
			return t, nil
		case <-s.ctx.Done():
			return "", s.wrap(s.ctx.Err())
		case <-time.After(pollEvery):
		}
		st, ok, err := s.state()
		if err != nil {
			select {
			case t := <-tickets:
				return t, nil
			default:
			}
			return "", err
		}
		if !ok {
			continue
		}
		if t := ticketFromURL(st.URL); t != "" {
			return t, nil
		}
		if err := s.interaction(st); err != nil {
			// A ticket may have arrived during the same tick.
			select {
			case t := <-tickets:
				return t, nil
			default:
			}
			return "", err
		}
	}
}
