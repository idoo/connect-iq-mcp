package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/idoo/garmin-mcp/internal/config"
	"github.com/idoo/garmin-mcp/internal/state"
)

// authTestEnv isolates the auth CLI in temp dirs with the keychain off and
// returns the path of the default profile's credentials file.
func authTestEnv(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for k, v := range map[string]string{
		"HOME":                             home,
		"USERPROFILE":                      home,
		"XDG_CONFIG_HOME":                  filepath.Join(home, "cfg"),
		"AppData":                          filepath.Join(home, "cfg"),
		"XDG_STATE_HOME":                   filepath.Join(home, "state"),
		"LocalAppData":                     filepath.Join(home, "state"),
		"GARMIN_MCP_KEYRING":               "off",
		"GARMIN_MCP_PROFILE":               "",
		"GARMIN_MCP_PORTAL_COOKIES":        "",
		"GARMIN_MCP_CONNECT_REFRESH_TOKEN": "",
	} {
		t.Setenv(k, v)
	}
	dir, err := state.DefaultDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "credentials", config.DefaultProfileName+".json")
}

// R8: the `auth` CLI layer: exit codes and key output tokens.
func TestAuthCLI(t *testing.T) {
	seal := "Fe26.2**salt*iv*enc*" + strconv.FormatInt(time.Now().Add(30*24*time.Hour).UnixMilli(), 10) + "*hsalt*hmac"
	cases := []struct {
		name    string
		args    []string
		stored  string // credentials file content; "" = none
		stdin   string
		code    int
		want    []string
		notWant []string
	}{
		{name: "status signed out", args: []string{"status"}, code: 0, want: []string{"not signed in"}},
		{
			name: "status signed in", args: []string{"status", "--json"}, code: 0,
			stored: `{"portal":{"cookies":"session=` + seal + `"}}`,
			want:   []string{`"present": true`, `"profile": "default"`}, notWant: []string{"Fe26"},
		},
		{name: "status unreadable", args: []string{"status"}, stored: "{not json", code: 1, want: []string{"unreadable"}},
		{name: "export nothing stored", args: []string{"export", "--portal"}, code: 1, want: []string{"auth_required"}},
		{
			name: "paste garbage", args: []string{"login", "--paste"}, code: 1,
			stdin: "garbage SECRETNAME=hunter2secret; other", notWant: []string{"hunter2secret"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cred := authTestEnv(t)
			if tc.stored != "" {
				if err := os.MkdirAll(filepath.Dir(cred), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(cred, []byte(tc.stored), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			old := authStdin
			authStdin = strings.NewReader(tc.stdin)
			t.Cleanup(func() { authStdin = old })
			var stdout, stderr bytes.Buffer
			code := authCLI(tc.args, &stdout, &stderr)
			out := stdout.String() + stderr.String()
			if code != tc.code {
				t.Fatalf("exit = %d, want %d\n%s", code, tc.code, out)
			}
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("output lacks %q:\n%s", w, out)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(out, w) {
					t.Errorf("output contains %q:\n%s", w, out)
				}
			}
		})
	}
}
