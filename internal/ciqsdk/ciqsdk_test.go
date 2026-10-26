package ciqsdk

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func mkSDK(t *testing.T, root, dir string) string {
	t.Helper()
	p := filepath.Join(root, "Sdks", dir)
	if err := os.MkdirAll(filepath.Join(p, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "bin", "monkeybrains.jar"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCompareVersionAndSort(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"10.0.0", "9.2.0", 1}, {"9.2.0", "9.2.0", 0}, {"8.4.0", "9.2.0", -1}, {"5.2", "5.2.0", 0}, {"6.0.3", "5.2.0", 1},
	}
	for _, c := range cases {
		if got := CompareVersion(c.a, c.b); got != c.want {
			t.Errorf("CompareVersion(%s,%s)=%d want %d", c.a, c.b, got, c.want)
		}
	}
	root := t.TempDir()
	mkSDK(t, root, "connectiq-sdk-mac-9.2.0-2026-06-09-92a1605b2")
	mkSDK(t, root, "connectiq-sdk-mac-10.0.0-2027-01-01-aaaa")
	mkSDK(t, root, "connectiq-sdk-mac-8.4.0-2025-12-03-5122605dc")
	os.MkdirAll(filepath.Join(root, "Sdks", "junk"), 0o755)
	s, err := ListSDKs(root)
	if err != nil {
		t.Fatal(err)
	}
	var vs []string
	for _, x := range s {
		vs = append(vs, x.Version)
	}
	if strings.Join(vs, ",") != "10.0.0,9.2.0,8.4.0" {
		t.Fatalf("order %v", vs)
	}
}

func TestParseCurrentCfg(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/a/b/sdk/\n", "/a/b/sdk"},
		{"  /a/b/sdk/  ", "/a/b/sdk"},
		{"Sdks/x/", "/root/Sdks/x"},
	}
	for _, c := range cases {
		got, err := ParseCurrentCfg([]byte(c.in), "/root")
		if err != nil || got != filepath.FromSlash(c.want) {
			t.Errorf("%q → %q, %v", c.in, got, err)
		}
	}
	if _, err := ParseCurrentCfg([]byte(" \n"), "/root"); err == nil {
		t.Error("empty cfg must fail")
	}
}

func TestResolvePrecedence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix paths")
	}
	root := t.TempDir()
	p92 := mkSDK(t, root, "connectiq-sdk-mac-9.2.0-2026-06-09-92a1605b2")
	p84 := mkSDK(t, root, "connectiq-sdk-mac-8.4.0-2025-12-03-5122605dc")
	cfg := func(s string) {
		if s == "" {
			os.Remove(filepath.Join(root, "current-sdk.cfg"))
			return
		}
		os.WriteFile(filepath.Join(root, "current-sdk.cfg"), []byte(s), 0o644)
	}
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	cases := []struct {
		name, arg, cfg string
		env            map[string]string
		wantPath       string
		wantSrc        string
		wantErr        bool
	}{
		{"newest", "", "", nil, p92, SourceNewest, false},
		{"cfg beats newest", "", p84 + "/", nil, p84, SourceConfig, false},
		{"broken cfg errors", "", root + "/Sdks/missing/", nil, "", "", true},
		{"env beats cfg", "", p84 + "/", map[string]string{"CIQ_SDK": "9.2.0"}, p92, SourceEnv, false},
		{"GARMIN_MCP_SDK beats CIQ_SDK", "", "", map[string]string{"CIQ_SDK": "9.2.0", "GARMIN_MCP_SDK": p84}, p84, SourceEnv, false},
		{"broken env errors", "", p84 + "/", map[string]string{"CIQ_SDK": "7.0.0"}, "", "", true},
		{"arg beats env", "8.4.0", "", map[string]string{"CIQ_SDK": "9.2.0"}, p84, SourceArg, false},
		{"arg prefix", "9.2", "", nil, p92, SourceArg, false},
		{"broken arg errors", "/nope", "", nil, "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg(c.cfg)
			r, err := Resolve(ResolveOpts{Arg: c.arg, DataRoot: root, Getenv: env(c.env)})
			if c.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", r)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if r.SDK.Path != c.wantPath || r.Source != c.wantSrc {
				t.Fatalf("got %s/%s want %s/%s", r.SDK.Path, r.Source, c.wantPath, c.wantSrc)
			}
		})
	}
}

