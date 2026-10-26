package sdk

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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
		"while [ $# -gt 0 ]; do if [ \"$1\" = -o ]; then out=$2; fi; shift; done\necho new > \"$out\"\n"
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
  <iq:application id="0123456789abcdef0123456789abcdef" type="watchapp" version="1.2.3" minApiLevel="3.0.0">
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
