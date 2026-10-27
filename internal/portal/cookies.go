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
	// `\$?` is bash's $'…' quoting; `\^?` before either quote is the cmd.exe
	// form of DevTools "Copy as cURL (cmd)" (-H ^"cookie: …^").
	curlHeaderRE = regexp.MustCompile(`(?is)(?:-H|--header)\s+\$?(\^?)(['"])\s*cookie\s*:\s*(.*?)\^?(?:'|")`)
	curlCookieRE = regexp.MustCompile(`(?is)(?:-b|--cookie)\s+\$?(\^?)(['"])(.*?)\^?(?:'|")`)
	cmdCaretRE   = regexp.MustCompile(`\^(.)`)
	cookieNameRE = regexp.MustCompile(`^[!#$%&'*+\-.^_` + "`" + `|~0-9A-Za-z]+$`)
)

// ParseCookieInput reduces pasted portal cookies to the `session` cookie
// header (see SessionOnly). It accepts "a=b; c=d", "Cookie: a=b; c=d", or a
// DevTools "Copy as cURL" command, bash or cmd (-H 'cookie: …', -b '…' or
// -b ^"…^"). Later duplicates win. Errors never echo cookie values.
func ParseCookieInput(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("no cookies given")
	}
	if strings.HasPrefix(strings.ToLower(s), "curl ") || strings.Contains(s, "\ncurl ") {
		var parts []string
		for _, re := range []*regexp.Regexp{curlHeaderRE, curlCookieRE} {
			for _, m := range re.FindAllStringSubmatch(s, -1) {
				v := m[3]
				if m[1] != "" { // cmd.exe: ^X escapes X
					v = cmdCaretRE.ReplaceAllString(v, "$1")
				}
				parts = append(parts, v)
			}
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
	h, err := sessionOnly(pairs)
	if err != nil {
		return "", errors.New(err.Error() + "; copy the cookies from a request to apps.garmin.com while signed in")
	}
	return h, nil
}

// SessionOnly reduces a Cookie header to "session=<seal>". The developer
// portal authenticates every request by that cookie alone; the rest of the
// apps.garmin.com jar (refresh_token, CIQ_DI_session, consent and analytics
// cookies) is neither needed nor stored. Errors never echo cookie values.
func SessionOnly(header string) (string, error) {
	return sessionOnly(splitCookies(header))
}

func sessionOnly(ps []pair) (string, error) {
	v, ok := lookup(ps, "session")
	if !ok || v == "" {
		return "", errors.New("the `session` cookie for apps.garmin.com is missing")
	}
	return join([]pair{{"session", v}}), nil
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

// SealExpiry returns the expiry of the `session` iron (Fe26.2) seal: field
// index 5 holds it in Unix milliseconds. Past that the portal needs a full
// Garmin SSO login.
func SealExpiry(header string) (time.Time, error) {
	v, ok := lookup(splitCookies(header), "session")
	if !ok {
		return time.Time{}, errors.New("cookie `session` is missing")
	}
	t, err := sealExpiry(v)
	if err != nil {
		return time.Time{}, errors.New("cookie `session`: " + err.Error())
	}
	return t, nil
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
