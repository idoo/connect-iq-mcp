package ciqsdk

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"time"
)

// Java is a discovered java executable.
type Java struct {
	Path    string `json:"path"`
	Version string `json:"version,omitempty"`
	Source  string `json:"source"`
}

var javaVersionRE = regexp.MustCompile(`version "([^"]+)"`)

// FindJava looks for java in JAVA_HOME, PATH, then macOS /usr/libexec/java_home.
func FindJava(ctx context.Context, getenv func(string) string) (Java, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	exe := "java"
	if runtime.GOOS == "windows" {
		exe = "java.exe"
	}
	var j Java
	if h := getenv("JAVA_HOME"); h != "" {
		p := filepath.Join(h, "bin", exe)
		if !exists(p) {
			return Java{}, errors.New("JAVA_HOME is set but " + p + " does not exist")
		}
		j = Java{Path: p, Source: "JAVA_HOME"}
	} else if p, err := exec.LookPath(exe); err == nil && !isMacStub(ctx, p) {
		j = Java{Path: p, Source: "PATH"}
	} else if runtime.GOOS == "darwin" {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(cctx, "/usr/libexec/java_home").Output()
		if err != nil {
			return Java{}, errors.New("no Java found (JAVA_HOME unset, no java on PATH, /usr/libexec/java_home failed); install Java 17+")
		}
		j = Java{Path: filepath.Join(string(bytes.TrimSpace(out)), "bin", exe), Source: "java_home"}
	} else {
		return Java{}, errors.New("no Java found (JAVA_HOME unset, no java on PATH); install Java 17+")
	}
	j.Version = javaVersion(ctx, j.Path)
	return j, nil
}

// isMacStub reports the macOS /usr/bin/java stub without a JDK behind it.
func isMacStub(ctx context.Context, p string) bool {
	if runtime.GOOS != "darwin" || p != "/usr/bin/java" {
		return false
	}
	return javaVersion(ctx, p) == ""
}

func javaVersion(ctx context.Context, p string) string {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, _ := exec.CommandContext(cctx, p, "-version").CombinedOutput()
	if m := javaVersionRE.FindSubmatch(out); m != nil {
		return string(m[1])
	}
	return ""
}

// Java entry points in bin/monkeybrains.jar (SDK 9.x bin scripts).
const (
	ClassMonkeyc  = "com.garmin.monkeybrains.Monkeybrains"
	ClassMonkeydo = "com.garmin.monkeybrains.monkeydodeux.MonkeyDoDeux"
	ClassBarrel   = "com.garmin.monkeybrains.MonkeyBarrelRunNoEvil"
)

// Jar returns the path of bin/monkeybrains.jar.
func (s SDK) Jar() string { return filepath.Join(s.Bin(), "monkeybrains.jar") }

// MonkeycArgv returns the argv (program first) for monkeyc. It calls Java
// directly when the jar and java are available, else the bin/ wrapper.
func (s SDK) MonkeycArgv(java string, args []string) []string {
	if java != "" && exists(s.Jar()) {
		return append([]string{java, "-Xms1g", "-Dfile.encoding=UTF-8", "-Dapple.awt.UIElement=true",
			"-classpath", s.Jar(), ClassMonkeyc}, args...)
	}
	return append([]string{filepath.Join(s.Bin(), wrapperName("monkeyc"))}, args...)
}

// MonkeydoArgv returns the argv to push prg to device on a running
// simulator (extra are appended, e.g. "-t", testName).
func (s SDK) MonkeydoArgv(java, prg, device string, extra ...string) []string {
	if java != "" && exists(s.Jar()) {
		a := []string{java, "-classpath", s.Jar(), ClassMonkeydo, "-f", prg, "-d", device, "-s", filepath.Join(s.Bin(), "shell")}
		return append(a, extra...)
	}
	return append([]string{filepath.Join(s.Bin(), wrapperName("monkeydo")), prg, device}, extra...)
}
