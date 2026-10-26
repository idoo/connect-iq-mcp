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
// It only ever returns an absolute path: a relative JAVA_HOME or a PATH hit
// in (or relative to) the working directory is rejected, so a repo cannot
// plant its own java.
func FindJava(ctx context.Context, getenv func(string) string) (Java, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	exe := javaExe()
	var j Java
	if h := getenv("JAVA_HOME"); h != "" {
		if !filepath.IsAbs(h) {
			return Java{}, errors.New("JAVA_HOME must be an absolute path, got " + h)
		}
		p := filepath.Join(h, "bin", exe)
		if !exists(p) {
			return Java{}, errors.New("JAVA_HOME is set but " + p + " does not exist")
		}
		j = Java{Path: p, Source: "JAVA_HOME"}
	} else if p, err := lookPathAbs(exe); err == nil && !isMacStub(ctx, p) {
		j = Java{Path: p, Source: "PATH"}
	} else if runtime.GOOS == "darwin" {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		out, err := exec.CommandContext(cctx, "/usr/libexec/java_home").Output()
		home := string(bytes.TrimSpace(out))
		if err != nil || !filepath.IsAbs(home) {
			return Java{}, errors.New("no Java found (JAVA_HOME unset, no java on PATH, /usr/libexec/java_home failed); install Java 17+")
		}
		j = Java{Path: filepath.Join(home, "bin", exe), Source: "java_home"}
	} else {
		return Java{}, errors.New("no Java found (JAVA_HOME unset, no java on PATH); install Java 17+")
	}
	j.Version = javaVersion(ctx, j.Path)
	return j, nil
}

func javaExe() string {
	if runtime.GOOS == "windows" {
		return "java.exe"
	}
	return "java"
}

// lookPathAbs is exec.LookPath that refuses results in or relative to the
// working directory (exec.ErrDot, "." or relative PATH entries).
func lookPathAbs(name string) (string, error) {
	p, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(p) {
		return "", errors.New(name + " on PATH resolves relative to the working directory: " + p)
	}
	return p, nil
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
// directly when the jar and java are available, else the bin/monkeyc wrapper
// (never on Windows: the .bat wrappers would run through cmd.exe).
func (s SDK) MonkeycArgv(java string, args []string) []string {
	if java, ok := s.javaFor(java); ok {
		return append([]string{
			java, "-Xms1g", "-Dfile.encoding=UTF-8", "-Dapple.awt.UIElement=true",
			"-classpath", s.Jar(), ClassMonkeyc,
		}, args...)
	}
	return append([]string{filepath.Join(s.Bin(), "monkeyc")}, args...)
}

// MonkeydoArgv returns the argv to push prg to device on a running
// simulator (extra are appended, e.g. "-t", testName).
func (s SDK) MonkeydoArgv(java, prg, device string, extra ...string) []string {
	if java, ok := s.javaFor(java); ok {
		a := []string{java, "-classpath", s.Jar(), ClassMonkeydo, "-f", prg, "-d", device, "-s", filepath.Join(s.Bin(), "shell")}
		return append(a, extra...)
	}
	return append([]string{filepath.Join(s.Bin(), "monkeydo"), prg, device}, extra...)
}

// javaFor picks the java to run the jar with. On Windows it always uses the
// jar: a missing java becomes an absolute PATH lookup, or the bare name so the
// command fails to start (os/exec refuses cwd-relative hits) instead of
// falling back to a .bat wrapper.
func (s SDK) javaFor(java string) (string, bool) {
	if java != "" && exists(s.Jar()) {
		return java, true
	}
	if runtime.GOOS != "windows" {
		return "", false
	}
	if p, err := lookPathAbs(javaExe()); err == nil {
		return p, true
	}
	return javaExe(), true
}
