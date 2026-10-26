package mcpserver

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/idoo/garmin-mcp/internal/tool"
)

func testHTTPHandler(t *testing.T) http.Handler {
	t.Helper()
	srv := mcp.NewServer(&mcp.Implementation{Name: "garmin-mcp", Version: "test"}, nil)
	h, err := HTTPHandler(HTTPOptions{
		Token:          testToken,
		AllowedOrigins: []string{"https://app.example"},
		NewServer:      func(*http.Request) *mcp.Server { return srv },
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// testToken is a bearer token long enough for HTTPHandler.
var testToken = strings.Repeat("t", 32)

const initBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`

func doHTTP(t *testing.T, h http.Handler, path, auth, origin string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(initBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHTTPAuthAndOrigin(t *testing.T) {
	h := testHTTPHandler(t)
	tests := []struct {
		name, auth, origin string
		want               int
	}{
		{"no token", "", "", http.StatusUnauthorized},
		{"wrong token", "Bearer nope", "", http.StatusUnauthorized},
		{"basic scheme", "Basic " + testToken, "", http.StatusUnauthorized},
		{"token prefix", "Bearer " + testToken[:len(testToken)-1], "", http.StatusUnauthorized},
		{"bad origin", "Bearer " + testToken, "https://evil.example", http.StatusForbidden},
		{"bad origin no token", "", "http://attacker.test", http.StatusForbidden},
		{"localhost lookalike", "Bearer " + testToken, "http://localhost.evil.test", http.StatusForbidden},
		{"ok", "Bearer " + testToken, "", http.StatusOK},
		{"ok loopback origin", "Bearer " + testToken, "http://127.0.0.1:3000", http.StatusOK},
		{"ok localhost origin", "Bearer " + testToken, "http://localhost:5173", http.StatusOK},
		{"ok configured origin", "Bearer " + testToken, "https://app.example", http.StatusOK},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := doHTTP(t, h, "/mcp", tt.auth, tt.origin)
			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d (body %q)", rec.Code, tt.want, rec.Body.String())
			}
			if tt.want == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") == "" {
				t.Error("missing WWW-Authenticate")
			}
		})
	}
}

func TestHTTPHealthz(t *testing.T) {
	h := testHTTPHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Errorf("healthz = %d %q, want 200 empty", rec.Code, rec.Body.String())
	}
}

func TestCheckListenAddr(t *testing.T) {
	tls := HTTPOptions{TLSCert: "c.pem", TLSKey: "k.pem"}
	remoteTLS := tls
	remoteTLS.AllowRemote = true
	tests := []struct {
		addr string
		opts HTTPOptions
		ok   bool
	}{
		{"127.0.0.1:8765", HTTPOptions{}, true},
		{"[::1]:8765", HTTPOptions{}, true},
		{"localhost:8765", HTTPOptions{}, true},
		{"0.0.0.0:8765", HTTPOptions{}, false},
		{":8765", HTTPOptions{}, false},
		{"192.168.1.5:8765", HTTPOptions{}, false},
		{"0.0.0.0:8765", HTTPOptions{AllowRemote: true}, false},
		{"0.0.0.0:8765", remoteTLS, true},
		{"127.0.0.1:8765", HTTPOptions{TLSCert: "c.pem"}, false},
		{"nonsense", HTTPOptions{}, false},
	}
	for _, tt := range tests {
		err := CheckListenAddr(tt.addr, tt.opts)
		if (err == nil) != tt.ok {
			t.Errorf("CheckListenAddr(%q, %+v) = %v, want ok=%v", tt.addr, tt.opts, err, tt.ok)
		}
	}
}

func TestServeHTTPRefusals(t *testing.T) {
	ctx := context.Background()
	newSrv := func(*http.Request) *mcp.Server { return nil }
	if err := ServeHTTP(ctx, "0.0.0.0:0", HTTPOptions{Token: testToken, NewServer: newSrv}); err == nil {
		t.Error("non-loopback without AllowRemote accepted")
	}
	no := false
	if err := ServeHTTP(ctx, "127.0.0.1:0", HTTPOptions{NewServer: newSrv, StderrIsTTY: &no}); !errors.Is(err, ErrTokenRequired) {
		t.Errorf("no token, no TTY: err = %v, want ErrTokenRequired", err)
	}
	if _, err := HTTPHandler(HTTPOptions{NewServer: newSrv}); !errors.Is(err, ErrTokenRequired) {
		t.Errorf("HTTPHandler without token: err = %v", err)
	}
}

func TestGenerateTokenOnTTY(t *testing.T) {
	yes := true
	var buf strings.Builder
	tok, err := generateToken(HTTPOptions{StderrIsTTY: &yes, Stderr: &buf})
	if err != nil {
		t.Fatal(err)
	}
	if len(tok) != 64 || !strings.Contains(buf.String(), tok) {
		t.Errorf("token %q, stderr %q", tok, buf.String())
	}
}

func TestServeHTTPShutsDownOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		errc <- ServeHTTP(ctx, "127.0.0.1:0", HTTPOptions{Token: testToken, NewServer: func(*http.Request) *mcp.Server { return nil }})
	}()
	cancel()
	if err := <-errc; err != nil {
		t.Errorf("ServeHTTP after cancel = %v", err)
	}
}

func TestSessionFactoryServesInitialize(t *testing.T) {
	h, err := HTTPHandler(HTTPOptions{Token: testToken, NewServer: SessionFactory(Deps{Tools: []tool.Tool{}, Version: "test"})})
	if err != nil {
		t.Fatal(err)
	}
	rec := doHTTP(t, h, "/mcp", "Bearer "+testToken, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"name":"garmin-mcp"`) {
		t.Errorf("initialize = %d %q", rec.Code, rec.Body.String())
	}
}

// countingTransport counts round trips and answers 204.
type countingTransport struct{ calls int }

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.calls++
	return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Request: r}, nil
}

