package ciqsdk

import (
	"archive/zip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// confineTree makes <base>/root/products/app (the project, with source/ and
// resources/strings/), <base>/root/source (shared code two levels up) and
// <base>/outside/secret; it returns root, project and outside.
func confineTree(t *testing.T) (root, proj, outside string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root, outside = filepath.Join(base, "root"), filepath.Join(base, "outside")
	proj = filepath.Join(root, "products", "app")
	for _, d := range []string{
		filepath.Join(proj, "source"), filepath.Join(proj, "resources", "strings"),
		filepath.Join(root, "source"), filepath.Join(outside, "secret", "sub"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "secret", "Leak.mc"), []byte("LEAKMARKER"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root, proj, outside
}

func writeJungle(t *testing.T, proj, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(proj, "monkey.jungle"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeBarrel(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for n, body := range files {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(body))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
}

func mustRefuse(t *testing.T, name string, err error, want ...string) {
	t.Helper()
	var je *JungleError
	if !errors.As(err, &je) {
		t.Fatalf("%s: err = %v, want a JungleError", name, err)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("%s: %q lacks %q", name, err, w)
		}
	}
}

// RS-2: values are read as monkeyc reads them (quotes stripped, \ as a
// separator, symlinks before .. followed) and must stay inside the root.
func TestCheckJungleRefusesEscapes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	root, proj, outside := confineTree(t)
	secret := filepath.Join(outside, "secret")
	if err := os.Symlink(filepath.Join(secret, "sub"), filepath.Join(proj, "link")); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ body, want string }{
		"quoted absolute":      {"base.sourcePath = source;\"" + secret + "\"\n", "absolute"},
		"absolute inside":      {"base.sourcePath = " + filepath.Join(root, "source") + "\n", "absolute"},
		"drive letter":         {"base.sourcePath = C:/x\n", "drive letter"},
		"unc":                  {"base.sourcePath = \\\\host\\share\n", "absolute or UNC"},
		"tilde":                {"base.sourcePath = ~/x\n", "~"},
		"inner quote":          {"base.sourcePath = so\"urce\n", "quote"},
		"quoted dotdot":        {"base.sourcePath = \"../../../outside/secret\"\n", "resolves outside the root"},
		"backslash dotdot":     {"base.sourcePath = source;..\\..\\..\\outside\\secret\n", "resolves outside the root"},
		"symlink dotdot":       {"base.sourcePath = source;link/..\n", "resolves outside the root"},
		"symlink":              {"base.sourcePath = source;link\n", "resolves outside the root"},
		"via variable":         {"lib = ../../../outside\nbase.sourcePath = $(lib)\n", "resolves outside the root"},
		"missing past outside": {"base.sourcePath = ../../../outside/nothing/x\n", "resolves outside the root"},
		"missing then dotdot":  {"base.sourcePath = nothing/../../../../outside\n", "resolves outside the root"},
	} {
		writeJungle(t, proj, c.body)
		mustRefuse(t, name, CheckJungle(root, proj, ""), "Path = ", c.want, "inside the root")
	}
	// A symlink inside a named folder that leaves the root.
	writeJungle(t, proj, "base.sourcePath = source\n")
	if err := os.Symlink(secret, filepath.Join(proj, "source", "lib")); err != nil {
		t.Fatal(err)
	}
	mustRefuse(t, "symlink in folder", CheckJungle(root, proj, ""), "source/lib")
	// The same symlink in the default source folder, with no sourcePath at all.
	writeJungle(t, proj, "project.manifest = manifest.xml\n")
	mustRefuse(t, "symlink in default folder", CheckJungle(root, proj, ""))
	_ = os.Remove(filepath.Join(proj, "source", "lib"))
	// The jungle path itself.
	mustRefuse(t, "semicolon", CheckJungle(root, proj, "monkey.jungle;x.jungle"))
	mustRefuse(t, "variable", CheckJungle(root, proj, "$(x).jungle"))
	if err := os.WriteFile(filepath.Join(outside, "x.jungle"), []byte("base.sourcePath = source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRefuse(t, "jungle outside", CheckJungle(root, proj, filepath.Join(outside, "x.jungle")), "outside the root")
	mustRefuse(t, "missing jungle outside", CheckJungle(root, proj, filepath.Join(outside, "y.jungle")), "outside the root")
	// A nested jungle is held to the same rules.
	if err := os.WriteFile(filepath.Join(proj, "extra.jungle"), []byte("base.sourcePath = ../../../outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeJungle(t, proj, "base.sourcePath = extra.jungle\n")
	mustRefuse(t, "nested jungle", CheckJungle(root, proj, ""), "extra.jungle")
}

// Quoted parent paths that stay inside the root pass (WatchFaceKit's
// products/<name>/monkey.jungle shape); the same value fails when the root
// is the project itself.
func TestCheckJungleAllowsParentsInsideRoot(t *testing.T) {
	root, proj, _ := confineTree(t)
	for _, body := range []string{
		"base.sourcePath = source;\"../../source\"\n",
		"base.sourcePath = source;'../../source'\n",
		"base.sourcePath = source;..\\..\\source\n",
		"base.resourcePath = resources\nx.resourcePath = $(base.resourcePath);resources/strings;\"../../source/missing/x\"\n",
	} {
		writeJungle(t, proj, body)
		if err := CheckJungle(root, proj, ""); err != nil {
			t.Fatalf("%q: %v", body, err)
		}
	}
	writeJungle(t, proj, "base.sourcePath = source;\"../../source\"\n")
	mustRefuse(t, "root = project", CheckJungle(proj, proj, ""), "resolves outside the root")
	mustRefuse(t, "empty root means project", CheckJungle("", proj, ""), "resolves outside the root")
}

// RS-2: every jungle inside a barrel is checked, and an unreadable barrel is refused.
func TestCheckJungleChecksBarrels(t *testing.T) {
	root, proj, outside := confineTree(t)
	if err := os.MkdirAll(filepath.Join(proj, "barrels"), 0o755); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(proj, "barrels", "Lib.barrel")
	writeBarrel(t, bad, map[string]string{"manifest.xml": "<x/>", "Lib/monkey.jungle": "base.sourcePath = " + outside + "\n"})
	writeJungle(t, proj, "base.barrelPath = barrels/Lib.barrel\n")
	mustRefuse(t, "barrel file", CheckJungle(root, proj, ""), "Lib/monkey.jungle")
	writeJungle(t, proj, "base.barrelPath = barrels\n")
	mustRefuse(t, "barrel in folder", CheckJungle(root, proj, ""), "Lib/monkey.jungle")
	writeBarrel(t, bad, map[string]string{"monkey.jungle": "base.sourcePath = source;\"..\\x\"\n"})
	mustRefuse(t, "barrel dotdot", CheckJungle(root, proj, ""), "leaves the barrel")
	if err := os.WriteFile(bad, []byte("not a zip"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRefuse(t, "unreadable barrel", CheckJungle(root, proj, ""), "cannot be read")
	writeBarrel(t, bad, map[string]string{"monkey.jungle": "base.sourcePath = \"source\"\nbase.resourcePath = resources\n"})
	if err := CheckJungle(root, proj, ""); err != nil {
		t.Fatalf("clean barrel refused: %v", err)
	}
}

// A normal project passes: relative paths, subfolders, compiler-default
// references, a symlink that stays inside, and no jungle at all.
func TestCheckJungleAllowsNormalProjects(t *testing.T) {
	root, proj, _ := confineTree(t)
	if err := CheckJungle(root, proj, ""); err != nil {
		t.Fatalf("missing jungle: %v", err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Symlink(filepath.Join(proj, "resources", "strings"), filepath.Join(proj, "source", "strings")); err != nil {
			t.Fatal(err)
		}
	}
	writeJungle(t, proj, "project.manifest = manifest.xml\n"+
		"base.sourcePath = $(base.sourcePath);source;./source\n"+
		"base.resourcePath = resources/strings;resources-missing\n"+
		"fenix7.resourcePath = $(base.resourcePath)\n"+
		"base.excludeAnnotations = /etc;\"x\"\n")
	for _, r := range []string{root, proj, ""} {
		if err := CheckJungle(r, proj, ""); err != nil {
			t.Fatalf("root %q: %v", r, err)
		}
	}
	if err := CheckJungle(root, proj, filepath.Join(proj, "monkey.jungle")); err != nil {
		t.Fatal(err)
	}
}

// RS-1: Build itself refuses, so no caller can reach monkeyc with an escaping jungle.
func TestBuildRefusesEscapingJungleBeforeMonkeyc(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script java")
	}
	sdk, _ := fakeMonkeycSDK(t)
	marker := filepath.Join(t.TempDir(), "ran")
	java := filepath.Join(t.TempDir(), "java")
	if err := os.WriteFile(java, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, proj, _ := confineTree(t)
	writeJungle(t, proj, "base.sourcePath = ../../../outside\n")
	for _, f := range []func(context.Context, BuildOpts) (BuildResult, error){Build, Export} {
		_, err := f(context.Background(), BuildOpts{SDK: sdk, Java: java, ProjectDir: proj, Root: root, Device: "fenix7", Key: "/k", Output: filepath.Join(proj, "bin", "a.prg")})
		mustRefuse(t, "build", err, "base.sourcePath = ")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("monkeyc ran with an escaping jungle")
	}
}
