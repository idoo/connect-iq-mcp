package mcpserver

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/idoo/connect-iq-mcp/internal/tool"
)

// EnvHTTPToken is the environment variable holding the HTTP bearer token.
const EnvHTTPToken = "CIQ_MCP_HTTP_TOKEN" //nolint:gosec // variable name, not a credential.

// Session limits for HTTP mode, overridable by HTTPOptions or env.
const (
	EnvHTTPSessionTimeout = "CIQ_MCP_HTTP_SESSION_TIMEOUT"
	EnvHTTPMaxSessions    = "CIQ_MCP_HTTP_MAX_SESSIONS"
	// DefaultSessionTimeout closes idle sessions; it outlives tool.PreviewTTL
	// so a pending preview never loses its session first.
	DefaultSessionTimeout = 30 * time.Minute
	// DefaultMaxSessions caps concurrently open sessions.
	DefaultMaxSessions = 64
)

// sessionIDHeader is the Streamable HTTP session header.
const sessionIDHeader = "Mcp-Session-Id"

// ErrTokenRequired means HTTP mode has no token and cannot print a generated
// one because stderr is not a terminal.
var ErrTokenRequired = errors.New("mcpserver: http: set " + EnvHTTPToken + " (a token is generated only when stderr is a terminal)")

// MinTokenLength is the shortest accepted bearer token: 32 characters (128
// bits as hex). Shorter tokens fail at startup.
const MinTokenLength = 32

// ErrTokenTooShort means the configured bearer token is shorter than
// [MinTokenLength].
var ErrTokenTooShort = fmt.Errorf("mcpserver: http: %s must be at least %d characters (try `openssl rand -hex 32`)", EnvHTTPToken, MinTokenLength)

// HTTPOptions configures [ServeHTTP].
type HTTPOptions struct {
	// Token is the required bearer token; empty generates one (TTY only).
	Token string
	// AllowRemote permits binding a non-loopback address; it requires TLS.
	AllowRemote bool
	// TLSCert and TLSKey are PEM file paths; both or neither.
	TLSCert, TLSKey string
	// AllowedOrigins lists extra exact Origin values (scheme://host[:port])
	// besides loopback origins.
	AllowedOrigins []string
	// NewServer builds one MCP server per client session.
	NewServer func(*http.Request) *mcp.Server
	// Logger receives transport logs; nil discards.
	Logger *slog.Logger
	// Stderr receives the generated token; nil means os.Stderr.
	Stderr io.Writer
	// StderrIsTTY overrides terminal detection of os.Stderr (tests).
	StderrIsTTY *bool
	// SessionTimeout closes sessions idle this long; 0 reads
	// CIQ_MCP_HTTP_SESSION_TIMEOUT (a Go duration), else DefaultSessionTimeout.
	// Negative never expires sessions.
	SessionTimeout time.Duration
	// MaxSessions caps open sessions (new ones get 503); 0 reads
	// CIQ_MCP_HTTP_MAX_SESSIONS, else DefaultMaxSessions. Negative is unlimited.
	MaxSessions int
}

// ServeHTTP serves MCP over Streamable HTTP on addr until ctx ends. It binds
// loopback only unless AllowRemote (with TLS), requires a bearer token,
// rejects non-loopback Origins, and exposes an unauthenticated /healthz.
func ServeHTTP(ctx context.Context, addr string, opts HTTPOptions) error {
	if err := CheckListenAddr(addr, opts); err != nil {
		return err
	}
	if opts.Token == "" {
		tok, err := generateToken(opts)
		if err != nil {
			return err
		}
		opts.Token = tok
	}
	h, err := HTTPHandler(opts)
	if err != nil {
		return err
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("mcpserver: http: listen %s: %w", addr, err)
	}
	// BaseContext ties every request (including open SSE GET streams) to
	// ctx, so streams end on shutdown instead of holding Shutdown open.
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	errc := make(chan error, 1)
	go func() {
		if opts.TLSCert != "" {
			errc <- srv.ServeTLS(ln, opts.TLSCert, opts.TLSKey)
		} else {
			errc <- srv.Serve(ln)
		}
	}()
	select {
	case err := <-errc:
		return fmt.Errorf("mcpserver: http: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			// A handler ignored cancellation: force-close what is left.
			// Exiting is still the requested, clean outcome.
			_ = srv.Close()
		}
		return nil
	}
}

