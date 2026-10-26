package sdk

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/idoo/garmin-mcp/internal/ciqsdk"
	"github.com/idoo/garmin-mcp/internal/sim"
	"github.com/idoo/garmin-mcp/internal/tool"
)

// allDevices is the sdk_build device value that exports the store package.
const allDevices = "all"

type buildIn struct {
	BuildID   string `json:"build_id,omitempty" jsonschema:"wait for a build that returned still_processing; other args are ignored"`
	SDK       string `json:"sdk,omitempty" jsonschema:"installed SDK version or dir name, e.g. 9.2.0 (default: GARMIN_MCP_SDK env, current-sdk.cfg, newest installed); a custom SDK outside the SDK Manager dir only via GARMIN_MCP_SDK"`
	Project   string `json:"project,omitempty" jsonschema:"project dir inside the root (default: root)"`
	Jungle    string `json:"jungle,omitempty" jsonschema:"jungle file relative to the project (default monkey.jungle)"`
	Device    string `json:"device,omitempty" jsonschema:"device id, e.g. fenix7 (writes <project>/bin/<device>.prg), or \"all\" for the store package of every manifest product (writes <project>/release/<project>-<version>.iq; slow)"`
	Key       string `json:"key,omitempty" jsonschema:"developer key (.der) inside the root (default: GARMIN_MCP_KEY, set by the human, may be outside the root)"`
	Release   bool   `json:"release,omitempty" jsonschema:"release build (-r); always on for device \"all\""`
	Test      bool   `json:"test,omitempty" jsonschema:"unit-test build (-t) of one device; not with release"`
	Version   string `json:"version,omitempty" jsonschema:"device \"all\": version for the .iq name (default: manifest version, else a YYYYMMDD-HHMMSS timestamp)"`
	Overwrite bool   `json:"overwrite,omitempty" jsonschema:"device \"all\": replace an existing .iq of the same name"`
}

type buildOut struct {
	OK          bool                `json:"ok"`
	Output      string              `json:"output"`
	SDK         string              `json:"sdk"`
	SDKSource   string              `json:"sdk_source"`
	ExitCode    int                 `json:"exit_code"`
	Errors      int                 `json:"errors"`
	Warnings    int                 `json:"warnings"`
	Diagnostics []ciqsdk.Diagnostic `json:"diagnostics"`
	ByDevice    map[string]int      `json:"errors_by_device,omitempty"`
	Tail        string              `json:"tail,omitempty"`
	DurationMS  int64               `json:"duration_ms"`
}

// pendingOut is returned while a build keeps running in the background.
type pendingOut struct {
	Status    string `json:"status"`
	BuildID   string `json:"build_id"`
	Running   string `json:"running"`
	ElapsedMS int64  `json:"elapsed_ms"`
	Next      string `json:"next"`
}

// maxDiagnostics caps diagnostics returned in a result.
const maxDiagnostics = 200

// Build time limits: one device, the store package of every product.
const (
	deviceTimeout = 2 * time.Minute
	exportTimeout = 30 * time.Minute
)

// buildWait bounds how long one sdk_build call waits before answering
// still_processing; clients cancel a request after about 60 s. Tests shorten it.
var buildWait = 45 * time.Second

// progressEvery is the progress note interval while a call waits.
var progressEvery = 10 * time.Second

// versionRE bounds the version used in the .iq file name.
var versionRE = regexp.MustCompile(`^[0-9A-Za-z][0-9A-Za-z._+-]{0,63}$`)

// prepared holds resolved, confined build inputs; opts.Output is set by the caller.
type prepared struct {
	se   sdkEnv
	opts ciqsdk.BuildOpts
}

