// Package auth holds Connect IQ credentials: developer-portal cookies for
// https://apps.garmin.com and the Garmin Connect DI OAuth2 token.
package auth

import (
	"context"
	"errors"
	"io"
	"time"
)

// Store provides credentials to tools. Implementations never log secrets.
type Store interface {
	// PortalCookies returns a valid Cookie header for https://apps.garmin.com, renewing if needed.
	PortalCookies(ctx context.Context) (string, error)
	// SavePortalCookies persists rotated cookies (Set-Cookie applied by the caller).
	SavePortalCookies(ctx context.Context, header string) error
	// ConnectToken returns a valid DI OAuth2 access token, refreshing if needed.
	ConnectToken(ctx context.Context) (string, error)
	// Status reports per-credential state without secrets.
	Status(ctx context.Context) Status
}

// Status is the state of every credential.
type Status struct{ Portal, Connect CredStatus }

// CredStatus is the state of one credential, without secrets.
type CredStatus struct {
	Present   bool
	Source    string
	ExpiresAt time.Time
	Renewable bool
	Note      string
}

// ErrAuthRequired means a credential is missing or can no longer be renewed.
var ErrAuthRequired = errors.New("auth_required")

// LoginOptions configures an interactive sign-in.
type LoginOptions struct {
	// Connect also signs in to Garmin Connect.
	Connect bool
	// BrowserPath overrides browser auto-detection ("" = auto).
	BrowserPath string
	// Timeout bounds the wait for the human (0 = browser default).
	Timeout time.Duration
	// SkipIfValid returns without a browser when the requested credentials
	// still work (portal cookies verify, Connect token valid or refreshable).
	SkipIfValid bool
	// Log receives human-readable progress lines; never secrets.
	Log io.Writer
}

// LoginResult describes a sign-in, without secrets.
type LoginResult struct {
	// AlreadyValid means no browser was opened.
	AlreadyValid  bool
	DeveloperID   string
	DeveloperName string
	// PortalExpires is the developer-portal session expiry (zero if unknown).
	PortalExpires time.Time
	// Connect reports a Garmin Connect sign-in; ConnectName is its display name.
	Connect     bool
	ConnectName string
}

// Loginer is implemented by stores that can sign in through an interactive
// browser window (the code path behind `ciq-mcp auth login`).
type Loginer interface {
	Login(ctx context.Context, o LoginOptions) (LoginResult, error)
}
