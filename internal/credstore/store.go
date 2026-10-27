// Package credstore implements auth.Store: the Connect IQ developer-portal session cookie
// and Garmin Connect DI OAuth2 tokens kept in the OS keychain (or a 0600 file),
// cross-process locked refresh, silent renewal through a dedicated browser
// profile, and the `garmin-mcp auth` CLI.
package credstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/idoo/garmin-mcp/internal/auth"
	"github.com/idoo/garmin-mcp/internal/browser"
	"github.com/idoo/garmin-mcp/internal/config"
	"github.com/idoo/garmin-mcp/internal/portal"
	"github.com/idoo/garmin-mcp/internal/state"
)

const (
	srcKeychain = "keychain"
	srcFile     = "file"
	srcEnv      = "env"
)

// blob is the stored JSON credential document. SavedAt (Unix nanoseconds)
// decides between a keychain and a file copy: the newer one wins.
type blob struct {
	Portal  *portalCred  `json:"portal,omitempty"`
	Connect *connectCred `json:"connect,omitempty"`
	SavedAt int64        `json:"saved_at,omitempty"`
}

type portalCred struct {
	Cookies       string `json:"cookies"`
	DeveloperID   string `json:"developer_id,omitempty"`
	DeveloperName string `json:"developer_name,omitempty"`
	SavedAt       int64  `json:"saved_at,omitempty"`
}

type connectCred struct {
	ClientID         string `json:"client_id,omitempty"`
	AccessToken      string `json:"access_token,omitempty"`
	AccessExpiresAt  int64  `json:"access_expires_at,omitempty"`
	RefreshToken     string `json:"refresh_token"`
	RefreshExpiresAt int64  `json:"refresh_expires_at,omitempty"`
	DisplayName      string `json:"display_name,omitempty"`
}

// Store implements auth.Store for one profile.
type Store struct {
	profile   string
	dir       string // garmin-mcp state directory
	autoRenew bool

	hc         *http.Client
	diauthURL  string
	connectAPI string
	now        func() time.Time
	login      func(context.Context, browser.Options, browser.Request) (browser.Result, error)
	hasProfile func(dir string) bool
	verify     func(context.Context, *http.Client, string) (portal.Developer, string, error)
	getenv     func(string) string

	mu sync.Mutex
	// Env overrides are read-only: rotations live here, in memory only.
	envPortal  *portalCred
	envConnect *connectCred
	// saveWarnings collects notes from saves that replaced an unreadable
	// stored blob; takeSaveWarnings drains them.
	saveWarnings []string
	// pendingConnect is a refreshed Connect credential whose save failed. It
	// carries the rotated refresh token, so view() prefers it until a save
	// succeeds (LC-6).
	pendingConnect *connectCred
}