// CheckListenAddr enforces the bind policy: loopback hosts only, unless
// AllowRemote is set together with a TLS certificate and key.
func CheckListenAddr(addr string, opts HTTPOptions) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("mcpserver: http: address %q: %w", addr, err)
	}
	if (opts.TLSCert == "") != (opts.TLSKey == "") {
		return errors.New("mcpserver: http: --tls-cert and --tls-key must be given together")
	}
	if isLoopbackHost(host) {
		return nil
	}
	if !opts.AllowRemote {
		return fmt.Errorf("mcpserver: http: refusing non-loopback address %q without --allow-remote", addr)
	}
	if opts.TLSCert == "" {
		return errors.New("mcpserver: http: --allow-remote requires --tls-cert and --tls-key")
	}
	return nil
}

// HTTPHandler returns the authenticated MCP handler. opts.Token must be set.
func HTTPHandler(opts HTTPOptions) (http.Handler, error) {
	if opts.Token == "" {
		return nil, ErrTokenRequired
	}
	if len(opts.Token) < MinTokenLength {
		return nil, ErrTokenTooShort
	}
	if opts.NewServer == nil {
		return nil, errors.New("mcpserver: http: NewServer is required")
	}
	want := sha256.Sum256([]byte(opts.Token))
	timeout, maxSessions, err := sessionLimits(opts)
	if err != nil {
		return nil, err
	}
	mcpHandler := mcp.NewStreamableHTTPHandler(opts.NewServer, &mcp.StreamableHTTPOptions{Logger: opts.Logger, SessionTimeout: timeout})
	limit := &sessionLimiter{max: maxSessions}
	throttle := newAuthThrottle(time.Now)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !originAllowed(r.Header.Get("Origin"), opts.AllowedOrigins) {
			http.Error(w, "forbidden origin", http.StatusForbidden)
			return
		}
		// The token is compared first, so a correct token is never refused by
		// the throttle. Only requests carrying a wrong Authorization header
		// count as failures (a page can make the browser send header-less
		// requests at will), and loopback peers are never throttled: every
		// local process shares 127.0.0.1, and a 32+ character token cannot
		// be guessed online anyway.
		authz := r.Header.Get("Authorization")
		got, ok := strings.CutPrefix(authz, "Bearer ")
		sum := sha256.Sum256([]byte(got))
		if !ok || subtle.ConstantTimeCompare(sum[:], want[:]) != 1 {
			if key := throttleKey(r); key != "" && authz != "" {
				if wait := throttle.blocked(key); wait > 0 {
					w.Header().Set("Retry-After", strconv.Itoa(int(wait.Round(time.Second)/time.Second)+1))
					http.Error(w, "too many failed authentication attempts", http.StatusTooManyRequests)
					return
				}
				throttle.fail(key)
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="connect-iq-mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if key := throttleKey(r); key != "" {
			throttle.ok(key)
		}
		if r.Header.Get(sessionIDHeader) != "" {
			mcpHandler.ServeHTTP(w, r)
			return
		}
		if !limit.acquire() {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "too many open sessions; close one or wait for idle ones to expire", http.StatusServiceUnavailable)
			return
		}
		slot := &buildSlot{}
		mcpHandler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), buildSlotKey{}, slot)))
		limit.release(slot.srv)
	}))
	return mux, nil
}

// Failed-auth backoff: authFreeFailures misses per client key are free, then
// each further miss locks that key out for authBaseLockout doubling up to
// authMaxLockout. A success or authForget of quiet resets the key. At most
// authMaxTracked keys are kept; the least recently failed one is evicted.
const (
	authFreeFailures = 5
	authBaseLockout  = time.Second
	authMaxLockout   = time.Minute
	authForget       = 10 * time.Minute
	authMaxTracked   = 4096
)

type authFailures struct {
	n     int
	last  time.Time
	until time.Time
}

// authThrottle slows bearer-token guessing per client key (see throttleKey).
type authThrottle struct {
	now func() time.Time
	mu  sync.Mutex
	ips map[string]*authFailures
}

