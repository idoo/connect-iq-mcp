// Package httpx holds HTTP helpers shared by the upstream clients.
package httpx

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// RetryAfter parses a Retry-After header value as delay-seconds or an HTTP
// date relative to now. Missing, malformed or past values give 0.
func RetryAfter(v string, now time.Time) time.Duration {
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