// newStore returns the store for profile. The name becomes a file name and a
// keychain account, so it must pass config.ValidateProfileName.
func newStore(profile, stateDir string, autoRenew bool) (*Store, error) {
	if err := config.ValidateProfileName(profile); err != nil {
		return nil, err
	}
	s := &Store{
		profile:    profile,
		dir:        stateDir,
		autoRenew:  autoRenew,
		hc:         newHTTPClient(),
		diauthURL:  defaultDIAuthURL,
		connectAPI: defaultConnectAPI,
		now:        time.Now,
		login:      browser.Login,
		hasProfile: browser.ProfileExists,
		verify:     portal.Verify,
		getenv:     os.Getenv,
	}
	s.loadEnv()
	return s, nil
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout:       60 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// loadEnv reads the environment overrides into memory.
func (s *Store) loadEnv() {
	if v := s.getenv("GARMIN_MCP_PORTAL_COOKIES"); v != "" {
		s.envPortal = &portalCred{Cookies: portalSession(v)}
	}
	if v := s.getenv("GARMIN_MCP_CONNECT_REFRESH_TOKEN"); v != "" {
		s.envConnect = &connectCred{RefreshToken: v, ClientID: s.getenv("GARMIN_MCP_CONNECT_CLIENT_ID")}
	}
}

func (s *Store) credFile() string {
	return filepath.Join(s.dir, "credentials", s.profile+".json")
}
func (s *Store) lockFile() string { return filepath.Join(s.dir, "locks", s.profile+".lock") }

// loginLockFile serializes browser sign-ins on the profile's browser
// directory; the profile lock (lockFile) only guards read-modify-writes.
func (s *Store) loginLockFile() string { return filepath.Join(s.dir, "locks", s.profile+".login") }

// stampFile is the renewal stamp of one credential kind (stampPortal or
// stampConnect).
func (s *Store) stampFile(kind string) string {
	return filepath.Join(s.dir, "locks", s.profile+".renew-"+kind)
}

func (s *Store) browserDir() string {
	return filepath.Join(s.dir, "browser", s.profile)
}

// keyringOff reports why configuration disables the keychain ("" = use it).
func (s *Store) keyringOff() string {
	if strings.EqualFold(strings.TrimSpace(s.getenv("GARMIN_MCP_KEYRING")), "off") {
		return "GARMIN_MCP_KEYRING=off"
	}
	return ""
}

// readStored returns the persisted blob and where it came from: "" if none,
// "keychain", or "file (reason)". When both the keychain and the file hold a
// copy, the newer saved_at wins. Only an unavailable keychain falls through to
// the file; a locked keychain (errKeyringLocked) or a damaged blob
// (errStoredUnreadable) is returned as an error.
func (s *Store) readStored() (blob, string, error) {
	var kb blob
	inKeychain := false
	reason := s.keyringOff()
	if reason == "" {
		data, err := keychainRead(s.profile)
		switch {
		case errors.Is(err, errKeyringUnavailable):
			reason = err.Error()
		case err != nil:
			return blob{}, "", fmt.Errorf("read keychain: %w", err)
		case data != "":
			if err := json.Unmarshal([]byte(data), &kb); err != nil {
				return blob{}, "", fmt.Errorf("%w: keychain copy: %w", errStoredUnreadable, err)
			}
			inKeychain = true
		}
	}
	fb, inFile, err := s.readFile()
	if err != nil {
		return blob{}, "", err
	}
	switch {
	case inKeychain && (!inFile || kb.SavedAt >= fb.SavedAt):
		return kb, srcKeychain, nil
	case inFile:
		switch {
		case reason != "":
		case inKeychain:
			reason = "newer than the keychain copy"
		default:
			reason = "moves to the keychain on next save"
		}
		return fb, srcFile + " (" + reason + ")", nil
	}
	return blob{}, "", nil
}

// readFile reads the fallback credentials file (DPAPI-protected on Windows).
func (s *Store) readFile() (blob, bool, error) {
	raw, err := os.ReadFile(s.credFile())
	if errors.Is(err, os.ErrNotExist) {
		return blob{}, false, nil
	}
	if err != nil {
		return blob{}, false, fmt.Errorf("read credentials file: %w", err)
	}
	if raw, err = unprotectFile(raw); err != nil {
		return blob{}, false, fmt.Errorf("%w: credentials file %s: %w", errStoredUnreadable, s.credFile(), err)
	}
	var b blob
	if err := json.Unmarshal(raw, &b); err != nil {
		return blob{}, false, fmt.Errorf("%w: credentials file %s: %w", errStoredUnreadable, s.credFile(), err)
	}
	return b, true, nil
}

// readForSave is readStored for a read-modify-write: a damaged stored blob
// is replaced (with a warning) rather than blocking every future save.
func (s *Store) readForSave() (blob, error) {
	stored, _, err := s.readStored()
	if errors.Is(err, errStoredUnreadable) {
		s.mu.Lock()
		s.saveWarnings = append(s.saveWarnings, "replaced unreadable stored credentials ("+err.Error()+"); anything else they held must be signed in again")
		s.mu.Unlock()
		return blob{}, nil
	}
	return stored, err
}

// takeSaveWarnings returns and clears the warnings of earlier saves.
func (s *Store) takeSaveWarnings() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	w := s.saveWarnings
	s.saveWarnings = nil
	return w
}

// Status note prefixes for credentials that exist but could not be read.
const (
	noteLocked     = "locked: "
	noteUnreadable = "unreadable: "
)

// statusNote is the Status note for a failed credential read.
func statusNote(err error) string {
	if errors.Is(err, errKeyringLocked) {
		return noteLocked + err.Error()
	}
	return noteUnreadable + err.Error()
}

// UnreadableState returns "locked" or "unreadable" when c reports stored
// credentials that could not be read, else "". `auth status` and doctor show
// it as the state and fail.
func UnreadableState(c auth.CredStatus) string {
	switch {
	case strings.HasPrefix(c.Note, noteLocked):
		return "locked"
	case strings.HasPrefix(c.Note, noteUnreadable):
		return "unreadable"
	}
	return ""
}

