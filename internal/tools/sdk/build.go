package sdk

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
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

	"github.com/idoo/connect-iq-mcp/internal/ciqsdk"
	"github.com/idoo/connect-iq-mcp/internal/sim"
	"github.com/idoo/connect-iq-mcp/internal/tool"
)

// allDevices is the sdk_build device value that exports the store package.
const allDevices = "all"

type buildIn struct {
	BuildID   string `json:"build_id,omitempty" jsonschema:"wait for a build that returned still_processing; other args are ignored"`
	SDK       string `json:"sdk,omitempty" jsonschema:"installed SDK version or dir name, e.g. 9.2.0 (default: CIQ_MCP_SDK env, current-sdk.cfg, newest installed); a custom SDK outside the SDK Manager dir only via CIQ_MCP_SDK"`
	Project   string `json:"project,omitempty" jsonschema:"project dir inside the root (default: root)"`
	Jungle    string `json:"jungle,omitempty" jsonschema:"jungle file relative to the project (default monkey.jungle)"`
	Device    string `json:"device,omitempty" jsonschema:"device id, e.g. fenix7 (writes <project>/bin/<device>.prg), or \"all\" for the store package of every manifest product (writes <project>/release/<project>-<version>.iq; slow)"`
	Key       string `json:"key,omitempty" jsonschema:"developer key (.der) inside the root (default: CIQ_MCP_KEY, set by the human, may be outside the root)"`
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
	if err := CheckProject(env, "project", project, pdir); err != nil {
		return prepared{}, err
	}
	if jungle == "" {
		jungle = "monkey.jungle"
	}
	if strings.Contains(jungle, ";") {
		return prepared{}, tool.InvalidArgs("jungle %q: one jungle file only (';' would make monkeyc read a second one)", jungle)
	}
	jabs, err := env.Root.Resolve(filepath.Join(pdir, jungle))
	if err != nil {
		return prepared{}, err
	}
	if _, err := os.Stat(jabs); err != nil {
		return prepared{}, tool.InvalidArgs("jungle %s: %v", jabs, err)
	}
	root, err := env.Root.Resolve(".")
	if err != nil {
		return prepared{}, err
	}
	// ciqsdk.Build checks again; checking here refuses before a job starts.
	if err := ciqsdk.CheckJungle(root, pdir, jabs); err != nil {
		return prepared{}, jungleArgs(err)
	}
	kabs, err := resolveKey(env, key)
	if err != nil {
		return prepared{}, err
	}
	if _, err := ciqsdk.CheckKey(kabs); err != nil {
		return prepared{}, tool.InvalidArgs("key: %v", err)
	}
	// No java means no build: the SDK's bin/monkeyc wrapper would run a bare
	// `java` that the shell may find in the project directory.
	j, err := ciqsdk.FindJava(ctx, os.Getenv)
	if err != nil {
		return prepared{}, fmt.Errorf("%w: java: %v; install a JDK 17+ and set JAVA_HOME to its absolute path (or put java on PATH) %s", tool.ErrUnavailable, err, ciqsdk.ClientEnvHint)
	}
	return prepared{se: se, opts: ciqsdk.BuildOpts{SDK: se.Res.SDK, Java: j.Path, ProjectDir: pdir, Root: root, Jungle: jabs, Key: kabs, Warnings: true}}, nil
}

// jungleArgs reports a jungle refused by ciqsdk.CheckJungle as invalid arguments.
func jungleArgs(err error) error {
	var je *ciqsdk.JungleError
	if errors.As(err, &je) {
		return tool.InvalidArgs("%v", je)
	}
	return err
}

