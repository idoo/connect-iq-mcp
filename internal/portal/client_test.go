package portal

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/idoo/garmin-mcp/internal/auth"
)

type fakeCreds struct{}

func (fakeCreds) PortalCookies(context.Context) (string, error)             { return "session=S", nil }
func (fakeCreds) SavePortalCookies(context.Context, string, []string) error { return nil }
func (fakeCreds) ConnectToken(context.Context) (string, error)              { return "", nil }
func (fakeCreds) Status(context.Context) auth.Status                        { return auth.Status{} }

func fakeServer(t *testing.T, h http.HandlerFunc) (*Client, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	old := baseURL
	baseURL = srv.URL
	t.Cleanup(func() { baseURL = old })
	c := New(srv.Client(), fakeCreds{})
	c.devID = "dev-1"
	return c, &hits
}

func TestBadIDsNeverReachServer(t *testing.T) {
	c, hits := fakeServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	ctx := context.Background()
	const app = "11111111-2222-3333-4444-555555555555"
	for _, id := range []string{"..", ".", "a/b", "", "../../developers/x", "a?b", "a%2Fb"} {
		calls := map[string]error{
			"AppMetadata":       func() error { _, err := c.AppMetadata(ctx, id); return err }(),
			"DeleteApp":         c.DeleteApp(ctx, id),
			"CreateReply":       func() error { _, err := c.CreateReply(ctx, app, id, "hi"); return err }(),
			"DeleteReply":       c.DeleteReply(ctx, app, id),
			"DeleteReplyApp":    c.DeleteReply(ctx, id, "r1"),
			"DeleteScreenshot":  c.DeleteScreenshot(ctx, app, id),
			"ReorderScreenshot": c.ReorderScreenshot(ctx, app, id, 1),
			"Reference":         func() error { _, err := c.Reference(ctx, id); return err }(),
			"FullInstalls":      func() error { _, err := c.FullInstalls(ctx, id, "2026-01-01", "2026-01-31"); return err }(),
		}
		for name, err := range calls {
			if !errors.Is(err, ErrInvalidID) {
				t.Errorf("%s(%q) err = %v, want ErrInvalidID", name, id, err)
			}
		}
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("%d requests reached the server", n)
	}
	// A bad developer id from the server is rejected too.
	c.devID = ".."
	if _, err := c.AppMetadata(ctx, app); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("bad developer id err = %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("%d requests reached the server", n)
	}
}

func TestValidIDsPass(t *testing.T) {
	c, hits := fakeServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	for _, id := range []string{"11111111-2222-3333-4444-555555555555", "123456", "r_1"} {
		if err := c.DeleteScreenshot(context.Background(), id, id); err != nil {
			t.Fatalf("%q: %v", id, err)
		}
	}
	if hits.Load() != 3 {
		t.Fatalf("hits = %d", hits.Load())
	}
}

func TestRateLimited(t *testing.T) {
	for _, tc := range []struct {
		status int
		ra     string
		want   time.Duration
	}{
		{http.StatusTooManyRequests, "7", 7 * time.Second},
		{http.StatusTooManyRequests, "", 0},
		{http.StatusServiceUnavailable, "30", 30 * time.Second},
	} {
		c, hits := fakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
			if tc.ra != "" {
				w.Header().Set("Retry-After", tc.ra)
			}
			w.WriteHeader(tc.status)
		})
		_, err := c.Reviews(context.Background(), "app-1", ReviewsQuery{PageSize: 20})
		var rl *RateLimitError
		if !errors.As(err, &rl) || rl.Status != tc.status || rl.RetryAfter != tc.want {
			t.Fatalf("%d/%q: err = %v", tc.status, tc.ra, err)
		}
		if hits.Load() != 1 {
			t.Fatalf("%d/%q: hits = %d (no retries expected)", tc.status, tc.ra, hits.Load())
		}
	}
	// A plain 503 stays an APIError.
	c, _ := fakeServer(t, func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) })
	var ae *APIError
	if _, err := c.Reviews(context.Background(), "app-1", ReviewsQuery{}); !errors.As(err, &ae) || ae.Status != 503 {
		t.Fatalf("plain 503 err = %v", err)
	}
}

// LF-4: only a 403 on /developers means "not signed in"; a 403 elsewhere is
// an endpoint refusal and a Cloudflare page is a block, never auth_required.
func TestForbiddenClassification(t *testing.T) {
	const app = "11111111-2222-3333-4444-555555555555"
	ctx := context.Background()
	plain := func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }

	c, _ := fakeServer(t, plain)
	c.devID = ""
	if _, err := c.Developer(ctx); !errors.Is(err, auth.ErrAuthRequired) {
		t.Fatalf("403 on /developers err = %v, want auth required", err)
	}

	c, _ = fakeServer(t, plain)
	_, err := c.AppMetadata(ctx, app)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusForbidden || ae.Code != "forbidden" || errors.Is(err, auth.ErrAuthRequired) {
		t.Fatalf("403 on metadata err = %v, want APIError 403 forbidden", err)
	}

	c, _ = fakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cf-Ray", "8abc-AMS")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<html><title>Attention Required! | Cloudflare</title>error code: 1020</html>"))
	})
	c.devID = ""
	if _, err := c.Developer(ctx); !errors.Is(err, ErrBlocked) || errors.Is(err, auth.ErrAuthRequired) {
		t.Fatalf("Cloudflare 1020 on /developers err = %v, want ErrBlocked", err)
	}

	c, _ = fakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Cf-Ray", "8abc-AMS")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte("error code: 1015"))
	})
	var rl *RateLimitError
	if _, err := c.AppMetadata(ctx, app); !errors.As(err, &rl) || !rl.Cloudflare {
		t.Fatalf("Cloudflare 1015 err = %v, want Cloudflare RateLimitError", err)
	}
}

type failingSave struct{ fakeCreds }

func (failingSave) SavePortalCookies(context.Context, string, []string) error {
	return errors.New("keychain timeout")
}

// LF-5: a write that succeeded is not turned into a failure by a cookie save
// error (it becomes a warning); a GET still fails on it.
func TestCookieSaveErrorAfterWrite(t *testing.T) {
	const app = "11111111-2222-3333-4444-555555555555"
	ctx := context.Background()
	c, _ := fakeServer(t, func(w http.ResponseWriter, _ *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "session", Value: "S2"})
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"r1"}`))
	})
	c.creds = failingSave{}
	if _, err := c.CreateReply(ctx, app, "rev-1", "thanks"); err != nil {
		t.Fatalf("write after save error = %v, want success", err)
	}
	if w := c.Warnings(); len(w) != 1 || c.Warnings() != nil {
		t.Fatalf("warnings = %v, want one, then cleared", w)
	}
	if _, err := c.AppMetadata(ctx, app); err == nil {
		t.Fatal("GET with a failed cookie save succeeded")
	}
}
