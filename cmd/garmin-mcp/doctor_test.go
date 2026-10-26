package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/idoo/garmin-mcp/internal/auth"
	"github.com/idoo/garmin-mcp/internal/ciqsdk"
)

type fakeStore struct{ auth.Store }

func stubLocal(t *testing.T, sdkErr, javaErr, browserErr error) {
	t.Helper()
	oldSDK, oldJava, oldBrowser, oldDisplay := resolveSDK, findJava, findBrowser, hasDisplay
	t.Cleanup(func() { resolveSDK, findJava, findBrowser, hasDisplay = oldSDK, oldJava, oldBrowser, oldDisplay })
	hasDisplay = func() bool { return true }
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

func TestLocalChecksNoDisplayWarns(t *testing.T) {
	stubLocal(t, nil, nil, nil)
	hasDisplay = func() bool { return false }
	var b bytes.Buffer
	rep := &report{w: &b}
	localChecks(context.Background(), rep)
	if rep.failed || !strings.Contains(b.String(), "warn  display:") || !strings.Contains(b.String(), "GARMIN_MCP_PORTAL_COOKIES") {
		t.Fatalf("want a display warning:\n%s", b.String())
	}
}

func TestKeyCheck(t *testing.T) {
	run := func() (string, bool) {
		var b bytes.Buffer
		rep := &report{w: &b}
		keyCheck(rep)
		return b.String(), rep.failed
	}

	t.Setenv(ciqsdk.EnvKeyVar, "")
	if out, failed := run(); failed || !strings.Contains(out, "ok    developer key: GARMIN_MCP_KEY not set") {
		t.Fatalf("unset: %s", out)
	}

	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.der")
	t.Setenv(ciqsdk.EnvKeyVar, missing)
	if out, failed := run(); failed || !strings.Contains(out, "warn  developer key:") || !strings.Contains(out, missing) {
		t.Fatalf("missing: %s", out)
	}

	key := filepath.Join(dir, "developer_key.der")
	ki, err := ciqsdk.GenerateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(ciqsdk.EnvKeyVar, key)
	out, failed := run()
	if failed || !strings.Contains(out, "ok    developer key "+key) || !strings.Contains(out, ki.Fingerprint) || !strings.Contains(out, "RSA-4096") {
		t.Fatalf("present: %s", out)
	}
	der, err := os.ReadFile(key)
	if err != nil {
		t.Fatal(err)
	}
	// No key material: neither raw bytes nor any 16-byte hex run of the DER.
	if strings.Contains(out, string(der[:64])) || strings.Contains(out, fmt.Sprintf("%x", der[100:116])) {
		t.Fatalf("key bytes leaked: %s", out)
	}
}
