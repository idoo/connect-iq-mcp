package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/idoo/garmin-mcp/internal/auth"
	"github.com/idoo/garmin-mcp/internal/browser"
	"github.com/idoo/garmin-mcp/internal/ciqsdk"
	"github.com/idoo/garmin-mcp/internal/config"
	"github.com/idoo/garmin-mcp/internal/portal"
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
	localChecks(ctx, rep)
	if p.AllowWrites {
		rep.ok("mode: writes enabled")
	} else {
		rep.ok("mode: read-only (use --allow-writes or allow_writes=true)")
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
// returns the store when portal cookies are present, else nil.
func credentialChecks(ctx context.Context, rep *report, profile string) auth.Store {
	if auth.Open == nil {
		rep.warn("credentials: no credential store in this build")
		return nil
	}
	store, err := auth.Open(ctx, profile)
	if err != nil {
		rep.fail("credentials: %v", err)
		return nil
	}
	if store == nil {
		rep.warn("credentials: none configured; run `garmin-mcp auth login`")
		return nil
	}
	st := store.Status(ctx)
	credLine(rep, "portal cookies", st.Portal)
	credLine(rep, "Connect token", st.Connect)
	if !st.Portal.Present {
		return nil
	}
	return store
}

// portalProbe makes one portal call; any error is a warning, never a failure.
func portalProbe(ctx context.Context, rep *report, store auth.Store) {
	if store == nil {
		rep.warn("portal: skipped, no portal cookies")
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
func localChecks(ctx context.Context, rep *report) {
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
		rep.warn("browser: %v (needed for `garmin-mcp auth login`)", err)
	} else {
		rep.ok("browser: %s", b)
	}
}

func credLine(rep *report, name string, c auth.CredStatus) {
	if !c.Present {
		rep.warn("%s: missing; run `garmin-mcp auth login`%s", name, note(c.Note))
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