// resolveKey returns the developer key path: an agent-supplied key confined
// to the root, else the human-set CIQ_MCP_KEY (the only path allowed
// outside the root; it is only ever read).
func resolveKey(env *tool.Env, key string) (string, error) {
	if key != "" {
		return env.Root.Resolve(key)
	}
	if p, ok := ciqsdk.EnvKey(); ok {
		return p, nil
	}
	if v := os.Getenv(ciqsdk.EnvKeyVar); v != "" {
		return "", tool.InvalidArgs("%s=%q is not an absolute path to an existing key file; ask the human to fix it %s", ciqsdk.EnvKeyVar, v, ciqsdk.ClientEnvHint)
	}
	return "", tool.InvalidArgs("key is required: ask the human to set %s to the app's existing developer key (it may live outside the repo) %s, or pass key inside the root. Only a brand-new app gets a new key from sdk_key_generate; updates must be signed with the key of the first upload", ciqsdk.EnvKeyVar, ciqsdk.ClientEnvHint)
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
		jobs := jobsOf(env)
		j := jobs.lookup(in.BuildID)
		if j == nil {
			return nil, tool.InvalidArgs("unknown build_id %q (finished results are returned once; builds are lost when the server restarts)", in.BuildID)
		}
		return jobs.wait(ctx, env, j)
	}
	all := in.Device == allDevices
	note := ""
	if !all {
		dr, err := dataRoot()
		if err != nil {
			return nil, err
		}
		if in.Device, note, err = ResolveDevice(dr, in.Device); err != nil {
			return nil, err
		}
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
	ident, err := buildIdentity(p, all)
	if err != nil {
		return nil, err
	}
	// The build outlives this call if needed (a poll with build_id, or the
	// same build again, picks it up), so cancelling the request does not
	// kill monkeyc; it runs until it finishes or the server stops.
	jobs := jobsOf(env)
	j, err := jobs.start(env.Lifetime, p, all, what, ident)
	if err != nil {
		return nil, err
	}
	res, err := jobs.wait(ctx, env, j)
	if err != nil {
		return nil, err
	}
	return withNote(res, note), nil
}

// buildIdentity names everything that reaches monkeyc's argv, plus the key
// file's content, so only a truly identical call joins a running build.
func buildIdentity(p prepared, all bool) (string, error) {
	key, err := os.ReadFile(p.opts.Key)
	if err != nil {
		return "", tool.InvalidArgs("key: %v", err)
	}
	sum := sha256.Sum256(key)
	o := p.opts
	return strings.Join([]string{
		o.Output, o.Device, o.Key, hex.EncodeToString(sum[:]), o.SDK.Path, o.Jungle,
		fmt.Sprint(all, o.Release, o.Test, o.NoClobber, o.Warnings, o.TypeCheck != nil),
	}, "\x00"), nil
}

// buildJob is a build running independently of the tool call that started it.
type buildJob struct {
	id     string
	what   string
	output string
	ident  string // buildIdentity of the call that started it
	start  time.Time
	done   chan struct{}
	// res, err and end are written before done is closed.
	res *tool.Result
	err error
	end time.Time
}

func (j *buildJob) finished() bool {
	select {
	case <-j.done:
		return true
	default:
		return false
	}
}

// jobKeep is how long a finished, never-collected result is kept.
const jobKeep = 30 * time.Minute

// jobRegistry is the server's background builds (one per [tool.Shared]).
// Jobs are added and removed only under mu; a job's result fields are
// written before its done channel closes, and every waiter gets a copy.
type jobRegistry struct {
	mu   sync.Mutex
	jobs map[string]*buildJob
	// buildWait bounds how long one sdk_build call waits before answering
	// still_processing; clients cancel a request after about 60 s. Tests
	// shorten it on their own registry.
	buildWait time.Duration
	// progressEvery is the progress note interval while a call waits.
	progressEvery time.Duration
}

func newJobRegistry() *jobRegistry {
	return &jobRegistry{jobs: map[string]*buildJob{}, buildWait: 45 * time.Second, progressEvery: 10 * time.Second}
}

// jobsOf returns the server's build registry.
func jobsOf(env *tool.Env) *jobRegistry { return tool.SharedValue(env.Shared, newJobRegistry) }

