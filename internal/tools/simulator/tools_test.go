package simulator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/idoo/garmin-mcp/internal/ciqsdk"
	"github.com/idoo/garmin-mcp/internal/sim"
	"github.com/idoo/garmin-mcp/internal/tool"
	"github.com/idoo/garmin-mcp/internal/tool/tooltest"
)

func TestRegistered(t *testing.T) {
	for _, n := range []string{"sim_run", "sim_logs", "sim_stop", "sim_screenshot", "sim_input"} {
		tl, ok := tool.Lookup(n)
		if !ok {
			t.Fatalf("%s not registered", n)
		}
		if m := tl.Meta(); !m.Local || m.Confirm != tool.ConfirmNone {
			t.Fatalf("%s must be Local without confirm gate", n)
		}
	}
	// GUI tools are registered (and documented) everywhere but declared
	// macOS-only; the description says so.
	for _, n := range []string{"sim_screenshot", "sim_input"} {
		tl, _ := tool.Lookup(n)
		m := tl.Meta()
		if !m.SupportedOn("darwin") || m.SupportedOn("linux") || m.SupportedOn("windows") {
			t.Errorf("%s platforms %v", n, m.Platforms)
		}
		if !strings.HasPrefix(m.Description, tool.PlatformLabel(m.Platforms)+". ") {
			t.Errorf("%s description must start with %q", n, tool.PlatformLabel(m.Platforms))
		}
	}
}