func prepare(ctx context.Context, env *tool.Env, sdkArg, project, jungle, key string) (prepared, error) {
	se, err := resolveSDK(sdkArg)
	if err != nil {
		return prepared{}, err
	}
	pdir, err := env.Root.Resolve(orDot(project))
	if err != nil {
		return prepared{}, err
	}
	if st, err := os.Stat(pdir); err != nil || !st.IsDir() {
		return prepared{}, tool.InvalidArgs("project %s is not a directory", pdir)
	}
	if jungle == "" {
		jungle = "monkey.jungle"
	}
	jabs, err := env.Root.Resolve(filepath.Join(pdir, jungle))
	if err != nil {
		return prepared{}, err
	}
	if _, err := os.Stat(jabs); err != nil {
		return prepared{}, tool.InvalidArgs("jungle %s: %v", jabs, err)
	}
	kabs, err := resolveKey(env, key)
	if err != nil {
		return prepared{}, err
	}
	if _, err := ciqsdk.CheckKey(kabs); err != nil {
		return prepared{}, tool.InvalidArgs("key: %v", err)
	}
	var java string
	if j, err := ciqsdk.FindJava(ctx, os.Getenv); err == nil {
		java = j.Path
	}
	return prepared{se: se, opts: ciqsdk.BuildOpts{SDK: se.Res.SDK, Java: java, ProjectDir: pdir, Jungle: jabs, Key: kabs, Warnings: true}}, nil
}

// resolveKey returns the developer key path: an agent-supplied key confined
// to the root, else the human-set GARMIN_MCP_KEY (the only path allowed
// outside the root; it is only ever read).
func resolveKey(env *tool.Env, key string) (string, error) {
	if key != "" {
		return env.Root.Resolve(key)
	}
	if p, ok := ciqsdk.EnvKey(); ok {
		return p, nil
	}
	if v := os.Getenv(ciqsdk.EnvKeyVar); v != "" {
		return "", tool.InvalidArgs("%s=%q is not an absolute path to an existing key file; ask the human to fix it", ciqsdk.EnvKeyVar, v)
	}
	return "", tool.InvalidArgs("key is required: ask the human to set %s to the app's existing developer key (it may live outside the repo), or pass key inside the root. Only a brand-new app gets a new key from sdk_key_generate; updates must be signed with the key of the first upload", ciqsdk.EnvKeyVar)
}

func outFor(p prepared, r ciqsdk.BuildResult) buildOut {
	o := buildOut{
		OK: r.OK, Output: r.Output, SDK: p.se.Res.SDK.Version, SDKSource: p.se.Res.Source, ExitCode: r.ExitCode,
		Errors: r.Errors, Warnings: r.Warnings, Diagnostics: r.Diagnostics, Tail: r.Tail, DurationMS: r.DurationMS,
	}
	// Errors first, then cap.
	sort.SliceStable(o.Diagnostics, func(i, j int) bool {
		return o.Diagnostics[i].Severity == "error" && o.Diagnostics[j].Severity != "error"
	})
	if len(o.Diagnostics) > maxDiagnostics {
		o.Diagnostics = o.Diagnostics[:maxDiagnostics]
	}
	if !r.OK {
		o.Output = ""
	}
	return o
}

func sdkBuild(ctx context.Context, env *tool.Env, in buildIn) (*tool.Result, error) {
	if in.BuildID != "" {
		j := lookupJob(in.BuildID)
		if j == nil {
			return nil, tool.InvalidArgs("unknown build_id %q (finished results are returned once; builds are lost when the server restarts)", in.BuildID)
		}
		return waitJob(ctx, env, j)
	}
	all := in.Device == allDevices
	if !all && !ciqsdk.ValidDeviceID(in.Device) {
		return nil, tool.InvalidArgs("invalid device id %q (or \"all\" for the store package)", in.Device)
	}
	if in.Test && (in.Release || all) {
		return nil, tool.InvalidArgs("test builds one device in debug mode; not with release or device \"all\"")
	}
	if in.Version != "" && (!all || !versionRE.MatchString(in.Version)) {
		return nil, tool.InvalidArgs("version %q: only for device \"all\", letters, digits and ._+- (e.g. 1.4.0)", in.Version)
	}
	p, err := prepare(ctx, env, in.SDK, in.Project, in.Jungle, in.Key)
	if err != nil {
		return nil, err
	}
	if all {
		p.opts.Output, err = packagePath(env, p.opts.ProjectDir, in.Version, in.Overwrite)
		if err != nil {
			return nil, err
		}
		// Install with a link that fails on an existing file, so a
		// concurrent export of the same name cannot replace this one.
		p.opts.NoClobber = !in.Overwrite
	} else {
		if _, err := ciqsdk.LoadDevice(p.se.DataRoot, in.Device); errors.Is(err, os.ErrNotExist) {
			return nil, tool.InvalidArgs("device %q is not installed; download it with SdkManager", in.Device)
		}
		suffix := ""
		if in.Test {
			suffix = "-test"
		}
		if p.opts.Output, err = env.Root.Resolve(filepath.Join(p.opts.ProjectDir, "bin", in.Device+suffix+".prg")); err != nil {
			return nil, err
		}
		p.opts.Device, p.opts.Release, p.opts.Test = in.Device, in.Release, in.Test
	}
	p.opts.Timeout = deviceTimeout
	what := "monkeyc -d " + in.Device
	if all {
		p.opts.Timeout, what = exportTimeout, "monkeyc -e -r (all manifest products)"
	}
	// The build outlives this call if needed (a poll with build_id picks
	// it up), so cancelling the request does not kill monkeyc.
	return waitJob(ctx, env, startJob(context.WithoutCancel(ctx), p, all, what))
}

