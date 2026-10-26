// Package credstore implements auth.Store: Connect IQ developer-portal cookies
// and Garmin Connect DI OAuth2 tokens kept in the OS keychain (or a 0600 file),
// cross-process locked refresh, silent renewal through a dedicated browser
// profile, and the `ciq-mcp auth` CLI.
package credstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/zalando/go-keyring"

	"github.com/idoo/ciq-mcp/internal/browser"
	"github.com/idoo/ciq-mcp/internal/portal"
)

const (
	keyringService = "ciq-mcp"
	// Windows Credential Manager caps blobs at 2560 bytes; elsewhere the
	// keyring reports its own limit through Set errors.
	windowsKeyringMax = 2500

	srcKeychain = "keychain"
	srcFile     = "file"
	srcEnv      = "env"
)

// blob is the stored JSON credential document.
type blob struct {
	Portal  *portalCred  `json:"portal,omitempty"`
	Connect *connectCred `json:"connect,omitempty"`
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
	dir       string // ciq-mcp state directory
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
	if v := s.getenv("CIQ_MCP_PORTAL_COOKIES"); v != "" {
		s.envPortal = &portalCred{Cookies: v}
	}
	if v := s.getenv("CIQ_MCP_CONNECT_REFRESH_TOKEN"); v != "" {
		s.envConnect = &connectCred{RefreshToken: v, ClientID: s.getenv("CIQ_MCP_CONNECT_CLIENT_ID")}
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

// readStored returns the persisted blob and where it came from ("" if none).
func (s *Store) readStored() (blob, string, error) {
	var b blob
	data, kerr := keyring.Get(keyringService, s.profile)
	if kerr == nil {
		if err := json.Unmarshal([]byte(data), &b); err != nil {
			return blob{}, "", fmt.Errorf("credentials in keychain are unreadable: %w", err)
		}
		return b, srcKeychain, nil
	}
	raw, err := os.ReadFile(s.credFile())
	if errors.Is(err, os.ErrNotExist) {
		return blob{}, "", nil
	}
	if err != nil {
		return blob{}, "", fmt.Errorf("read credentials file: %w", err)
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return blob{}, "", fmt.Errorf("credentials file %s is unreadable: %w", s.credFile(), err)
	}
	return b, srcFile, nil
}

// writeStored persists b in the keychain, falling back to a 0600 file when the
// keychain fails or the blob is too large. It returns the backend used.
func (s *Store) writeStored(b blob) (string, error) {
	data, err := json.Marshal(b)
	if err != nil {
		return "", err
	}
	if runtime.GOOS != "windows" || len(data) <= windowsKeyringMax {
		if err := keyring.Set(keyringService, s.profile, string(data)); err == nil {
			if err := os.Remove(s.credFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
				return srcKeychain, fmt.Errorf("remove stale credentials file: %w", err)
			}
			return srcKeychain, nil
		}
	}
	if err := writeFile0600(s.credFile(), data); err != nil {
		return "", fmt.Errorf("save credentials: %w", err)
	}
	_ = keyring.Delete(keyringService, s.profile) // drop a stale keychain copy
	return srcFile, nil
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
	_, err = s.writeStored(stored)
	return err
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
	_, err = s.writeStored(stored)
	return err
}

// deleteAll removes the keychain item, the credentials file, the renewal
// stamp and the browser profile.
func (s *Store) deleteAll() error {
	var errs []error
	if err := keyring.Delete(keyringService, s.profile); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		errs = append(errs, fmt.Errorf("keychain: %w", err))
	}
	for _, p := range []string{s.credFile(), s.stampFile()} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
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
