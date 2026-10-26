//go:build darwin

package browser

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

var detectDefault = func() defaultBrowser {
	home, _ := os.UserHomeDir()
	out, err := exec.Command("plutil", "-convert", "json", "-o", "-",
		filepath.Join(home, "Library", "Preferences", "com.apple.LaunchServices", "com.apple.launchservices.secure.plist")).Output()
	if err != nil {
		return defaultBrowser{}
	}
	id := httpsHandlerFromPlistJSON(out)
	if id == "" {
		return defaultBrowser{}
	}
	kb, ok := macBrowser(id)
	if !ok {
		return defaultBrowser{Name: id}
	}
	d := defaultBrowser{Name: kb.name, Chromium: kb.chromium}
	if !kb.chromium {
		return d
	}
	for _, app := range macAppPaths(id, kb.name, home) {
		bin, err := exec.Command("plutil", "-extract", "CFBundleExecutable", "raw", filepath.Join(app, "Contents", "Info.plist")).Output()
		if err != nil {
			continue
		}
		exe := filepath.Join(app, "Contents", "MacOS", strings.TrimSpace(string(bin)))
		if isFile(exe) {
			d.Exe = exe
			return d
		}
	}
	return d
}

// macAppPaths lists candidate .app bundles for a bundle id: Spotlight first,
// then the usual install locations.
func macAppPaths(id, name, home string) []string {
	var apps []string
	if out, err := exec.Command("mdfind", "kMDItemCFBundleIdentifier == '"+strings.ReplaceAll(id, "'", "")+"'c").Output(); err == nil {
		for _, l := range strings.Split(string(out), "\n") {
			if l = strings.TrimSpace(l); strings.HasSuffix(l, ".app") {
				apps = append(apps, l)
			}
		}
	}
	apps = append(apps, filepath.Join("/Applications", name+".app"))
	if home != "" {
		apps = append(apps, filepath.Join(home, "Applications", name+".app"))
	}
	return apps
}