// start starts p under the server lifetime, or returns the running job
// with the same identity (an agent re-calling instead of polling, or a retry
// after a cancelled call, gets the same build_id). A running job writing the
// same output with other options is a conflict: joining it would hand back
// an artefact built with options this call did not ask for.
func (r *jobRegistry) start(life *tool.Lifetime, p prepared, all bool, what, ident string) (*buildJob, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked(time.Now())
	for _, j := range r.jobs {
		if j.output != p.opts.Output || j.finished() {
			continue
		}
		if j.ident == ident {
			return j, nil
		}
		return nil, &tool.Error{
			Code:    "conflict",
			Message: fmt.Sprintf("another build is writing %s (build_id %s); wait for it or choose another output", j.output, j.id),
			Hint:    "call sdk_build {\"build_id\": \"" + j.id + "\"} to wait for it, then retry this build",
		}
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	j := &buildJob{id: hex.EncodeToString(b[:]), what: what, output: p.opts.Output, ident: ident, start: time.Now(), done: make(chan struct{})}
	r.jobs[j.id] = j
	life.Go(func(ctx context.Context) {
		defer close(j.done)
		j.res, j.err = runBuild(ctx, p, all)
		j.end = time.Now()
	})
	return j, nil
}

// sweepLocked drops finished jobs whose result nobody collected within jobKeep.
func (r *jobRegistry) sweepLocked(now time.Time) {
	for id, j := range r.jobs {
		if j.finished() && now.Sub(j.end) > jobKeep {
			delete(r.jobs, id)
		}
	}
}

func (r *jobRegistry) lookup(id string) *buildJob {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked(time.Now())
	return r.jobs[id]
}

// wait waits up to buildWait for j, sending strictly increasing progress
// (no total). A finished job is removed and its result returned; otherwise
// the caller gets still_processing with the build_id to poll. A cancelled
// call leaves the job running: a poll or the same build again finds it.
func (r *jobRegistry) wait(ctx context.Context, env *tool.Env, j *buildJob) (*tool.Result, error) {
	r.mu.Lock()
	buildWait, progressEvery := r.buildWait, r.progressEvery
	r.mu.Unlock()
	timer := time.NewTimer(buildWait)
	defer timer.Stop()
	tick := time.NewTicker(progressEvery)
	defer tick.Stop()
	n := 1.0
	env.Progress(n, 0, j.what)
	for {
		select {
		case <-j.done:
			r.mu.Lock()
			delete(r.jobs, j.id)
			r.mu.Unlock()
			// Every joined caller writes its own Meta and render state.
			return j.res.Clone(), j.err
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
		return nil, jungleArgs(err)
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
	Path string `json:"path,omitempty" jsonschema:"developer key (.der) inside the root (default: CIQ_MCP_KEY)"`
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
		"note": "Back this key up somewhere safe outside the repo. The store binds your app to the key of its first upload; a lost key means no more updates. The human can point " + ciqsdk.EnvKeyVar + " at the backup " + ciqsdk.ClientEnvHint + ".",
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
	out, err := gitCmd(ctx, git, "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return nil //nolint:nilerr // not a git work tree
	}
	top := strings.TrimSpace(string(out))
	rel, err := filepath.Rel(top, filepath.Join(dir, rest))
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil //nolint:nilerr // outside the work tree
	}
	rel = filepath.ToSlash(rel)
	err = gitCmd(ctx, git, "-C", top, "check-ignore", "--no-index", "-q", "--", rel).Run()
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

// gitCmd runs git with repo-controlled code execution switched off: no
// fsmonitor or hooks from the project's .git/config, no system config, and
// no inherited GIT_* variables (GIT_DIR, GIT_WORK_TREE, GIT_CONFIG_*...)
// that could point the check at another repo. The user's global config stays,
// since their ignore rules live there.
func gitCmd(ctx context.Context, git string, args ...string) *exec.Cmd {
	a := append([]string{"-c", "core.fsmonitor=false", "-c", "core.hooksPath=" + os.DevNull}, args...)
	cmd := exec.CommandContext(ctx, git, a...) //nolint:gosec // G204: git from PATH, fixed flags, confined paths
	cmd.Env = gitEnv(os.Environ())
	return cmd
}

// gitEnv returns environ without GIT_* variables, plus GIT_CONFIG_NOSYSTEM=1.
func gitEnv(environ []string) []string {
	out := make([]string, 0, len(environ)+1)
	for _, kv := range environ {
		if !strings.HasPrefix(strings.ToUpper(kv), "GIT_") {
			out = append(out, kv)
		}
	}
	return append(out, "GIT_CONFIG_NOSYSTEM=1")
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
