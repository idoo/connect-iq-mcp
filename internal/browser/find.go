// Package browser drives a dedicated Chromium-based browser profile to sign in
// to Garmin and capture the apps.garmin.com portal cookies and, optionally, a
// Garmin Connect mobile service ticket.
package browser

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ErrNoBrowser means no Chrome, Brave, Edge or Chromium executable was found.
var ErrNoBrowser = errors.New("no Chromium-based browser found")

// EnvBrowser overrides browser auto-detection.
const EnvBrowser = "CIQ_MCP_BROWSER"

// Find resolves the browser executable: an explicit path first, then
// $CIQ_MCP_BROWSER, then well-known per-OS locations.
func Find(path string) (string, error) {
	var tried []string
	for _, p := range []string{path, os.Getenv(EnvBrowser)} {
		if p == "" {
			continue
		}
		if r, ok := resolve(p); ok {
			return r, nil
		}
		tried = append(tried, p)
	}
	home, _ := os.UserHomeDir()
	for _, c := range candidates(runtime.GOOS, home, os.Getenv) {
		if r, ok := resolve(c); ok {
			return r, nil
		}
		tried = append(tried, c)
	}
	return "", fmt.Errorf("%w (tried: %s); install Chrome, Brave, Edge or Chromium, or set %s", ErrNoBrowser, strings.Join(tried, ", "), EnvBrowser)
}

// resolve accepts an absolute/relative path to an existing file or a bare
// command name found on PATH.
func resolve(p string) (string, bool) {
	if !strings.ContainsRune(p, filepath.Separator) && !strings.ContainsRune(p, '/') {
		r, err := exec.LookPath(p)
		return r, err == nil
	}
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return "", false
	}
	return p, true
}

// candidates lists well-known browser executables for goos. Linux entries are
// bare command names resolved on PATH.
func candidates(goos, home string, getenv func(string) string) []string {
	switch goos {
	case "darwin":
		apps := []struct{ bundle, bin string }{
			{"Google Chrome", "Google Chrome"},
			{"Brave Browser", "Brave Browser"},
			{"Microsoft Edge", "Microsoft Edge"},
			{"Chromium", "Chromium"},
		}
		roots := []string{"/Applications"}
		if home != "" {
			roots = append(roots, filepath.Join(home, "Applications"))
		}
		var out []string
		for _, a := range apps {
			for _, r := range roots {
				out = append(out, r+"/"+a.bundle+".app/Contents/MacOS/"+a.bin)
			}
		}
		return out
	case "windows":
		rels := []string{
			`Google\Chrome\Application\chrome.exe`,
			`BraveSoftware\Brave-Browser\Application\brave.exe`,
			`Microsoft\Edge\Application\msedge.exe`,
			`Chromium\Application\chrome.exe`,
		}
		var roots []string
		for _, k := range []string{"ProgramFiles", "ProgramFiles(x86)", "LocalAppData"} {
			if v := getenv(k); v != "" {
				roots = append(roots, v)
			}
		}
		var out []string
		for _, rel := range rels {
			for _, r := range roots {
				out = append(out, strings.TrimRight(r, `\`)+`\`+rel)
			}
		}
		return out
	default:
		return []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "brave-browser", "microsoft-edge", "microsoft-edge-stable"}
	}
}

// ProfileExists reports whether dir looks like a profile a previous login
// used (Chromium has written its cookie database there).
func ProfileExists(dir string) bool {
	if dir == "" {
		return false
	}
	for _, p := range []string{
		filepath.Join(dir, "Default", "Network", "Cookies"),
		filepath.Join(dir, "Default", "Cookies"),
	} {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Size() > 0 {
			return true
		}
	}
	return false
}
