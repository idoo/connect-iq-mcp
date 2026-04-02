// Package redact strips credentials (JWTs, PEM keys, auth headers, cookies,
// signed URL queries) from text before it reaches logs or tool output.
package redact

import (
	"bytes"
	"net/url"
	"regexp"
	"strings"
)

// Redaction placeholders.
const (
	redactedJWT   = "<redacted:jwt>"
	redactedPEM   = "<redacted:pem>"
	redactedValue = "<redacted>"
	redactedQuery = "?<redacted>"
)

var (
	// pemBlock matches a complete PEM block, including JSON-escaped newlines.
	pemBlock = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]+-----[\s\S]*?-----END [A-Z0-9 ]+-----`)
	// pemOpenKey matches an unterminated private key (e.g. a cut log line).
	pemOpenKey = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----(?:[A-Za-z0-9+/=\s]|\\[nr])*`)
	jwtToken   = regexp.MustCompile(`eyJ[A-Za-z0-9_-]{2,}\.eyJ[A-Za-z0-9_-]{2,}\.[A-Za-z0-9_-]*`)
	// authHeader matches "Authorization: <scheme> <credentials>" in header,
	// JSON and key=value forms.
	authHeader = regexp.MustCompile(`(?i)(\bauthorization["']?\s*[:=]\s*["']?(?:bearer\s+|basic\s+)?)[^\s"',;]+`)
	// cookieHeader matches Cookie / Set-Cookie header values (portal
	// sessions) in header, JSON and key=value forms.
	cookieHeader = regexp.MustCompile(`(?i)(\b(?:set-)?cookie["']?\s*[:=]\s*["']?)[^\r\n"']+`)
	// bearerToken matches a bare "Bearer <token>".
	bearerToken = regexp.MustCompile(`(?i)\b(bearer\s+)([A-Za-z0-9._~+/=-]{16,})`)
	// anyURL matches http(s) URLs, including JSON-escaped "&" and "/".
	anyURL = regexp.MustCompile(`https?://(?:[^\s"'<>\\]|\\u0026|\\/)+`)
	// signedParam matches signing parameters outside a URL.
	signedParam = regexp.MustCompile(`(?i)\b(X-Amz-Signature|X-Amz-Credential|X-Amz-Security-Token|Key-Pair-Id)=[^&\s"'\\]+`)
	// ironSeal matches a bare Fe26.2 iron seal (portal session cookie value).
	ironSeal = regexp.MustCompile(`Fe26\.2\*[A-Za-z0-9*_~.+/=-]+`)
	// serviceTicket matches an SSO service ticket (ST-<n>-<random>…).
	serviceTicket = regexp.MustCompile(`\bST-[0-9]+-[0-9A-Za-z._-]{3,}`)
	// tokenField matches token fields in JSON ("k":"v") and key=value forms.
	// Tickets are caught by shape (serviceTicket) and in URLs, so prose like
	// "open a ticket: support" stays readable.
	tokenField = regexp.MustCompile(`(?i)(\b(?:refresh|access|id)_token|\boauth_token(?:_secret)?)(["']?\s*[:=]\s*["']?)[^\s"'&;,\\<>]+`)
	// sessionPair matches a session=<value> cookie pair outside a Cookie header.
	sessionPair = regexp.MustCompile(`(?i)(\bsession=)[^\s"'&;,\\<>]+`)

	redactLetters = regexp.MustCompile(`^[A-Za-z]+$`)
)

// Redact removes credentials from s: JWTs, PEM blocks, Authorization and
// Bearer credentials, cookies, Fe26.2 iron seals, session= pairs, SSO service
// tickets (ST-…), *_token / oauth_token fields, and the query string of signed
// or credential-carrying URLs (X-Amz-*, Signature, Expires, Key-Pair-Id,
// Policy, token, *_token, ticket, code, sig), keeping their scheme, host and
// path. Text without secrets is returned unchanged.
func Redact(s string) string {
	b := Bytes([]byte(s))
	if bytes.Equal(b, []byte(s)) {
		return s
	}
	return string(b)
}

// Bytes is [Redact] for byte slices. It returns b itself when nothing
// needs redacting.
func Bytes(b []byte) []byte {
	if pemBlock.Match(b) {
		b = pemBlock.ReplaceAllLiteral(b, []byte(redactedPEM))
	}
	if pemOpenKey.Match(b) {
		b = pemOpenKey.ReplaceAllLiteral(b, []byte(redactedPEM))
	}
	if jwtToken.Match(b) {
		b = jwtToken.ReplaceAllLiteral(b, []byte(redactedJWT))
	}
	if authHeader.Match(b) {
		b = authHeader.ReplaceAll(b, []byte("${1}"+redactedValue))
	}
	if cookieHeader.Match(b) {
		b = cookieHeader.ReplaceAll(b, []byte("${1}"+redactedValue))
	}
	if bearerToken.Match(b) {
		b = bearerToken.ReplaceAllFunc(b, redactBearer)
	}
	if anyURL.Match(b) {
		b = anyURL.ReplaceAllFunc(b, redactURL)
	}
	if signedParam.Match(b) {
		b = signedParam.ReplaceAll(b, []byte("${1}="+redactedValue))
	}
	if ironSeal.Match(b) {
		b = ironSeal.ReplaceAllLiteral(b, []byte(redactedValue))
	}
	if serviceTicket.Match(b) {
		b = serviceTicket.ReplaceAllLiteral(b, []byte(redactedValue))
	}
	if tokenField.Match(b) {
		b = tokenField.ReplaceAll(b, []byte("${1}${2}"+redactedValue))
	}
	if sessionPair.Match(b) {
		b = sessionPair.ReplaceAll(b, []byte("${1}"+redactedValue))
	}
	return b
}

// redactBearer replaces a bearer credential unless it is a plain word
// ("bearer responsibilities") or already redacted.
func redactBearer(m []byte) []byte {
	sub := bearerToken.FindSubmatch(m)
	if redactLetters.Match(sub[2]) {
		return m
	}
	return append(append([]byte{}, sub[1]...), redactedValue...)
}

// redactURL drops the query and fragment of a URL carrying signing
// parameters.
func redactURL(m []byte) []byte {
	q := bytes.IndexByte(m, '?')
	if q < 0 || !signedQuery(string(m[q+1:])) {
		return m
	}
	return append(append([]byte{}, m[:q]...), redactedQuery...)
}

// signedQuery reports whether a raw query string contains a signing key.
func signedQuery(raw string) bool {
	raw = strings.ReplaceAll(raw, `\u0026`, "&")
	if i := strings.IndexByte(raw, '#'); i >= 0 {
		raw = raw[:i]
	}
	for part := range strings.SplitSeq(raw, "&") {
		key, _, _ := strings.Cut(part, "=")
		if k, err := url.QueryUnescape(key); err == nil {
			key = k
		}
		switch k := strings.ToLower(key); {
		case strings.HasPrefix(k, "x-amz-"), strings.HasPrefix(k, "x-goog-"):
			return true
		case k == "signature", k == "expires", k == "key-pair-id", k == "policy",
			k == "token", k == "access_token", k == "refresh_token", k == "id_token",
			k == "oauth_token", k == "oauth_verifier", k == "ticket", k == "code", k == "sig":
			return true
		}
	}
	return false
}
