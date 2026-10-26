package sim

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/idoo/garmin-mcp/internal/ciqsdk"
	"github.com/idoo/garmin-mcp/internal/state"
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

// FindPID returns the pid of a Connect IQ SDK simulator run by this user
// (see [IsSDKSimulator]), or 0 when none is running. Processes merely named
// simulator, or another user's, are ignored.
func FindPID(ctx context.Context) (int, error) {
	pids, err := candidatePIDs(ctx)
	if err != nil {
		return 0, err
	}
	for _, pid := range pids {
		if IsSDKSimulator(executable(ctx, pid)) {
			return pid, nil
		}
	}
	return 0, nil
}

// candidatePIDs lists this user's processes named like the simulator.
func candidatePIDs(ctx context.Context) ([]int, error) {
	if runtime.GOOS == "windows" {
		args := []string{"/FI", "IMAGENAME eq simulator.exe", "/FO", "CSV", "/NH"}
		if u, err := user.Current(); err == nil {
			args = append(args, "/FI", "USERNAME eq "+u.Username)
		}
		out, err := exec.CommandContext(ctx, "tasklist", args...).Output() //nolint:gosec // G204: fixed program; filters are a constant image name and the current user's name
		if err != nil {
			return nil, err
		}
		var pids []int
		for _, line := range strings.Split(string(out), "\n") {
			f := strings.Split(strings.TrimSpace(line), ",")
			if len(f) < 2 || !strings.EqualFold(strings.Trim(f[0], `"`), "simulator.exe") {
				continue
			}
			if pid, err := strconv.Atoi(strings.Trim(f[1], `"`)); err == nil {
				pids = append(pids, pid)
			}
		}
		return pids, nil
	}
	out, err := exec.CommandContext(ctx, "pgrep", "-U", strconv.Itoa(os.Getuid()), "-x", processName()).Output() //nolint:gosec // G204: fixed program; integer uid and a per-OS constant name
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, f := range strings.Fields(string(out)) {
		if pid, err := strconv.Atoi(f); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

// ErrPortNotOurs means 127.0.0.1:1234 is held by a process other than the
// verified simulator; nothing is pushed to it.
var ErrPortNotOurs = errors.New("port 1234 is held by a process other than the Connect IQ simulator")

// verifyListener checks that every listener on port 1234 is pid.
func verifyListener(ctx context.Context, pid int) error {
	pids, err := listenerPIDs(ctx)
	if err != nil {
		return fmt.Errorf("%w: cannot identify the listener: %v", ErrPortNotOurs, err)
	}
	if pid <= 0 || len(pids) == 0 {
		return fmt.Errorf("%w (listener pid unknown, simulator pid %d)", ErrPortNotOurs, pid)
	}
	for _, p := range pids {
		if p != pid {
			return fmt.Errorf("%w (listener pid %d, simulator pid %d)", ErrPortNotOurs, p, pid)
		}
	}
	return nil
}

// listenerPIDs returns the pids listening on TCP port 1234.
func listenerPIDs(ctx context.Context) ([]int, error) {
	if runtime.GOOS == "windows" {
		out, err := exec.CommandContext(ctx, "netstat", "-ano", "-p", "TCP").Output()
		if err != nil {
			return nil, err
		}
		return parseNetstatPIDs(string(out), "1234"), nil
	}
	out, err := exec.CommandContext(ctx, "lsof", "-nP", "-iTCP:1234", "-sTCP:LISTEN", "-Fp").Output()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return parseLsofPIDs(string(out)), nil
	case errors.As(err, &ee) && ee.ExitCode() == 1:
		return nil, nil
	case runtime.GOOS == "linux" && errors.Is(err, exec.ErrNotFound):
		out, err := exec.CommandContext(ctx, "ss", "-Hltnp", "sport = :1234").Output()
		if err != nil {
			return nil, err
		}
		return parseSSPIDs(string(out)), nil
	}
	return nil, err
}

// parseLsofPIDs reads `lsof -Fp` output (lines "p<pid>").
func parseLsofPIDs(out string) []int {
	var pids []int
	for _, line := range strings.Split(out, "\n") {
		l := strings.TrimSpace(line)
		if !strings.HasPrefix(l, "p") {
			continue
		}
		if pid, err := strconv.Atoi(l[1:]); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

var ssPIDRe = regexp.MustCompile(`pid=(\d+)`)

// parseSSPIDs reads `ss -Hltnp` output. A listener whose process is not
// shown (another user's) yields pid 0, which never matches.
func parseSSPIDs(out string) []int {
	var pids []int
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		m := ssPIDRe.FindAllStringSubmatch(line, -1)
		if len(m) == 0 {
			pids = append(pids, 0)
		}
		for _, g := range m {
			pid, _ := strconv.Atoi(g[1])
			pids = append(pids, pid)
		}
	}
	return pids
}

// parseNetstatPIDs reads `netstat -ano` output for LISTENING sockets on port.
func parseNetstatPIDs(out, port string) []int {
	var pids []int
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 5 || !strings.EqualFold(f[0], "TCP") || f[3] != "LISTENING" || !strings.HasSuffix(f[1], ":"+port) {
			continue
		}
		if pid, err := strconv.Atoi(f[4]); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// executable returns the binary path of pid (best effort).
func executable(ctx context.Context, pid int) string {
	switch runtime.GOOS {
	case "linux":
		p, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		return p
	case "windows":
		out, _ := exec.CommandContext(ctx, "powershell", "-NoProfile", "-Command", fmt.Sprintf("(Get-Process -Id %d).Path", pid)).Output() //nolint:gosec // G204: fixed program; only an integer pid is formatted in
		return strings.TrimSpace(string(out))
	}
	out, _ := exec.CommandContext(ctx, "ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output() //nolint:gosec // G204: fixed program; integer pid argument
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
		if err := verifyListener(ctx, st.PID); err != nil {
			return st, false, err
		}
		return st, true, nil
	}
	bin := sdk.Bin()
	switch runtime.GOOS {
	case "darwin":
		app := filepath.Join(bin, "ConnectIQ.app")
		if out, err := exec.CommandContext(ctx, "open", "-a", app).CombinedOutput(); err != nil { //nolint:gosec // G204: fixed program; app is the resolved SDK's bundled ConnectIQ.app
			return st, false, fmt.Errorf("open -a %s: %v: %s", app, err, strings.TrimSpace(string(out)))
		}
	default:
		exe := filepath.Join(bin, processName())
		cmd := exec.Command(exe) //nolint:gosec // G204: exe is the simulator binary inside the resolved SDK's bin dir
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
	if err != nil {
		return st, false, err
	}
	return st, false, verifyListener(ctx, st.PID)
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
		killVerified(ctx, pid)
	}
	return killedClients, wasRunning, WaitClosed(ctx, clk, Dial, Addr, 20*time.Second)
}

// killVerified terminates pid only when its executable is a Connect IQ SDK
// simulator (never an unrelated process merely named "simulator").
func killVerified(ctx context.Context, pid int) {
	if !IsSDKSimulator(executable(ctx, pid)) {
		return
	}
	if runtime.GOOS == "windows" {
		_ = exec.CommandContext(ctx, "taskkill", "/F", "/PID", strconv.Itoa(pid)).Run() //nolint:gosec // G204: fixed program; integer pid of a verified SDK simulator
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Signal(syscall.SIGTERM)
	}
}

// IsSDKSimulator reports whether exe is the simulator binary inside a
// Connect IQ SDK: named simulator(.exe), under <sdk>/bin/, with the SDK's
// monkeydo next to it.
func IsSDKSimulator(exe string) bool {
	if exe == "" || !strings.EqualFold(filepath.Base(exe), processName()) {
		return false
	}
	sdk := SDKOf(exe)
	if sdk == "" {
		return false
	}
	for _, m := range []string{"monkeydo", "monkeydo.bat"} {
		if fi, err := os.Stat(filepath.Join(sdk, "bin", m)); err == nil && fi.Mode().IsRegular() {
			return true
		}
	}
	return false
}
