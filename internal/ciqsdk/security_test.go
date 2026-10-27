package ciqsdk

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolveArgRejectsSDKOutsideDataRoot(t *testing.T) {
	root := t.TempDir()
	inside := mkSDK(t, root, "connectiq-sdk-mac-9.2.0-2026-06-09-92a1605b2")
	// A repo-shipped "SDK" with a valid layout, outside the data root.
	repo := t.TempDir()
	evil := mkSDK(t, repo, "connectiq-sdk-mac-9.9.9-x")
	// A symlink inside Sdks that points at it.
	link := filepath.Join(root, "Sdks", "connectiq-sdk-mac-9.9.8-link")
	if err := os.Symlink(evil, link); err != nil {
		t.Fatal(err)
	}
	noenv := func(string) string { return "" }
	for _, arg := range []string{evil, link, filepath.Join(root, "Sdks", "..", "..", filepath.Base(repo), "Sdks", "connectiq-sdk-mac-9.9.9-x"), "vendor/sdk", "./x"} {
		_, err := Resolve(ResolveOpts{Arg: arg, DataRoot: root, Getenv: noenv})
		if !errors.Is(err, ErrUntrustedSDK) {
			t.Errorf("arg %q: want ErrUntrustedSDK, got %v", arg, err)
		}
	}
	if r, err := Resolve(ResolveOpts{Arg: inside, DataRoot: root, Getenv: noenv}); err != nil || r.SDK.Path != inside {
		t.Fatalf("installed SDK by path: %+v %v", r, err)
	}
	// The human may point GARMIN_MCP_SDK anywhere.
	env := func(k string) string {
		if k == "GARMIN_MCP_SDK" {
			return evil
		}
		return ""
	}
	if r, err := Resolve(ResolveOpts{DataRoot: root, Getenv: env}); err != nil || r.SDK.Path != evil {
		t.Fatalf("env SDK: %+v %v", r, err)
	}
}

// fakeMonkeycSDK returns an SDK with a bin/monkeybrains.jar and a fake
// absolute java that, like the real compiler, writes -o, its debug xml and
// gen/ next to it.
func fakeMonkeycSDK(t *testing.T) (SDK, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script compiler")
	}
	p := filepath.Join(t.TempDir(), "sdk")
	if err := os.MkdirAll(filepath.Join(p, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "bin", "monkeybrains.jar"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nwhile [ $# -gt 0 ]; do if [ \"$1\" = -o ]; then out=$2; fi; shift; done\n" +
		"echo prg > \"$out\"; echo dbg > \"$out.debug.xml\"; mkdir -p \"$(dirname \"$out\")/gen\"; echo g > \"$(dirname \"$out\")/gen/x\"\n"
	java := filepath.Join(t.TempDir(), "java")
	if err := os.WriteFile(java, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return SDK{Path: p}, java
}

// SE-2: without java the build fails with the FindJava error; the SDK's
// bin/monkeyc wrapper (which runs a bare `java`) never runs.
func TestBuildWithoutJavaNeverRunsWrapper(t *testing.T) {
	sdk, _ := fakeMonkeycSDK(t)
	marker := filepath.Join(t.TempDir(), "ran")
	if err := os.WriteFile(filepath.Join(sdk.Bin(), "monkeyc"), []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	noJava := errors.New("no Java found (JAVA_HOME unset, no java on PATH); install Java 17+")
	noJavaFind := func(context.Context, func(string) string) (Java, error) { return Java{}, noJava }
	proj := t.TempDir()
	_, err := Build(context.Background(), BuildOpts{SDK: sdk, ProjectDir: proj, Device: "fenix7", Key: "/k", Output: filepath.Join(proj, "bin", "a.prg"), findJava: noJavaFind})
	if !errors.Is(err, noJava) {
		t.Fatalf("err = %v, want the FindJava error", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("bin/monkeyc wrapper ran")
	}
	// A relative java is refused, and argv never names a wrapper.
	if _, err := Build(context.Background(), BuildOpts{SDK: sdk, Java: "java", ProjectDir: proj, Device: "fenix7", Key: "/k", Output: filepath.Join(proj, "bin", "a.prg")}); err == nil {
		t.Fatal("relative java accepted")
	}
	for _, argv := range [][]string{sdk.MonkeycArgv("", nil), sdk.MonkeydoArgv("", "/a.prg", "fenix7")} {
		if argv[0] != javaExe() || filepath.Dir(argv[0]) == sdk.Bin() {
			t.Errorf("argv without java = %v, want bare %s", argv, javaExe())
		}
	}
}

func TestBuildRefusesSymlinkedOutputDir(t *testing.T) {
	sdk, java := fakeMonkeycSDK(t)
	proj := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(proj, "bin")); err != nil {
		t.Fatal(err)
	}
	_, err := Build(context.Background(), BuildOpts{
		SDK: sdk, Java: java, ProjectDir: proj, Device: "fenix7", Key: "/k",
		Output: filepath.Join(proj, "bin", "a.prg"),
	})
	if err == nil {
		t.Fatal("symlinked output dir must be refused")
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatalf("wrote through symlink: %v", ents)
	}
	// A symlinked output file is refused too.
	os.Remove(filepath.Join(proj, "bin"))
	if err := os.MkdirAll(filepath.Join(proj, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "x.prg"), filepath.Join(proj, "bin", "a.prg")); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(context.Background(), BuildOpts{
		SDK: sdk, Java: java, ProjectDir: proj, Device: "fenix7", Key: "/k",
		Output: filepath.Join(proj, "bin", "a.prg"),
	}); err == nil {
		t.Fatal("symlinked output file must be refused")
	}
}

func TestBuildMovesOnlyArtefacts(t *testing.T) {
	sdk, java := fakeMonkeycSDK(t)
	proj := t.TempDir()
	out := filepath.Join(proj, "bin", "a.prg")
	r, err := Build(context.Background(), BuildOpts{SDK: sdk, Java: java, ProjectDir: proj, Device: "fenix7", Key: "/k", Output: out})
	if err != nil || !r.OK {
		t.Fatalf("%+v %v", r, err)
	}
	for _, f := range []string{out, out + ".debug.xml"} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(proj, "bin", "gen")); !os.IsNotExist(err) {
		t.Errorf("gen/ leaked into output dir: %v", err)
	}
}

func TestFindJavaNeverRelative(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	exe := javaExe()
	if err := os.MkdirAll(filepath.Join(cwd, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{filepath.Join(cwd, exe), filepath.Join(cwd, "bin", exe)} {
		if err := os.WriteFile(f, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := FindJava(context.Background(), func(k string) string {
		if k == "JAVA_HOME" {
			return "."
		}
		return ""
	}); err == nil {
		t.Error("relative JAVA_HOME must be rejected")
	}
	for _, path := range []string{".", "bin", ""} {
		t.Setenv("PATH", path)
		if p, err := lookPathAbs(exe); err == nil {
			t.Errorf("PATH=%q: got cwd-relative java %q", path, p)
		}
		if runtime.GOOS != "darwin" {
			if j, err := FindJava(context.Background(), func(string) string { return "" }); err == nil && !filepath.IsAbs(j.Path) {
				t.Errorf("PATH=%q: FindJava returned %q", path, j.Path)
			}
		}
	}
}
