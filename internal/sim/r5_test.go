package sim

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/idoo/connect-iq-mcp/internal/ciqsdk"
)

// J6: a launched simulator that never opens its port fails with its last
// output and a next step, not a bare "port not ready".
func TestStartNeverListeningNamesOutputAndNextStep(t *testing.T) {
	var launches atomic.Int32
	l := fakeLauncher(Status{}, refusedDial, &launches, nil)
	l.launch = func(context.Context, ciqsdk.SDK) (launched, error) {
		return launched{path: "/sdk/bin/simulator", output: func() string { return "starting\nboom: renderer gave up\n" }}, nil
	}
	_, _, err := l.start(t.Context(), &fakeClock{t: time.Unix(0, 0)}, ciqsdk.SDK{})
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("err = %v", err)
	}
	for _, want := range []string{"never opened", "boom: renderer gave up", "open the simulator once by hand", "/sdk/bin/simulator"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err lacks %q: %v", want, err)
		}
	}
}

// J6: the next step follows what the output names.
func TestLaunchHintByOutput(t *testing.T) {
	for out, want := range map[string]string{
		"Gtk-WARNING: cannot open display": "desktop session",
		"Error: no JVM found":              "JAVA_HOME",
		"device data incompatible":         "another installed SDK",
		"":                                 "open the simulator once by hand: /x/simulator",
	} {
		if got := launchHint("linux", out, "/x/simulator"); !strings.Contains(got, want) {
			t.Errorf("launchHint(%q) = %q, want %q", out, got, want)
		}
	}
}

// J9: sim_stop never stops a simulator this server did not start, unless
// forced; the one it launched is stopped without force.
func TestStopRefusesForeignSimulatorUnlessForced(t *testing.T) {
	var launches atomic.Int32
	l := fakeLauncher(Status{Running: true, PID: 4242}, refusedDial, &launches, nil)
	var killed []int
	l.kill = func(_ context.Context, pid int) { killed = append(killed, pid) }
	clk := &fakeClock{t: time.Unix(0, 0)}

	_, was, err := l.stop(t.Context(), clk, false)
	if !errors.Is(err, ErrForeignSimulator) || was || len(killed) != 0 {
		t.Fatalf("foreign stop: was=%v killed=%v err=%v", was, killed, err)
	}
	if !strings.Contains(err.Error(), "pid 4242") || !strings.Contains(err.Error(), "ask the human") || !strings.Contains(err.Error(), "force:true") {
		t.Fatalf("foreign stop message: %v", err)
	}

	if _, was, err = l.stop(t.Context(), clk, true); err != nil || !was || len(killed) != 1 || killed[0] != 4242 {
		t.Fatalf("forced stop: was=%v killed=%v err=%v", was, killed, err)
	}

	killed = nil
	l.owned.set(4242)
	if _, was, err = l.stop(t.Context(), clk, false); err != nil || !was || len(killed) != 1 {
		t.Fatalf("own stop: was=%v killed=%v err=%v", was, killed, err)
	}
}

// J9: a launched simulator becomes ours; the SDK-mismatch error for a
// foreign one asks the human instead of inviting a bare sim_stop.
func TestStartOwnershipAndMismatchAdvice(t *testing.T) {
	var launches atomic.Int32
	l := fakeLauncher(Status{}, openDial, &launches, nil)
	l.status = func(context.Context) (Status, error) {
		if launches.Load() == 0 {
			return Status{}, nil
		}
		return Status{Running: true, PID: 77, PortOpen: true, Ready: true}, nil
	}
	l.alive = func(context.Context) bool { return launches.Load() > 0 }
	if _, _, err := l.start(t.Context(), &fakeClock{t: time.Unix(0, 0)}, ciqsdk.SDK{}); err != nil {
		t.Fatal(err)
	}
	if !l.owned.is(77) {
		t.Fatal("launched simulator not recorded as ours")
	}

	f := fakeLauncher(Status{Running: true, PID: 9, SDKPath: "/sdks/a"}, openDial, &launches, nil)
	_, _, err := f.start(t.Context(), &fakeClock{t: time.Unix(0, 0)}, ciqsdk.SDK{Path: "/sdks/b"})
	if !errors.Is(err, ErrSDKMismatch) || !strings.Contains(err.Error(), "ask the human") || !strings.Contains(err.Error(), "force:true") {
		t.Fatalf("mismatch: %v", err)
	}
}
