// Package portal is the client for the Connect IQ developer portal's internal
// API on https://apps.garmin.com, plus the cookie utilities credstore uses.
// Garmin has no public store API; see docs for the endpoint list.
package portal

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// BaseURL is the developer portal origin. Cookies are only ever sent here.
const BaseURL = "https://apps.garmin.com"

var (
	curlHeaderRE = regexp.MustCompile(`(?is)(?:-H|--header)\s+\$?(['"])\s*cookie\s*:\s*(.*?)(?:'|")`)
	curlCookieRE = regexp.MustCompile(`(?is)(?:-b|--cookie)\s+\$?(['"])(.*?)(?:'|")`)
	cookieNameRE = regexp.MustCompile(`^[!#$%&'*+\-.^_` + "`" + `|~0-9A-Za-z]+$`)
)

// ParseCookieInput normalizes pasted portal cookies to a Cookie header value.
// It accepts "a=b; c=d", "Cookie: a=b; c=d", or a DevTools "Copy as cURL"
// command (-H 'cookie: …' or -b '…'). Only name=value pairs are kept (later
// duplicates win) and a `session` cookie is required. Errors never echo
// cookie values.
func ParseCookieInput(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("no cookies given")
	}
	if strings.HasPrefix(strings.ToLower(s), "curl ") || strings.Contains(s, "\ncurl ") {
		var parts []string
		for _, m := range curlHeaderRE.FindAllStringSubmatch(s, -1) {
			parts = append(parts, m[2])
		}
		for _, m := range curlCookieRE.FindAllStringSubmatch(s, -1) {
			parts = append(parts, m[2])
		}
		if len(parts) == 0 {
			return "", errors.New("the cURL command has no cookie header (-H 'cookie: …' or -b '…')")
		}
		s = strings.Join(parts, "; ")
	} else if len(s) >= 7 && strings.EqualFold(s[:7], "cookie:") {
		s = s[7:]
	}
	pairs := splitCookies(s)
	if len(pairs) == 0 {
		return "", errors.New("no name=value cookies found")
	}
	if _, ok := lookup(pairs, "session"); !ok {
		return "", errors.New("the `session` cookie for apps.garmin.com is missing; copy the cookies from a request to apps.garmin.com while signed in")
	}
	return join(pairs), nil
}

type pair struct{ name, value string }

// splitCookies parses a Cookie header; invalid entries are dropped, later
// duplicates replace earlier ones in place.
func splitCookies(h string) []pair {
	var out []pair
	for raw := range strings.SplitSeq(h, ";") {
		name, value, ok := strings.Cut(strings.TrimSpace(raw), "=")
		name = strings.TrimSpace(name)
		if !ok || !cookieNameRE.MatchString(name) {
			continue
		}
		out = set(out, name, strings.TrimSpace(value))
	}
	return out
}

func set(ps []pair, name, value string) []pair {
	for i := range ps {
		if ps[i].name == name {
			ps[i].value = value
			return ps
		}
	}
	return append(ps, pair{name, value})
}

func lookup(ps []pair, name string) (string, bool) {
	for _, p := range ps {
		if p.name == name {
			return p.value, true
		}
	}
	return "", false
}

func join(ps []pair) string {
	parts := make([]string, len(ps))
	for i, p := range ps {
		parts[i] = p.name + "=" + p.value
	}
	return strings.Join(parts, "; ")
}

// MergeSetCookie applies Set-Cookie lines to a Cookie header: new values
// replace old ones, Max-Age<=0 or a past Expires deletes the cookie.
func MergeSetCookie(header string, setCookie []string) string {
	if len(setCookie) == 0 {
		return header
	}
	ps := splitCookies(header)
	now := time.Now()
	for _, line := range setCookie {
		c, err := http.ParseSetCookie(line)
		if err != nil || c.Name == "" {
			continue
		}
		del := c.MaxAge < 0 || (!c.Expires.IsZero() && c.Expires.Before(now))
		if del {
			ps = remove(ps, c.Name)
			continue
		}
		ps = set(ps, c.Name, c.Value)
	}
	return join(ps)
}

func remove(ps []pair, name string) []pair {
	out := ps[:0]
	for _, p := range ps {
		if p.name != name {
			out = append(out, p)
		}
	}
	return out
}

// SealExpiry returns the earlier expiry of the `session` and `refresh_token`
// iron (Fe26.2) seals: field index 5 holds the expiry in Unix milliseconds.
// Past that the portal needs a full Garmin SSO login.
func SealExpiry(header string) (time.Time, error) {
	ps := splitCookies(header)
	var min time.Time
	for _, name := range []string{"session", "refresh_token"} {
		v, ok := lookup(ps, name)
		if !ok {
			return time.Time{}, errors.New("cookie `" + name + "` is missing")
		}
		t, err := sealExpiry(v)
		if err != nil {
			return time.Time{}, errors.New("cookie `" + name + "`: " + err.Error())
		}
		if min.IsZero() || t.Before(min) {
			min = t
		}
	}
	return min, nil
}

func sealExpiry(v string) (time.Time, error) {
	if u, err := url.QueryUnescape(v); err == nil {
		v = u
	}
	v = strings.Trim(v, `"`)
	f := strings.Split(v, "*")
	if len(f) < 6 || f[0] != "Fe26.2" {
		return time.Time{}, errors.New("not an iron Fe26.2 seal")
	}
	ms, err := strconv.ParseInt(f[5], 10, 64)
	if err != nil || ms <= 0 {
		return time.Time{}, errors.New("seal has no expiry")
	}
	return time.UnixMilli(ms), nil
}
