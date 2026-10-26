package connectapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/idoo/ciq-mcp/internal/auth"
)

type countingCreds struct {
	calls  atomic.Int32
	tokens []string
}

func (c *countingCreds) PortalCookies(context.Context) (string, error) {
	return "", auth.ErrAuthRequired
}
func (c *countingCreds) SavePortalCookies(context.Context, string) error { return nil }
func (c *countingCreds) Status(context.Context) auth.Status              { return auth.Status{} }
func (c *countingCreds) ConnectToken(context.Context) (string, error) {
	n := int(c.calls.Add(1)) - 1
	if n >= len(c.tokens) {
		n = len(c.tokens) - 1
	}
	return c.tokens[n], nil
}

func newTest(t *testing.T, h http.HandlerFunc, tokens ...string) (*Client, *countingCreds) {
	t.Helper()
	srv := httptest.NewTLSServer(h)
	t.Cleanup(srv.Close)
	cr := &countingCreds{tokens: tokens}
	return New(srv.Client(), cr, srv.URL), cr
}

func TestHeaders(t *testing.T) {
	var got http.Header
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		_, _ = w.Write([]byte(`{"ok":true}`))
	}, "tok-1")
	var v map[string]any
	if err := c.GetJSON(context.Background(), "/x", nil, &v); err != nil {
		t.Fatal(err)
	}
	if got.Get("Authorization") != "Bearer tok-1" {
		t.Fatalf("Authorization = %q", got.Get("Authorization"))
	}
	for k, want := range NativeHeaders {
		if got.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, got.Get(k), want)
		}
	}
}

func TestRetryOnceOn401(t *testing.T) {
	var hits atomic.Int32
	c, cr := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer fresh" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}, "stale", "fresh")
	var v any
	if err := c.GetJSON(context.Background(), "/x", nil, &v); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 2 || cr.calls.Load() != 2 {
		t.Fatalf("hits=%d token calls=%d, want 2/2", hits.Load(), cr.calls.Load())
	}

	hits.Store(0)
	c2, _ := newTest(t, func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"access_token":"leak"}`))
	}, "stale")
	err := c2.GetJSON(context.Background(), "/x", nil, &v)
	if !errors.Is(err, auth.ErrAuthRequired) || hits.Load() != 2 {
		t.Fatalf("err=%v hits=%d", err, hits.Load())
	}
	if strings.Contains(err.Error(), "leak") || strings.Contains(err.Error(), "stale") {
		t.Fatalf("error leaks body or token: %v", err)
	}
}

func TestRateLimit(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "42")
		w.WriteHeader(http.StatusTooManyRequests)
	}, "t")
	var v any
	err := c.GetJSON(context.Background(), "/x", nil, &v)
	var rl *RateLimitError
	if !errors.As(err, &rl) || rl.RetryAfter != 42*time.Second {
		t.Fatalf("err = %v", err)
	}
}

func TestStatusErrorHasNoBody(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("Bearer secret-token"))
	}, "t")
	var v any
	err := c.GetJSON(context.Background(), "/x", nil, &v)
	var se *StatusError
	if !errors.As(err, &se) || se.Status != 500 || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err = %v", err)
	}
}

func TestNoRedirects(t *testing.T) {
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/x" {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		t.Errorf("redirect followed to %s", r.URL.Path)
	}, "t")
	var v any
	var se *StatusError
	if err := c.GetJSON(context.Background(), "/x", nil, &v); !errors.As(err, &se) || se.Status != 302 {
		t.Fatalf("err = %v", err)
	}
}

func TestDisplayNameCached(t *testing.T) {
	var hits atomic.Int32
	c, _ := newTest(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/userprofile-service/socialProfile" {
			hits.Add(1)
			_, _ = w.Write([]byte(`{"displayName":"abc def","fullName":"A B"}`))
		}
	}, "t")
	for range 3 {
		n, err := c.DisplayName(context.Background())
		if err != nil || n != "abc%20def" {
			t.Fatalf("DisplayName = %q, %v", n, err)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("socialProfile fetched %d times", hits.Load())
	}
}

func TestRetryAfterDate(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if d := retryAfter(now.Add(30*time.Second).Format(http.TimeFormat), now); d != 30*time.Second {
		t.Fatalf("got %v", d)
	}
	if d := retryAfter("junk", now); d != 0 {
		t.Fatalf("got %v", d)
	}
}
