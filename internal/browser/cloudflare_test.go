package browser

import (
	"errors"
	"testing"

	"github.com/idoo/connect-iq-mcp/internal/auth"
)

const (
	cf1015Text = "Access denied\nError 1015\nRay ID: 8d1f2c3b4a5e6f70 • 2026-10-26 10:00:00 UTC\nYou are being rate limited\nWhat happened?\nThe owner of this website (sso.garmin.com) has banned you temporarily from accessing this website.\nCloudflare Ray ID: 8d1f2c3b4a5e6f70"
	cf1020Text = "Access denied\nError code 1020\nYou do not have access to sso.garmin.com.\nThe site owner may have set restrictions that prevent you from accessing the site.\nError details\nProvided to the site owner: Error 1020\nPerformance & security by Cloudflare"
)

func TestCloudflareBlock(t *testing.T) {
	cases := []struct {
		name, title, text string
		want              cfBlock
	}{
		{"1015", "Access denied | sso.garmin.com used Cloudflare to restrict access", cf1015Text, cfRateLimited},
		{"1015 text only", "", "Error 1015 You are being rate limited. Cloudflare", cfRateLimited},
		{"1020", "Access denied | sso.garmin.com used Cloudflare to restrict access", cf1020Text, cfDenied},
		{"1020 attention title", "Attention Required! | Cloudflare", cf1020Text, cfDenied},
		{"plain access denied", "Access denied", "", cfDenied},
		{"challenge", "Just a moment...", "Checking your browser. Cloudflare", cfNone},
		{"attention challenge", "Attention Required! | Cloudflare", "Please complete the security check. Cloudflare Ray ID", cfNone},
		{"sign-in page", "Garmin Connect", "Sign In Email Password Remember me", cfNone},
		{"garmin text mentions rate limit", "Garmin", "you are being rate limited", cfNone},
	}
	for _, c := range cases {
		if got := cloudflareBlock(c.title, c.text); got != c.want {
			t.Errorf("%s: cloudflareBlock = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestInteractionCloudflareBlock(t *testing.T) {
	title := "Access denied | sso.garmin.com used Cloudflare to restrict access"
	for _, headless := range []bool{true, false} {
		s := &session{headless: headless}
		if err := s.interaction(pageState{Title: title, Text: cf1015Text}); !errors.Is(err, auth.ErrRateLimited) {
			t.Errorf("headless=%v 1015: err = %v, want ErrRateLimited", headless, err)
		}
		err := s.interaction(pageState{Title: title, Text: cf1020Text})
		if !errors.Is(err, auth.ErrInteractionRequired) || errors.Is(err, auth.ErrRateLimited) {
			t.Errorf("headless=%v 1020: err = %v, want ErrInteractionRequired", headless, err)
		}
	}
	// A headful sign-in form is left for the user.
	if err := (&session{}).interaction(pageState{Title: "Garmin", Form: true}); err != nil {
		t.Errorf("headful form: err = %v, want nil", err)
	}
}
