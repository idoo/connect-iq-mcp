package sdk

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/idoo/garmin-mcp/internal/ciqsdk"
	"github.com/idoo/garmin-mcp/internal/tool"
	"github.com/idoo/garmin-mcp/internal/tool/tooltest"
)

// addDevice installs a fake device definition under the data root.
func addDevice(t *testing.T, id, name string) {
	t.Helper()
	dr, err := dataRoot()
	if err != nil {
		t.Fatal(err)
	}
	d := filepath.Join(ciqsdk.DevicesDir(dr), id)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "compiler.json"), []byte(`{"deviceId":"`+id+`","displayName":"`+name+`","resolution":{"width":260,"height":260}}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func errText(t *testing.T, b tool.ErrorBody) string {
	t.Helper()
	out, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// J1: "Fenix 7" resolves to fenix7 with a note; "fenix" is ambiguous and
// lists the candidates; the list query ignores case, accents and ®.
func TestDeviceFreeTextNames(t *testing.T) {
	k, _, _ := exportFixture(t)
	addDevice(t, "fenix7", "fēnix® 7")
	addDevice(t, "fenix7pro", "fēnix® 7 Pro")
	addDevice(t, "venu2", "Venu® 2")

	res := tooltest.MustOK(t, callBuild(t, k, `{"project":"app","device":"Fenix 7","key":"dev.der"}`))
	if out := tooltest.Decode[buildOut](t, res); !out.OK || !strings.HasSuffix(out.Output, filepath.Join("bin", "fenix7.prg")) {
		t.Fatalf("build: %+v", out)
	}
	if !strings.Contains(strings.Join(res.Meta.Warnings, "\n"), `device "Fenix 7" → fenix7`) {
		t.Fatalf("no mapping note: %v", res.Meta.Warnings)
	}

	b := tooltest.MustFail(t, callBuild(t, k, `{"project":"app","device":"fenix","key":"dev.der"}`), "invalid_arguments")
	if s := errText(t, b); !strings.Contains(s, "fenix7") || !strings.Contains(s, "fenix7pro") || strings.Contains(s, "venu2") {
		t.Fatalf("ambiguous: %s", s)
	}

	get, _ := tool.Lookup("sdk_device_get")
	b = tooltest.MustFail(t, tooltest.Call(t, get, k.Env, `{"id":"Forerunner 9000"}`), "invalid_arguments")
	if s := errText(t, b); !strings.Contains(s, "unknown device") || !strings.Contains(s, "sdk_devices_list") {
		t.Fatalf("unknown free text: %s", s)
	}

	list, _ := tool.Lookup("sdk_devices_list")
	type listOut struct {
		Devices []deviceRow `json:"devices"`
	}
	for _, q := range []string{"Fenix 7", "fēnix 7", "FENIX® 7 PRO"} {
		out := tooltest.Decode[listOut](t, tooltest.MustOK(t, tooltest.Call(t, list, k.Env, `{"query":"`+q+`"}`)))
		if len(out.Devices) == 0 || !strings.HasPrefix(out.Devices[0].ID, "fenix7") {
			t.Errorf("query %q: %+v", q, out.Devices)
		}
	}
}

// J2: a root above the project gives the not-a-project hint listing the
// projects under it, for sdk_build and sdk_manifest_get.
func TestParentDirListsSubprojects(t *testing.T) {
	k, _, _ := exportFixture(t)
	addDevice(t, "fenix7", "fēnix® 7")
	other := filepath.Join(k.Files.Root, "watch", "Face2")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "manifest.xml"), []byte("<x/>"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := tooltest.MustFail(t, callBuild(t, k, `{"device":"fenix7","key":"dev.der"}`), "invalid_arguments")
	if s := errText(t, b); !strings.Contains(s, "is not a Connect IQ project (no manifest.xml)") || !strings.Contains(s, `pass project:\"\u003csubdir\u003e\"`) || !strings.Contains(s, "app") || !strings.Contains(s, "watch/Face2") {
		t.Fatalf("build: %s", s)
	}
	get, _ := tool.Lookup("sdk_manifest_get")
	b = tooltest.MustFail(t, tooltest.Call(t, get, k.Env, `{}`), "invalid_arguments")
	if s := errText(t, b); !strings.Contains(s, "not a Connect IQ project") || !strings.Contains(s, "app") {
		t.Fatalf("manifest_get: %s", s)
	}
}

// J8: project repeating the root's own name says to leave project out.
func TestProjectNamingRootSaysOmit(t *testing.T) {
	k, _, _ := exportFixture(t)
	addDevice(t, "fenix7", "fēnix® 7")
	root := filepath.Join(k.Files.Root, "app")
	k.Files.Root = root
	b := tooltest.MustFail(t, callBuild(t, k, `{"project":"app","device":"fenix7","key":"../dev.der"}`), "invalid_arguments")
	if s := errText(t, b); !strings.Contains(s, "names the root folder itself") || !strings.Contains(s, "leave project out") {
		t.Fatalf("J8: %s", s)
	}
}
