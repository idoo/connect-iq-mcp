// Package auth holds Connect IQ credentials: developer-portal cookies for
// https://apps.garmin.com and the Garmin Connect DI OAuth2 token.
package auth

import (
	"context"
	"errors"
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
