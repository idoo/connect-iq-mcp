// Package login signs a credential profile in through its dedicated Chromium
// profile: the interactive window behind `connect-iq-mcp auth login` and the
// auth_login tool, and (as credstore's [credstore.Browser]) the headless
// window of silent renewal.
package login

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/idoo/connect-iq-mcp/internal/auth"
	"github.com/idoo/connect-iq-mcp/internal/browser"
	"github.com/idoo/connect-iq-mcp/internal/credstore"
)

// Chromium runs credstore sign-ins in the dedicated Chromium profile.
type Chromium struct{}

var _ credstore.Browser = Chromium{}

// SignIn implements credstore.Browser with browser.Login.
func (Chromium) SignIn(ctx context.Context, r credstore.SignInRequest) (credstore.Capture, error) {
	res, err := browser.Login(ctx,
		browser.Options{ProfileDir: r.ProfileDir, BrowserPath: r.BrowserPath, Headless: r.Headless, Timeout: r.Timeout, Log: r.Log},
		browser.Request{Portal: r.Portal, Connect: r.Connect, VerifyPortal: r.VerifyPortal})
	if err != nil {
		return credstore.Capture{}, err
	}
	return credstore.Capture{
		PortalCookies:     cookieHeader(res.PortalCookies),
		ConnectTicket:     res.ConnectTicket,
		ConnectServiceURL: res.ConnectServiceURL,
	}, nil
}

// ProfileExists implements credstore.Browser.
func (Chromium) ProfileExists(dir string) bool { return browser.ProfileExists(dir) }

// RemoveProfile implements credstore.Browser.
func (Chromium) RemoveProfile(dir string) error { return browser.RemoveProfile(dir) }

func cookieHeader(cs []browser.Cookie) string {
	parts := make([]string, 0, len(cs))
	for _, c := range cs {
		if c.Name != "" {
			parts = append(parts, c.Name+"="+c.Value)
		}
	}
	return strings.Join(parts, "; ")
}

// Store is a credstore.Store that can also sign in interactively; it
// implements auth.Store and auth.Loginer.
type Store struct{ *credstore.Store }

var _ auth.Loginer = (*Store)(nil)

// Open returns profile's credential store with Chromium sign-ins.
func Open(ctx context.Context, profile string) (*Store, error) {
	s, err := credstore.Open(ctx, profile, Chromium{})
	if err != nil {
		return nil, err
	}
	return &Store{s}, nil
}

// Login signs in through the dedicated browser profile (interactive window)
// and stores what it captured. It is the one code path behind both
// `connect-iq-mcp auth login` and the auth_login tool. On any error nothing is saved.
func (s *Store) Login(ctx context.Context, o auth.LoginOptions) (auth.LoginResult, error) {
	return login(ctx, s.Store, o)
}

// creds is what an interactive sign-in needs from the credential store.
type creds interface {
	RefuseEnv(connect bool) error
	LockSignIn(ctx context.Context) (func(), error)
	Lock(ctx context.Context) (func(), error)
	ValidLocked(ctx context.Context, connect bool) (auth.LoginResult, bool)
	SignIn(ctx context.Context, r credstore.SignInRequest) (*credstore.SignedIn, error)
	CommitLocked(r *credstore.SignedIn) (auth.LoginResult, error)
}

// login is Store.Login over creds. The window is guarded by the
// <profile>.login lock, not the profile lock: other calls keep using
// still-valid credentials while the human signs in (LC-3). The profile lock
// is taken only for the validity check and the save.
func login(ctx context.Context, s creds, o auth.LoginOptions) (auth.LoginResult, error) {
	if err := s.RefuseEnv(o.Connect); err != nil {
		return auth.LoginResult{}, err
	}
	unlockLogin, err := s.LockSignIn(ctx)
	if err != nil {
		return auth.LoginResult{}, fmt.Errorf("lock browser profile: %w", err)
	}
	defer unlockLogin()
	if o.SkipIfValid {
		unlock, err := s.Lock(ctx)
		if err != nil {
			return auth.LoginResult{}, fmt.Errorf("lock credentials: %w", err)
		}
		r, ok := s.ValidLocked(ctx, o.Connect)
		unlock()
		if ok {
			return r, nil
		}
	}
	res, err := s.SignIn(ctx, credstore.SignInRequest{BrowserPath: o.BrowserPath, Timeout: o.Timeout, Log: o.Log, Portal: true, Connect: o.Connect})
	if err != nil {
		return auth.LoginResult{}, loginError(ctx, err, o.Timeout)
	}
	unlock, err := s.Lock(ctx)
	if err != nil {
		return auth.LoginResult{}, fmt.Errorf("lock credentials: %w; nothing was saved", err)
	}
	defer unlock()
	return s.CommitLocked(res)
}

// ErrTimedOut marks a sign-in the human did not finish in time.
var ErrTimedOut = errors.New("sign-in timed out")

// timeoutError is ErrTimedOut with the wait it gave up after.
type timeoutError struct{ after time.Duration }

func (e timeoutError) Error() string {
	return fmt.Sprintf("sign-in timed out after %s without finishing in the browser window; nothing was saved", e.after)
}

func (timeoutError) Unwrap() error { return ErrTimedOut }

// loginError turns browser failures into actionable messages.
func loginError(ctx context.Context, err error, timeout time.Duration) error {
	switch {
	case errors.Is(err, browser.ErrNoBrowser):
		return fmt.Errorf("%w: set %s to a Chrome, Edge, Brave or Chromium executable; when none is installed connect-iq-mcp downloads a pinned Chrome for Testing on first use", err, browser.EnvBrowser)
	case errors.Is(err, auth.ErrRateLimited):
		return fmt.Errorf("%w; nothing was saved", err)
	case ctx.Err() != nil:
		return fmt.Errorf("sign-in cancelled; nothing was saved: %w", ctx.Err())
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "timed out"):
		if timeout <= 0 {
			timeout = 10 * time.Minute
		}
		return timeoutError{after: timeout.Round(time.Minute)}
	default:
		return fmt.Errorf("sign-in did not finish (%w); nothing was saved", err)
	}
}
