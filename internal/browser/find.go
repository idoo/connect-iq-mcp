// Package browser drives a dedicated Chromium-based browser profile to sign in
// to Garmin and capture the apps.garmin.com portal cookies and, optionally, a
// Garmin Connect mobile service ticket.
package browser

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/idoo/garmin-mcp/internal/state"
)

// ErrNoBrowser means no Chrome, Brave, Edge or Chromium executable was found.
var ErrNoBrowser = errors.New("no Chromium-based browser found")

// EnvBrowser overrides browser auto-detection. The value "bundled" forces the
// pinned Chrome for Testing build.
const EnvBrowser = "GARMIN_MCP_BROWSER"

// Bundled is the browser path or $GARMIN_MCP_BROWSER value that selects Chrome for Testing.
const Bundled = "bundled"

// Find resolves the browser executable: an explicit path first, then
// $GARMIN_MCP_BROWSER ("bundled" forces Chrome for Testing), then the system
// default browser if it is Chromium-based, then other installed Chromium
// browsers, then an already downloaded Chrome for Testing. It never downloads.
func Find(path string) (string, error) {
	p, tried := locateHere(path, defaultBinDir())
	if !p.cft || isFile(p.exe) {
		return p.exe, nil
	}
	return "", noBrowser(tried, p.exe)
}

// resolveBrowser is Find plus the Chrome for Testing download when needed. It
// also returns the browser's display name and an optional note explaining
// why it is not the user's default browser.
func resolveBrowser(ctx context.Context, path, binDir string, log io.Writer) (exe, name, note string, err error) {
	p, tried := locateHere(path, binDir)
	if p.cft {
		if !isFile(p.exe) {
			if p.exe == "" {
				return "", "", "", noBrowser(tried, p.exe)
			}
			if p.exe, err = ensureCfT(ctx, binDir, log); err != nil {
				return "", "", "", err
			}
		}
		return p.exe, "Chrome for Testing", p.note, nil
	}
	return p.exe, displayName(p.exe), p.note, nil
}

func locateHere(path, binDir string) (pick, []string) {
	home, _ := os.UserHomeDir()
	return locate(path, os.Getenv(EnvBrowser), detectDefault, candidates(runtime.GOOS, home, os.Getenv), cftExe(binDir, cftPlatform(runtime.GOOS, runtime.GOARCH)))
}

// pick is locate's answer. cft means exe is the Chrome for Testing path,
// which may not be downloaded yet ("" when the platform has no build). note,
// if set, tells the user why their default browser is not used.
type pick struct {
	exe  string
	cft  bool
	note string
}

// locate applies the precedence. def is only consulted when neither an
// explicit path nor the environment chooses the browser.
func locate(path, env string, def func() defaultBrowser, cands []string, cftPath string) (pick, []string) {
	var tried []string
	for _, p := range []string{path, env} {
		if p == "" {
			continue
		}
		if strings.EqualFold(p, Bundled) {
			return pick{exe: cftPath, cft: true}, tried
		}
		if r, ok := resolve(p); ok {
			return pick{exe: r}, nil
		}
		tried = append(tried, p)
	}
	var d defaultBrowser
	if def != nil {
		d = def()
	}
	if d.Chromium && d.Exe != "" {
		if r, ok := resolve(d.Exe); ok {
			return pick{exe: r}, nil
		}
		tried = append(tried, d.Exe)
	}
	because := func(using string) string {
		if d.Name == "" {
			return ""
		}
		return "Your default browser (" + d.Name + ") can't be driven; using " + using + " in a separate profile."
	}
	for _, c := range cands {
		if r, ok := resolve(c); ok {
			return pick{exe: r, note: because(displayName(r))}, nil
		}
		tried = append(tried, c)
	}
	note := because("Chrome for Testing " + cftVersion)
	if note == "" {
		note = "No Chromium-based browser found; using Chrome for Testing " + cftVersion + " in a separate profile."
	}
	return pick{exe: cftPath, cft: true, note: note}, tried
}

func noBrowser(tried []string, cftPath string) error {
	msg := "no Chrome for Testing build for this platform"
	if cftPath != "" {
		msg = "Chrome for Testing " + cftVersion + " not downloaded yet (garmin-mcp auth login fetches it)"
	}
	return fmt.Errorf("%w (tried: %s; %s); install Chrome, Brave, Edge or Chromium, or set %s", ErrNoBrowser, strings.Join(tried, ", "), msg, EnvBrowser)
}

// defaultBinDir is <state dir>/browser-bin.
func defaultBinDir() string {
	d, err := state.DefaultDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "browser-bin")
}

// appBundle returns the .app bundle containing a macOS executable, or "".
func appBundle(exe string) string {
	i := strings.Index(exe, ".app/Contents/MacOS/")
	if i < 0 {
		return ""
	}
	return exe[:i+len(".app")]
}

// resolve accepts an absolute/relative path to an existing file or a bare
// command name found on PATH.
func resolve(p string) (string, bool) {
	if !strings.ContainsRune(p, filepath.Separator) && !strings.ContainsRune(p, '/') {
		r, err := exec.LookPath(p)
		return r, err == nil
	}
	fi, err := os.Stat(p) //nolint:gosec // G703: stat only; p is the user's own browser path setting
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
