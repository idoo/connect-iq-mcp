package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/idoo/ciq-mcp/internal/tool"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestResolve(t *testing.T) {
	home := t.TempDir()
	proj := filepath.Join(home, "proj")
	cfgPath := filepath.Join(home, ".config", "ciq-mcp", "config.toml")
	writeFile(t, cfgPath, "default_profile = \"rw\"\n[profiles.rw]\nallow_writes = true\n[profiles.ro]\n")
	if err := os.Mkdir(filepath.Join(home, "proj"), 0o700); err != nil {
		t.Fatal(err)
	}
	resolve := func(env map[string]string, f Flags) *Resolved {
		t.Helper()
		r, err := Resolve(Options{Flags: f, Getenv: envMap(env), ConfigPath: cfgPath, Cwd: proj, Home: home})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}

	r := resolve(nil, Flags{})
	if r.Profile.Name != "rw" || !r.Profile.AllowWrites || !slices.Equal(r.Toolsets, tool.DefaultToolsets) {
		t.Fatalf("default = %+v toolsets=%v", r.Profile, r.Toolsets)
	}
	if r = resolve(map[string]string{EnvProfile: "ro", EnvToolsets: "+connect"}, Flags{}); r.Profile.Name != "ro" || r.Profile.AllowWrites || !slices.Contains(r.Toolsets, tool.Connect) {
		t.Fatalf("env = %+v toolsets=%v", r.Profile, r.Toolsets)
	}
	if r = resolve(map[string]string{EnvAllowWrites: "0"}, Flags{}); r.Profile.AllowWrites {
		t.Fatal("CIQ_MCP_ALLOW_WRITES=0 ignored")
	}

	// A pin can only narrow writes.
	writeFile(t, filepath.Join(proj, PinFileName), "allow_writes = false\n")
	if r = resolve(nil, Flags{}); r.Profile.AllowWrites || !r.WritesLocked || r.Profile.PinSource == "" {
		t.Fatalf("pinned = %+v locked=%v", r.Profile, r.WritesLocked)
	}

	// No config at all: an implicit read-only default profile.
	r, err := Resolve(Options{Getenv: envMap(nil), ConfigPath: filepath.Join(home, "none.toml"), Cwd: home, Home: home})
	if err != nil || r.Profile.Name != DefaultProfileName || r.Profile.AllowWrites {
		t.Fatalf("empty = %+v, %v", r, err)
	}
	if _, err := Resolve(Options{Flags: Flags{Profile: "nope"}, Getenv: envMap(nil), ConfigPath: cfgPath, Cwd: home, Home: home}); err == nil {
		t.Fatal("unknown profile accepted")
	}
}

func TestParseRejectsUnknownKeys(t *testing.T) {
	if _, err := Parse([]byte("[profiles.a]\nkey_id = \"x\"\n")); err == nil {
		t.Fatal("unknown key accepted")
	}
	if _, err := Parse([]byte("default_profile = \"missing\"\n")); err == nil {
		t.Fatal("undefined default_profile accepted")
	}
}
