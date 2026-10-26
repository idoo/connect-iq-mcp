package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// L7: installing into a symlinked client config (dotfiles) updates the
// link's target, keeps the link, and keeps numbers exactly as written.
func TestInstallThroughSymlinkKeepsLinkAndNumbers(t *testing.T) {
	home := t.TempDir()
	dotfiles := t.TempDir()
	target := filepath.Join(dotfiles, "mcp.json")
	orig := `{"mcpServers":{"other":{"command":"x","timeout":12345678901234567890}},"fontSize":13.50,"big":1e400}` + "\n"
	if err := os.WriteFile(target, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	link := clientConfigPath("cursor", home)
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := install("cursor", "/usr/local/bin/garmin-mcp", "", home, io.Discard); err != nil {
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
	home := t.TempDir()
	link := clientConfigPath("cursor", home)
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone", "mcp.json"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := install("cursor", "/usr/local/bin/garmin-mcp", "", home, io.Discard); err == nil {
		t.Fatal("dangling symlink overwritten")
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("dangling link was replaced")
	}
}
