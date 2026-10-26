package browser

import (
	"testing"
	"time"
)

func TestClassifyURL(t *testing.T) {
	cases := map[string]pageKind{
		"https://apps.garmin.com/en-US/developer/dashboard":                         pagePortal,
		"https://apps.garmin.com/en-US/":                                            pagePortal,
		"https://apps.garmin.com/sso/signin?x=1":                                    pageSSO,
		"https://sso.garmin.com/portal/sso/en-US/sign-in?clientId=X":                pageSSO,
		"https://sso.garmin.com/mobile/sso/en_US/sign-in?clientId=GCM_ANDROID_DARK": pageSSO,
		"https://mobile.integration.garmin.com/gcm/android?ticket=ST-1":             pageService,
		"https://apps.garmin.com/cdn-cgi/challenge-platform/h/b/orchestrate":        pageChallenge,
		"https://apps.garmin.com/en-US/?__cf_chl_rt_tk=abc":                         pageChallenge,
		"https://connect.garmin.com/modern/":                                        pageOther,
		"about:blank":                                                               pageOther,
		"::bad":                                                                     pageOther,
	}
	for u, want := range cases {
		if got := classifyURL(u); got != want {
			t.Errorf("classifyURL(%q) = %v, want %v", u, got, want)
		}
	}
}

func TestChallengeTitle(t *testing.T) {
	for _, s := range []string{"Just a moment...", "  just a moment…", "Attention Required! | Cloudflare"} {
		if !challengeTitle(s) {
			t.Errorf("challengeTitle(%q) = false", s)
		}
	}
	if challengeTitle("Developer Dashboard") {
		t.Error("dashboard title classified as challenge")
	}
}

func TestTicketFromURL(t *testing.T) {
	cases := map[string]string{
		"https://mobile.integration.garmin.com/gcm/android?ticket=ST-0123-abc-cas": "ST-0123-abc-cas",
		"https://mobile.integration.garmin.com/gcm/android?ticket=XX-1":            "",
		"https://mobile.integration.garmin.com/gcm/android":                        "",
		"::bad": "",
	}
	for u, want := range cases {
		if got := ticketFromURL(u); got != want {
			t.Errorf("ticketFromURL(%q) = %q, want %q", u, got, want)
		}
	}
}

func TestTicketFromJSON(t *testing.T) {
	cases := map[string]string{
		`{"serviceTicketId":"ST-1-x"}`:                                   "ST-1-x",
		`{"responseStatus":{"type":"SUCCESS"},"serviceTicketId":"ST-9"}`: "ST-9",
		`{"a":[{"b":{"serviceTicketId":"ST-deep"}}]}`:                    "ST-deep",
		`{"serviceTicketId":"nope"}`:                                     "",
		`{"responseStatus":{"type":"MFA_REQUIRED"}}`:                     "",
		`not json`: "",
	}
	for in, want := range cases {
		if got := ticketFromJSON([]byte(in)); got != want {
			t.Errorf("ticketFromJSON(%s) = %q, want %q", in, got, want)
		}
	}
}

func TestIsMobileLoginAPI(t *testing.T) {
	if !isMobileLoginAPI("https://sso.garmin.com/mobile/api/login?clientId=GCM_ANDROID_DARK") {
		t.Error("login API not recognised")
	}
	if isMobileLoginAPI("https://evil.example/mobile/api/login") || isMobileLoginAPI("https://sso.garmin.com/mobile/api/other") {
		t.Error("false positive")
	}
}

func TestPortalDomainAndHeader(t *testing.T) {
	for d, want := range map[string]bool{"apps.garmin.com": true, ".apps.garmin.com": true, ".garmin.com": false, "sso.garmin.com": false} {
		if isPortalDomain(d) != want {
			t.Errorf("isPortalDomain(%q) != %v", d, want)
		}
	}
	h := cookieHeader([]Cookie{{Name: "session", Value: "a"}, {Name: "refresh_token", Value: "b"}})
	if h != "session=a; refresh_token=b" {
		t.Errorf("cookieHeader = %q", h)
	}
	if cookieHeader(nil) != "" {
		t.Error("empty header expected")
	}
}

func TestCdpExpires(t *testing.T) {
	if !cdpExpires(-1).IsZero() || !cdpExpires(0).IsZero() {
		t.Error("session cookie should have zero expiry")
	}
	if got := cdpExpires(1700000000.5); !got.Equal(time.Unix(1700000000, 5e8)) {
		t.Errorf("cdpExpires = %v", got)
	}
}

func TestHeadfulUA(t *testing.T) {
	in := "Mozilla/5.0 (Macintosh) AppleWebKit/537.36 (KHTML, like Gecko) HeadlessChrome/141.0.0.0 Safari/537.36"
	if got := headfulUA(in); got != "Mozilla/5.0 (Macintosh) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/141.0.0.0 Safari/537.36" {
		t.Errorf("headfulUA = %q", got)
	}
}

func TestCandidates(t *testing.T) {
	mac := candidates("darwin", "/Users/u", nil)
	if mac[0] != "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" || mac[1] != "/Users/u/Applications/Google Chrome.app/Contents/MacOS/Google Chrome" {
		t.Errorf("darwin candidates: %v", mac[:2])
	}
	if len(mac) != 8 {
		t.Errorf("darwin: %d candidates", len(mac))
	}
	win := candidates("windows", "", func(k string) string {
		if k == "ProgramFiles" {
			return `C:\Program Files\`
		}
		return ""
	})
	if win[0] != `C:\Program Files\Google\Chrome\Application\chrome.exe` || len(win) != 4 {
		t.Errorf("windows candidates: %v", win)
	}
	if lin := candidates("linux", "", nil); lin[0] != "google-chrome" {
		t.Errorf("linux candidates: %v", lin)
	}
}
