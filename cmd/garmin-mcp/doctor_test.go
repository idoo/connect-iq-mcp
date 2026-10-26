package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/idoo/garmin-mcp/internal/auth"
	"github.com/idoo/garmin-mcp/internal/ciqsdk"
)

type fakeStore struct{ auth.Store }

func stubLocal(t *testing.T, sdkErr, javaErr, browserErr error) {
	t.Helper()
	oldSDK, oldJava, oldBrowser := resolveSDK, findJava, findBrowser
	t.Cleanup(func() { resolveSDK, findJava, findBrowser = oldSDK, oldJava, oldBrowser })
	resolveSDK = func() (ciqsdk.Resolved, error) {
		return ciqsdk.Resolved{SDK: ciqsdk.SDK{Path: "/sdk/9.1.0", Version: "9.1.0"}, Source: "current-sdk.cfg"}, sdkErr
	}
	findJava = func(context.Context) (ciqsdk.Java, error) {
		return ciqsdk.Java{Path: "/usr/bin/java", Version: "21", Source: "PATH"}, javaErr
	}
	findBrowser = func() (string, error) { return "/Applications/Chromium.app", browserErr }
}

func TestLocalChecksOK(t *testing.T) {
	stubLocal(t, nil, nil, nil)
	var b bytes.Buffer
	rep := &report{w: &b}
	localChecks(context.Background(), rep)
	out := b.String()
	for _, want := range []string{"ok    Connect IQ SDK 9.1.0 (current-sdk.cfg): /sdk/9.1.0", "ok    Java 21 (PATH)", "ok    browser: /Applications/Chromium.app"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if rep.failed {
		t.Fatal("failed")
	}
}

func TestLocalChecksMissingAreWarnings(t *testing.T) {
	stubLocal(t, errors.New("no sdk"), errors.New("no java"), errors.New("no browser"))
	var b bytes.Buffer
	rep := &report{w: &b}
	localChecks(context.Background(), rep)
	out := b.String()
	if rep.failed || strings.Contains(out, "ok  ") || strings.Count(out, "warn") != 3 {
		t.Fatalf("want 3 warnings, no failure:\n%s", out)
	}
	if !strings.Contains(out, "SDK Manager") || !strings.Contains(out, "auth login") {
		t.Fatalf("missing install hints:\n%s", out)
	}
}

func TestPortalProbe(t *testing.T) {
	old := probePortal
	t.Cleanup(func() { probePortal = old })
	calls := 0
	probePortal = func(context.Context, auth.Store) (string, error) {
		calls++
		return "dev-1", nil
	}
	var b bytes.Buffer
	rep := &report{w: &b}
	portalProbe(context.Background(), rep, nil)
	if calls != 0 || !strings.Contains(b.String(), "skipped") {
		t.Fatalf("nil store probed: %d\n%s", calls, b.String())
	}
	b.Reset()
	portalProbe(context.Background(), rep, fakeStore{})
	if !strings.Contains(b.String(), "ok    portal: signed in as developer dev-1") {
		t.Fatalf("got %s", b.String())
	}
	probePortal = func(context.Context, auth.Store) (string, error) { return "", errors.New("offline") }
	b.Reset()
	portalProbe(context.Background(), rep, fakeStore{})
	if rep.failed || !strings.Contains(b.String(), "warn  portal: offline") {
		t.Fatalf("error must be a warning: %s", b.String())
	}
}
