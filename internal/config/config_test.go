package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// LC-8: profile names become file names and keychain accounts.
func TestValidateProfileName(t *testing.T) {
	for _, ok := range []string{"default", "work", "a", "0", "work-2_b", strings.Repeat("a", 63)} {
		if err := ValidateProfileName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", ".", "..", "a#1", "Work", "a/b", `a\b`, "-a", "_a", "a.b", "a b", strings.Repeat("a", 64)} {
		if err := ValidateProfileName(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestConfigRejectsBadProfileKeys(t *testing.T) {
	if _, _, err := Parse([]byte("[profiles.\"..\"]\n")); err == nil || !strings.Contains(err.Error(), "invalid profile name") {
		t.Fatalf("err = %v", err)
	}
}

// SG-3: allow_profile_switch was removed; old config files must still load.
func TestLegacyAllowProfileSwitchLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "garmin-mcp", "config.toml")
	writeFile(t, path, "default_profile = \"work\"\nallow_profile_switch = true\n\n[profiles.work]\nallow_writes = true\n")
	cfg, warnings, err := Load(path)
	if err != nil {
		t.Fatalf("legacy config refused: %v", err)
	}
	if cfg.DefaultProfile != "work" || !cfg.Profiles["work"].AllowWrites {
		t.Fatalf("cfg = %+v", cfg)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %q, want one for the retired key", warnings)
	}
}

func TestResolveRejectsBadProfileFlag(t *testing.T) {
	dir := t.TempDir()
	_, err := Resolve(Options{
		ConfigPath: filepath.Join(dir, "missing.toml"), Home: dir, Cwd: dir,
		Getenv: func(string) string { return "" },
		Flags:  Flags{Profile: "../x"},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid profile name") {
		t.Fatalf("err = %v", err)
	}
}

// LD-6: a 0664 file in the user's private group is fine; 0666 never is; a
// shared group is not.
func TestGroupWritableUserPrivateGroup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX modes")
	}
	path := filepath.Join(t.TempDir(), PinFileName)
	if err := os.WriteFile(path, []byte("allow_writes = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	gid, ok := fileGid(info)
	if !ok {
		t.Fatal("no gid")
	}
	g, gn, un := getgid, lookupGroupName, currentUserName
	t.Cleanup(func() { getgid, lookupGroupName, currentUserName = g, gn, un })
	getgid = func() int { return gid }
	currentUserName = func() (string, error) { return "ivan", nil }

	check := func(mode os.FileMode, group string, wantOK bool) {
		t.Helper()
		lookupGroupName = func(int) (string, error) { return group, nil }
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		_, err := loadPin(path)
		if (err == nil) != wantOK {
			t.Errorf("mode %04o group %q: err = %v, want ok=%v", mode, group, err, wantOK)
		}
	}
	check(0o664, "ivan", true)
	check(0o666, "ivan", false)
	check(0o664, "staff", false)
	check(0o644, "staff", true)
	getgid = func() int { return gid + 1 }
	check(0o664, "ivan", false)
}
