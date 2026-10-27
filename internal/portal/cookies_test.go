package portal

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/idoo/connect-iq-mcp/internal/auth"
)

func seal(ms int64) string {
	return "Fe26.2**salt*iv*enc*" + strconv.FormatInt(ms, 10) + "*hsalt*hmac"
}

func TestParseCookieInput(t *testing.T) {
	cases := map[string]string{
		"header":     "session=S1; CIQ_DI_session=D1",
		"prefix":     "Cookie: session=S1; CIQ_DI_session=D1",
		"curl -H":    "curl 'https://apps.garmin.com/x' \\\n  -H 'accept: */*' \\\n  -H 'cookie: session=S1; CIQ_DI_session=D1' \\\n  -H 'referer: x'",
		"curl -b":    "curl 'https://apps.garmin.com/x' -H 'accept: */*' -b 'session=S1; CIQ_DI_session=D1' --compressed",
		"curl $quot": "curl 'https://apps.garmin.com/x' -b $'session=S1; CIQ_DI_session=D1'",
		"cmd -H":     "curl ^\"https://apps.garmin.com/x^\" ^\n  -H ^\"accept: */*^\" ^\n  -H ^\"cookie: session=S1; CIQ_DI_session=D1^\" ^\n  -H ^\"referer: x^\"",
		"cmd -b":     "curl ^\"https://apps.garmin.com/x^\" ^\n  -b ^\"session=S1; CIQ_DI_session=D1^\" ^\n  --compressed",
		"dupes":      "session=old; CIQ_DI_session=D1; session=S1; junk",
	}
	for name, in := range cases {
		got, err := ParseCookieInput(in)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got != "session=S1" {
			t.Fatalf("%s: got %q", name, got)
		}
	}
	for _, bad := range []string{"", "CIQ_DI_session=D1", "curl 'https://x' -H 'accept: x'"} {
		if _, err := ParseCookieInput(bad); err == nil {
			t.Fatalf("%q: want error", bad)
		}
	}
	_, err := ParseCookieInput("CIQ_DI_session=SECRETVALUE")
	if err == nil || strings.Contains(err.Error(), "SECRETVALUE") {
		t.Fatalf("error must not echo values: %v", err)
	}
	// cmd.exe caret escapes inside the value are removed (Chrome writes % as ^%^).
	got, err := ParseCookieInput("curl ^\"https://apps.garmin.com/x^\" -b ^\"session=S^%^3D1; a=b^\"")
	if err != nil || got != "session=S%3D1" {
		t.Fatalf("cmd escapes: %q, %v", got, err)
	}
	// A bash-quoted value keeps its carets.
	if got, err := ParseCookieInput("curl 'https://x' -b 'session=S^1'"); err != nil || got != "session=S^1" {
		t.Fatalf("bash caret: %q, %v", got, err)
	}
}

func TestMergeSetCookie(t *testing.T) {
	h := "session=S1; CIQ_DI_session=D1; refresh_token=R1"
	got := MergeSetCookie(h, []string{
		"CIQ_DI_session=D2; Path=/; HttpOnly; Secure",
		"refresh_token=; Max-Age=0; Path=/",
		"new=N; Path=/",
		"old=; Expires=Thu, 01 Jan 1970 00:00:00 GMT",
	})
	if got != "session=S1; CIQ_DI_session=D2; new=N" {
		t.Fatalf("got %q", got)
	}
	if MergeSetCookie(h, nil) != h {
		t.Fatal("no Set-Cookie must keep header")
	}
}

func TestSessionOnly(t *testing.T) {
	jar := "notice_behavior=implied; TAsessionID=t; session=S1; refresh_token=R1; CIQ_DI_session=D1; ADRUM_BT=x"
	if got, err := SessionOnly(jar); err != nil || got != "session=S1" {
		t.Fatalf("got %q, %v", got, err)
	}
	for _, bad := range []string{"", "session=", "refresh_token=SECRETVALUE; CIQ_DI_session=SECRETVALUE"} {
		if _, err := SessionOnly(bad); err == nil || strings.Contains(err.Error(), "SECRETVALUE") {
			t.Fatalf("%q: want value-free error, got %v", bad, err)
		}
	}
}

func TestSealExpiry(t *testing.T) {
	a := int64(1_800_000_000_000)
	got, err := SealExpiry("session=" + strings.ReplaceAll(seal(a), "*", "%2A"))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(time.UnixMilli(a)) {
		t.Fatalf("got %v", got)
	}
	if _, err := SealExpiry("refresh_token=" + seal(a)); err == nil {
		t.Fatal("missing session must fail")
	}
	if _, err := SealExpiry("session=abc"); err == nil {
		t.Fatal("bad seal must fail")
	}
}

func TestVerify(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Cookie") == "session=dead" {
			http.Redirect(w, r, "https://sso.garmin.com/portal/sso", http.StatusTemporaryRedirect)
			return
		}
		if r.URL.Path != "/api/ciq-developerservices/developers" || r.Header.Get("Origin") != BaseURL {
			t.Errorf("bad request %s origin=%q", r.URL.Path, r.Header.Get("Origin"))
		}
		w.Header().Add("Set-Cookie", "CIQ_DI_session=D2; Path=/")
		_, _ = w.Write([]byte(`{"developer":true,"userId":"dev-1"}`))
	}))
	defer srv.Close()
	old := baseURL
	baseURL = srv.URL
	defer func() { baseURL = old }()

	d, h, err := Verify(context.Background(), srv.Client(), "session=S; CIQ_DI_session=D1")
	if err != nil || d.ID != "dev-1" || h != "session=S; CIQ_DI_session=D2" {
		t.Fatalf("d=%+v h=%q err=%v", d, h, err)
	}
	if _, _, err := Verify(context.Background(), srv.Client(), "session=dead"); !errors.Is(err, auth.ErrAuthRequired) {
		t.Fatalf("want auth required, got %v", err)
	}
}
