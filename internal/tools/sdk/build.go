package sdk

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/idoo/ciq-mcp/internal/ciqsdk"
	"github.com/idoo/ciq-mcp/internal/tool"
)

type buildIn struct {
	SDK       string `json:"sdk,omitempty" jsonschema:"SDK path or installed version, e.g. 9.2.0; default CIQ_MCP_SDK/CIQ_SDK, current-sdk.cfg, newest"`
	Project   string `json:"project,omitempty" jsonschema:"project dir inside the root (default: root)"`
	Jungle    string `json:"jungle,omitempty" jsonschema:"jungle file relative to the project (default monkey.jungle)"`
	Device    string `json:"device" jsonschema:"device id, e.g. fenix7"`
	Key       string `json:"key" jsonschema:"developer key (.der) inside the root"`
	Output    string `json:"output,omitempty" jsonschema:"PRG path inside the root (default <project>/bin/<device>.prg)"`
	Release   bool   `json:"release,omitempty" jsonschema:"release build (-r)"`
	Test      bool   `json:"test,omitempty" jsonschema:"unit-test build (-t); not with release"`
	TypeCheck *int   `json:"type_check,omitempty" jsonschema:"type check level 0-3 (-l)"`
}

type exportIn struct {
	SDK     string `json:"sdk,omitempty" jsonschema:"SDK path or installed version, e.g. 9.2.0; default CIQ_MCP_SDK/CIQ_SDK, current-sdk.cfg, newest"`
	Project string `json:"project,omitempty" jsonschema:"project dir inside the root (default: root)"`
	Jungle  string `json:"jungle,omitempty" jsonschema:"jungle file relative to the project (default monkey.jungle)"`
	Key     string `json:"key" jsonschema:"developer key (.der) inside the root"`
	Output  string `json:"output,omitempty" jsonschema:"package path inside the root (default <project>/release/<dir>.iq)"`
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

// prepared holds resolved, confined build inputs.
type prepared struct {
	se   sdkEnv
	opts ciqsdk.BuildOpts
}

func prepare(ctx context.Context, env *tool.Env, sdkArg, project, jungle, key, output, defOut string) (prepared, error) {
	se, err := resolveSDK(sdkArg)
	if err != nil {
		return prepared{}, err
	}
	if project == "" {
		project = "."
	}
	pdir, err := env.Root.Resolve(project)
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
	if output == "" {
		output = filepath.Join(pdir, defOut)
	}
	oabs, err := env.Root.Resolve(output)
	if err != nil {
		return prepared{}, err
	}
	if err := os.MkdirAll(filepath.Dir(oabs), 0o755); err != nil {
		return prepared{}, err
	}
	var java string
	if j, err := ciqsdk.FindJava(ctx, os.Getenv); err == nil {
		java = j.Path
	}
	return prepared{se: se, opts: ciqsdk.BuildOpts{SDK: se.Res.SDK, Java: java, ProjectDir: pdir, Jungle: jabs, Key: kabs, Output: oabs, Warnings: true}}, nil
}

func outFor(p prepared, r ciqsdk.BuildResult) buildOut {
	o := buildOut{OK: r.OK, Output: r.Output, SDK: p.se.Res.SDK.Version, SDKSource: p.se.Res.Source, ExitCode: r.ExitCode,
		Errors: r.Errors, Warnings: r.Warnings, Diagnostics: r.Diagnostics, Tail: r.Tail, DurationMS: r.DurationMS}
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
	if !ciqsdk.ValidDeviceID(in.Device) {
		return nil, tool.InvalidArgs("invalid device id %q", in.Device)
	}
	if in.Release && in.Test {
		return nil, tool.InvalidArgs("release and test are mutually exclusive")
	}
	suffix := ""
	if in.Test {
		suffix = "-test"
	}
	p, err := prepare(ctx, env, in.SDK, in.Project, in.Jungle, in.Key, in.Output, filepath.Join("bin", in.Device+suffix+".prg"))
	if err != nil {
		return nil, err
	}
	if _, err := ciqsdk.LoadDevice(p.se.DataRoot, in.Device); errors.Is(err, os.ErrNotExist) {
		return nil, tool.InvalidArgs("device %q is not installed; download it with SdkManager", in.Device)
	}
	p.opts.Device, p.opts.Release, p.opts.Test, p.opts.TypeCheck = in.Device, in.Release, in.Test, in.TypeCheck
	r, err := ciqsdk.Build(ctx, p.opts)
	if err != nil {
		return nil, err
	}
	return tool.OK(outFor(p, r)), nil
}

func sdkExport(ctx context.Context, env *tool.Env, in exportIn) (*tool.Result, error) {
	pdir, err := env.Root.Resolve(orDot(in.Project))
	if err != nil {
		return nil, err
	}
	p, err := prepare(ctx, env, in.SDK, in.Project, in.Jungle, in.Key, in.Output, filepath.Join("release", filepath.Base(pdir)+".iq"))
	if err != nil {
		return nil, err
	}
	p.opts.Timeout = 30 * time.Minute
	done := make(chan struct{})
	go func() {
		start := time.Now()
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				env.Progress(0, 0, fmt.Sprintf("monkeyc -e running for %s", time.Since(start).Round(time.Second)))
			}
		}
	}()
	env.Progress(0, 0, "exporting all manifest products (monkeyc -e -r)")
	r, err := ciqsdk.Export(ctx, p.opts)
	close(done)
	if err != nil {
		return nil, err
	}
	o := outFor(p, r)
	o.ByDevice = map[string]int{}
	for _, d := range r.Diagnostics {
		if d.Severity == "error" && d.Device != "" {
			o.ByDevice[d.Device]++
		}
	}
	return tool.OK(o), nil
}

func orDot(s string) string {
	if strings.TrimSpace(s) == "" {
		return "."
	}
	return s
}

// --- keys ---

type keyGenIn struct {
	tool.ConfirmFlag
	Path string `json:"path" jsonschema:"new key file inside the root, e.g. keys/developer_key.der"`
}

type keyCheckIn struct {
	Path string `json:"path" jsonschema:"developer key (.der) inside the root"`
}

func sdkKeyGenerate(ctx context.Context, env *tool.Env, in keyGenIn) (*tool.Result, error) {
	abs, err := env.Root.Resolve(in.Path)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(abs); err == nil {
		return nil, tool.InvalidArgs("%s already exists; refusing to overwrite a developer key", abs)
	}
	_, gate := tool.RequireConfirm(ctx, env, in.Confirm, tool.PlanPreview{
		Summary:  fmt.Sprintf("create RSA-%d developer key %s (mode 0600)", ciqsdk.KeyBits, abs),
		Affected: []string{abs},
	})
	if gate != nil {
		return gate, nil
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return nil, err
	}
	ki, err := ciqsdk.GenerateKey(abs)
	if err != nil {
		return nil, err
	}
	return tool.OK(map[string]any{"key": ki,
		"note": "Back this key up somewhere safe. The store binds your app to the key of its first upload; a lost key means no more updates. Keep it out of git."}), nil
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