func TestSessionWritesAreSessionScoped(t *testing.T) {
	base := &countingTransport{}
	var shared []bool
	deps := Deps{Profile: tool.ProfileInfo{Name: "p"}}
	deps.Env.HTTP = &http.Client{Transport: base}
	deps.Env.SetWrites = func(on bool) { shared = append(shared, on) }

	do := func(d Deps, method string) error {
		req, _ := http.NewRequestWithContext(context.Background(), method, "https://example.test/things", nil)
		res, err := d.Env.HTTP.Do(req)
		if err == nil {
			_ = res.Body.Close()
		}
		return err
	}
	a, b := sessionDeps(deps), sessionDeps(deps)
	a.Env.SetWrites(true)
	if err := do(a, http.MethodPost); err != nil {
		t.Fatalf("session A after enable_writes: %v", err)
	}
	if err := do(b, http.MethodPost); !errors.Is(err, tool.ErrWritesDisabled) {
		t.Fatalf("session B write err = %v, want ErrWritesDisabled", err)
	}
	if err := do(b, http.MethodGet); err != nil {
		t.Fatalf("session B read: %v", err)
	}
	a.Env.SetWrites(false)
	if err := do(a, http.MethodPost); !errors.Is(err, tool.ErrWritesDisabled) {
		t.Fatalf("session A after disable err = %v", err)
	}
	if base.calls != 2 || len(shared) != 1 || !shared[0] {
		t.Fatalf("base calls = %d, shared SetWrites = %v", base.calls, shared)
	}
}

// authTransport adds the bearer token to every request.
type authTransport struct{ token string }

func (a authTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+a.token)
	return http.DefaultTransport.RoundTrip(r)
}

