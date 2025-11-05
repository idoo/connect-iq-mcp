package tool

import (
	"fmt"
	"net/http"
)

// GuardWrites wraps base (nil means http.DefaultTransport) so that every
// request other than GET, HEAD or OPTIONS fails unless its context carries
// write capability, which [Tool.Call] grants only after the class, confirm
// and approval gates pass.
func GuardWrites(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return writeGuard{base: base}
}

type writeGuard struct{ base http.RoundTripper }

func (g writeGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	switch req.Method {
	case "", http.MethodGet, http.MethodHead, http.MethodOptions:
	default:
		if !IsWrite(req.Context()) {
			return nil, fmt.Errorf("%s %s: %w", req.Method, req.URL.Redacted(), ErrWriteGuard)
		}
	}
	return g.base.RoundTrip(req)
}
