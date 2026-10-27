package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/idoo/garmin-mcp/internal/auth"
	"github.com/idoo/garmin-mcp/internal/browser"
	"github.com/idoo/garmin-mcp/internal/ciqsdk"
	"github.com/idoo/garmin-mcp/internal/config"
	"github.com/idoo/garmin-mcp/internal/credstore"
	"github.com/idoo/garmin-mcp/internal/portal"
	"github.com/idoo/garmin-mcp/internal/tool"
)

// Local lookups doctor runs; tests replace them so no real SDK, Java or
// browser is needed.
var (
	resolveSDK = func() (ciqsdk.Resolved, error) {
		root, err := ciqsdk.DataRoot(nil)
		if err != nil {
			return ciqsdk.Resolved{}, err
		}
		return ciqsdk.Resolve(ciqsdk.ResolveOpts{DataRoot: root})
	}
	findJava    = func(ctx context.Context) (ciqsdk.Java, error) { return ciqsdk.FindJava(ctx, nil) }
	findBrowser = func() (string, error) { return browser.Find("") }
	// browserPreflight: Linux-only Chrome for Testing checks (AppArmor
	// userns block, missing shared libraries); nil elsewhere.
	browserPreflight = browser.Preflight
	hasDisplay       = browser.HasDisplay
	// fromShell reports whether doctor runs in an interactive terminal.
	fromShell = func() bool {
		fi, err := os.Stdin.Stat()
		return err == nil && fi.Mode()&os.ModeCharDevice != 0
	}
	// probePortal asks the portal who is signed in (GET /developers).
	probePortal = func(ctx context.Context, store auth.Store) (string, error) {
		d, err := portal.New(&http.Client{Timeout: 15 * time.Second}, store).Developer(ctx)
		return d.ID, err
	}
)

// report collects doctor lines and remembers failures.
type report struct {
	w      io.Writer
	failed bool
}

func (r *report) ok(format string, a ...any)   { r.line("ok  ", format, a...) }
func (r *report) info(format string, a ...any) { r.line("info", format, a...) }
func (r *report) warn(format string, a ...any) { r.line("warn", format, a...) }
func (r *report) fail(format string, a ...any) {
	r.failed = true
	r.line("FAIL", format, a...)
}

func (r *report) line(tag, format string, a ...any) {
	fmt.Fprintf(r.w, "%s  %s\n", tag, fmt.Sprintf(format, a...))
}

func runDoctor(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(stderr)
	profile := fs.String("profile", "", "profile to check")
	probe := fs.Bool("probe", false, "also ask the developer portal who is signed in (one network call)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return usageErr(err.Error())
	}
	rep := &report{w: stdout}
	doctor(ctx, rep, *profile, *probe)
	if rep.failed {
		return errors.New("doctor found problems")
	}
	return nil
}

func doctor(ctx context.Context, rep *report, profile string, probe bool) {
	fmt.Fprintf(rep.w, "garmin-mcp %s\n", buildVersion())
	r, err := config.Resolve(config.Options{Flags: config.Flags{Profile: profile}})
	if err != nil {
		rep.fail("config: %v", err)
		return
	}
	for _, w := range r.Warnings {
		rep.warn("%s", w)
	}
	for _, p := range r.Profile.Problems {
		rep.warn("%s", p)
	}
	p := r.Profile
	line := fmt.Sprintf("profile %q", p.Name)
	if p.PinSource != "" {
		line += " selected by " + p.PinSource
	}
	rep.ok("%s", line)
	store := credentialChecks(ctx, rep, p.Name)
	if probe {
		portalProbe(ctx, rep, store)
	}
	localChecks(ctx, rep, p.Name)
	keyCheck(rep)
	shellNote(rep)
	if p.AllowWrites {
		rep.ok("mode: writes enabled")
	} else {
		rep.ok("mode: read-only (ask the agent to enable writes, or set GARMIN_MCP_ALLOW_WRITES=1)")
	}
	names := make([]string, len(r.Toolsets))
	for i, ts := range r.Toolsets {
		names[i] = string(ts)
	}
	if r.Discovery {
		rep.ok("toolsets: discovery mode (core listed; others enabled at runtime)")
	} else {
		rep.ok("toolsets: %s", strings.Join(names, ", "))
	}
}

// credentialChecks reports each credential's state without secrets. It
// returns the store when the portal session cookie is present, else nil.
func credentialChecks(ctx context.Context, rep *report, profile string) auth.Store {
	store, err := credstore.Open(ctx, profile)
	if err != nil {
		rep.fail("credentials: %v", err)
		return nil
	}
	st := store.Status(ctx)
	credLine(rep, profile, "portal session", st.Portal, false)
	credLine(rep, profile, "Connect token", st.Connect, true)
	if !st.Portal.Present {
		return nil
	}
	return store
}

