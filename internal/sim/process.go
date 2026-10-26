package sim

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/idoo/ciq-mcp/internal/ciqsdk"
	"github.com/idoo/ciq-mcp/internal/state"
)

// Addr is the simulator's loopback command port.
const Addr = "127.0.0.1:1234"

// Clock is the time source of wait loops (tool.Clock satisfies it).
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration) error
}

// DialFunc opens a TCP connection (net.Dialer.DialContext in production).
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

func portOpen(ctx context.Context, dial DialFunc, addr string) bool {
	c, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	conn, err := dial(c, "tcp", addr)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// WaitReady polls addr until connects keep succeeding for stable, and alive
// (nil = always) reports the process is running. It returns an error after
// timeout or when the process dies.
func WaitReady(ctx context.Context, clk Clock, dial DialFunc, addr string, alive func() bool, stable, timeout time.Duration) error {
	deadline := clk.Now().Add(timeout)
	var upSince time.Time
	for {
		now := clk.Now()
		if portOpen(ctx, dial, addr) && (alive == nil || alive()) {
			if upSince.IsZero() {
				upSince = now
			}
			if now.Sub(upSince) >= stable {
				return nil
			}
		} else {
			upSince = time.Time{}
		}
		if now.After(deadline) {
			return fmt.Errorf("simulator port %s not ready after %s", addr, timeout)
		}
		if err := clk.Sleep(ctx, 25*time.Millisecond); err != nil {
			return err
		}
	}
}

// WaitClosed polls addr until connects fail.
func WaitClosed(ctx context.Context, clk Clock, dial DialFunc, addr string, timeout time.Duration) error {
	deadline := clk.Now().Add(timeout)
	for portOpen(ctx, dial, addr) {
		if clk.Now().After(deadline) {
			return fmt.Errorf("simulator port %s still open after %s", addr, timeout)
		}
		if err := clk.Sleep(ctx, 50*time.Millisecond); err != nil {
			return err
		}
	}
	return nil
}

// Dial is the production DialFunc.
var Dial DialFunc = (&net.Dialer{}).DialContext

// Lease takes the OS-wide simulator lease (flock under the state dir); hold
// it for every simulator-mutating call.
func Lease(ctx context.Context) (release func(), err error) {
	dir, err := state.DefaultDir()
	if err != nil {
		return nil, err
	}
	return lockPath(ctx, filepath.Join(dir, "sim.lock"))
}

// Status describes the simulator process.
type Status struct {
	Running    bool   `json:"running"`
	PID        int    `json:"pid,omitempty"`
	Executable string `json:"executable,omitempty"`
	SDKPath    string `json:"sdk_path,omitempty"`
	PortOpen   bool   `json:"port_open"`
	Ready      bool   `json:"ready"`
}

// processName is the simulator image name per OS.
func processName() string {
	if runtime.GOOS == "windows" {
		return "simulator.exe"
	}
	return "simulator"
}

// FindPID returns the simulator pid, or 0 when not running.
func FindPID(ctx context.Context) (int, error) {
	if runtime.GOOS == "windows" {
		out, err := exec.CommandContext(ctx, "tasklist", "/FI", "IMAGENAME eq simulator.exe", "/FO", "CSV", "/NH").Output()
		if err != nil {
			return 0, err
		}
		f := strings.Split(strings.TrimSpace(string(out)), ",")
		if len(f) < 2 || !strings.Contains(strings.ToLower(f[0]), "simulator") {
			return 0, nil
		}
		return strconv.Atoi(strings.Trim(f[1], `"`))
	}
	out, err := exec.CommandContext(ctx, "pgrep", "-x", processName()).Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	first := strings.Fields(string(out))
	if len(first) == 0 {
		return 0, nil
	}
	return strconv.Atoi(first[0])
}

// executable returns the binary path of pid (best effort).
func executable(ctx context.Context, pid int) string {
	switch runtime.GOOS {
	case "linux":
		p, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		return p
	case "windows":
		return ""
	}
	out, _ := exec.CommandContext(ctx, "ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	return strings.TrimSpace(string(out))
}

// SDKOf derives the SDK root from a simulator executable path
// (<sdk>/bin/ConnectIQ.app/Contents/MacOS/simulator or <sdk>/bin/simulator).
func SDKOf(exe string) string {
	exe = filepath.ToSlash(exe)
	if i := strings.Index(exe, "/bin/"); i > 0 {
		return filepath.FromSlash(exe[:i])
	}
	return ""
}

// GetStatus reports the simulator process and port state.
func GetStatus(ctx context.Context) (Status, error) {
	pid, err := FindPID(ctx)
	if err != nil {
		return Status{}, err
	}
	st := Status{PID: pid, Running: pid > 0, PortOpen: portOpen(ctx, Dial, Addr)}
	if pid > 0 {
		st.Executable = executable(ctx, pid)
		st.SDKPath = SDKOf(st.Executable)
	}
	st.Ready = st.Running && st.PortOpen
	return st, nil
}

// ErrSDKMismatch means the running simulator belongs to another SDK.
var ErrSDKMismatch = errors.New("the running simulator belongs to a different SDK")

// SameSDK compares SDK roots loosely (trailing slash, symlinks, case on mac).
func SameSDK(a, b string) bool {
	clean := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			p = r
		}
		p = filepath.Clean(p)
		if runtime.GOOS != "linux" {
			p = strings.ToLower(p)
		}
		return p
	}
	return clean(a) == clean(b)
}