// RS-1: sim_run and sim_run with test build a project through ciqsdk.Build,
// which refuses a jungle naming paths outside the project before monkeyc runs.
func TestRunRefusesEscapingJungleBeforeMonkeyc(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script java")
	}
	t.Setenv("HOME", t.TempDir())
	sdk := filepath.Join(t.TempDir(), "sdk")
	if err := os.MkdirAll(filepath.Join(sdk, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sdk, "bin", "monkeybrains.jar"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "ran")
	javaHome := t.TempDir()
	if err := os.MkdirAll(filepath.Join(javaHome, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n[ \"$1\" = -version ] && { echo 'openjdk version \"21\"' >&2; exit 0; }\ntouch '" + marker + "'\n"
	if err := os.WriteFile(filepath.Join(javaHome, "bin", "java"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GARMIN_MCP_SDK", sdk)
	t.Setenv("CIQ_SDK", "")
	t.Setenv("JAVA_HOME", javaHome)
	k := tooltest.New(t)
	proj := filepath.Join(k.Files.Root, "app")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(proj, "monkey.jungle"), []byte("base.sourcePath = "+outside+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ciqsdk.GenerateKey(filepath.Join(k.Files.Root, "dev.der")); err != nil {
		t.Fatal(err)
	}
	for _, test := range []bool{false, true} {
		_, err := simRun(context.Background(), k.Env, runIn{Project: "app", Device: "fenix7", Key: "dev.der", Test: test})
		if !errors.Is(err, tool.ErrInvalidArgs) || !strings.Contains(err.Error(), "base.sourcePath = "+outside) {
			t.Fatalf("test=%v: err = %v, want the jungle refusal", test, err)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("monkeyc ran with an escaping jungle")
	}
}

func TestInputRefusesWithoutAllowFocus(t *testing.T) {
	var te *tool.Error
	if _, err := simInput(context.Background(), nil, inputIn{Steps: []inputStep{{Press: "up"}}}); !errors.As(err, &te) || te.Code != "focus_required" {
		t.Fatalf("got %v", err)
	}
}

func TestInputStepValidation(t *testing.T) {
	bad := [][]inputStep{
		nil,
		{{}},
		{{Press: "up", WaitMS: 10}},
		{{Press: "home"}},
		{{Position: &position{Lat: 91}}},
		{{WaitMS: -1}},
		{{HoldMS: 200}},
		{{WaitMS: 61_000}},
		{{Press: "up", HoldMS: sim.MaxHoldMS + 1}},
		{{Press: "up", HoldMS: -1}},
		make([]inputStep, maxInputSteps+1),
	}
	for _, steps := range bad {
		_, err := simInput(context.Background(), nil, inputIn{Steps: steps, AllowFocus: true})
		if !errors.Is(err, tool.ErrInvalidArgs) {
			t.Errorf("%+v: %v", steps, err)
		}
	}
	ok := []inputStep{{Press: "menu", HoldMS: 1200}, {Position: &position{Lat: 59.3, Lon: 18.1}}, {WaitMS: 500}}
	if _, err := checkSteps(ok); err != nil {
		t.Fatal(err)
	}
}

func TestHoldCountsTowardTotalWait(t *testing.T) {
	total, err := checkSteps([]inputStep{{Press: "up", HoldMS: 5000}, {Press: "menu"}, {WaitMS: 500}})
	if err != nil {
		t.Fatal(err)
	}
	if want := 5000*time.Millisecond + sim.MenuHoldMS*time.Millisecond + 500*time.Millisecond; total != want {
		t.Fatalf("total %s, want %s", total, want)
	}
	// 13 five-second holds exceed the one-minute cap without any wait_ms.
	steps := make([]inputStep, 13)
	for i := range steps {
		steps[i] = inputStep{Press: "up", HoldMS: 5000}
	}
	if _, err := simInput(context.Background(), nil, inputIn{Steps: steps, AllowFocus: true}); !errors.Is(err, tool.ErrInvalidArgs) {
		t.Fatalf("long holds accepted: %v", err)
	}
}

func TestWaitCapWithoutProgress(t *testing.T) {
	if got := waitCap(nil, 5*time.Minute); got != tool.NoProgressWaitCap {
		t.Fatalf("got %s", got)
	}
	if got := waitCap(nil, time.Second); got != time.Second {
		t.Fatalf("got %s", got)
	}
}

func TestLocalSimToolsAvailableReadOnly(t *testing.T) {
	names := []string{"sim_run", "sim_stop", "sim_input", "sim_screenshot"}
	for _, n := range names {
		tl, _ := tool.Lookup(n)
		if !tl.ListedIn(tool.ProfileInfo{}) {
			t.Errorf("%s hidden in read-only mode", n)
		}
	}
}

func TestScreenshotIsAWriteTool(t *testing.T) {
	tl, _ := tool.Lookup("sim_screenshot")
	m := tl.Meta()
	a := m.Annotations()
	if m.Class != tool.Write || a.ReadOnlyHint || a.IdempotentHint || !m.AvailableReadOnly {
		t.Fatalf("meta %+v annotations %+v", m, a)
	}
}

func TestPRGMustBeARegularPRGFile(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "developer_key.der")
	_ = os.WriteFile(key, []byte("KEY"), 0o600)
	disguised := filepath.Join(dir, "dir.prg")
	_ = os.Mkdir(disguised, 0o700)
	for _, p := range []string{key, disguised, filepath.Join(dir, "missing.prg")} {
		if err := checkPRG(p); !errors.Is(err, tool.ErrInvalidArgs) {
			t.Errorf("%s accepted: %v", p, err)
		}
	}
	ok := filepath.Join(dir, "app.PRG")
	_ = os.WriteFile(ok, []byte("PRG"), 0o600)
	if err := checkPRG(ok); err != nil {
		t.Fatal(err)
	}
	// The key itself handed over as prg never reaches monkeydo.
	k := tooltest.New(t)
	kp := filepath.Join(k.Files.Root, "keys", "developer_key.der")
	_ = os.MkdirAll(filepath.Dir(kp), 0o700)
	_ = os.WriteFile(kp, []byte("KEY"), 0o600)
	if _, _, err := prgFor(context.Background(), k.Env, ciqsdk.Resolved{}, "/usr/bin/java", runIn{Device: "fenix7", PRG: "keys/developer_key.der"}, false); !errors.Is(err, tool.ErrInvalidArgs) {
		t.Fatalf("key accepted as prg: %v", err)
	}
}

// SE-2: without a usable java the simulator tools fail with the install
// hint instead of falling back to the SDK's wrapper scripts.
func TestFindJavaErrorIsReturned(t *testing.T) {
	t.Setenv("JAVA_HOME", "relative/jdk")
	_, err := findJava(context.Background())
	if !errors.Is(err, tool.ErrUnavailable) || !strings.Contains(err.Error(), "JAVA_HOME must be an absolute path") || !strings.Contains(err.Error(), "JDK 17+") {
		t.Fatalf("err = %v", err)
	}
}

func TestSimKeyDefaultsToEnvKey(t *testing.T) {
	k := tooltest.New(t)
	outside := filepath.Join(t.TempDir(), "developer_key.der")
	_ = os.WriteFile(outside, []byte("KEY"), 0o600)
	t.Setenv(ciqsdk.EnvKeyVar, outside)
	if got, err := simKey(k.Env, ""); err != nil || got != filepath.Clean(outside) {
		t.Fatalf("got %q, %v", got, err)
	}
	t.Setenv(ciqsdk.EnvKeyVar, "")
	if _, err := simKey(k.Env, ""); !errors.Is(err, tool.ErrInvalidArgs) || !strings.Contains(err.Error(), ciqsdk.EnvKeyVar) {
		t.Fatalf("missing key: %v", err)
	}
}

func TestRunFilterCannotBecomeFlags(t *testing.T) {
	for _, f := range []string{"-a foo", "-d", "a b", "x;rm", "a\nb", string(make([]byte, 201))} {
		_, err := simRun(context.Background(), nil, runIn{Test: true, Filter: f})
		if !errors.Is(err, tool.ErrInvalidArgs) {
			t.Errorf("filter %q: %v", f, err)
		}
	}
	if _, err := simRun(context.Background(), nil, runIn{Filter: "MyTests.testFoo"}); !errors.Is(err, tool.ErrInvalidArgs) {
		t.Errorf("filter without test: %v", err)
	}
	if !testFilterRe.MatchString("MyTests.testFoo*") || !testFilterRe.MatchString("a:b-c_d") {
		t.Fatal("valid test names refused")
	}
}

func TestBoundCrash(t *testing.T) {
	e := sim.CrashEntry{Details: strings.Repeat("x", 10*maxCrashText), Stack: make([]sim.CrashFrame, 100)}
	b := boundCrash(e)
	if len(b.Details) > maxCrashText+len("…") || len(b.Stack) != maxCrashFrames {
		t.Fatalf("details %d, frames %d", len(b.Details), len(b.Stack))
	}
}

func TestScreenshotNeverClobbers(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("screenshots are macOS-only")
	}
	k := tooltest.New(t)
	key := filepath.Join(k.Files.Root, "keys", "developer_key.der")
	png := filepath.Join(k.Files.Root, "shot.png")
	_ = os.MkdirAll(filepath.Dir(key), 0o700)
	_ = os.WriteFile(key, []byte("KEY"), 0o600)
	_ = os.WriteFile(png, []byte("OLD"), 0o600)
	for _, in := range []shotIn{
		{Device: "fenix7", Output: "keys/developer_key.der", Overwrite: true},
		{Device: "fenix7", Output: "shot.png"},
	} {
		if _, err := simScreenshot(context.Background(), k.Env, in); !errors.Is(err, tool.ErrInvalidArgs) {
			t.Errorf("%+v: %v", in, err)
		}
	}
	if b, _ := os.ReadFile(key); string(b) != "KEY" {
		t.Fatalf("key clobbered: %q", b)
	}
	if b, _ := os.ReadFile(png); string(b) != "OLD" {
		t.Fatalf("png clobbered: %q", b)
	}
}

func TestFocusLostMapsToErrorCode(t *testing.T) {
	var te *tool.Error
	if err := simErr(sim.ErrFocusLost); !errors.As(err, &te) || te.Code != "focus_lost" {
		t.Fatalf("got %v", err)
	}
}