// buildJob is a build running independently of the tool call that started it.
type buildJob struct {
	id    string
	what  string
	start time.Time
	done  chan struct{}
	res   *tool.Result
	err   error
}

var (
	jobsMu sync.Mutex
	jobs   = map[string]*buildJob{}
)

func startJob(ctx context.Context, p prepared, all bool, what string) *buildJob {
	var b [8]byte
	_, _ = rand.Read(b[:])
	j := &buildJob{id: hex.EncodeToString(b[:]), what: what, start: time.Now(), done: make(chan struct{})}
	jobsMu.Lock()
	jobs[j.id] = j
	jobsMu.Unlock()
	go func() {
		defer close(j.done)
		j.res, j.err = runBuild(ctx, p, all)
	}()
	return j
}

func lookupJob(id string) *buildJob {
	jobsMu.Lock()
	defer jobsMu.Unlock()
	return jobs[id]
}

// waitJob waits up to buildWait for j, sending strictly increasing progress
// (no total). A finished job is removed and its result returned; otherwise
// the caller gets still_processing with the build_id to poll.
func waitJob(ctx context.Context, env *tool.Env, j *buildJob) (*tool.Result, error) {
	timer := time.NewTimer(buildWait)
	defer timer.Stop()
	tick := time.NewTicker(progressEvery)
	defer tick.Stop()
	n := 1.0
	env.Progress(n, 0, j.what)
	for {
		select {
		case <-j.done:
			jobsMu.Lock()
			delete(jobs, j.id)
			jobsMu.Unlock()
			return j.res, j.err
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
			n++
			env.Progress(n, 0, fmt.Sprintf("%s running for %s", j.what, time.Since(j.start).Round(time.Second)))
		case <-timer.C:
			return tool.OK(pendingOut{
				Status: "still_processing", BuildID: j.id, Running: j.what,
				ElapsedMS: time.Since(j.start).Milliseconds(),
				Next:      "call sdk_build {\"build_id\": \"" + j.id + "\"} to wait for the result",
			}), nil
		}
	}
}

func runBuild(ctx context.Context, p prepared, all bool) (*tool.Result, error) {
	var r ciqsdk.BuildResult
	var err error
	if all {
		r, err = ciqsdk.Export(ctx, p.opts)
	} else {
		r, err = ciqsdk.Build(ctx, p.opts)
	}
	if err != nil {
		return nil, err
	}
	o := outFor(p, r)
	if all {
		o.ByDevice = map[string]int{}
		for _, d := range r.Diagnostics {
			if d.Severity == "error" && d.Device != "" {
				o.ByDevice[d.Device]++
			}
		}
	}
	return tool.OK(o), nil
}

// packagePath returns <pdir>/release/<project>-<version>.iq, with version
// from the argument, else the manifest, else a timestamp; vetted by
// sim.CheckDest so a previous package is replaced only with overwrite.
func packagePath(env *tool.Env, pdir, version string, overwrite bool) (string, error) {
	if version == "" {
		m, _, err := readManifest(env, pdir)
		if err != nil {
			return "", err
		}
		if m.Version != "" && !versionRE.MatchString(m.Version) {
			return "", tool.InvalidArgs("manifest version %q is not usable in a file name; pass version", m.Version)
		}
		version = m.Version
	}
	if version == "" {
		version = time.Now().Format("20060102-150405")
	}
	abs, err := env.Root.Resolve(filepath.Join(pdir, "release", filepath.Base(pdir)+"-"+version+".iq"))
	if err != nil {
		return "", err
	}
	if err := sim.CheckDest(abs, overwrite, ".iq"); err != nil {
		return "", tool.InvalidArgs("%v", err)
	}
	return abs, nil
}

