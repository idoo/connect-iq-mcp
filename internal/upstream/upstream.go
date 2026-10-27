// Package upstream holds the one error type every Garmin HTTP client returns
// for a non-auth, non-2xx answer. Response bodies are never kept raw: they
// may echo cookies or tokens.
package upstream

import (
	"fmt"
	"strings"
	"time"
)

// Service names the upstream in messages and hints.
type Service string

// The upstreams connect-iq-mcp talks to.
const (
	Portal  Service = "portal"
	Connect Service = "Garmin Connect"
)

// HTTPError is a non-2xx upstream answer that is not an auth failure.
type HTTPError struct {
	Service Service
	Status  int
	// Method and Path name the request when the client reports it; the
	// message then reads "<service> HTTP <status> on <method> <path>".
	Method string
	Path   string
	// Code and Message are the decoded error body ({code,message} or a
	// validation result).
	Code    string
	Message string
	// Fields is the portal's validationResult (field or code → code).
	Fields map[string]string
	// RateLimited marks a rate limit: HTTP 429, a portal 503 carrying
	// Retry-After, or a Cloudflare 1015 page.
	RateLimited bool
	// RetryAfter is the server's Retry-After (0 when absent or unparsable).
	RetryAfter time.Duration
	// Cloudflare is set for a Cloudflare 1015 rate-limit page.
	Cloudflare bool
}

func (e *HTTPError) Error() string {
	switch {
	case e.RateLimited && e.Path != "":
		if e.RetryAfter > 0 {
			return fmt.Sprintf("%s rate limit on %s; retry after %s", e.Service, e.Path, e.RetryAfter)
		}
		return fmt.Sprintf("%s rate limit on %s", e.Service, e.Path)
	case e.RateLimited:
		s := fmt.Sprintf("%s HTTP %d: rate limited", e.Service, e.Status)
		if e.Cloudflare {
			s = fmt.Sprintf("%s HTTP %d: Cloudflare rate limit (error 1015)", e.Service, e.Status)
		}
		if e.RetryAfter > 0 {
			s += fmt.Sprintf(" (Retry-After %ds)", int(e.RetryAfter/time.Second))
		}
		return s
	case e.Path != "":
		return fmt.Sprintf("%s HTTP %d on %s %s", e.Service, e.Status, e.Method, e.Path)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s HTTP %d", e.Service, e.Status)
	if e.Code != "" {
		b.WriteString(" " + e.Code)
	}
	if e.Message != "" && e.Message != e.Code {
		b.WriteString(": " + e.Message)
	}
	return b.String()
}

// Detail is the decoded message, or the request line when the body had none.
func (e *HTTPError) Detail() string {
	if e.Message == "" && e.Path != "" {
		return e.Method + " " + e.Path
	}
	return e.Message
}