func TestHTTPBuildsOneServerPerSession(t *testing.T) {
	factory := SessionFactory(Deps{Tools: []tool.Tool{}, Version: "test"})
	var mu sync.Mutex
	seen := map[*mcp.Server]bool{}
	h, err := HTTPHandler(HTTPOptions{Token: testToken, NewServer: func(r *http.Request) *mcp.Server {
		s := factory(r)
		mu.Lock()
		seen[s] = true
		mu.Unlock()
		return s
	}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()
	ctx := context.Background()
	tr := &mcp.StreamableClientTransport{Endpoint: ts.URL, HTTPClient: &http.Client{Transport: authTransport{testToken}}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "1"}, nil).Connect(ctx, tr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	for range 10 {
		if _, err := cs.ListTools(ctx, nil); err != nil {
			t.Fatal(err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	// The probe, the session server, and one throwaway Build for the
	// client's server/discover attempt (rejected: garmin-mcp caps at 2025-11-25).
	// Before the fix, every request built a server (~13 here).
	live := 0
	for s := range seen {
		for range s.Sessions() {
			live++
		}
	}
	if len(seen) > 3 || live != 1 {
		t.Fatalf("distinct servers = %d (want ≤ 3), live sessions = %d (want 1)", len(seen), live)
	}
}

func TestHTTPSessionCapAndIdleTimeout(t *testing.T) {
	h, err := HTTPHandler(HTTPOptions{
		Token: testToken, SessionTimeout: 100 * time.Millisecond, MaxSessions: 1,
		NewServer: SessionFactory(Deps{Tools: []tool.Tool{}, Version: "test"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if rec := doHTTP(t, h, "/mcp", "Bearer "+testToken, ""); rec.Code != http.StatusOK || rec.Header().Get(sessionIDHeader) == "" {
		t.Fatalf("first session = %d", rec.Code)
	}
	if rec := doHTTP(t, h, "/mcp", "Bearer "+testToken, ""); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("second session over cap = %d, want 503", rec.Code)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		rec := doHTTP(t, h, "/mcp", "Bearer "+testToken, "")
		if rec.Code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("idle session never expired: still %d", rec.Code)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestSessionLimitsFromEnv(t *testing.T) {
	t.Setenv(EnvHTTPSessionTimeout, "5m")
	t.Setenv(EnvHTTPMaxSessions, "-1")
	d, n, err := sessionLimits(HTTPOptions{})
	if err != nil || d != 5*time.Minute || n != 0 {
		t.Fatalf("env limits = %v %d %v", d, n, err)
	}
	t.Setenv(EnvHTTPSessionTimeout, "")
	t.Setenv(EnvHTTPMaxSessions, "")
	if d, n, _ := sessionLimits(HTTPOptions{}); d != DefaultSessionTimeout || n != DefaultMaxSessions {
		t.Fatalf("defaults = %v %d", d, n)
	}
	t.Setenv(EnvHTTPSessionTimeout, "soon")
	if _, _, err := sessionLimits(HTTPOptions{}); err == nil {
		t.Fatal("bad duration accepted")
	}
}

// HA-10: a short (guessable) token is refused at startup.
func TestHTTPRejectsShortToken(t *testing.T) {
	newSrv := func(*http.Request) *mcp.Server { return nil }
	if _, err := HTTPHandler(HTTPOptions{Token: "1", NewServer: newSrv}); !errors.Is(err, ErrTokenTooShort) {
		t.Fatalf("token \"1\": err = %v, want ErrTokenTooShort", err)
	}
	short := testToken[:MinTokenLength-1]
	if err := ServeHTTP(context.Background(), "127.0.0.1:0", HTTPOptions{Token: short, NewServer: newSrv}); !errors.Is(err, ErrTokenTooShort) {
		t.Fatalf("31-char token: err = %v, want ErrTokenTooShort", err)
	}
}

// HA-10: repeated wrong tokens from one IP get locked out, even when the
// next guess is right; other IPs are unaffected.
func TestHTTPThrottlesFailedAuth(t *testing.T) {
	h := testHTTPHandler(t)
	for i := range authFreeFailures {
		if rec := doHTTP(t, h, "/mcp", "Bearer guess", ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("miss %d = %d, want 401", i, rec.Code)
		}
	}
	if rec := doHTTP(t, h, "/mcp", "Bearer guess", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("lockout-triggering miss = %d, want 401", rec.Code)
	}
	rec := doHTTP(t, h, "/mcp", "Bearer "+testToken, "")
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("right token during lockout = %d, want 429 with Retry-After", rec.Code)
	}
	other := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(initBody))
	other.RemoteAddr = "198.51.100.7:4000"
	other.Header.Set("Content-Type", "application/json")
	other.Header.Set("Accept", "application/json, text/event-stream")
	other.Header.Set("Authorization", "Bearer "+testToken)
	orec := httptest.NewRecorder()
	h.ServeHTTP(orec, other)
	if orec.Code != http.StatusOK {
		t.Fatalf("other IP = %d, want 200", orec.Code)
	}
}

func TestAuthThrottleBackoffAndReset(t *testing.T) {
	now := time.Unix(0, 0)
	a := newAuthThrottle(func() time.Time { return now })
	for range authFreeFailures {
		a.fail("ip")
	}
	if a.blocked("ip") != 0 {
		t.Fatal("free failures must not lock out")
	}
	a.fail("ip")
	if d := a.blocked("ip"); d != authBaseLockout {
		t.Fatalf("first lockout = %v", d)
	}
	a.fail("ip")
	if d := a.blocked("ip"); d != 2*authBaseLockout {
		t.Fatalf("second lockout = %v, want doubled", d)
	}
	for range 20 {
		a.fail("ip")
	}
	if d := a.blocked("ip"); d != authMaxLockout {
		t.Fatalf("capped lockout = %v", d)
	}
	now = now.Add(authMaxLockout)
	if a.blocked("ip") != 0 {
		t.Fatal("lockout must expire")
	}
	a.ok("ip")
	a.fail("ip")
	if a.blocked("ip") != 0 {
		t.Fatal("success must reset the count")
	}
}

// G8: an open standalone SSE GET stream must not hold shutdown for the 5 s
// timeout; ServeHTTP returns nil promptly on cancel (SIGINT).
func TestServeHTTPShutdownWithOpenSSEStream(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		errc <- ServeHTTP(ctx, addr, HTTPOptions{Token: testToken, NewServer: SessionFactory(Deps{Tools: []tool.Tool{}, Version: "test"})})
	}()
	base := "http://" + addr + "/mcp"
	post := func(body, sid string) *http.Response {
		t.Helper()
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, base, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("Authorization", "Bearer "+testToken)
		if sid != "" {
			req.Header.Set(sessionIDHeader, sid)
		}
		var res *http.Response
		for range 50 { // wait for the listener
			if res, err = http.DefaultClient.Do(req); err == nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
			req.Body = io.NopCloser(strings.NewReader(body))
		}
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	res := post(initBody, "")
	sid := res.Header.Get(sessionIDHeader)
	_, _ = io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	if sid == "" {
		t.Fatalf("initialize = %d, no session id", res.StatusCode)
	}
	res = post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`, sid)
	_ = res.Body.Close()

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, base, nil)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set(sessionIDHeader, sid)
	stream, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stream.Body.Close() }()
	if stream.StatusCode != http.StatusOK {
		t.Fatalf("GET stream = %d", stream.StatusCode)
	}
	start := time.Now()
	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("ServeHTTP = %v, want nil", err)
		}
		if el := time.Since(start); el > 3*time.Second {
			t.Fatalf("shutdown took %v; the SSE stream held it open", el)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ServeHTTP did not return")
	}
}
