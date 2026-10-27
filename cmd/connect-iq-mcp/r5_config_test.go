package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

// J5: config install --key/--java-home writes the env into the client
// entry (JSON clients, Claude Code's -e flags, Codex's env table).
func TestInstallWritesKeyAndJavaEnv(t *testing.T) {
	key := filepath.Join(t.TempDir(), "developer_key.der")
	if err := os.WriteFile(key, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	jdk := t.TempDir()
	o := entryOpts{Key: key, JavaHome: jdk}

	e := testEnv(t, "linux")
	if err := install("cursor", "/usr/local/bin/connect-iq-mcp", o, e, io.Discard); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(clientConfigPaths("cursor", e)[0])
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		MCPServers map[string]struct {
			Env map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if env := doc.MCPServers[serverName].Env; env["CIQ_MCP_KEY"] != key || env["JAVA_HOME"] != jdk {
		t.Fatalf("cursor env = %v\n%s", env, b)
	}

	args := claudeAddArgs("/usr/local/bin/connect-iq-mcp", o)
	if !slices.Contains(args, "CIQ_MCP_KEY="+key) || !slices.Contains(args, "JAVA_HOME="+jdk) {
		t.Fatalf("claude args = %v", args)
	}

	var codex struct {
		MCPServers map[string]struct {
			Env map[string]string `toml:"env"`
		} `toml:"mcp_servers"`
	}
	if err := toml.Unmarshal([]byte(codexBlock("/usr/local/bin/connect-iq-mcp", o)), &codex); err != nil || codex.MCPServers[serverName].Env["CIQ_MCP_KEY"] != key {
		t.Fatalf("codex: %v %+v", err, codex)
	}
}

// J5: a relative or missing --key is refused before anything is written.
func TestConfigRefusesRelativeKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "cfg"))
	t.Setenv("AppData", filepath.Join(home, "cfg"))
	for _, args := range [][]string{
		{"install", "cursor", "--key", "developer_key.der"},
		{"snippet", "cursor", "--key", filepath.Join(home, "missing.der")},
		{"snippet", "cursor", "--java-home", "jdk"},
	} {
		var out, errOut bytes.Buffer
		err := runConfig(args, &out, &errOut)
		if err == nil || out.Len() != 0 {
			t.Fatalf("%v: err=%v out=%s", args, err, out.String())
		}
		if args[2] == "--key" && args[3] == "developer_key.der" && !strings.Contains(err.Error(), "absolute path") {
			t.Fatalf("%v: %v", args, err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".cursor", "mcp.json")); err == nil {
		t.Fatal("install wrote a config despite the refused key")
	}
}
