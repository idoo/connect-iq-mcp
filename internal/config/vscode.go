package config

import (
	"os"
	"path/filepath"
)

// VSCodeUserConfigs returns mcp.json in every existing VS Code User dir
// (Code, Code - OSS, VSCodium, and on Linux their Flatpaks), or Code's when
// none exists yet. These are the files `connect-iq-mcp config install vscode`
// writes; configDir is os.UserConfigDir.
func VSCodeUserConfigs(goos, home, configDir string) []string {
	dirs := []string{
		filepath.Join(configDir, "Code", "User"),
		filepath.Join(configDir, "Code - OSS", "User"),
		filepath.Join(configDir, "VSCodium", "User"),
	}
	if goos == "linux" {
		dirs = append(dirs,
			filepath.Join(home, ".var", "app", "com.visualstudio.code", "config", "Code", "User"),
			filepath.Join(home, ".var", "app", "com.vscodium.codium", "config", "VSCodium", "User"))
	}
	var out []string
	for _, d := range dirs {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			out = append(out, filepath.Join(d, "mcp.json"))
		}
	}
	if len(out) == 0 {
		out = []string{filepath.Join(dirs[0], "mcp.json")}
	}
	return out
}