func TestParseDiagnostics(t *testing.T) {
	out := "WARNING: fenix7: /p/source/A.mc:10: Unused variable 'x'.\r\n" +
		"ERROR: fenix7: /path/source/View.mc:42,8: Undefined symbol ':foo' detected.\n" +
		"ERROR: vivoactive6: Device not supported by this SDK.\n" +
		"BUILD: Complete\n"
	ds := ParseDiagnostics(out)
	want := []Diagnostic{
		{"warning", "fenix7", "/p/source/A.mc", 10, 0, "Unused variable 'x'."},
		{"error", "fenix7", "/path/source/View.mc", 42, 8, "Undefined symbol ':foo' detected."},
		{"error", "vivoactive6", "", 0, 0, "Device not supported by this SDK."},
	}
	if len(ds) != len(want) {
		t.Fatalf("got %d: %+v", len(ds), ds)
	}
	for i := range want {
		if ds[i] != want[i] {
			t.Errorf("%d: got %+v want %+v", i, ds[i], want[i])
		}
	}
}

func TestBuildArgs(t *testing.T) {
	o := BuildOpts{ProjectDir: "/p", Key: "/k", Output: "/o", Device: "fenix7", Release: true, Test: true}
	if _, err := BuildArgs(o, false); err == nil {
		t.Error("release+test must fail")
	}
	o.Test = false
	a, _ := BuildArgs(o, false)
	if strings.Join(a, " ") != "-f monkey.jungle -d fenix7 -r -y /k -o /o" {
		t.Errorf("args %v", a)
	}
	a, _ = BuildArgs(o, true)
	if strings.Join(a, " ") != "-f monkey.jungle -e -r -y /k -o /o" {
		t.Errorf("export args %v", a)
	}
}

const manifestSrc = `<?xml version="1.0"?>
<!-- generated -->
<iq:manifest xmlns:iq="http://www.garmin.com/xml/connectiq" version="1">
        <iq:application entry="App" id="93da161e" minSdkVersion="3.1.0" name="@Strings.AppName" type="watch-app">
                <iq:products>
                        <!-- Approach Series -->
                        <iq:product id="approachs50"/>
                        <iq:product id="fenix7"/>
                </iq:products>
                <iq:permissions>
                        <iq:uses id="Positioning"/>
                </iq:permissions>
                <iq:languages>
                        <iq:language>eng</iq:language>
                        <iq:language>swe</iq:language>
                </iq:languages>
        </iq:application>
</iq:manifest>
`

