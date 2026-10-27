package sim

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/idoo/garmin-mcp/internal/ciqsdk"
)

func refusedDial(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("connection refused")
}

func openDial(context.Context, string, string) (net.Conn, error) {
	a, b := net.Pipe()
	_ = b.Close()
	return a, nil
}

// fakeLauncher: no real simulator, port or process table.
func fakeLauncher(st Status, dial DialFunc, launches *atomic.Int32, exited <-chan error) launcher {
	return launcher{
		status: func(context.Context) (Status, error) { return st, nil },
		launch: func(context.Context, ciqsdk.SDK) (launched, error) {
			launches.Add(1)
			return launched{exited: exited, path: "/sdk/bin/simulator"}, nil
		},
		alive:   func(context.Context) bool { return st.Running },
		verify:  func(context.Context, int) error { return nil },
		pid:     func(context.Context) (int, error) { return st.PID, nil },
		kill:    func(context.Context, int) {},
		owned:   &owner{},
		dial:    dial,
		timeout: 5 * time.Second,
	}
}

// LF-9: a simulator that is running but not listening yet is waited for,
// never launched a second time.
func TestStartNeverSpawnsSecondSimulator(t *testing.T) {
	st := Status{Running: true, PID: 42}
	var launches atomic.Int32
	l := fakeLauncher(st, openDial, &launches, nil)
	if _, already, err := l.start(t.Context(), &fakeClock{t: time.Unix(0, 0)}, ciqsdk.SDK{}); err != nil || already {
		t.Fatalf("start: already=%v err=%v", already, err)
	}
	l = fakeLauncher(st, refusedDial, &launches, nil)
	_, _, err := l.start(t.Context(), &fakeClock{t: time.Unix(0, 0)}, ciqsdk.SDK{})
	if err == nil || !strings.Contains(err.Error(), "running but not listening") || !strings.Contains(err.Error(), "sim_stop") {
		t.Fatalf("hung simulator: %v", err)
	}
	if n := launches.Load(); n != 0 {
		t.Fatalf("launched %d simulators next to a running one", n)
	}
}

// LD-7: a launched simulator that exits ends the wait at once with its
// output, not after the 60 s port timeout.
func TestStartReturnsEarlyWhenSimulatorExits(t *testing.T) {
	exited := make(chan error, 1)
	exited <- errors.New("simulator exited before opening its port (exit status 1): cannot open display")
	var launches atomic.Int32
	l := fakeLauncher(Status{}, refusedDial, &launches, exited)
	clk := &fakeClock{t: time.Unix(0, 0)}
	_, _, err := l.start(t.Context(), clk, ciqsdk.SDK{})
	if err == nil || !strings.Contains(err.Error(), "cannot open display") {
		t.Fatalf("err = %v", err)
	}
	if el := clk.t.Sub(time.Unix(0, 0)); el > time.Second {
		t.Fatalf("waited %s after the simulator exited", el)
	}
	if launches.Load() != 1 {
		t.Fatalf("launches = %d", launches.Load())
	}
}

// The real spawn path reports the child's exit status and output tail.
func TestSpawnReportsExitWithOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script")
	}
	exe := filepath.Join(t.TempDir(), "simulator")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho starting\necho 'error while loading shared libraries: libwebkit2gtk-4.0.so' >&2\nexit 3\n"), 0o700); err != nil { //nolint:gosec // G306: test script must be executable
		t.Fatal(err)
	}
	la, err := spawn(exe, filepath.Dir(exe))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	err = WaitReady(ctx, &fakeClock{t: time.Unix(0, 0)}, refusedDial, Addr, func() bool { return false }, la.exited, 100*time.Millisecond, time.Hour)
	if !errors.Is(err, ErrSimulatorExited) || !strings.Contains(err.Error(), "exit status 3") || !strings.Contains(err.Error(), "libwebkit2gtk") {
		t.Fatalf("err = %v", err)
	}
}

func TestTailBufferKeepsLastBytes(t *testing.T) {
	b := &tailBuffer{max: 4}
	_, _ = b.Write([]byte("abc"))
	_, _ = b.Write([]byte("defg"))
	if got := b.String(); got != "defg" {
		t.Fatalf("tail = %q", got)
	}
}

// LF-8: the ring evicts by bytes as well as lines, keeping the newest line.
func TestRingByteBudget(t *testing.T) {
	r := NewRing(100)
	r.maxBytes = 9
	for _, s := range []string{"aaaa", "bbbb", "cccc", "dd"} {
		r.Append("stdout", s)
	}
	l, _, latest, dropped := r.Since(0, 0)
	if len(l) != 2 || l[0].Text != "cccc" || l[1].Text != "dd" || latest != 4 || dropped != 2 || r.bytes != 6 {
		t.Fatalf("lines %+v latest %d dropped %d bytes %d", l, latest, dropped, r.bytes)
	}
	r.Append("stdout", strings.Repeat("x", 50))
	if l, _, _, _ := r.Since(0, 0); len(l) != 1 || len(l[0].Text) != 50 {
		t.Fatalf("oversized newest line not kept alone: %+v", l)
	}
}

// LF-8: no more than MaxRunningSessions clients run at once.
func TestManagerCapsRunningSessions(t *testing.T) {
	m := &Manager{sessions: map[string]*Session{}}
	for i := range MaxRunningSessions {
		m.sessions[string(rune('a'+i))] = &Session{done: make(chan struct{})}
	}
	m.sessions["old"] = exitedSession("old", time.Now())
	if _, err := m.Start([]string{"/nonexistent/monkeydo"}, "", "", false); !errors.Is(err, ErrTooManySessions) {
		t.Fatalf("err = %v", err)
	}
	close(m.sessions["a"].done)
	if err := m.CheckRoom(); err != nil {
		t.Fatal(err)
	}
}

// RS-5: parallel starts never run more than MaxRunningSessions clients, and
// a failed launch gives its reserved slot back.
func TestManagerCapHoldsUnderParallelStarts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	m := &Manager{sessions: map[string]*Session{}}
	for range 2 * MaxRunningSessions {
		if _, err := m.Start([]string{"/nonexistent/monkeydo"}, "", "", false); err == nil || errors.Is(err, ErrTooManySessions) {
			t.Fatalf("failed launch: err = %v", err)
		}
	}
	if m.pending != 0 {
		t.Fatalf("failed launches leaked %d slots", m.pending)
	}
	t.Cleanup(func() { m.KillAll() })
	const n = 16
	var ok, busy atomic.Int32
	start := make(chan struct{})
	done := make(chan struct{})
	for range n {
		go func() {
			defer func() { done <- struct{}{} }()
			<-start
			_, err := m.Start([]string{"/bin/sh", "-c", "sleep 30"}, "", "", false)
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, ErrTooManySessions):
				busy.Add(1)
			default:
				t.Error(err)
			}
		}()
	}
	close(start)
	for range n {
		<-done
	}
	if ok.Load() != MaxRunningSessions || busy.Load() != n-MaxRunningSessions {
		t.Fatalf("started %d, refused %d; want %d and %d", ok.Load(), busy.Load(), MaxRunningSessions, n-MaxRunningSessions)
	}
	if m.pending != 0 {
		t.Fatalf("pending = %d after all starts", m.pending)
	}
}
