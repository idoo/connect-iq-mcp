// Package tooltest provides in-memory fakes of every [tool.Env] dependency and
// helpers to call tools directly, so tool slices unit-test handlers without
// the MCP server.
//
//	k := tooltest.New(t)
//	srv := httptest.NewTLSServer(handler)
//	k.UseServer(srv)
//	res := tooltest.Call(t, myTool, k.Env, `{"app_id":"x"}`)
//	tooltest.AssertGolden(t, "testdata/golden/apps_list.json", res)
//
// Run `go test ./... -update` to rewrite golden files.
package tooltest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/idoo/ciq-mcp/internal/auth"
	"github.com/idoo/ciq-mcp/internal/tool"
)

// Kit bundles a ready [tool.Env] with handles to its fakes.
type Kit struct {
	Env      *tool.Env
	Creds    *FakeCredentials
	HTTP     *FakeHTTP
	Plans    *FakePlanner
	State    *FakeStore
	Files    *FakeFiles
	Fetch    *FakeFetcher
	Toolsets *FakeToolsets
	Clock    *FakeClock
}

// New returns a Kit whose Env uses fresh fakes, an HTTP client over
// [FakeHTTP] wrapped by [tool.GuardWrites] and the profile
// {Name:"test", AllowWrites:true}.
func New(t testing.TB) *Kit {
	t.Helper()
	clock := NewFakeClock()
	k := &Kit{
		Creds:    &FakeCredentials{Cookie: "SESSION=test", Token: "test-token"},
		HTTP:     &FakeHTTP{},
		Plans:    NewFakePlanner(clock.Now),
		State:    NewFakeStore(),
		Files:    NewFakeFiles(t),
		Fetch:    NewFakeFetcher(t),
		Toolsets: NewFakeToolsets(),
		Clock:    clock,
	}
	k.Env = &tool.Env{
		Credentials: k.Creds,
		HTTP:        &http.Client{Transport: tool.GuardWrites(k.HTTP)},
		Plans:       k.Plans,
		State:       k.State,
		Toolsets:    k.Toolsets,
		Fetch:       k.Fetch,
		Root:        k.Files,
		Clock:       k.Clock,
		Version:     "test",
		Profile:     tool.ProfileInfo{Name: "test", AllowWrites: true},
	}
	return k
}

// UseServer points the Env's HTTP client at srv (its transport, still behind
// the write guard) and returns k. k.HTTP then sees no requests.
func (k *Kit) UseServer(srv *httptest.Server) *Kit {
	k.Env.HTTP = &http.Client{Transport: tool.GuardWrites(srv.Client().Transport)}
	return k
}

// FakeHTTP is an [http.RoundTripper] that records every request and answers
// with Handler (default: 204 No Content).
type FakeHTTP struct {
	mu       sync.Mutex
	requests []*http.Request
	// Handler, when set, serves each request.
	Handler http.Handler
}

// RoundTrip implements http.RoundTripper.
func (f *FakeHTTP) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	h := f.Handler
	f.mu.Unlock()
	rec := httptest.NewRecorder()
	if h == nil {
		rec.WriteHeader(http.StatusNoContent)
	} else {
		h.ServeHTTP(rec, req)
	}
	res := rec.Result()
	res.Request = req
	return res, nil
}

// Mutations returns the recorded requests other than GET/HEAD/OPTIONS.
func (f *FakeHTTP) Mutations() []*http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []*http.Request
	for _, r := range f.requests {
		switch r.Method {
		case "", http.MethodGet, http.MethodHead, http.MethodOptions:
		default:
			out = append(out, r)
		}
	}
	return out
}

// ReadOnly switches the Kit's profile to read-only mode and returns k.
func (k *Kit) ReadOnly() *Kit {
	k.Env.Profile.AllowWrites = false
	return k
}

// Elicit installs an elicitation handler that answers accept and records
// the questions asked.
func (k *Kit) Elicit(accept bool) *[]string {
	asked := &[]string{}
	k.Env.Elicit = func(_ context.Context, msg string) (bool, error) {
		*asked = append(*asked, msg)
		return accept, nil
	}
	return asked
}

// FakeCredentials is an in-memory [auth.Store]. Empty Cookie or Token makes
// the matching getter fail with [auth.ErrAuthRequired].
type FakeCredentials struct {
	mu     sync.Mutex
	Cookie string
	Token  string
	// Saved records every SavePortalCookies header.
	Saved []string
}

var _ auth.Store = (*FakeCredentials)(nil)

// PortalCookies implements auth.Store.
func (c *FakeCredentials) PortalCookies(context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Cookie == "" {
		return "", auth.ErrAuthRequired
	}
	return c.Cookie, nil
}

// SavePortalCookies implements auth.Store.
func (c *FakeCredentials) SavePortalCookies(_ context.Context, header string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Cookie = header
	c.Saved = append(c.Saved, header)
	return nil
}

// ConnectToken implements auth.Store.
func (c *FakeCredentials) ConnectToken(context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Token == "" {
		return "", auth.ErrAuthRequired
	}
	return c.Token, nil
}

// Status implements auth.Store.
func (c *FakeCredentials) Status(context.Context) auth.Status {
	c.mu.Lock()
	defer c.mu.Unlock()
	return auth.Status{
		Portal:  auth.CredStatus{Present: c.Cookie != "", Source: "fake"},
		Connect: auth.CredStatus{Present: c.Token != "", Source: "fake"},
	}
}
