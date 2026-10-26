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
	// GUI tools are documented everywhere but only offered on macOS.
	for _, n := range []string{"sim_screenshot", "sim_input"} {
		tl, _ := tool.Lookup(n)
		if got := tl.ListedIn(tool.ProfileInfo{AllowWrites: true}); got != (runtime.GOOS == "darwin") {
			t.Errorf("%s listed=%v on %s", n, got, runtime.GOOS)
		}
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

func TestWaitCapWithoutProgress(t *testing.T) {
	if got := waitCap(nil, 5*time.Minute); got != tool.NoProgressWaitCap {
		t.Fatalf("got %s", got)
	}
	if got := waitCap(nil, time.Second); got != time.Second {
		t.Fatalf("got %s", got)
	}
}

func TestLocalSimToolsAvailableReadOnly(t *testing.T) {
	names := []string{"sim_run", "sim_stop"}
	if runtime.GOOS == "darwin" {
		names = append(names, "sim_input")
	}
	for _, n := range names {
		tl, _ := tool.Lookup(n)
		if !tl.ListedIn(tool.ProfileInfo{}) {
			t.Errorf("%s hidden in read-only mode", n)
		}
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
