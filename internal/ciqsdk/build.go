package ciqsdk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
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
	Java       string // absolute java path; empty runs FindJava (its error fails the build)
	ProjectDir string
	// Root bounds every path the jungle names (the server's file root);
	// empty means ProjectDir.
	Root      string
	Jungle    string // default <ProjectDir>/monkey.jungle
	Device    string // required for Build, ignored by Export
	Key       string
	Output    string
	Release   bool
	Test      bool
	Warnings  bool          // -w
	TypeCheck *int          // -l 0..3
	Timeout   time.Duration // default 2 min (Build) / 30 min (Export)
	// NoClobber installs Output with a hard link that fails if the file
	// already exists, so a concurrent build never replaces it.
	NoClobber bool
	// findJava replaces [FindJava] when Java is empty; nil is FindJava (tests set it).
	findJava func(context.Context, func(string) string) (Java, error)
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
	if strings.Contains(jungle, ";") {
		return nil, fmt.Errorf("jungle path %q contains ';', which monkeyc reads as a list of jungle files", jungle)
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
	if _, err := BuildArgs(o, export); err != nil {
		return BuildResult{}, err
	}
	// Every caller gets the jungle confinement before monkeyc can run.
	if err := CheckJungle(o.Root, o.ProjectDir, o.Jungle); err != nil {
		return BuildResult{}, err
	}
	java, err := requireJava(ctx, o.Java, o.findJava)
	if err != nil {
		return BuildResult{}, err
	}
	if !exists(o.SDK.Jar()) {
		return BuildResult{}, fmt.Errorf("sdk %s has no bin/monkeybrains.jar; reinstall it with SdkManager", o.SDK.Path)
	}
	if err := checkOutputDest(o.Output); err != nil {
		return BuildResult{}, err
	}
	// monkeyc writes the artefact plus gen/, mir/ and debug files next to
	// -o; build into a private temp dir so none of it can follow a repo
	// symlink, then move only the top-level artefacts.
	tmp, err := os.MkdirTemp("", "garmin-mcp-build-")
	if err != nil {
		return BuildResult{}, err
	}
	defer os.RemoveAll(tmp)
	to := o
	to.Output = filepath.Join(tmp, filepath.Base(o.Output))
	args, err := BuildArgs(to, export)
	if err != nil {
		return BuildResult{}, err
	}
	argv := o.SDK.MonkeycArgv(java, args)
	cctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...) //nolint:gosec // G204: argv is the resolved SDK's monkeyc/java with BuildArgs-validated paths; no shell
	cmd.Dir = o.ProjectDir
	ownGroup(cmd)
	// Grandchildren holding the output pipe must not keep Run waiting.
	cmd.WaitDelay = 5 * time.Second
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
	if r.OK {
		noClobber := ""
		if o.NoClobber {
			noClobber = filepath.Base(o.Output)
		}
		if err := moveArtefacts(tmp, filepath.Dir(o.Output), noClobber); err != nil {
			return r, fmt.Errorf("install build output: %w", err)
		}
	}
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

// checkOutputDest refuses an output path whose file or parent directory is
// a symlink, and creates the parent directory if missing.
func checkOutputDest(out string) error {
	if !filepath.IsAbs(out) {
		return fmt.Errorf("output %q must be absolute", out)
	}
	dir := filepath.Dir(out)
	for _, p := range []string{out, dir} {
		if st, err := os.Lstat(p); err == nil && st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing to write build output through symlink %s", p)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // G301: build output dir in the user's project; normal project-dir permissions
		return err
	}
	if st, err := os.Lstat(dir); err != nil {
		return err
	} else if !st.IsDir() {
		return fmt.Errorf("output directory %s is not a directory", dir)
	}
	return nil
}

// moveArtefacts moves the regular files at the top of src (the .prg/.iq,
// .debug.xml, settings json) into dst. Subdirectories (gen/, mir/) are
// dropped. Each file is staged in dst and renamed over its target, so an
// existing symlink at the target is replaced, never followed. The file named
// noClobber (if any) is moved first and linked instead, failing if it exists.
func moveArtefacts(src, dst, noClobber string) error {
	if st, err := os.Lstat(dst); err != nil {
		return err
	} else if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
		return fmt.Errorf("refusing to write build output into %s: not a real directory", dst)
	}
	ents, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if noClobber != "" {
		if err := moveFile(filepath.Join(src, noClobber), dst, noClobber, true); err != nil {
			return err
		}
	}
	for _, e := range ents {
		if !e.Type().IsRegular() || e.Name() == noClobber {
			continue
		}
		if err := moveFile(filepath.Join(src, e.Name()), dst, e.Name(), false); err != nil {
			return err
		}
	}
	return nil
}

// moveFile copies from into a staged temp file in dir, then installs it as
// name: renamed over the target, or with link true hard-linked so an
// existing target makes it fail with os.ErrExist.
func moveFile(from, dir, name string, link bool) error {
	in, err := os.Open(from) //nolint:gosec // G304: from is a regular file in our own temp build dir
	if err != nil {
		return err
	}
	defer in.Close()
	f, err := os.CreateTemp(dir, "."+name+".tmp-*")
	if err != nil {
		return err
	}
	_, err = io.Copy(f, in)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		if link {
			err = os.Link(f.Name(), filepath.Join(dir, name))
			if errors.Is(err, os.ErrExist) {
				err = fmt.Errorf("%s already exists (written concurrently?); refusing to replace it", filepath.Join(dir, name))
			}
			os.Remove(f.Name())
			return err
		}
		err = os.Rename(f.Name(), filepath.Join(dir, name))
	}
	if err != nil {
		os.Remove(f.Name())
	}
	return err
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