// writeStored persists b in the keychain (in chunks). Only an unavailable
// keychain (no backend, no D-Bus session, GARMIN_MCP_KEYRING=off) falls back
// to the 0600 file; a locked keychain or any other error is returned.
func (s *Store) writeStored(b blob) error {
	b.SavedAt = s.now().UnixNano()
	data, err := json.Marshal(b)
	if err != nil {
		return err
	}
	if s.keyringOff() == "" {
		err := keychainWrite(s.profile, string(data))
		if err == nil {
			if err := os.Remove(s.credFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove stale credentials file: %w", err)
			}
			return nil
		}
		if !errors.Is(err, errKeyringUnavailable) {
			return fmt.Errorf("save credentials to keychain: %w", err)
		}
	}
	enc, err := protectFile(data)
	if err != nil {
		return fmt.Errorf("save credentials: %w", err)
	}
	if err := writeFile0600(s.credFile(), enc); err != nil {
		return fmt.Errorf("save credentials: %w", err)
	}
	return nil
}

// view merges stored credentials with env overrides and reports each source.
func (s *Store) view() (b blob, portalSrc, connectSrc string, err error) {
	stored, src, err := s.readStored()
	if err != nil {
		return blob{}, "", "", err
	}
	b = withSessionOnly(stored)
	if b.Portal != nil {
		portalSrc = src
	}
	if b.Connect != nil {
		connectSrc = src
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.envPortal != nil {
		p := *s.envPortal
		b.Portal, portalSrc = &p, srcEnv
	}
	if s.envConnect != nil {
		c := *s.envConnect
		b.Connect, connectSrc = &c, srcEnv
	} else if s.pendingConnect != nil && b.Connect != nil {
		c := *s.pendingConnect // unsaved rotation; consent still stored
		b.Connect = &c
	}
	return b, portalSrc, connectSrc, nil
}

// savePortal stores p; env-sourced credentials are updated in memory only.
// Callers hold the profile lock.
func (s *Store) savePortal(p *portalCred) error {
	s.mu.Lock()
	if s.envPortal != nil {
		cp := *p
		s.envPortal = &cp
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	stored, err := s.readForSave()
	if err != nil {
		return err
	}
	stored.Portal = p
	return s.writeStored(stored)
}

// portalSession is header cut to its `session` cookie, or "" without one.
func portalSession(header string) string {
	h, err := portal.SessionOnly(header)
	if err != nil {
		return ""
	}
	return h
}

// withSessionOnly returns b with its portal header cut to the `session`
// cookie. Older versions stored the whole apps.garmin.com cookie jar; it is
// trimmed on read and persisted trimmed by the next save.
func withSessionOnly(b blob) blob {
	if b.Portal != nil {
		p := *b.Portal
		p.Cookies = portalSession(p.Cookies)
		b.Portal = &p
	}
	return b
}

// saveConnect stores c; env-sourced tokens are updated in memory only.
// Callers hold the profile lock.
func (s *Store) saveConnect(c *connectCred) error {
	s.mu.Lock()
	if s.envConnect != nil {
		cp := *c
		s.envConnect = &cp
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	stored, err := s.readForSave()
	if err != nil {
		return err
	}
	stored = withSessionOnly(stored)
	stored.Connect = c
	if err := s.writeStored(stored); err != nil {
		return err
	}
	s.mu.Lock()
	s.pendingConnect = nil
	s.mu.Unlock()
	return nil
}

// logout deletes everything under the profile lock, so a concurrent renewal
// cannot write the session back afterwards. An interactive sign-in in
// progress loses its window (deleteAll closes the browser) and saves nothing.
func (s *Store) logout(ctx context.Context) error {
	unlock, err := lockPath(ctx, s.lockFile())
	if err != nil {
		return fmt.Errorf("lock credentials: %w", err)
	}
	defer unlock()
	s.mu.Lock()
	s.pendingConnect = nil
	s.mu.Unlock()
	return s.deleteAll()
}

// deleteAll removes the keychain items, the credentials file, the renewal
// stamp and the browser profile (after closing any browser still using it).
// The keychain is cleared even with GARMIN_MCP_KEYRING=off, so an old copy
// cannot come back once the variable is unset.
func (s *Store) deleteAll() error {
	var errs []error
	if err := keychainDelete(s.profile); err != nil && !errors.Is(err, errKeyringUnavailable) {
		errs = append(errs, fmt.Errorf("keychain: %w", err))
	}
	stamps := []string{s.stampFile(stampPortal), s.stampFile(stampConnect), filepath.Join(s.dir, "locks", s.profile+".renew")}
	for _, p := range append([]string{s.credFile()}, stamps...) {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if err := browser.RemoveProfile(s.browserDir()); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// writeFile0600 atomically writes data with mode 0600 under a 0700 directory.
func writeFile0600(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	if _, err = f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return state.Rename(tmp, path) // retries while Windows reports the target busy
}
