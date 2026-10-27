package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testEnv is an installEnv rooted in temp dirs.
func testEnv(t *testing.T, goos string) installEnv {
	t.Helper()
	home := t.TempDir()
	return installEnv{goos: goos, home: home, configDir: filepath.Join(home, "cfg"), localAppData: filepath.Join(home, "local")}
}

func mkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
}

func mustContainServer(t *testing.T, path string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s not written: %v", path, err)
	}
	if !strings.Contains(string(b), `"`+serverName+`"`) {
		t.Fatalf("%s lacks the server:\n%s", path, b)
	}
}

// N2: --profile work with no [profiles.work] section is valid, and the note
// signs in that profile.
func TestConfigSnippetSectionlessProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "cfg"))
	t.Setenv("AppData", filepath.Join(home, "cfg"))
	var out, errOut bytes.Buffer
	if err := runConfig([]string{"snippet", "cursor", "--profile", "work"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"work"`) || !strings.Contains(errOut.String(), "`garmin-mcp auth login --profile work`") {
		t.Fatalf("stdout:\n%s\nstderr:\n%s", out.String(), errOut.String())
	}
}

// WD-7/LD-10: the per-user config dir comes from os.UserConfigDir
// (%AppData%, XDG_CONFIG_HOME, ~/Library/Application Support), not home.
func TestCurrentInstallEnvUsesUserConfigDir(t *testing.T) {
	dir := t.TempDir()
	var want string
	switch runtime.GOOS {
	case "windows":
		t.Setenv("AppData", dir)
		want = dir
	case "darwin", "ios", "plan9":
		t.Setenv("HOME", dir)
		t.Setenv("home", dir)
		want = filepath.Join(dir, "Library", "Application Support")
		if runtime.GOOS == "plan9" {
			want = filepath.Join(dir, "lib")
		}
	default:
		t.Setenv("XDG_CONFIG_HOME", dir)
		want = dir
	}
	e, err := currentInstallEnv()
	if err != nil {
		t.Fatal(err)
	}
	if e.configDir != want {
		t.Fatalf("configDir = %q, want %q", e.configDir, want)
	}
}

func TestInstallClaudeDesktopUsesConfigDir(t *testing.T) {
	e := testEnv(t, "windows")
	if err := install("claude-desktop", `C:\bin\garmin-mcp.exe`, entryOpts{}, e, io.Discard); err != nil {
		t.Fatal(err)
	}
	mustContainServer(t, filepath.Join(e.configDir, "Claude", "claude_desktop_config.json"))
	if _, err := os.Stat(filepath.Join(e.home, "AppData")); err == nil {
		t.Fatal("wrote under home\\AppData instead of the config dir")
	}
}

// WD-7: an MSIX Claude Desktop reads its virtualized copy; both are written.
func TestInstallClaudeDesktopWritesMSIXCopy(t *testing.T) {
	e := testEnv(t, "windows")
	msix := filepath.Join(e.localAppData, "Packages", "Claude_pzs8sxrjxfjjc", "LocalCache", "Roaming", "Claude")
	mkdir(t, msix)
	mkdir(t, filepath.Join(e.localAppData, "Packages", "Other_123", "LocalCache", "Roaming", "Claude"))
	var out bytes.Buffer
	if err := install("claude-desktop", `C:\bin\garmin-mcp.exe`, entryOpts{}, e, &out); err != nil {
		t.Fatal(err)
	}
	roaming := filepath.Join(e.configDir, "Claude", "claude_desktop_config.json")
	virt := filepath.Join(msix, "claude_desktop_config.json")
	mustContainServer(t, roaming)
	mustContainServer(t, virt)
	if _, err := os.Stat(filepath.Join(e.localAppData, "Packages", "Other_123", "LocalCache", "Roaming", "Claude", "claude_desktop_config.json")); err == nil {
		t.Fatal("wrote into a non-Claude package")
	}
	for _, p := range []string{roaming, virt} {
		if !strings.Contains(out.String(), p) {
			t.Errorf("output does not name %s:\n%s", p, out.String())
		}
	}
	// Off Windows the Packages tree is never consulted.
	e.goos = "linux"
	if got := clientConfigPaths("claude-desktop", e); len(got) != 1 {
		t.Fatalf("linux claude-desktop paths = %v", got)
	}
}

// LD-10: with XDG_CONFIG_HOME (configDir), the existing Code - OSS and
// VSCodium User dirs are chosen; Code's is not invented next to them.
func TestInstallVSCodeChoosesExistingForks(t *testing.T) {
	e := testEnv(t, "linux")
	oss := filepath.Join(e.configDir, "Code - OSS", "User")
	codium := filepath.Join(e.configDir, "VSCodium", "User")
	mkdir(t, oss)
	mkdir(t, codium)
	if err := install("vscode", "/usr/bin/garmin-mcp", entryOpts{}, e, io.Discard); err != nil {
		t.Fatal(err)
	}
	mustContainServer(t, filepath.Join(oss, "mcp.json"))
	mustContainServer(t, filepath.Join(codium, "mcp.json"))
	if _, err := os.Stat(filepath.Join(e.configDir, "Code")); err == nil {
		t.Fatal("created Code/User although only forks exist")
	}

	flat := testEnv(t, "linux")
	fp := filepath.Join(flat.home, ".var", "app", "com.visualstudio.code", "config", "Code", "User")
	mkdir(t, fp)
	if got := clientConfigPaths("vscode", flat); len(got) != 1 || got[0] != filepath.Join(fp, "mcp.json") {
		t.Fatalf("flatpak paths = %v", got)
	}

	none := testEnv(t, "linux")
	if got := clientConfigPaths("vscode", none); len(got) != 1 || got[0] != filepath.Join(none.configDir, "Code", "User", "mcp.json") {
		t.Fatalf("default path = %v", got)
	}
}

// LD-10: inside WSL the Windows clients never read Linux files, so install
// prints a wsl.exe snippet and writes nothing.
func TestInstallInWSLPrintsSnippet(t *testing.T) {
	for _, client := range []string{"claude-desktop", "vscode"} {
		e := testEnv(t, "linux")
		e.wslDistro = "Ubuntu-24.04"
		mkdir(t, filepath.Join(e.configDir, "Code", "User"))
		var out bytes.Buffer
		if err := install(client, "/home/u/go/bin/garmin-mcp", entryOpts{Profile: "work"}, e, &out); err != nil {
			t.Fatal(err)
		}
		s := out.String()
		for _, want := range []string{"not installed", `"wsl.exe"`, `"Ubuntu-24.04"`, `"/home/u/go/bin/garmin-mcp"`, `"--profile"`, `"work"`} {
			if !strings.Contains(s, want) {
				t.Errorf("%s: output lacks %s:\n%s", client, want, s)
			}
		}
		if strings.Contains(s, "installed "+serverName) {
			t.Errorf("%s: claims success:\n%s", client, s)
		}
		for _, p := range clientConfigPaths(client, e) {
			if _, err := os.Stat(p); err == nil {
				t.Errorf("%s: wrote %s", client, p)
			}
		}
	}
}

// L7: installing into a symlinked client config (dotfiles) updates the
// link's target, keeps the link, and keeps numbers exactly as written.
func TestInstallThroughSymlinkKeepsLinkAndNumbers(t *testing.T) {
	e := testEnv(t, runtime.GOOS)
	dotfiles := t.TempDir()
	target := filepath.Join(dotfiles, "mcp.json")
	orig := `{"mcpServers":{"other":{"command":"x","timeout":12345678901234567890}},"fontSize":13.50,"big":1e400}` + "\n"
	if err := os.WriteFile(target, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	link := clientConfigPaths("cursor", e)[0]
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := install("cursor", "/usr/local/bin/garmin-mcp", entryOpts{}, e, io.Discard); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("config link replaced by a regular file: %v %v", fi, err)
	}
	b, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{"12345678901234567890", "13.50", "1e400", `"other"`, `"` + serverName + `"`} {
		if !strings.Contains(got, want) {
			t.Errorf("target lost %s:\n%s", want, got)
		}
	}
	backups, _ := filepath.Glob(target + ".garmin-mcp-backup-*")
	if len(backups) != 1 {
		t.Errorf("backups next to the target = %v", backups)
	}
}

func TestInstallRefusesDanglingSymlink(t *testing.T) {
	e := testEnv(t, runtime.GOOS)
	link := clientConfigPaths("cursor", e)[0]
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone", "mcp.json"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := install("cursor", "/usr/local/bin/garmin-mcp", entryOpts{}, e, io.Discard); err == nil {
		t.Fatal("dangling symlink overwritten")
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("dangling link was replaced")
	}
}
