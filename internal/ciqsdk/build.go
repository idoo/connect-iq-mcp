package ciqsdk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Diagnostic is one parsed monkeyc message.
type Diagnostic struct {
	Severity string `json:"severity"` // error | warning | info
	Device   string `json:"device,omitempty"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Col      int    `json:"col,omitempty"`
	Message  string `json:"message"`
}

var (
	diagLocRE  = regexp.MustCompile(`^(ERROR|WARNING): (\S+): (.+?):(\d+)(?:,(\d+))?: (.+)$`)
	diagBareRE = regexp.MustCompile(`^(ERROR|WARNING): (\S+): (.+)$`)
)

// ParseDiagnostics extracts ERROR/WARNING lines from monkeyc output.
// Non-matching lines are ignored (they stay in the raw tail).
func ParseDiagnostics(out string) []Diagnostic {
	var ds []Diagnostic
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if m := diagLocRE.FindStringSubmatch(line); m != nil {
			l, _ := strconv.Atoi(m[4])
			c, _ := strconv.Atoi(m[5])
			ds = append(ds, Diagnostic{Severity: strings.ToLower(m[1]), Device: m[2], File: m[3], Line: l, Col: c, Message: m[6]})
		} else if m := diagBareRE.FindStringSubmatch(line); m != nil {
			ds = append(ds, Diagnostic{Severity: strings.ToLower(m[1]), Device: m[2], Message: m[3]})
		}
	}
	return ds
}

// BuildOpts configures [Build] and [Export]. Paths are absolute.
type BuildOpts struct {
	SDK        SDK
	Java       string // java path; empty uses the bin/ wrapper
	ProjectDir string
	Jungle     string // default <ProjectDir>/monkey.jungle
	Device     string // required for Build, ignored by Export
	Key        string
	Output     string
	Release    bool
	Test       bool
	Warnings   bool          // -w
	TypeCheck  *int          // -l 0..3
	Timeout    time.Duration // default 2 min (Build) / 30 min (Export)
}

// BuildResult is the outcome of a monkeyc run.
type BuildResult struct {
	OK          bool         `json:"ok"`
	ExitCode    int          `json:"exit_code"`
	Output      string       `json:"output"`
	Diagnostics []Diagnostic `json:"diagnostics"`
	Errors      int          `json:"errors"`
	Warnings    int          `json:"warnings"`
	Tail        string       `json:"tail,omitempty"`
	DurationMS  int64        `json:"duration_ms"`
	Argv        []string     `json:"-"`
}

// MaxTail caps the raw output tail returned on failure.
const MaxTail = 4096

// BuildArgs returns monkeyc arguments for o (export adds -e -r, no -d).
func BuildArgs(o BuildOpts, export bool) ([]string, error) {
	if o.ProjectDir == "" || o.Key == "" || o.Output == "" {
		return nil, errors.New("project, key and output are required")
	}
	if o.Release && o.Test {
		return nil, errors.New("release and test builds are mutually exclusive")
	}
	jungle := o.Jungle
	if jungle == "" {
		jungle = "monkey.jungle"
	}
	a := []string{"-f", jungle}
	if export {
		if o.Test {
			return nil, errors.New("export cannot be a test build")
		}
		a = append(a, "-e", "-r")
	} else {
		if o.Device == "" {
			return nil, errors.New("device is required")
		}
		if !ValidDeviceID(o.Device) {
			return nil, fmt.Errorf("invalid device id %q", o.Device)
		}
		a = append(a, "-d", o.Device)
		if o.Release {
			a = append(a, "-r")
		}
		if o.Test {
			a = append(a, "-t")
		}
	}
	a = append(a, "-y", o.Key, "-o", o.Output)
	if o.Warnings {
		a = append(a, "-w")
	}
	if o.TypeCheck != nil {
		if *o.TypeCheck < 0 || *o.TypeCheck > 3 {
			return nil, errors.New("type_check must be 0..3")
		}
		a = append(a, "-l", strconv.Itoa(*o.TypeCheck))
	}
	return a, nil
}

// Build compiles one device PRG.
func Build(ctx context.Context, o BuildOpts) (BuildResult, error) {
	if o.Timeout == 0 {
		o.Timeout = 2 * time.Minute
	}
	return run(ctx, o, false)
}

// Export builds the store .iq for all manifest products (-e -r).
func Export(ctx context.Context, o BuildOpts) (BuildResult, error) {
	if o.Timeout == 0 {
		o.Timeout = 30 * time.Minute
	}
	o.Release = true
	return run(ctx, o, true)
}

func run(ctx context.Context, o BuildOpts, export bool) (BuildResult, error) {
	args, err := BuildArgs(o, export)
	if err != nil {
		return BuildResult{}, err
	}
	argv := o.SDK.MonkeycArgv(o.Java, args)
	cctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	cmd.Dir = o.ProjectDir
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	start := time.Now()
	err = cmd.Run()
	r := BuildResult{Argv: argv, DurationMS: time.Since(start).Milliseconds(), Output: o.Output}
	out := buf.String()
	var ee *exec.ExitError
	switch {
	case err == nil:
	case cctx.Err() == context.DeadlineExceeded:
		return r, fmt.Errorf("monkeyc timed out after %s; %s", o.Timeout, tail(out))
	case ctx.Err() != nil:
		return r, ctx.Err()
	case errors.As(err, &ee):
		r.ExitCode = ee.ExitCode()
	default:
		return r, fmt.Errorf("start monkeyc: %w", err)
	}
	r.Diagnostics = ParseDiagnostics(out)
	for _, d := range r.Diagnostics {
		switch d.Severity {
		case "error":
			r.Errors++
		case "warning":
			r.Warnings++
		}
	}
	r.OK = r.ExitCode == 0
	if !r.OK {
		r.Tail = tail(out)
		if r.Errors == 0 {
			r.Diagnostics = append(r.Diagnostics, Diagnostic{Severity: "error", Message: fmt.Sprintf("monkeyc exited %d: %s", r.ExitCode, lastLine(out))})
			r.Errors = 1
		}
	}
	if r.Diagnostics == nil {
		r.Diagnostics = []Diagnostic{}
	}
	return r, nil
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > MaxTail {
		s = "…" + s[len(s)-MaxTail:]
	}
	return s
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return strings.TrimSpace(s)
}