func orDot(s string) string {
	if strings.TrimSpace(s) == "" {
		return "."
	}
	return s
}

// --- keys ---

type keyGenIn struct {
	Path string `json:"path" jsonschema:"new key file inside the root and ignored by git, e.g. keys/developer_key.der"`
}

type keyCheckIn struct {
	Path string `json:"path,omitempty" jsonschema:"developer key (.der) inside the root (default: GARMIN_MCP_KEY)"`
}

func sdkKeyGenerate(ctx context.Context, env *tool.Env, in keyGenIn) (*tool.Result, error) {
	abs, err := env.Root.Resolve(in.Path)
	if err != nil {
		return nil, err
	}
	if p, ok := ciqsdk.EnvKey(); ok && sameFile(p, abs) {
		return nil, tool.InvalidArgs("%s is the %s key; it is never written", abs, ciqsdk.EnvKeyVar)
	}
	if _, err := os.Lstat(abs); err == nil {
		return nil, tool.InvalidArgs("%s already exists; refusing to overwrite a developer key", abs)
	}
	if err := gitIgnored(ctx, abs); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, err
	}
	ki, err := ciqsdk.GenerateKey(abs)
	if err != nil {
		return nil, err
	}
	return tool.OK(map[string]any{
		"key":  ki,
		"note": "Back this key up somewhere safe outside the repo. The store binds your app to the key of its first upload; a lost key means no more updates. The human can point " + ciqsdk.EnvKeyVar + " at the backup.",
	}), nil
}

// sameFile reports whether a and b name the same file (b may not exist).
func sameFile(a, b string) bool {
	if filepath.Clean(a) == filepath.Clean(b) {
		return true
	}
	sa, errA := os.Stat(a)
	sb, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(sa, sb)
}

// gitIgnored refuses a key path inside a git work tree unless git ignores it,
// so one `git add -A` cannot publish the key. Outside a work tree (or without
// git) it allows the path.
func gitIgnored(ctx context.Context, abs string) error {
	git, err := exec.LookPath("git")
	if err != nil {
		return nil //nolint:nilerr // no git binary: nothing can commit the key through git
	}
	// Resolve the deepest existing ancestor so the path matches git's
	// symlink-free work tree path (e.g. /var vs /private/var on macOS).
	dir, rest := filepath.Dir(abs), filepath.Base(abs)
	for {
		if st, err := os.Stat(dir); err == nil && st.IsDir() {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	out, err := exec.CommandContext(ctx, git, "-C", dir, "rev-parse", "--show-toplevel").Output() //nolint:gosec // G204: git from PATH, fixed args
	if err != nil {
		return nil //nolint:nilerr // not a git work tree
	}
	top := strings.TrimSpace(string(out))
	rel, err := filepath.Rel(top, filepath.Join(dir, rest))
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil //nolint:nilerr // outside the work tree
	}
	rel = filepath.ToSlash(rel)
	err = exec.CommandContext(ctx, git, "-C", top, "check-ignore", "-q", "--", rel).Run() //nolint:gosec // G204: git from PATH, rel is a confined path
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return tool.InvalidArgs("%s is inside the git work tree %s and not ignored; add the line /%s to %s first (or have the human keep the key outside the repo and set %s)",
			abs, top, rel, filepath.Join(top, ".gitignore"), ciqsdk.EnvKeyVar)
	}
	return fmt.Errorf("git check-ignore %s: %w", rel, err)
}

func sdkKeyCheck(_ context.Context, env *tool.Env, in keyCheckIn) (*tool.Result, error) {
	abs, err := resolveKey(env, in.Path)
	if err != nil {
		return nil, err
	}
	ki, err := ciqsdk.CheckKey(abs)
	if err != nil {
		return nil, tool.InvalidArgs("%v", err)
	}
	return tool.OK(ki), nil
}