// Start launches the simulator of sdk (no-op if one is ready) and waits for
// the port. A simulator of another SDK is an [ErrSDKMismatch].
func Start(ctx context.Context, clk Clock, sdk ciqsdk.SDK) (st Status, already bool, err error) {
	st, err = GetStatus(ctx)
	if err != nil {
		return st, false, err
	}
	if st.Running && st.SDKPath != "" && !SameSDK(st.SDKPath, sdk.Path) {
		return st, false, fmt.Errorf("%w: running %s, requested %s (sim_stop first)", ErrSDKMismatch, st.SDKPath, sdk.Path)
	}
	if st.Ready {
		return st, true, nil
	}
	bin := sdk.Bin()
	switch runtime.GOOS {
	case "darwin":
		app := filepath.Join(bin, "ConnectIQ.app")
		if out, err := exec.CommandContext(ctx, "open", "-a", app).CombinedOutput(); err != nil {
			return st, false, fmt.Errorf("open -a %s: %v: %s", app, err, strings.TrimSpace(string(out)))
		}
	default:
		exe := filepath.Join(bin, processName())
		cmd := exec.Command(exe)
		cmd.Dir = bin
		ownGroup(cmd)
		if err := cmd.Start(); err != nil {
			return st, false, fmt.Errorf("start %s: %w", exe, err)
		}
		go func() { _ = cmd.Wait() }()
	}
	alive := func() bool { pid, _ := FindPID(ctx); return pid > 0 }
	if err := WaitReady(ctx, clk, Dial, Addr, alive, 100*time.Millisecond, 60*time.Second); err != nil {
		return st, false, err
	}
	st, err = GetStatus(ctx)
	return st, false, err
}

// Stop kills monkeydo clients, then the simulator, and waits for the port
// to close.
func Stop(ctx context.Context, clk Clock) (killedClients int, wasRunning bool, err error) {
	killedClients = Sessions.KillAll()
	pid, err := FindPID(ctx)
	if err != nil {
		return killedClients, false, err
	}
	if pid > 0 {
		wasRunning = true
		if runtime.GOOS == "windows" {
			_ = exec.CommandContext(ctx, "taskkill", "/F", "/IM", "simulator.exe").Run()
		} else {
			_ = exec.CommandContext(ctx, "pkill", "-x", "simulator").Run()
		}
	}
	return killedClients, wasRunning, WaitClosed(ctx, clk, Dial, Addr, 20*time.Second)
}
