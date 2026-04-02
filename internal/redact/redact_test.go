package redact

import (
	"strings"
	"testing"
)

func TestRedact(t *testing.T) {
	jwt := "eyJhbGciOiJFUzI1NiIsImtpZCI6IkFCQyJ9.eyJpc3MiOiJ4IiwiYXVkIjoiYXBwc3RvcmVjb25uZWN0LXYxIn0.MEUCIQDx-_abc"
	pem := "-----BEGIN PRIVATE KEY-----\nMIGTAgEAMBMGByqGSM49AgEGCCqGSM49AwEHBHkwdwIBAQQg\n-----END PRIVATE KEY-----"
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "jwt", in: "token " + jwt + " end", want: "token <redacted:jwt> end"},
		{name: "pem block", in: "key:\n" + pem + "\nok", want: "key:\n<redacted:pem>\nok"},
		{name: "pem json escaped", in: `{"k":"-----BEGIN EC PRIVATE KEY-----\nMIGT\n-----END EC PRIVATE KEY-----"}`, want: `{"k":"<redacted:pem>"}`},
		{name: "unterminated key", in: `"-----BEGIN PRIVATE KEY-----\nMIGTAgEA`, want: `"<redacted:pem>`},
		{name: "authorization header", in: "Authorization: Bearer abc.def.ghi", want: "Authorization: Bearer <redacted>"},
		{name: "authorization header with jwt", in: "Authorization: Bearer " + jwt, want: "Authorization: Bearer <redacted>"},
		{name: "authorization json", in: `{"Authorization":"Basic dXNlcjpwYXNz"}`, want: `{"Authorization":"Basic <redacted>"}`},
		{name: "cookie header", in: "Cookie: SESSION=abc; CIQ=def", want: "Cookie: <redacted>"},
		{name: "set-cookie json", in: `{"Set-Cookie":"SESSION=abc; Path=/"}`, want: `{"Set-Cookie":"<redacted>"}`},
		{name: "bare bearer", in: "sent bearer abcdEFGH1234ijkl5678", want: "sent bearer <redacted>"},
		{
			name: "signed s3 url",
			in:   "see https://bucket.s3.amazonaws.com/a/b.png?X-Amz-Algorithm=AWS4&X-Amz-Signature=deadbeef now",
			want: "see https://bucket.s3.amazonaws.com/a/b.png?<redacted> now",
		},
		{
			name: "signed url json escaped",
			in:   `{"url":"https://cdn.example/f.gz?Expires=1\u0026Signature=abc\u0026Key-Pair-Id=K"}`,
			want: `{"url":"https://cdn.example/f.gz?<redacted>"}`,
		},
		{name: "token param", in: "https://x.example/p?id=1&token=s3cret", want: "https://x.example/p?<redacted>"},
		{name: "loose signature param", in: "X-Amz-Signature=deadbeef&a=1", want: "X-Amz-Signature=<redacted>&a=1"},
		{name: "session pair with iron seal", in: "session=Fe26.2**a*b*c*1*d*e", want: "session=<redacted>"},
		{name: "bare iron seal", in: "value Fe26.2**abc*def*1*x end", want: "value <redacted> end"},
		{name: "service ticket url", in: "go https://x.example/?ticket=ST-1-abc now", want: "go https://x.example/?<redacted> now"},
		{name: "bare service ticket", in: "got ST-123456-AbCdEf-cas", want: "got <redacted>"},
		{name: "json refresh token", in: `{"refresh_token":"rt-1"}`, want: `{"refresh_token":"<redacted>"}`},
		{name: "json access token", in: `{"access_token": "at-1","x":1}`, want: `{"access_token": "<redacted>","x":1}`},
		{name: "oauth_token query", in: "https://x.example/cb?oauth_token=abc&oauth_verifier=v", want: "https://x.example/cb?<redacted>"},
		{name: "code query", in: "https://x.example/cb?code=abc123&state=s", want: "https://x.example/cb?<redacted>"},
		{name: "kv refresh token", in: "refresh_token=rt-2 next", want: "refresh_token=<redacted> next"},
		{name: "plain session word", in: "the sim session started", want: "the sim session started"},
		{name: "plain ticket prose", in: "please open a ticket: support", want: "please open a ticket: support"},
		{name: "json ticket value", in: `{"ticket":"ST-0123456-AbCdEfGh-cas"}`, want: `{"ticket":"<redacted>"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Redact(tt.in); got != tt.want {
				t.Errorf("Redact = %q, want %q", got, tt.want)
			}
			if got := string(Bytes([]byte(tt.in))); got != tt.want {
				t.Errorf("Bytes = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRedactNoFalsePositives(t *testing.T) {
	for _, s := range []string{
		"The token was invalid; check your API key in doctor.",
		"Authorization failed for this request",
		"He is the bearer of bad news and bearer responsibilities",
		"https://apps.garmin.com/api/ciq-developerservices/developers?limit=10&sort=name",
		"https://developer.garmin.com/connect-iq/",
		"eyJ alone is not a JWT, nor is eyJa.b.c",
		"-----BEGIN nothing here",
		`{"signatureRequired":true,"expiresAt":"2026-10-25"}`,
		"",
	} {
		if got := Redact(s); got != s {
			t.Errorf("Redact(%q) = %q, want unchanged", s, got)
		}
		b := []byte(s)
		if got := Bytes(b); string(got) != s {
			t.Errorf("Bytes(%q) = %q", s, got)
		}
	}
}

func TestRedactBytesNoCopyWhenClean(t *testing.T) {
	b := []byte(strings.Repeat("plain text ", 10))
	if got := Bytes(b); &got[0] != &b[0] {
		t.Fatal("clean input was copied")
	}
}
