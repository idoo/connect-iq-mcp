package credstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/idoo/garmin-mcp/internal/auth"
	"github.com/idoo/garmin-mcp/internal/flock"
	"github.com/idoo/garmin-mcp/internal/portal"
)

// Browser drives the profile's dedicated browser directory. The store uses
// it for silent renewal (headless) and status; internal/auth/login
// implements it with Chromium and runs interactive sign-ins through
// [Store.SignIn].
type Browser interface {
	// SignIn opens r.ProfileDir and captures what r asks for.
	SignIn(ctx context.Context, r SignInRequest) (Capture, error)
	// ProfileExists reports whether dir holds a used browser profile.
	ProfileExists(dir string) bool
	// RemoveProfile stops every browser still using dir, then deletes it.
	RemoveProfile(dir string) error
}

// SignInRequest says how to run one browser sign-in and what to capture.
type SignInRequest struct {
	// ProfileDir is the dedicated browser directory; [Store.SignIn] sets it.
	ProfileDir  string
	BrowserPath string        // "" = auto-detect
	Headless    bool          // true for silent renewal
	Timeout     time.Duration // 0 = browser default
	Log         io.Writer     // human-readable progress lines; never secrets
	Portal      bool          // capture developer-portal cookies
	Connect     bool          // capture a Garmin Connect service ticket
	// VerifyPortal checks a candidate developer-portal Cookie header; nil
	// means signed in. [Store.SignIn] sets it.
	VerifyPortal func(ctx context.Context, header string) error
}

// Capture is what a browser sign-in captured, before verification.
type Capture struct {
	PortalCookies     string // Cookie header of the apps.garmin.com cookies
	ConnectTicket     string // "ST-...", empty if not requested
	ConnectServiceURL string
}

// SignedIn is a verified sign-in that is not saved yet; [Store.CommitLocked]
// saves it.
type SignedIn struct {
	portal  *portalCred
	connect *connectCred
	expires time.Time
	// portalNotExtended: the renewed seal expires no later than the stored
	// one, so portal is nil and the stored session is kept.
	portalNotExtended bool
	// warnings come from saves that replaced unreadable stored credentials.
	warnings []string
}

// ErrPortalFromEnv and ErrConnectFromEnv refuse a sign-in whose result a
// read-only environment override would shadow.
var (
	ErrPortalFromEnv  = errors.New("the developer-portal session comes from GARMIN_MCP_PORTAL_COOKIES; unset it to sign in with the browser")
	ErrConnectFromEnv = errors.New("the Garmin Connect token comes from GARMIN_MCP_CONNECT_REFRESH_TOKEN; unset it to sign in with the browser")
)

// RefuseEnv rejects a sign-in whose result would be shadowed by a read-only
// environment override.
func (s *Store) RefuseEnv(connect bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.envPortal != nil {
		return ErrPortalFromEnv
	}
	if connect && s.envConnect != nil {
		return ErrConnectFromEnv
	}
	return nil
}

// LockSignIn takes the <profile>.login lock that serializes browser sign-ins
// on the profile's browser directory.
func (s *Store) LockSignIn(ctx context.Context) (unlock func(), err error) {
	return flock.Lock(ctx, s.loginLockFile())
}

// Lock takes the profile lock that guards read-modify-writes of the stored
// credentials.
func (s *Store) Lock(ctx context.Context) (unlock func(), err error) {
	return flock.Lock(ctx, s.lockFile())
}

// ValidLocked reports whether the requested credentials still work: stored
// portal cookies pass Verify (rotations are saved) and, when connect is
// requested, the access token is fresh or the refresh token still refreshes.
// Callers hold the profile lock.
func (s *Store) ValidLocked(ctx context.Context, connect bool) (auth.LoginResult, bool) {
	b, _, _, err := s.view()
	if err != nil || b.Portal == nil || b.Portal.Cookies == "" || s.portalDead(b.Portal) {
		return auth.LoginResult{}, false
	}
	dev, session, err := s.verifyPortal(ctx, b.Portal.Cookies)
	if err != nil {
		return auth.LoginResult{}, false
	}
	p := *b.Portal
	p.DeveloperID, p.DeveloperName = firstNonEmpty(dev.ID, p.DeveloperID), firstNonEmpty(dev.Name, p.DeveloperName)
	if session != p.Cookies {
		p.Cookies, p.SavedAt = session, s.now().Unix()
	}
	if p != *b.Portal {
		if err := s.savePortal(&p); err != nil {
			return auth.LoginResult{}, false
		}
	}
	r := SignedIn{portal: &p}
	r.expires, _ = portal.SealExpiry(p.Cookies)
	if connect {
		c := b.Connect
		if c == nil || c.RefreshToken == "" {
			return auth.LoginResult{}, false
		}
		if _, ok := s.accessUsable(c); !ok {
			nc, err := s.diRefresh(ctx, c)
			if err != nil {
				return auth.LoginResult{}, false
			}
			s.keepConnect(nc) // a failed save keeps the rotated token in memory
			c = nc
		}
		r.connect = c
	}
	out := r.result()
	out.AlreadyValid = true
	return out, true
}

