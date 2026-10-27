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

// exportFixture sets up a fake SDK (via GARMIN_MCP_SDK) whose monkeyc records
// its args and writes -o, and a project with manifest version 1.2.3 and a key.
func exportFixture(t *testing.T) (k *tooltest.Kit, argsFile, iq string) {
	t.Helper()
	return exportFixtureWith(t, "", ` version="1.2.3"`)
}

// exportFixtureWith is exportFixture with extra shell run by monkeyc before it
// writes -o (it may set $content) and the manifest version attribute.
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
	argsFile = filepath.Join(t.TempDir(), "args")
	script := "#!/bin/sh\necho \"$@\" > '" + argsFile + "'\n" +
		"content=new\n" + pre + "\n" +
		"while [ $# -gt 0 ]; do if [ \"$1\" = -o ]; then out=$2; fi; shift; done\necho $content > \"$out\"\n"
	if err := os.WriteFile(filepath.Join(sdk, "bin", "monkeyc"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GARMIN_MCP_SDK", sdk)
	t.Setenv("CIQ_SDK", "")
	t.Setenv("JAVA_HOME", "")

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
	if _, err := ciqsdk.GenerateKey(filepath.Join(root, "dev.der")); err != nil {
		t.Fatal(err)
	}
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

func setBuildWait(t *testing.T, d time.Duration) {
	old := buildWait
	buildWait = d
	t.Cleanup(func() { buildWait = old })
}

// The human-set GARMIN_MCP_KEY is used when key is omitted, even outside the
// root, and is only read.
func TestBuildUsesEnvKeyOutsideRoot(t *testing.T) {
	k, argsFile, iq := exportFixture(t)
	envKey := filepath.Join(t.TempDir(), "outside.der")
	if _, err := ciqsdk.GenerateKey(envKey); err != nil {
		t.Fatal(err)
	}
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

// A store build outliving the wait returns a build_id; polling returns the result.
func TestBuildAllOutlivesWait(t *testing.T) {
	k, _, iq := exportFixtureWith(t, "sleep 1", ` version="1.2.3"`)
	setBuildWait(t, 100*time.Millisecond)
	p := tooltest.Decode[pendingOut](t, tooltest.MustOK(t, callBuild(t, k, `{"project":"app","device":"all","key":"dev.der"}`)))
	if p.Status != "still_processing" || p.BuildID == "" {
		t.Fatalf("pending = %+v", p)
	}
	setBuildWait(t, 10*time.Second)
	out := tooltest.Decode[buildOut](t, tooltest.MustOK(t, callBuild(t, k, `{"build_id":"`+p.BuildID+`"}`)))
	if !out.OK || out.Output != iq {
		t.Fatalf("polled = %+v", out)
	}
	// A result is handed out once.
	tooltest.MustFail(t, callBuild(t, k, `{"build_id":"`+p.BuildID+`"}`), "invalid_arguments")
}

// Two concurrent exports of the same name: exactly one succeeds, the other is
// refused (up front if the winner already finished, else at install), and the
// winner's file is never replaced. Which call wins is up to the scheduler.
func TestBuildAllConcurrentNoClobber(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "lock")
	pre := "if mkdir '" + lock + "' 2>/dev/null; then sleep 0.3; content=first; else sleep 1.5; content=second; fi"
	k, _, iq := exportFixtureWith(t, pre, ` version="1.2.3"`)
	setBuildWait(t, 50*time.Millisecond)
	start := func() *tool.Result {
		return callBuild(t, k, `{"project":"app","device":"all","key":"dev.der"}`)
	}
	results := []*tool.Result{start(), start()}
	setBuildWait(t, 10*time.Second)
	ok := 0
	for i, r := range results {
		if !r.IsError {
			if p := tooltest.Decode[pendingOut](t, r); p.BuildID != "" {
				r = callBuild(t, k, `{"build_id":"`+p.BuildID+`"}`)
			}
		}
		if !r.IsError {
			ok++
		} else {
			t.Logf("call %d refused: %s", i, tooltest.JSON(t, r.Data))
		}
	}
	if ok != 1 {
		t.Fatalf("%d builds succeeded, want exactly 1", ok)
	}
	if got, _ := os.ReadFile(iq); strings.TrimSpace(string(got)) != "first" {
		t.Fatalf("winning package replaced: %q", got)
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

func mustErr(t *testing.T, res *tool.Result) {
	t.Helper()
	if !res.IsError {
		t.Fatalf("expected an error, got %s", tooltest.JSON(t, res.Data))
	}
}
