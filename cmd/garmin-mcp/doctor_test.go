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
	oldSDK, oldJava, oldBrowser, oldDisplay, oldPre := resolveSDK, findJava, findBrowser, hasDisplay, browserPreflight
	t.Cleanup(func() {
		resolveSDK, findJava, findBrowser, hasDisplay, browserPreflight = oldSDK, oldJava, oldBrowser, oldDisplay, oldPre
	})
	hasDisplay = func() bool { return true }
	browserPreflight = func(context.Context) error { return nil }
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
	localChecks(context.Background(), rep, "default")
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
	localChecks(context.Background(), rep, "default")
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
	localChecks(context.Background(), rep, "work")
	out := b.String()
	if rep.failed || !strings.Contains(out, "warn  display:") || !strings.Contains(out, "GARMIN_MCP_PORTAL_COOKIES") ||
		!strings.Contains(out, "`garmin-mcp auth login --paste --profile work`") || !strings.Contains(out, "Copy as cURL") {
		t.Fatalf("want a display warning naming --paste:\n%s", out)
	}
}

// N9: Connect is opt-in, so a missing token is an info line naming --connect;
// N4: the portal line names the profile.
func TestCredLineConnectOptional(t *testing.T) {
	var b bytes.Buffer
	rep := &report{w: &b}
	credLine(rep, "work", "portal session", auth.CredStatus{}, false)
	credLine(rep, "work", "Connect token", auth.CredStatus{}, true)
	out := b.String()
	if rep.failed || strings.Count(out, "warn") != 1 || !strings.Contains(out, "warn  portal session: missing; run `garmin-mcp auth login --profile work`") {
		t.Fatalf("portal line:\n%s", out)
	}
	if !strings.Contains(out, "info  Connect token: not enabled (optional; enable with garmin-mcp auth login --connect --profile work)") {
		t.Fatalf("connect line:\n%s", out)
	}
}

// N7: from a shell, doctor says the client's env may differ from this one.
func TestShellNote(t *testing.T) {
	old := fromShell
	t.Cleanup(func() { fromShell = old })
	var b bytes.Buffer
	rep := &report{w: &b}
	fromShell = func() bool { return false }
	shellNote(rep)
	if b.Len() != 0 {
		t.Fatalf("note outside a shell: %s", b.String())
	}
	fromShell = func() bool { return true }
	shellNote(rep)
	out := b.String()
	if rep.failed || !strings.HasPrefix(out, "info  env:") || !strings.Contains(out, ciqsdk.EnvKeyVar) || !strings.Contains(out, "JAVA_HOME") ||
		!strings.Contains(out, "garmin-mcp config install") || !strings.Contains(out, "restart the client") {
		t.Fatalf("note: %s", out)
	}
}

// r4: a failing Linux browser preflight (AppArmor, missing libraries) is a
// warning, not a failure.
func TestLocalChecksPreflightWarns(t *testing.T) {
	stubLocal(t, nil, nil, nil)
	browserPreflight = func(context.Context) error {
		return errors.New("Chrome for Testing can't run here: libnss3.so missing")
	}
	var b bytes.Buffer
	rep := &report{w: &b}
	localChecks(context.Background(), rep, "default")
	if rep.failed || !strings.Contains(b.String(), "warn  browser: Chrome for Testing can't run here") {
		t.Fatalf("want a preflight warning:\n%s", b.String())
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
	if out, failed := run(); failed || !strings.Contains(out, "info  developer key: GARMIN_MCP_KEY not set") || !strings.Contains(out, "config install <client> --key") {
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