// SignIn runs a browser sign-in in the profile's browser directory and
// verifies what it captured. It saves nothing and needs no profile lock.
func (s *Store) SignIn(ctx context.Context, r SignInRequest) (*SignedIn, error) {
	out, err := s.signIn(ctx, r, time.Time{})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// CommitLocked stores what SignIn captured. Callers hold the profile lock.
func (s *Store) CommitLocked(r *SignedIn) (auth.LoginResult, error) {
	if err := s.commitSignIn(r); err != nil {
		return auth.LoginResult{}, err
	}
	return r.result(), nil
}

// SavePortalHeader verifies a developer-portal Cookie header (pasted by the
// human) and stores it. Callers check RefuseEnv(false) first.
func (s *Store) SavePortalHeader(ctx context.Context, header string) (auth.LoginResult, error) {
	unlock, err := s.Lock(ctx)
	if err != nil {
		return auth.LoginResult{}, fmt.Errorf("lock credentials: %w", err)
	}
	defer unlock()
	dev, session, err := s.verifyPortal(ctx, header)
	if err != nil {
		return auth.LoginResult{}, err
	}
	r := SignedIn{portal: &portalCred{Cookies: session, DeveloperID: dev.ID, DeveloperName: dev.Name, SavedAt: s.now().Unix()}}
	r.expires, _ = portal.SealExpiry(session)
	if err := s.savePortal(r.portal); err != nil {
		return auth.LoginResult{}, err
	}
	r.warnings = s.takeSaveWarnings()
	return r.result(), nil
}

// signIn runs the browser and verifies what it captured; commitSignIn stores
// it. It saves nothing and needs no profile lock. With a non-zero oldSeal a
// portal seal expiring no later than it is dropped (portalNotExtended).
func (s *Store) signIn(ctx context.Context, r SignInRequest, oldSeal time.Time) (SignedIn, error) {
	var out SignedIn
	r.ProfileDir = s.browserDir()
	r.VerifyPortal = func(ctx context.Context, h string) error {
		_, _, err := s.verifyPortal(ctx, h)
		return err
	}
	res, err := s.login(ctx, r)
	if err != nil {
		return out, err
	}
	if r.Portal {
		if res.PortalCookies == "" {
			return out, errors.New("browser returned no developer-portal cookies")
		}
		dev, session, err := s.verifyPortal(ctx, res.PortalCookies)
		if err != nil {
			return out, err
		}
		exp, expErr := portal.SealExpiry(session)
		if !oldSeal.IsZero() && expErr == nil && !exp.After(oldSeal) {
			out.portalNotExtended = true
		} else {
			out.portal = &portalCred{Cookies: session, DeveloperID: dev.ID, DeveloperName: dev.Name, SavedAt: s.now().Unix()}
			out.expires = exp
		}
	}
	if r.Connect {
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
		out.connect = c
	}
	return out, nil
}

// commitSignIn stores what signIn captured. Callers hold the profile lock.
func (s *Store) commitSignIn(out *SignedIn) error {
	if out.portal != nil {
		if err := s.savePortal(out.portal); err != nil {
			return err
		}
	}
	if out.connect != nil {
		if err := s.saveConnect(out.connect); err != nil {
			return err
		}
	}
	out.warnings = s.takeSaveWarnings()
	return nil
}

func (r SignedIn) result() auth.LoginResult {
	var out auth.LoginResult
	if r.portal != nil {
		out.DeveloperID, out.DeveloperName, out.PortalExpires = r.portal.DeveloperID, r.portal.DeveloperName, r.expires
	}
	if r.connect != nil {
		out.Connect, out.ConnectName = true, r.connect.DisplayName
	}
	out.Warnings = r.warnings
	return out
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
