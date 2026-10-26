// Package connectapi is a read-only client for the Garmin Connect data API
// (https://connectapi.garmin.com) authenticated with the DI OAuth2 token.
package connectapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/idoo/ciq-mcp/internal/auth"
)

// BaseURL is the Connect API host.
const BaseURL = "https://connectapi.garmin.com"

const (
	// MaxJSON caps a decoded JSON response.
	MaxJSON = 16 << 20
	// MaxDownload caps an activity file download.
	MaxDownload = 128 << 20
)

// NativeHeaders are the Garmin Connect Android app headers sent on every call.
var NativeHeaders = map[string]string{
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

// RateLimitError is HTTP 429; RetryAfter is zero when the server sent none.
type RateLimitError struct {
	Path       string
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("Garmin Connect rate limit on %s; retry after %s", e.Path, e.RetryAfter)
	}
	return "Garmin Connect rate limit on " + e.Path
}

// StatusError is any other non-2xx answer. Response bodies are never kept:
// they may echo tokens.
type StatusError struct {
	Status int
	Path   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("Garmin Connect HTTP %d on GET %s", e.Status, e.Path)
}

// Client performs GET requests. It is safe for concurrent use.
type Client struct {
	http  *http.Client
	creds auth.Store
	base  string

	mu      sync.Mutex
	profile *Profile
}

// Profile is the socialProfile identity.
type Profile struct {
	DisplayName string `json:"displayName"`
	FullName    string `json:"fullName"`
	UserName    string `json:"userName"`
	Location    string `json:"location"`
}

// New returns a client for base (empty means [BaseURL]). hc is copied so
// redirects are never followed with the bearer token.
func New(hc *http.Client, creds auth.Store, base string) *Client {
	if base == "" {
		base = BaseURL
	}
	c := *hc
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{http: &c, creds: creds, base: strings.TrimRight(base, "/")}
}

func (c *Client) token(ctx context.Context) (string, error) {
	if c.creds == nil {
		return "", fmt.Errorf("%w: no Garmin Connect token configured", auth.ErrAuthRequired)
	}
	return c.creds.ConnectToken(ctx)
}

// get performs GET path?q, retrying once on 401 with a freshly fetched token.
// The caller closes the body of a 2xx response.
func (c *Client) get(ctx context.Context, path string, q url.Values) (*http.Response, error) {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	for attempt := 0; ; attempt++ {
		tok, err := c.token(ctx)
		if err != nil {
			return nil, err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		for k, v := range NativeHeaders {
			req.Header.Set(k, v)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		res, err := c.http.Do(req)
		if err != nil {
			return nil, fmt.Errorf("GET %s: %w", path, redactErr(err, tok))
		}
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			return res, nil
		}
		drain(res)
		switch {
		case res.StatusCode == http.StatusUnauthorized && attempt == 0:
			continue
		case res.StatusCode == http.StatusUnauthorized:
			return nil, fmt.Errorf("%w: Garmin Connect rejected the token (HTTP 401)", auth.ErrAuthRequired)
		case res.StatusCode == http.StatusTooManyRequests:
			return nil, &RateLimitError{Path: path, RetryAfter: retryAfter(res.Header.Get("Retry-After"), time.Now())}
		default:
			return nil, &StatusError{Status: res.StatusCode, Path: path}
		}
	}
}

func drain(res *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 64<<10))
	_ = res.Body.Close()
}

// redactErr strips the token from transport errors (defensive: url errors
// carry the URL, never headers).
func redactErr(err error, tok string) error {
	if tok == "" || !strings.Contains(err.Error(), tok) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), tok, "[redacted]"))
}

func retryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil && t.After(now) {
		return t.Sub(now).Round(time.Second)
	}
	return 0
}

// GetJSON decodes GET path?q into out. A 204 leaves out untouched.
func (c *Client) GetJSON(ctx context.Context, path string, q url.Values, out any) error {
	res, err := c.get(ctx, path, q)
	if err != nil {
		return err
	}
	defer drain(res)
	if res.StatusCode == http.StatusNoContent {
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, MaxJSON+1))
	if err != nil {
		return fmt.Errorf("GET %s: read: %w", path, err)
	}
	if len(data) > MaxJSON {
		return fmt.Errorf("GET %s: response larger than %d bytes", path, MaxJSON)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("GET %s: response is not JSON", path)
	}
	return nil
}

// Download returns the body of GET path, capped at [MaxDownload].
func (c *Client) Download(ctx context.Context, path string) ([]byte, error) {
	res, err := c.get(ctx, path, nil)
	if err != nil {
		return nil, err
	}
	defer drain(res)
	data, err := io.ReadAll(io.LimitReader(res.Body, MaxDownload+1))
	if err != nil {
		return nil, fmt.Errorf("GET %s: read: %w", path, err)
	}
	if len(data) > MaxDownload {
		return nil, fmt.Errorf("GET %s: file larger than %d bytes", path, MaxDownload)
	}
	return data, nil
}

// Profile returns the socialProfile, fetched once per Client.
func (c *Client) Profile(ctx context.Context) (Profile, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.profile != nil {
		return *c.profile, nil
	}
	var p Profile
	if err := c.GetJSON(ctx, "/userprofile-service/socialProfile", nil, &p); err != nil {
		return Profile{}, err
	}
	if p.DisplayName == "" {
		return Profile{}, errors.New("Garmin Connect socialProfile has no displayName")
	}
	c.profile = &p
	return p, nil
}

// DisplayName returns the cached profile displayName, path-escaped.
func (c *Client) DisplayName(ctx context.Context) (string, error) {
	p, err := c.Profile(ctx)
	if err != nil {
		return "", err
	}
	return url.PathEscape(p.DisplayName), nil
}
