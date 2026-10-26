// Package credstore implements auth.Store: Connect IQ developer-portal cookies
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

	"github.com/zalando/go-keyring"

	"github.com/idoo/garmin-mcp/internal/browser"
	"github.com/idoo/garmin-mcp/internal/portal"
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
}

func newStore(profile, stateDir string, autoRenew bool) *Store {
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
	return s
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
		s.envPortal = &portalCred{Cookies: v}
	}
	if v := s.getenv("GARMIN_MCP_CONNECT_REFRESH_TOKEN"); v != "" {
		s.envConnect = &connectCred{RefreshToken: v, ClientID: s.getenv("GARMIN_MCP_CONNECT_CLIENT_ID")}
	}
}

func (s *Store) credFile() string {
	return filepath.Join(s.dir, "credentials", s.profile+".json")
}
func (s *Store) lockFile() string  { return filepath.Join(s.dir, "locks", s.profile+".lock") }
func (s *Store) stampFile() string { return filepath.Join(s.dir, "locks", s.profile+".renew") }
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
// copy, the newer saved_at wins. A keychain error other than "unavailable"
// is returned rather than silently reading the file.
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
				return blob{}, "", fmt.Errorf("credentials in keychain are unreadable: %w", err)
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
		return blob{}, false, fmt.Errorf("credentials file %s: %w", s.credFile(), err)
	}
	var b blob
	if err := json.Unmarshal(raw, &b); err != nil {
		return blob{}, false, fmt.Errorf("credentials file %s is unreadable: %w", s.credFile(), err)
	}
	return b, true, nil
}

// writeStored persists b in the keychain (in chunks). Only an unavailable
// keychain (no backend, no D-Bus, timeout, GARMIN_MCP_KEYRING=off) falls back
// to the 0600 file; any other keychain error is returned.
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
	b = stored
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
	stored, _, err := s.readStored()
	if err != nil {
		return err
	}
	stored.Portal = p
	return s.writeStored(stored)
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
	stored, _, err := s.readStored()
	if err != nil {
		return err
	}
	stored.Connect = c
	return s.writeStored(stored)
}

// logout deletes everything under the profile lock, so a concurrent login or
// renewal cannot write the session back afterwards.
func (s *Store) logout(ctx context.Context) error {
	unlock, err := lockPath(ctx, s.lockFile())
	if err != nil {
		return fmt.Errorf("lock credentials: %w", err)
	}
	defer unlock()
	return s.deleteAll()
}

// deleteAll removes the keychain items, the credentials file, the renewal
// stamp and the browser profile (after closing any browser still using it).
func (s *Store) deleteAll() error {
	var errs []error
	if s.keyringOff() == "" {
		if err := keychainDelete(s.profile); err != nil && !errors.Is(err, keyring.ErrUnsupportedPlatform) {
			errs = append(errs, fmt.Errorf("keychain: %w", err))
		}
	}
	for _, p := range []string{s.credFile(), s.stampFile()} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	browser.CloseProfile(s.browserDir())
	if err := os.RemoveAll(s.browserDir()); err != nil {
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
	return os.Rename(tmp, path)
}