func newAuthThrottle(now func() time.Time) *authThrottle {
	return &authThrottle{now: now, ips: map[string]*authFailures{}}
}

// blocked returns how long ip must still wait, or 0.
func (a *authThrottle) blocked(ip string) time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	f := a.ips[ip]
	if f == nil {
		return 0
	}
	return max(f.until.Sub(a.now()), 0)
}

func (a *authThrottle) fail(ip string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.now()
	f := a.ips[ip]
	if f == nil && len(a.ips) >= authMaxTracked {
		a.evictLocked(now)
	}
	if f == nil || now.Sub(f.last) > authForget {
		f = &authFailures{}
		a.ips[ip] = f
	}
	f.n++
	f.last = now
	if extra := f.n - authFreeFailures; extra > 0 {
		d := authBaseLockout << min(extra-1, 6)
		f.until = now.Add(min(d, authMaxLockout))
	}
}

// evictLocked makes room for one new key: entries quiet for authForget go
// first; if none, the least recently failed entry is dropped, so the map
// never exceeds authMaxTracked.
func (a *authThrottle) evictLocked(now time.Time) {
	oldest, oldestAt := "", time.Time{}
	for k, f := range a.ips {
		if now.Sub(f.last) > authForget {
			delete(a.ips, k)
			continue
		}
		if oldest == "" || f.last.Before(oldestAt) {
			oldest, oldestAt = k, f.last
		}
	}
	if len(a.ips) >= authMaxTracked {
		delete(a.ips, oldest)
	}
}

func (a *authThrottle) ok(ip string) {
	a.mu.Lock()
	delete(a.ips, ip)
	a.mu.Unlock()
}

// throttleKey is the auth-throttle key of r's peer: "" for loopback (never
// throttled), the address for IPv4, and the /64 prefix for IPv6 (one host
// owns a whole /64 and can rotate addresses inside it).
func throttleKey(r *http.Request) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	switch {
	case ip == nil:
		return host
	case ip.IsLoopback():
		return ""
	case ip.To4() != nil:
		return ip.To4().String()
	default:
		return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
	}
}

// sessionLimits resolves the idle timeout (0 = never, for the SDK) and the
// session cap (0 = unlimited) from opts, then env, then defaults.
func sessionLimits(opts HTTPOptions) (time.Duration, int, error) {
	timeout, maxSessions := opts.SessionTimeout, opts.MaxSessions
	if timeout == 0 {
		timeout = DefaultSessionTimeout
		if v := os.Getenv(EnvHTTPSessionTimeout); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d == 0 {
				return 0, 0, fmt.Errorf("mcpserver: http: %s=%q: want a duration such as 30m (negative never expires)", EnvHTTPSessionTimeout, v)
			}
			timeout = d
		}
	}
	if maxSessions == 0 {
		maxSessions = DefaultMaxSessions
		if v := os.Getenv(EnvHTTPMaxSessions); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n == 0 {
				return 0, 0, fmt.Errorf("mcpserver: http: %s=%q: want a session count (negative is unlimited)", EnvHTTPMaxSessions, v)
			}
			maxSessions = n
		}
	}
	return max(timeout, 0), max(maxSessions, 0), nil
}

// buildSlotKey carries a *buildSlot through one session-less HTTP request.
type buildSlotKey struct{}

// buildSlot memoizes the server built for one new-session request: the SDK
// asks for a server twice on it (protocol check, then session creation).
type buildSlot struct{ srv *mcp.Server }

// sessionLimiter counts open sessions: each new-session request reserves a
// place, and afterwards its server counts while it still has a session (the
// SDK drops sessions on DELETE and idle timeout).
type sessionLimiter struct {
	max     int // 0 = unlimited
	mu      sync.Mutex
	pending int
	servers []*mcp.Server
}

func (l *sessionLimiter) acquire() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.servers = slices.DeleteFunc(l.servers, func(s *mcp.Server) bool {
		for range s.Sessions() {
			return false
		}
		return true
	})
	if l.max > 0 && len(l.servers)+l.pending >= l.max {
		return false
	}
	l.pending++
	return true
}

