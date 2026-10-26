package browser

import (
	"encoding/json"
	"net/url"
	"strings"
	"time"
)

const (
	dashboardURL      = "https://apps.garmin.com/en-US/developer/dashboard"
	portalCookieURL   = "https://apps.garmin.com/"
	connectSignInURL  = "https://sso.garmin.com/mobile/sso/en_US/sign-in?clientId=GCM_ANDROID_DARK&service=https://mobile.integration.garmin.com/gcm/android"
	connectServiceURL = "https://mobile.integration.garmin.com/gcm/android"
	connectHost       = "mobile.integration.garmin.com"
	portalHost        = "apps.garmin.com"
)

type pageKind int

const (
	pageOther     pageKind = iota
	pagePortal             // apps.garmin.com, not a sign-in page
	pageSSO                // Garmin SSO / sign-in page
	pageChallenge          // Cloudflare challenge
	pageService            // Connect service URL (ticket redirect target)
)

// classifyURL decides what kind of page the browser is on from its URL.
func classifyURL(raw string) pageKind {
	u, err := url.Parse(raw)
	if err != nil {
		return pageOther
	}
	host := strings.ToLower(u.Hostname())
	path := strings.ToLower(u.Path)
	q := strings.ToLower(u.RawQuery)
	if strings.Contains(path, "/cdn-cgi/challenge-platform") || strings.Contains(q, "__cf_chl") {
		return pageChallenge
	}
	switch {
	case host == connectHost:
		return pageService
	case host == "sso.garmin.com" || strings.HasSuffix(host, ".sso.garmin.com"):
		return pageSSO
	case host == portalHost:
		if strings.Contains(path, "/sso") || strings.Contains(path, "/signin") || strings.Contains(path, "/sign-in") || strings.Contains(path, "/login") {
			return pageSSO
		}
		return pagePortal
	}
	return pageOther
}

// challengeTitle reports whether a document title is a Cloudflare interstitial.
func challengeTitle(title string) bool {
	t := strings.ToLower(strings.TrimSpace(title))
	return strings.HasPrefix(t, "just a moment") || strings.HasPrefix(t, "attention required") || t == "один момент…"
}

// ticketFromURL extracts the "ST-..." service ticket from a URL's ticket param.
func ticketFromURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	t := u.Query().Get("ticket")
	if strings.HasPrefix(t, "ST-") {
		return t
	}
	return ""
}

// ticketFromJSON finds a serviceTicketId anywhere in a JSON document.
func ticketFromJSON(body []byte) string {
	var v any
	if json.Unmarshal(body, &v) != nil {
		return ""
	}
	return findTicket(v)
}

func findTicket(v any) string {
	switch x := v.(type) {
	case map[string]any:
		if s, ok := x["serviceTicketId"].(string); ok && strings.HasPrefix(s, "ST-") {
			return s
		}
		for _, e := range x {
			if t := findTicket(e); t != "" {
				return t
			}
		}
	case []any:
		for _, e := range x {
			if t := findTicket(e); t != "" {
				return t
			}
		}
	}
	return ""
}

// isPortalDomain reports whether a cookie domain belongs to apps.garmin.com.
func isPortalDomain(d string) bool {
	d = strings.ToLower(d)
	return d == portalHost || d == "."+portalHost
}

// isMobileLoginAPI reports whether a URL is the mobile SSO login API that
// returns the service ticket in its JSON body.
func isMobileLoginAPI(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	return strings.HasSuffix(strings.ToLower(u.Hostname()), "garmin.com") && strings.HasSuffix(u.Path, "/mobile/api/login")
}

// cookieHeader joins cookies into a Cookie request header value.
func cookieHeader(cs []Cookie) string {
	var b strings.Builder
	for i, c := range cs {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(c.Name)
		b.WriteByte('=')
		b.WriteString(c.Value)
	}
	return b.String()
}

// cdpExpires converts a CDP cookie expiry (seconds since epoch, <=0 for
// session cookies) to a time.
func cdpExpires(sec float64) time.Time {
	if sec <= 0 {
		return time.Time{}
	}
	whole := int64(sec)
	return time.Unix(whole, int64((sec-float64(whole))*1e9)).UTC()
}

// headfulUA strips the headless marker from a browser user agent.
func headfulUA(ua string) string {
	return strings.ReplaceAll(ua, "HeadlessChrome", "Chrome")
}