// portalProbe makes one portal call; any error is a warning, never a failure.
func portalProbe(ctx context.Context, rep *report, store auth.Store) {
	if store == nil {
		rep.warn("portal: skipped, no portal session")
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	id, err := probePortal(ctx, store)
	if err != nil {
		rep.warn("portal: %v", err)
		return
	}
	rep.ok("portal: signed in as developer %s", id)
}

// localChecks reports the SDK, Java and sign-in browser. Missing pieces only
// disable some tools, so they are warnings.
func localChecks(ctx context.Context, rep *report, profile string) {
	if s, err := resolveSDK(); err != nil {
		rep.warn("Connect IQ SDK: %v (install it with the SDK Manager: https://developer.garmin.com/connect-iq/sdk/)", err)
	} else {
		rep.ok("Connect IQ SDK %s (%s): %s", s.SDK.Version, s.Source, s.SDK.Path)
	}
	if j, err := findJava(ctx); err != nil {
		rep.warn("Java: %v", err)
	} else {
		v := j.Version
		if v == "" {
			v = "unknown version"
		}
		rep.ok("Java %s (%s): %s", v, j.Source, j.Path)
	}
	if b, err := findBrowser(); err != nil {
		rep.warn("browser: %v (needed for `%s`)", err, tool.LoginCommand(profile))
	} else {
		rep.ok("browser: %s", b)
	}
	if err := browserPreflight(ctx); err != nil {
		rep.warn("browser: %v", err)
	}
	if !hasDisplay() {
		rep.warn("display: DISPLAY and WAYLAND_DISPLAY are unset, so `%s` can't open a window; run `%s` and paste the Cookie of a signed-in apps.garmin.com request (DevTools → Network → Copy as cURL), or use GARMIN_MCP_PORTAL_COOKIES", tool.LoginCommand(profile), tool.LoginCommand(profile, "--paste"))
	}
}

// keyCheck reports the GARMIN_MCP_KEY signing key: set, present, and its
// public key SHA-256. It never prints key bytes.
func keyCheck(rep *report) {
	raw := os.Getenv(ciqsdk.EnvKeyVar)
	if raw == "" {
		rep.info("developer key: %s not set; to sign builds with your app's key, run `garmin-mcp config install <client> --key /absolute/path/developer_key.der` (until then each build needs a key file inside the project)", ciqsdk.EnvKeyVar)
		return
	}
	p, ok := ciqsdk.EnvKey()
	if !ok {
		rep.warn("developer key: %s=%s is not an absolute path to an existing file", ciqsdk.EnvKeyVar, raw)
		return
	}
	ki, err := ciqsdk.CheckKey(p)
	if err != nil {
		rep.warn("developer key: %v", err)
		return
	}
	line := fmt.Sprintf("developer key %s: RSA-%d, public key SHA-256 %s", ki.Path, ki.Bits, ki.Fingerprint)
	if len(ki.Warnings) > 0 {
		rep.warn("%s (%s)", line, strings.Join(ki.Warnings, "; "))
		return
	}
	rep.ok("%s", line)
}

// shellNote warns that doctor sees this shell's env, not the MCP client's:
// GUI clients never read shell exports, so a key or JAVA_HOME that is fine
// here can still be missing for the server.
func shellNote(rep *report) {
	if !fromShell() {
		return
	}
	rep.info("env: these checks use this shell's environment; your MCP client starts the server with its own, so set %s and JAVA_HOME in the client's garmin-mcp entry (`garmin-mcp config install <client> --key <abs path> --java-home <abs path>`), then restart the client", ciqsdk.EnvKeyVar)
}

// credLine reports one credential. Garmin Connect is opt-in, so a missing
// Connect token is information, not a problem.
func credLine(rep *report, profile, name string, c auth.CredStatus, optional bool) {
	if state := credstore.UnreadableState(c); state != "" {
		rep.fail("%s: %s (%s)", name, state, c.Note)
		return
	}
	if !c.Present && optional {
		rep.info("%s: not enabled (optional; enable with %s)", name, tool.LoginCommand(profile, "--connect"))
		return
	}
	if !c.Present {
		rep.warn("%s: missing; run `%s`%s", name, tool.LoginCommand(profile), note(c.Note))
		return
	}
	line := fmt.Sprintf("%s: present (%s)", name, c.Source)
	if !c.ExpiresAt.IsZero() {
		line += ", expires " + c.ExpiresAt.Local().Format(time.RFC3339)
	}
	if c.Renewable {
		line += ", renewable"
	}
	rep.ok("%s%s", line, note(c.Note))
}

func note(s string) string {
	if s == "" {
		return ""
	}
	return " (" + s + ")"
}