func (l *sessionLimiter) release(srv *mcp.Server) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pending--
	if srv != nil {
		l.servers = append(l.servers, srv)
	}
}

func originAllowed(origin string, extra []string) bool {
	if origin == "" {
		return true
	}
	if slices.Contains(extra, origin) {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	return isLoopbackHost(u.Hostname())
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// SessionFactory returns an [HTTPOptions.NewServer] that builds one server
// per HTTP session from deps, so toolset and write-mode state stay per
// session. Build failures are logged and answered with 400 by the SDK.
//
// The SDK also asks for a server on every request only to read its protocol
// versions; requests of an existing session get a tool-less probe with the
// same versions, and a new session's two lookups share one Build.
//
// All sessions share deps.Env.HTTP (one connection pool and credential
// store per profile), so each session wraps it in its own write guard:
// enable_writes in one session opens only that session's guard.
func SessionFactory(deps Deps) func(*http.Request) *mcp.Server {
	logger := deps.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	probe := mcp.NewServer(&mcp.Implementation{Name: "connect-iq-mcp", Version: deps.Version}, &mcp.ServerOptions{
		Logger:                    logger,
		SupportedProtocolVersions: legacyProtocolVersions(),
	})
	return func(req *http.Request) *mcp.Server {
		if req.Header.Get(sessionIDHeader) != "" {
			return probe
		}
		slot, _ := req.Context().Value(buildSlotKey{}).(*buildSlot)
		if slot != nil && slot.srv != nil {
			return slot.srv
		}
		srv, err := Build(req.Context(), sessionDeps(deps))
		if err != nil {
			logger.Error("build session server", "err", err)
			return nil
		}
		if slot != nil {
			slot.srv = srv
		}
		return srv
	}
}

// sessionDeps returns deps for one HTTP session: its HTTP client is wrapped
// in a session write guard that starts at the profile's mode, and SetWrites
// flips only that guard (calling the shared SetWrites when enabling).
func sessionDeps(deps Deps) Deps {
	g := &sessionGuard{base: http.DefaultTransport}
	c := &http.Client{}
	if deps.Env.HTTP != nil {
		*c = *deps.Env.HTTP
		if c.Transport != nil {
			g.base = c.Transport
		}
	}
	g.writes.Store(deps.Profile.AllowWrites)
	c.Transport = g
	deps.Env.HTTP = c
	shared := deps.Env.SetWrites
	deps.Env.SetWrites = func(on bool) {
		g.writes.Store(on)
		if on && shared != nil {
			shared(true) // other sessions stay closed by their own guards
		}
	}
	return deps
}

// sessionGuard is one HTTP session's view of the shared transport: requests
// other than GET/HEAD/OPTIONS fail with tool.ErrWritesDisabled unless this
// session enabled writes. The per-call [tool.GuardWrites] still applies below.
type sessionGuard struct {
	base   http.RoundTripper
	writes atomic.Bool
}

// RoundTrip implements http.RoundTripper.
func (s *sessionGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	switch strings.ToUpper(req.Method) {
	case "", http.MethodGet, http.MethodHead, http.MethodOptions:
	default:
		if !s.writes.Load() {
			return nil, fmt.Errorf("session: %s %s: %w", req.Method, req.URL.Redacted(), tool.ErrWritesDisabled)
		}
	}
	return s.base.RoundTrip(req)
}

func generateToken(opts HTTPOptions) (string, error) {
	tty := false
	if opts.StderrIsTTY != nil {
		tty = *opts.StderrIsTTY
	} else if fi, err := os.Stderr.Stat(); err == nil {
		tty = fi.Mode()&os.ModeCharDevice != 0
	}
	if !tty {
		return "", ErrTokenRequired
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mcpserver: http: token: %w", err)
	}
	tok := hex.EncodeToString(b[:])
	w := opts.Stderr
	if w == nil {
		w = os.Stderr
	}
	if _, err := fmt.Fprintf(w, "connect-iq-mcp: HTTP bearer token (set %s to reuse): %s\n", EnvHTTPToken, tok); err != nil {
		return "", fmt.Errorf("mcpserver: http: print token: %w", err)
	}
	return tok, nil
}