func TestManifestParseAndProducts(t *testing.T) {
	m, err := ParseManifest([]byte(manifestSrc))
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "93da161e" || m.Type != "watch-app" || m.MinAPILevel != "3.1.0" ||
		strings.Join(m.Products, ",") != "approachs50,fenix7" ||
		strings.Join(m.Permissions, ",") != "Positioning" || strings.Join(m.Languages, ",") != "eng,swe" {
		t.Fatalf("parsed %+v", m)
	}
	nb, added, removed, err := SetProducts([]byte(manifestSrc), []string{"vivoactive6", "fenix7"}, []string{"approachs50"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(added, ",") != "vivoactive6" || strings.Join(removed, ",") != "approachs50" {
		t.Fatalf("added %v removed %v", added, removed)
	}
	want := strings.Replace(manifestSrc, "                        <iq:product id=\"approachs50\"/>\n", "", 1)
	want = strings.Replace(want, "                </iq:products>", "                        <iq:product id=\"vivoactive6\"/>\n                </iq:products>", 1)
	if string(nb) != want {
		t.Fatalf("got\n%s", nb)
	}
	// Round trip back restores the original product set (order aside).
	back, _, _, err := SetProducts(nb, []string{"approachs50"}, []string{"vivoactive6"})
	if err != nil {
		t.Fatal(err)
	}
	m2, _ := ParseManifest(back)
	if strings.Join(m2.Products, ",") != "fenix7,approachs50" {
		t.Fatalf("round trip %v", m2.Products)
	}
	// No-op leaves bytes identical.
	same, a, r, _ := SetProducts([]byte(manifestSrc), []string{"fenix7"}, []string{"nope"})
	if !bytes.Equal(same, []byte(manifestSrc)) || len(a)+len(r) != 0 {
		t.Fatal("no-op changed the file")
	}
	if _, _, _, err := SetProducts([]byte(manifestSrc), []string{"../x"}, nil); err == nil {
		t.Fatal("bad id accepted")
	}
}

func TestManifestEmptyProducts(t *testing.T) {
	src := "<iq:manifest xmlns:iq=\"x\">\n    <iq:application id=\"a\" type=\"watchface\">\n        <iq:products/>\n    </iq:application>\n</iq:manifest>\n"
	nb, added, _, err := SetProducts([]byte(src), []string{"fenix7"}, nil)
	if err != nil || len(added) != 1 {
		t.Fatal(err, added)
	}
	m, err := ParseManifest(nb)
	if err != nil || strings.Join(m.Products, ",") != "fenix7" {
		t.Fatalf("%v %v\n%s", err, m.Products, nb)
	}
}

func TestKeyGenerateCheck(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k.der")
	ki, err := generateKey(p, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if ki.Bits != 1024 || ki.OK || len(ki.Fingerprint) != 64 {
		t.Fatalf("%+v", ki)
	}
	if runtime.GOOS != "windows" && ki.Mode != "0600" {
		t.Fatalf("mode %s", ki.Mode)
	}
	if _, err := generateKey(p, 1024); err == nil {
		t.Fatal("overwrite allowed")
	}
	again, err := CheckKey(p)
	if err != nil || again.Fingerprint != ki.Fingerprint {
		t.Fatal(err, again)
	}
	if runtime.GOOS != "windows" {
		os.Chmod(p, 0o644)
		w, _ := CheckKey(p)
		if len(w.Warnings) != 2 {
			t.Fatalf("warnings %v", w.Warnings)
		}
	}
	bad := filepath.Join(t.TempDir(), "bad.der")
	os.WriteFile(bad, []byte("nope"), 0o600)
	if _, err := CheckKey(bad); err == nil {
		t.Fatal("garbage key accepted")
	}
}

func TestLoadDevice(t *testing.T) {
	root := t.TempDir()
	d := filepath.Join(root, "Devices", "fenix7")
	os.MkdirAll(d, 0o755)
	os.WriteFile(filepath.Join(d, "compiler.json"), []byte(`{"deviceId":"fenix7","displayName":"fēnix 7","deviceFamily":"round-260x260","displayType":"mip","bitsPerPixel":8,"resolution":{"width":260,"height":260},"appTypes":[{"type":"watchApp","memoryLimit":131072}],"partNumbers":[{"connectIQVersion":"5.1.0","languages":[{"code":"eng"}]},{"connectIQVersion":"5.2.0","languages":[{"code":"swe"},{"code":"eng"}]}]}`), 0o644)
	os.WriteFile(filepath.Join(d, "simulator.json"), []byte(`{"display":{"shape":"round","isTouch":true},"keys":[{"id":"enter"},{"id":"esc"}]}`), 0o644)
	devs, warn, err := ListDevices(root)
	if err != nil || len(warn) != 0 || len(devs) != 1 {
		t.Fatal(err, warn, devs)
	}
	x := devs[0]
	if x.CIQVersion != "5.2.0" || x.Shape != "round" || x.Touch == nil || !*x.Touch || strings.Join(x.Buttons, ",") != "enter,esc" ||
		strings.Join(x.Languages, ",") != "eng,swe" || x.AppTypes[0].MemoryLimit != 131072 {
		t.Fatalf("%+v", x)
	}
	if _, err := LoadDevice(root, "../x"); err == nil {
		t.Fatal("traversal accepted")
	}
}
