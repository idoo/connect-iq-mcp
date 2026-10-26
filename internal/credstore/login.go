package credstore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/idoo/ciq-mcp/internal/auth"
	"github.com/idoo/ciq-mcp/internal/browser"
	"github.com/idoo/ciq-mcp/internal/portal"
)

var _ auth.Loginer = (*Store)(nil)

// Login signs in through the dedicated browser profile (interactive window)
// and stores what it captured. It is the one code path behind both
// `ciq-mcp auth login` and the auth_login tool. On any error nothing is saved.
func (s *Store) Login(ctx context.Context, o auth.LoginOptions) (auth.LoginResult, error) {
	if err := s.refuseEnv(o.Connect); err != nil {
		return auth.LoginResult{}, err
	}
	unlock, err := lockPath(ctx, s.lockFile())
	if err != nil {
		return auth.LoginResult{}, fmt.Errorf("lock credentials: %w", err)
	}
	defer unlock()
	if o.SkipIfValid {
		if r, ok := s.validLocked(ctx, o.Connect); ok {
			return r, nil
		}
	}
	bo := browser.Options{ProfileDir: s.browserDir(), BrowserPath: o.BrowserPath, Timeout: o.Timeout, Log: o.Log}
	res, err := s.signIn(ctx, bo, true, o.Connect)
	if err != nil {
		return auth.LoginResult{}, loginError(ctx, err, o.Timeout)
	}
	return res.result(), nil
}

// refuseEnv rejects a sign-in whose result would be shadowed by a read-only
// environment override.
func (s *Store) refuseEnv(connect bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.envPortal != nil {
		return errors.New("developer-portal cookies come from CIQ_MCP_PORTAL_COOKIES; unset it to sign in with the browser")
	}
	if connect && s.envConnect != nil {
		return errors.New("the Garmin Connect token comes from CIQ_MCP_CONNECT_REFRESH_TOKEN; unset it to sign in with the browser")
	}
	return nil
}

// validLocked reports whether the requested credentials still work: stored
// portal cookies pass Verify (rotations are saved) and, when connect is
// requested, the access token is fresh or the refresh token still refreshes.
// Callers hold the profile lock.
func (s *Store) validLocked(ctx context.Context, connect bool) (auth.LoginResult, bool) {
	b, _, _, err := s.view()
	if err != nil || b.Portal == nil || b.Portal.Cookies == "" || s.portalDead(b.Portal) {
		return auth.LoginResult{}, false
	}
	dev, rotated, err := s.verify(ctx, s.hc, b.Portal.Cookies)
	if err != nil {
		return auth.LoginResult{}, false
	}
	p := *b.Portal
	p.DeveloperID, p.DeveloperName = firstNonEmpty(dev.ID, p.DeveloperID), firstNonEmpty(dev.Name, p.DeveloperName)
	if rotated != "" && rotated != p.Cookies {
		p.Cookies, p.SavedAt = rotated, s.now().Unix()
	}
	if p != *b.Portal {
		if err := s.savePortal(&p); err != nil {
			return auth.LoginResult{}, false
		}
	}
	r := signInResult{Portal: &p}
	r.Expires, _ = portal.SealExpiry(p.Cookies)
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
			if err := s.saveConnect(nc); err != nil {
				return auth.LoginResult{}, false
			}
			c = nc
		}
		r.Connect = c
	}
	out := r.result()
	out.AlreadyValid = true
	return out, true
}

func (r signInResult) result() auth.LoginResult {
	var out auth.LoginResult
	if r.Portal != nil {
		out.DeveloperID, out.DeveloperName, out.PortalExpires = r.Portal.DeveloperID, r.Portal.DeveloperName, r.Expires
	}
	if r.Connect != nil {
		out.Connect, out.ConnectName = true, r.Connect.DisplayName
	}
	return out
}

// loginError turns browser failures into actionable messages.
func loginError(ctx context.Context, err error, timeout time.Duration) error {
	switch {
	case errors.Is(err, browser.ErrNoBrowser):
		return fmt.Errorf("%w: set %s to a Chrome, Edge, Brave or Chromium executable; when none is installed ciq-mcp downloads a pinned Chrome for Testing on first use", err, browser.EnvBrowser)
	case ctx.Err() != nil:
		return fmt.Errorf("sign-in cancelled; nothing was saved: %w", ctx.Err())
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "timed out"):
		if timeout <= 0 {
			timeout = 10 * time.Minute
		}
		return fmt.Errorf("sign-in timed out after %s without finishing in the browser window; nothing was saved", timeout.Round(time.Minute))
	default:
		return fmt.Errorf("sign-in did not finish (%w); nothing was saved", err)
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
