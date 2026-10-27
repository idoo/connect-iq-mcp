package sdk

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/idoo/garmin-mcp/internal/ciqsdk"
	"github.com/idoo/garmin-mcp/internal/ciqsdk/ciqsdktest"
	"github.com/idoo/garmin-mcp/internal/tool"
	"github.com/idoo/garmin-mcp/internal/tool/tooltest"
)

func TestLocalSDKToolsAvailableReadOnly(t *testing.T) {
	for _, n := range []string{"sdk_build", "sdk_key_generate", "sdk_manifest_set_products"} {
		tl, ok := tool.Lookup(n)
		if !ok {
			t.Fatalf("%s not registered", n)
		}
		if !tl.ListedIn(tool.ProfileInfo{}) {
			t.Errorf("%s not listed in read-only profile", n)
		}
	}
}

// exportFixture sets up a fake SDK (via GARMIN_MCP_SDK) and a fake java (via
// JAVA_HOME) that records its args and writes -o, and a project with manifest
// version 1.2.3 and a key.
func exportFixture(t *testing.T) (k *tooltest.Kit, argsFile, iq string) {
	t.Helper()
	return exportFixtureWith(t, "", ` version="1.2.3"`)
}

// exportFixtureWith is exportFixture with extra shell run by the fake
// compiler before it writes -o (it may set $content) and the manifest version attribute.
func exportFixtureWith(t *testing.T, pre, versionAttr string) (k *tooltest.Kit, argsFile, iq string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script compiler")
	}
	t.Setenv("HOME", t.TempDir())
	sdk := filepath.Join(t.TempDir(), "sdk")
	if err := os.MkdirAll(filepath.Join(sdk, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sdk, "bin", "monkeybrains.jar"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	argsFile = filepath.Join(t.TempDir(), "args")
	script := "#!/bin/sh\n[ \"$1\" = -version ] && { echo 'openjdk version \"21\"' >&2; exit 0; }\n" +
		"echo \"$@\" > '" + argsFile + "'\n" +
		"content=new\n" + pre + "\n" +
		"while [ $# -gt 0 ]; do if [ \"$1\" = -o ]; then out=$2; fi; shift; done\necho $content > \"$out\"\n"
	javaHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(javaHome, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(javaHome, "bin", "java"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GARMIN_MCP_SDK", sdk)
	t.Setenv("CIQ_SDK", "")
	t.Setenv("JAVA_HOME", javaHome)

	k = tooltest.New(t).ReadOnly()
	root := k.Files.Root
	proj := filepath.Join(root, "app")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "monkey.jungle"), []byte("project.manifest = manifest.xml\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "manifest.xml"), []byte(`<?xml version="1.0"?>
<iq:manifest xmlns:iq="http://www.garmin.com/xml/connectiq" version="3">
  <iq:application id="0123456789abcdef0123456789abcdef" type="watchapp"`+versionAttr+` minApiLevel="3.0.0">
    <iq:products><iq:product id="fenix7"/></iq:products>
  </iq:application>
</iq:manifest>
`), 0o644); err != nil {
		t.Fatal(err)
	}
	ciqsdktest.Key(t, filepath.Join(root, "dev.der"))
	return k, argsFile, filepath.Join(proj, "release", "app-1.2.3.iq")
}

func TestBuildAllExportsVersionedPackage(t *testing.T) {
	k, argsFile, iq := exportFixture(t)
	tl, _ := tool.Lookup("sdk_build")
	call := func(extra string) *tool.Result {
		return tooltest.Call(t, tl, k.Env, `{"project":"app","device":"all","key":"dev.der"`+extra+`}`)
	}

	out := tooltest.Decode[buildOut](t, tooltest.MustOK(t, call("")))
	if !out.OK || out.Output != iq {
		t.Fatalf("output = %+v, want %s", out, iq)
	}
	args, _ := os.ReadFile(argsFile)
	if a := " " + string(args); !strings.Contains(a, " -e -r ") || strings.Contains(a, " -d ") {
		t.Errorf("export args = %q, want -e -r and no -d", args)
	}

	// An existing package is never replaced without overwrite.
	if err := os.WriteFile(iq, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Remove(argsFile)
	tooltest.MustFail(t, call(""), "invalid_arguments")
	if b, _ := os.ReadFile(iq); string(b) != "old" {
		t.Fatalf("package overwritten without overwrite: %q", b)
	}
	if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
		t.Error("monkeyc ran although the destination was refused")
	}

	tooltest.MustOK(t, call(`,"overwrite":true`))
	if b, _ := os.ReadFile(iq); strings.TrimSpace(string(b)) != "new" {
		t.Fatalf("overwrite:true kept old package: %q", b)
	}
}

func TestBuildRejectsTestWithAll(t *testing.T) {
	k, _, _ := exportFixture(t)
	tl, _ := tool.Lookup("sdk_build")
	tooltest.MustFail(t, tooltest.Call(t, tl, k.Env, `{"project":"app","device":"all","key":"dev.der","test":true}`), "invalid_arguments")
}

func callBuild(t *testing.T, k *tooltest.Kit, args string) *tool.Result {
	t.Helper()
	tl, _ := tool.Lookup("sdk_build")
	return tooltest.Call(t, tl, k.Env, args)
}

// setBuildWait shortens the wait on k's own build registry.
func setBuildWait(k *tooltest.Kit, d time.Duration) {
	r := jobsOf(k.Env)
	r.mu.Lock()
	r.buildWait = d
	r.mu.Unlock()
}

// The human-set GARMIN_MCP_KEY is used when key is omitted, even outside the
// root, and is only read.
func TestBuildUsesEnvKeyOutsideRoot(t *testing.T) {
	k, argsFile, iq := exportFixture(t)
	envKey := filepath.Join(t.TempDir(), "outside.der")
	ciqsdktest.Key(t, envKey)
	before, _ := os.ReadFile(envKey)
	st0, _ := os.Stat(envKey)
	t.Setenv(ciqsdk.EnvKeyVar, envKey)

	out := tooltest.Decode[buildOut](t, tooltest.MustOK(t, callBuild(t, k, `{"project":"app","device":"all"}`)))
	if !out.OK || out.Output != iq {
		t.Fatalf("output = %+v", out)
	}
	if args, _ := os.ReadFile(argsFile); !strings.Contains(string(args), "-y "+envKey+" ") {
		t.Fatalf("monkeyc args %q do not use the env key", args)
	}
	// An agent-supplied key outside the root stays refused.
	mustErr(t, callBuild(t, k, `{"project":"app","device":"all","key":`+strconv.Quote(envKey)+`,"version":"9"}`))
	// sdk_key_generate never writes the env key, even when it is inside the root.
	inRoot := filepath.Join(k.Files.Root, "dev.der")
	t.Setenv(ciqsdk.EnvKeyVar, inRoot)
	inBefore, _ := os.ReadFile(inRoot)
	gen, _ := tool.Lookup("sdk_key_generate")
	tooltest.MustFail(t, tooltest.Call(t, gen, k.Env, `{"path":"dev.der"}`), "invalid_arguments")
	if b, _ := os.ReadFile(inRoot); !bytes.Equal(b, inBefore) {
		t.Fatal("in-root env key rewritten")
	}
	if after, _ := os.ReadFile(envKey); !bytes.Equal(before, after) {
		t.Fatal("env key changed")
	}
	if st1, _ := os.Stat(envKey); !st1.ModTime().Equal(st0.ModTime()) {
		t.Fatal("env key touched")
	}
}

func TestBuildMissingKeyHintNamesEnv(t *testing.T) {
	k, _, _ := exportFixture(t)
	t.Setenv(ciqsdk.EnvKeyVar, "")
	res := tooltest.MustFail(t, callBuild(t, k, `{"project":"app","device":"all"}`), "invalid_arguments")
	if b, _ := json.Marshal(res); !strings.Contains(string(b), ciqsdk.EnvKeyVar) {
		t.Fatalf("missing-key error does not name %s: %s", ciqsdk.EnvKeyVar, b)
	}
}

// SE-2: with no usable java sdk_build fails with the FindJava error and the
// install hint; the SDK's bin/monkeyc wrapper never runs.
func TestBuildWithoutJavaReturnsError(t *testing.T) {
	k, argsFile, _ := exportFixture(t)
	marker := filepath.Join(t.TempDir(), "ran")
	if err := os.WriteFile(filepath.Join(os.Getenv("GARMIN_MCP_SDK"), "bin", "monkeyc"), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JAVA_HOME", t.TempDir()) // no bin/java: FindJava fails deterministically
	res := tooltest.MustFail(t, callBuild(t, k, `{"project":"app","device":"all","key":"dev.der"}`), "unavailable")
	if b, _ := json.Marshal(res); !strings.Contains(string(b), "JAVA_HOME") {
		t.Fatalf("no-java error lacks the hint: %s", b)
	}
	for _, f := range []string{marker, argsFile} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Fatalf("%s exists: a compiler ran without java", f)
		}
	}
}

// SE-5: jungle path keys are confined to the root; excludeAnnotations is not a path.
func TestBuildRefusesJunglePathOutsideRoot(t *testing.T) {
	k, argsFile, _ := exportFixture(t)
	jungle := filepath.Join(k.Files.Root, "app", "monkey.jungle")
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "sub"), filepath.Join(k.Files.Root, "app", "link")); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		"project.manifest = manifest.xml\nbase.resourcePath = " + outside + "\n",
		"project.manifest = manifest.xml\nbase.resourcePath = resources;../../" + filepath.Base(outside) + "\n",
		"lib = " + outside + "\nproject.manifest = manifest.xml\nbase.sourcePath = $(lib)\n",
		// RS-2: monkeyc strips quotes, splits on \ and resolves .. after symlinks.
		"base.sourcePath = source;\"" + outside + "\"\n",
		"base.sourcePath = source;..\\..\\outside\n",
		"base.sourcePath = source;link/..\n",
	} {
		if err := os.WriteFile(jungle, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		res := tooltest.MustFail(t, callBuild(t, k, `{"project":"app","device":"all","key":"dev.der"}`), "invalid_arguments")
		if b, _ := json.Marshal(res); !strings.Contains(string(b), "Path = ") {
			t.Fatalf("refusal does not name the key: %s", b)
		}
		if _, err := os.Stat(argsFile); !os.IsNotExist(err) {
			t.Fatal("monkeyc ran with an outside-root jungle path")
		}
	}
	if err := os.WriteFile(jungle, []byte("project.manifest = manifest.xml\nbase.resourcePath = resources\nbase.excludeAnnotations = /etc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tooltest.MustOK(t, callBuild(t, k, `{"project":"app","device":"all","key":"dev.der"}`))
	tooltest.MustFail(t, callBuild(t, k, `{"project":"app","device":"all","key":"dev.der","jungle":"monkey.jungle;x.jungle"}`), "invalid_arguments")
}

func TestKeyGenerateRefusesUnignoredGitPath(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	k := tooltest.New(t).ReadOnly()
	root := k.Files.Root
	if out, err := exec.Command(git, "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	gen, _ := tool.Lookup("sdk_key_generate")
	res := tooltest.MustFail(t, tooltest.Call(t, gen, k.Env, `{"path":"keys/developer_key.der"}`), "invalid_arguments")
	if b, _ := json.Marshal(res); !strings.Contains(string(b), "/keys/developer_key.der") || !strings.Contains(string(b), ".gitignore") {
		t.Fatalf("hint lacks the .gitignore line: %s", b)
	}
	if _, err := os.Stat(filepath.Join(root, "keys", "developer_key.der")); !os.IsNotExist(err) {
		t.Fatal("key written although refused")
	}
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("/keys/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tooltest.MustOK(t, tooltest.Call(t, gen, k.Env, `{"path":"keys/developer_key.der"}`))
}

// gitInit makes root a git repo, skipping without git.
func gitInit(t *testing.T, root string) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("no git")
	}
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "t@example.test"}, {"config", "user.name", "t"}} {
		if out, err := exec.Command(git, append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil { //nolint:gosec // test
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return git
}

// SE-1: a core.fsmonitor command in the project's .git/config never runs.
func TestKeyGenerateNeverRunsRepoFsmonitor(t *testing.T) {
	k := tooltest.New(t).ReadOnly()
	root := k.Files.Root
	git := gitInit(t, root)
	marker := filepath.Join(t.TempDir(), "PWNED")
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "a"}, {"config", "core.fsmonitor", "touch '" + marker + "'; echo"}} {
		if out, err := exec.Command(git, append([]string{"-C", root}, args...)...).CombinedOutput(); err != nil { //nolint:gosec // test
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	_ = os.Remove(marker) // git add may already have run it
	gen, _ := tool.Lookup("sdk_key_generate")
	tooltest.MustFail(t, tooltest.Call(t, gen, k.Env, `{"path":"keys/developer_key.der"}`), "invalid_arguments")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("repo fsmonitor ran during sdk_key_generate (%v)", err)
	}
}

// SE-1: GIT_DIR (and other GIT_*) in the server env cannot point the check
// at another repo whose excludes would ignore the key.
func TestKeyGenerateIgnoresGitDirEnv(t *testing.T) {
	k := tooltest.New(t).ReadOnly()
	root := k.Files.Root
	gitInit(t, root)
	other := t.TempDir()
	gitInit(t, other)
	if err := os.WriteFile(filepath.Join(other, ".git", "info", "exclude"), []byte("/keys/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", root)
	gen, _ := tool.Lookup("sdk_key_generate")
	tooltest.MustFail(t, tooltest.Call(t, gen, k.Env, `{"path":"keys/developer_key.der"}`), "invalid_arguments")
	for _, kv := range gitEnv([]string{"GIT_DIR=/x", "git_index_file=/y", "PATH=/bin"}) {
		if strings.HasPrefix(strings.ToUpper(kv), "GIT_") && kv != "GIT_CONFIG_NOSYSTEM=1" {
			t.Errorf("gitEnv kept %s", kv)
		}
	}
}

// A store build outliving the wait returns a build_id; polling returns the result.
func TestBuildAllOutlivesWait(t *testing.T) {
	k, _, iq := exportFixtureWith(t, "sleep 1", ` version="1.2.3"`)
	setBuildWait(k, 100*time.Millisecond)
	p := tooltest.Decode[pendingOut](t, tooltest.MustOK(t, callBuild(t, k, `{"project":"app","device":"all","key":"dev.der"}`)))
	if p.Status != "still_processing" || p.BuildID == "" {
		t.Fatalf("pending = %+v", p)
	}
	setBuildWait(k, 10*time.Second)
	out := tooltest.Decode[buildOut](t, tooltest.MustOK(t, callBuild(t, k, `{"build_id":"`+p.BuildID+`"}`)))
	if !out.OK || out.Output != iq {
		t.Fatalf("polled = %+v", out)
	}
	// A result is handed out once.
	tooltest.MustFail(t, callBuild(t, k, `{"build_id":"`+p.BuildID+`"}`), "invalid_arguments")
}

// LF-1: a second build of the same output while the first runs (an agent
// re-calling instead of polling, or retrying after a cancelled call) gets the
// same build_id; only one compiler runs.
func TestBuildSameOutputSharesJob(t *testing.T) {
	runs := filepath.Join(t.TempDir(), "runs")
	k, _, iq := exportFixtureWith(t, "echo x >> '"+runs+"'; sleep 1", ` version="1.2.3"`)
	setBuildWait(k, 50*time.Millisecond)
	start := func() pendingOut {
		return tooltest.Decode[pendingOut](t, tooltest.MustOK(t, callBuild(t, k, `{"project":"app","device":"all","key":"dev.der"}`)))
	}
	a, b := start(), start()
	if a.BuildID == "" || a.BuildID != b.BuildID {
		t.Fatalf("build ids %q and %q, want the same running job", a.BuildID, b.BuildID)
	}
	setBuildWait(k, 10*time.Second)
	out := tooltest.Decode[buildOut](t, tooltest.MustOK(t, callBuild(t, k, `{"build_id":"`+a.BuildID+`"}`)))
	if !out.OK || out.Output != iq {
		t.Fatalf("polled = %+v", out)
	}
	if got, _ := os.ReadFile(runs); strings.Count(string(got), "x") != 1 {
		t.Fatalf("compiler ran %d times, want 1", strings.Count(string(got), "x"))
	}
}

// LF-1: finished results nobody collected are swept after jobKeep.
func TestSweepDropsStaleFinishedJobs(t *testing.T) {
	done := make(chan struct{})
	close(done)
	old := &buildJob{id: "old", done: done, end: time.Now().Add(-jobKeep - time.Minute)}
	fresh := &buildJob{id: "fresh", done: done, end: time.Now()}
	running := &buildJob{id: "running", done: make(chan struct{})}
	r := newJobRegistry()
	r.mu.Lock()
	for _, j := range []*buildJob{old, fresh, running} {
		r.jobs[j.id] = j
	}
	r.mu.Unlock()
	if r.lookup("old") != nil || r.lookup("fresh") == nil || r.lookup("running") == nil {
		t.Fatal("sweep kept a stale result or dropped a live one")
	}
}

// LF-2: stopping the server ends a running build (process group killed).
func TestBuildEndsWithServerLifetime(t *testing.T) {
	k, _, _ := exportFixtureWith(t, "sleep 30 & wait", ` version="1.2.3"`)
	life := tool.NewLifetime(t.Context())
	k.Env.Lifetime = life
	setBuildWait(k, 50*time.Millisecond)
	p := tooltest.Decode[pendingOut](t, tooltest.MustOK(t, callBuild(t, k, `{"project":"app","device":"all","key":"dev.der"}`)))
	j := jobsOf(k.Env).lookup(p.BuildID)
	if j == nil {
		t.Fatal("job not registered")
	}
	begin := time.Now()
	life.Close(10 * time.Second)
	select {
	case <-j.done:
	default:
		t.Fatal("build still running after the server lifetime closed")
	}
	if time.Since(begin) > 8*time.Second || j.err == nil {
		t.Fatalf("build not cancelled promptly: took %s, err %v", time.Since(begin), j.err)
	}
}

func TestBuildAllNamesIQ(t *testing.T) {
	k, _, _ := exportFixtureWith(t, "", "")
	out := tooltest.Decode[buildOut](t, tooltest.MustOK(t, callBuild(t, k, `{"project":"app","device":"all","key":"dev.der"}`)))
	if !regexp.MustCompile(`/release/app-[0-9]{8}-[0-9]{6}\.iq$`).MatchString(filepath.ToSlash(out.Output)) {
		t.Fatalf("unversioned manifest: output %s, want a timestamped name", out.Output)
	}
	out = tooltest.Decode[buildOut](t, tooltest.MustOK(t, callBuild(t, k, `{"project":"app","device":"all","key":"dev.der","version":"2.0.1"}`)))
	if filepath.Base(out.Output) != "app-2.0.1.iq" {
		t.Fatalf("version arg: output %s", out.Output)
	}
	tooltest.MustFail(t, callBuild(t, k, `{"project":"app","device":"all","key":"dev.der","version":"../x"}`), "invalid_arguments")
}

func TestDevicesListRejectsBadMinAPI(t *testing.T) {
	k := tooltest.New(t).ReadOnly()
	tl, _ := tool.Lookup("sdk_devices_list")
	tooltest.MustFail(t, tooltest.Call(t, tl, k.Env, `{"min_api":"abc"}`), "invalid_arguments")
}

// RS-3: a call writing the same output with a different key while a build
// runs is refused as a conflict, never handed the other call's artefact.
func TestBuildSameOutputOtherOptionsConflicts(t *testing.T) {
	runs := filepath.Join(t.TempDir(), "runs")
	k, _, _ := exportFixtureWith(t, "echo x >> '"+runs+"'; sleep 1", ` version="1.2.3"`)
	ciqsdktest.Key(t, filepath.Join(k.Files.Root, "other.der"))
	setBuildWait(k, 50*time.Millisecond)
	a := tooltest.Decode[pendingOut](t, tooltest.MustOK(t, callBuild(t, k, `{"project":"app","device":"all","key":"dev.der"}`)))
	for _, args := range []string{
		`{"project":"app","device":"all","key":"other.der"}`,
		`{"project":"app","device":"all","key":"dev.der","overwrite":true}`,
	} {
		res := tooltest.MustFail(t, callBuild(t, k, args), "conflict")
		if b, _ := json.Marshal(res); !strings.Contains(string(b), a.BuildID) || !strings.Contains(string(b), "another build is writing") {
			t.Fatalf("conflict lacks the running build: %s", b)
		}
	}
	setBuildWait(k, 10*time.Second)
	tooltest.MustOK(t, callBuild(t, k, `{"build_id":"`+a.BuildID+`"}`))
	if got, _ := os.ReadFile(runs); strings.Count(string(got), "x") != 1 {
		t.Fatalf("compiler ran %d times, want 1", strings.Count(string(got), "x"))
	}
}

// N10: an unknown device id points at sdk_devices_list; a known id without
// its definitions still points at SdkManager.
func TestBuildUnknownDevice(t *testing.T) {
	k, _, _ := exportFixture(t)
	res := tooltest.MustFail(t, callBuild(t, k, `{"project":"app","device":"fenix8","key":"dev.der"}`), "invalid_arguments")
	if b, _ := json.Marshal(res); !strings.Contains(string(b), "unknown device fenix8; list valid ids with sdk_devices_list") {
		t.Fatalf("unknown device: %s", b)
	}
	dr, err := dataRoot()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(ciqsdk.DevicesDir(dr), "fenix8"), 0o755); err != nil {
		t.Fatal(err)
	}
	res = tooltest.MustFail(t, callBuild(t, k, `{"project":"app","device":"fenix8","key":"dev.der"}`), "invalid_arguments")
	if b, _ := json.Marshal(res); !strings.Contains(string(b), "SdkManager") {
		t.Fatalf("known device without definitions: %s", b)
	}
	get, _ := tool.Lookup("sdk_device_get")
	res = tooltest.MustFail(t, tooltest.Call(t, get, k.Env, `{"id":"fenix9"}`), "invalid_arguments")
	if b, _ := json.Marshal(res); !strings.Contains(string(b), "unknown device fenix9") {
		t.Fatalf("sdk_device_get: %s", b)
	}
}

// N7: key and Java hints say the env belongs to the MCP client, not the shell.
func TestEnvHintsNameClientEnv(t *testing.T) {
	k, _, _ := exportFixture(t)
	t.Setenv(ciqsdk.EnvKeyVar, "")
	res := tooltest.MustFail(t, callBuild(t, k, `{"project":"app","device":"all"}`), "invalid_arguments")
	t.Setenv("JAVA_HOME", t.TempDir())
	res2 := tooltest.MustFail(t, callBuild(t, k, `{"project":"app","device":"all","key":"dev.der"}`), "unavailable")
	for _, r := range []tool.ErrorBody{res, res2} {
		if b, _ := json.Marshal(r); !strings.Contains(string(b), "restart the client") || !strings.Contains(string(b), "config install") {
			t.Fatalf("hint lacks the client env: %s", b)
		}
	}
}

func mustErr(t *testing.T, res *tool.Result) {
	t.Helper()
	if !res.IsError {
		t.Fatalf("expected an error, got %s", tooltest.JSON(t, res.Data))
	}
}
