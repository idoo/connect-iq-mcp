package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/idoo/garmin-mcp/internal/auth"
	"github.com/idoo/garmin-mcp/internal/config"
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
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return usageErr(err.Error())
	}
	rep := &report{w: stdout}
	doctor(ctx, rep, *profile)
	if rep.failed {
		return errors.New("doctor found problems")
	}
	return nil
}

func doctor(ctx context.Context, rep *report, profile string) {
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
	credentialChecks(ctx, rep, p.Name)
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

// credentialChecks reports each credential's state without secrets.
func credentialChecks(ctx context.Context, rep *report, profile string) {
	if auth.Open == nil {
		rep.warn("credentials: no credential store in this build")
		return
	}
	store, err := auth.Open(ctx, profile)
	if err != nil {
		rep.fail("credentials: %v", err)
		return
	}
	if store == nil {
		rep.warn("credentials: none configured; run `garmin-mcp auth login`")
		return
	}
	st := store.Status(ctx)
	credLine(rep, "portal cookies", st.Portal)
	credLine(rep, "Connect token", st.Connect)
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
