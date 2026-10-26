package sdk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/idoo/garmin-mcp/internal/ciqsdk"
	"github.com/idoo/garmin-mcp/internal/sim"
	"github.com/idoo/garmin-mcp/internal/tool"
)

// allDevices is the sdk_build device value that exports the store package.
const allDevices = "all"

type buildIn struct {
	SDK       string `json:"sdk,omitempty" jsonschema:"installed SDK version or dir name, e.g. 9.2.0 (default: GARMIN_MCP_SDK env, current-sdk.cfg, newest installed); a custom SDK outside the SDK Manager dir only via GARMIN_MCP_SDK"`
	Project   string `json:"project,omitempty" jsonschema:"project dir inside the root (default: root)"`
	Jungle    string `json:"jungle,omitempty" jsonschema:"jungle file relative to the project (default monkey.jungle)"`
	Device    string `json:"device" jsonschema:"device id, e.g. fenix7 (writes <project>/bin/<device>.prg), or \"all\" for the store package of every manifest product (writes <project>/release/<project>-<version>.iq; slow)"`
	Key       string `json:"key" jsonschema:"developer key (.der) inside the root"`
	Release   bool   `json:"release,omitempty" jsonschema:"release build (-r); always on for device \"all\""`
	Test      bool   `json:"test,omitempty" jsonschema:"unit-test build (-t) of one device; not with release"`
	Overwrite bool   `json:"overwrite,omitempty" jsonschema:"device \"all\": replace an existing .iq of the same version"`
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

// maxDiagnostics caps diagnostics returned in a result.
const maxDiagnostics = 200

// Build time limits: one device, the store package of every product.
const (
	deviceTimeout = 2 * time.Minute
	exportTimeout = 30 * time.Minute
)

// versionRE bounds the manifest version used in the .iq file name.
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
	if key == "" {
		return prepared{}, tool.InvalidArgs("key is required (sdk_key_generate creates one)")
	}
	kabs, err := env.Root.Resolve(key)
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
	all := in.Device == allDevices
	if !all && !ciqsdk.ValidDeviceID(in.Device) {
		return nil, tool.InvalidArgs("invalid device id %q (or \"all\" for the store package)", in.Device)
	}
	if in.Test && (in.Release || all) {
		return nil, tool.InvalidArgs("test builds one device in debug mode; not with release or device \"all\"")
	}
	p, err := prepare(ctx, env, in.SDK, in.Project, in.Jungle, in.Key)
	if err != nil {
		return nil, err
	}
	if all {
		p.opts.Output, err = packagePath(env, p.opts.ProjectDir, in.Overwrite)
		if err != nil {
			return nil, err
		}
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
	// Clients without a progress token cancel after about a minute; stop
	// monkeyc before that so the call ends with an answer, not a hang.
	capped := !env.HasProgress()
	if capped && p.opts.Timeout > tool.NoProgressWaitCap {
		p.opts.Timeout = tool.NoProgressWaitCap
	}
	start := time.Now()
	stop := reportProgress(env, what, start)
	var r ciqsdk.BuildResult
	if all {
		r, err = ciqsdk.Export(ctx, p.opts)
	} else {
		r, err = ciqsdk.Build(ctx, p.opts)
	}
	stop()
	if err != nil {
		if capped && time.Since(start) >= p.opts.Timeout {
			return nil, fmt.Errorf("%w (this client sent no progress token, so the build is capped at %s; use a client that sends progress notifications for long exports)", err, tool.NoProgressWaitCap)
		}
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

// packagePath returns <pdir>/release/<project>-<version>.iq, vetted by
// sim.CheckDest so a previous package is replaced only with overwrite.
func packagePath(env *tool.Env, pdir string, overwrite bool) (string, error) {
	m, _, err := readManifest(env, pdir)
	if err != nil {
		return "", err
	}
	name := filepath.Base(pdir)
	if m.Version != "" {
		if !versionRE.MatchString(m.Version) {
			return "", tool.InvalidArgs("manifest version %q is not usable in a file name", m.Version)
		}
		name += "-" + m.Version
	}
	abs, err := env.Root.Resolve(filepath.Join(pdir, "release", name+".iq"))
	if err != nil {
		return "", err
	}
	if err := sim.CheckDest(abs, overwrite, ".iq"); err != nil {
		return "", tool.InvalidArgs("%v", err)
	}
	return abs, nil
}

// reportProgress sends a progress note now and every 10 s until stop is called.
func reportProgress(env *tool.Env, what string, start time.Time) (stop func()) {
	env.Progress(0, 0, what)
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				env.Progress(0, 0, fmt.Sprintf("%s running for %s", what, time.Since(start).Round(time.Second)))
			}
		}
	}()
	return func() { close(done) }
}

func orDot(s string) string {
	if strings.TrimSpace(s) == "" {
		return "."
	}
	return s
}

// --- keys ---

type keyGenIn struct {
	Path string `json:"path" jsonschema:"new key file inside the root, e.g. keys/developer_key.der"`
}

type keyCheckIn struct {
	Path string `json:"path" jsonschema:"developer key (.der) inside the root"`
}

func sdkKeyGenerate(_ context.Context, env *tool.Env, in keyGenIn) (*tool.Result, error) {
	abs, err := env.Root.Resolve(in.Path)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(abs); err == nil {
		return nil, tool.InvalidArgs("%s already exists; refusing to overwrite a developer key", abs)
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
		"note": "Back this key up somewhere safe. The store binds your app to the key of its first upload; a lost key means no more updates. Keep it out of git.",
	}), nil
}

func sdkKeyCheck(_ context.Context, env *tool.Env, in keyCheckIn) (*tool.Result, error) {
	abs, err := env.Root.Resolve(in.Path)
	if err != nil {
		return nil, err
	}
	ki, err := ciqsdk.CheckKey(abs)
	if err != nil {
		return nil, tool.InvalidArgs("%v", err)
	}
	return tool.OK(ki), nil
}
