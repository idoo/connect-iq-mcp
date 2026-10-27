package sim

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/idoo/connect-iq-mcp/internal/ciqsdk"
	"github.com/idoo/connect-iq-mcp/internal/flock"
	"github.com/idoo/connect-iq-mcp/internal/state"
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

// ErrNotReady means the simulator port did not become ready in time.
var ErrNotReady = errors.New("simulator port not ready")

// WaitReady polls addr until connects keep succeeding for stable, and alive
// (nil = always) reports the process is running. It returns an error after
// timeout, and as soon as exited (nil = never) delivers the launched child's
// exit, unless alive still finds a simulator (one that re-execs itself).
func WaitReady(ctx context.Context, clk Clock, dial DialFunc, addr string, alive func() bool, exited <-chan error, stable, timeout time.Duration) error {
	deadline := clk.Now().Add(timeout)
	var upSince time.Time
	for {
		select {
		case err := <-exited:
			if alive == nil || !alive() {
				return err
			}
			exited = nil
		default:
		}
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
			return fmt.Errorf("%w: %s after %s", ErrNotReady, addr, timeout)
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
func Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

// Lease takes the OS-wide simulator lease (flock under the state dir); hold
// it for every simulator-mutating call.
func Lease(ctx context.Context) (release func(), err error) {
	dir, err := state.DefaultDir()
	if err != nil {
		return nil, err
	}
	return flock.Lock(ctx, filepath.Join(dir, "sim.lock"))
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
		return osListenerPIDs(1234)
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

// executable returns the binary path of pid (best effort).
func executable(ctx context.Context, pid int) string {
	switch runtime.GOOS {
	case "linux":
		p, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
		return p
	case "windows":
		return osExecutable(pid)
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

// ErrNoDisplay means a Linux simulator could not open a window.
var ErrNoDisplay = errors.New("no graphical display: the Connect IQ simulator is a GUI app and neither DISPLAY nor WAYLAND_DISPLAY is set")

// ErrSimulatorExited means the launched simulator exited before it listened.
var ErrSimulatorExited = errors.New("simulator exited before opening its port")

// ErrForeignSimulator means a simulator this server did not start is
// running: another agent's or the user's. sim_stop leaves it alone unless forced.
var ErrForeignSimulator = errors.New("a Connect IQ simulator not started by this server is running")

// foreignErr is the [ErrForeignSimulator] for pid with the one next step.
func foreignErr(pid int) error {
	return fmt.Errorf("%w (pid %d); ask the human before stopping it, then call sim_stop with force:true", ErrForeignSimulator, pid)
}

// owner remembers the pid of the simulator this server process launched.
type owner struct {
	mu  sync.Mutex
	pid int
}

func (o *owner) set(pid int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.pid = pid
}

func (o *owner) is(pid int) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return pid > 0 && o.pid == pid
}

// launched is a simulator being started: exited delivers the child's exit
// (nil = never, as for macOS `open -a`), output returns its newest output,
// and path is what a human opens by hand.
type launched struct {
	exited <-chan error
	output func() string
	path   string
}

// launcher holds Start's and Stop's process and network hooks; tests replace them.
type launcher struct {
	status   func(context.Context) (Status, error)
	launch   func(context.Context, ciqsdk.SDK) (launched, error)
	alive    func(context.Context) bool
	verify   func(context.Context, int) error
	pid      func(context.Context) (int, error)
	kill     func(context.Context, int)
	owned    *owner
	sessions *Manager // clients killed by stop
	dial     DialFunc // nil = Dial
	timeout  time.Duration
}

// prodLauncher is the production launcher for the server whose state is m.
func prodLauncher(m *Manager) launcher {
	return launcher{
		status:   GetStatus,
		launch:   launch,
		alive:    func(ctx context.Context) bool { pid, _ := FindPID(ctx); return pid > 0 },
		verify:   verifyListener,
		pid:      FindPID,
		kill:     killVerified,
		owned:    &m.owned,
		sessions: m,
		timeout:  60 * time.Second,
	}
}

// stopAdvice is the next step for a simulator (pid) in the way: sim_stop
// for this server's own, the human's approval for anyone else's.
func (l launcher) stopAdvice(pid int) string {
	if l.owned.is(pid) {
		return "call sim_stop, then retry"
	}
	return foreignErr(pid).Error()
}

// Start launches the simulator of sdk (no-op if one is ready) and waits for
// the port, recording a launched simulator as m's. A simulator of another
// SDK is an [ErrSDKMismatch]. A simulator that is running but not listening
// yet is waited for, never launched twice.
func Start(ctx context.Context, clk Clock, m *Manager, sdk ciqsdk.SDK) (st Status, already bool, err error) {
	return prodLauncher(m).start(ctx, clk, sdk)
}

func (l launcher) start(ctx context.Context, clk Clock, sdk ciqsdk.SDK) (st Status, already bool, err error) {
	st, err = l.status(ctx)
	if err != nil {
		return st, false, err
	}
	if st.Running && st.SDKPath != "" && !SameSDK(st.SDKPath, sdk.Path) {
		return st, false, fmt.Errorf("%w: running %s, requested %s; %s", ErrSDKMismatch, st.SDKPath, sdk.Path, l.stopAdvice(st.PID))
	}
	if st.Ready {
		if err := l.verify(ctx, st.PID); err != nil {
			return st, false, err
		}
		return st, true, nil
	}
	dial := l.dial
	if dial == nil {
		dial = Dial
	}
	alive := func() bool { return l.alive(ctx) }
	launchedNow := !st.Running
	if st.Running {
		// Still booting, or hung after an earlier timeout: a second binary
		// would fight it for the port.
		if err := WaitReady(ctx, clk, dial, Addr, alive, nil, 100*time.Millisecond, l.timeout); err != nil {
			if errors.Is(err, ErrNotReady) {
				return st, false, fmt.Errorf("simulator (pid %d) is running but not listening on %s after %s; %s", st.PID, Addr, l.timeout, l.stopAdvice(st.PID))
			}
			return st, false, err
		}
	} else {
		la, err := l.launch(ctx, sdk)
		if err != nil {
			return st, false, err
		}
		if err := WaitReady(ctx, clk, dial, Addr, alive, la.exited, 100*time.Millisecond, l.timeout); err != nil {
			// Whatever came up is the one we launched: sim_stop may end it.
			if now, serr := l.status(ctx); serr == nil && now.PID > 0 {
				l.owned.set(now.PID)
			}
			if errors.Is(err, ErrNotReady) {
				return st, false, notStarted(la, l.timeout)
			}
			return st, false, err
		}
	}
	st, err = l.status(ctx)
	if err != nil {
		return st, false, err
	}
	if err := l.verify(ctx, st.PID); err != nil {
		return st, false, err
	}
	// A simulator that was already booting keeps its owner; the one this
	// call launched is ours, so sim_stop may end it without force.
	if launchedNow {
		l.owned.set(st.PID)
	}
	return st, false, nil
}

// notStarted explains a launched simulator that never opened its port:
// its last output lines and the next step for this OS.
func notStarted(la launched, timeout time.Duration) error {
	out := ""
	if la.output != nil {
		out = lastLines(la.output(), 10)
	}
	shown := out
	if shown == "" {
		shown = "none captured"
	}
	return fmt.Errorf("%w: the simulator connect-iq-mcp launched (%s) never opened %s within %s; last output: %s; next: %s",
		ErrNotReady, la.path, Addr, timeout, shown, launchHint(runtime.GOOS, out, la.path))
}

// launchHint picks the next step for a simulator that never listened from
// its output: display, Java or SDK problems, else opening it once by hand
// (a first launch can wait on a permission or first-run dialog).
func launchHint(goos, out, path string) string {
	lo := strings.ToLower(out)
	switch {
	case strings.Contains(lo, "display") || strings.Contains(lo, "gtk") || strings.Contains(lo, "x11"):
		return "the simulator could not open a window: run connect-iq-mcp inside a desktop session (DISPLAY or WAYLAND_DISPLAY set; xvfb-run, or WSLg on WSL), then retry sim_run"
	case strings.Contains(lo, "java") || strings.Contains(lo, "jvm") || strings.Contains(lo, "jdk"):
		return "the output names Java: set JAVA_HOME to a JDK 17+ in the MCP client's connect-iq-mcp env (config install <client> --java-home <dir>), then retry sim_run"
	case strings.Contains(lo, "incompatible") || strings.Contains(lo, "unsupported"):
		return "the output names the SDK: pick another installed SDK with sim_run's sdk argument (sdk_info lists them) or update it with SdkManager, then retry"
	}
	if goos == "darwin" {
		return "open the simulator once by hand: open -a " + path + " (accept any first-run or security prompt), then retry sim_run"
	}
	return "open the simulator once by hand: " + path + ", then retry sim_run"
}

// lastLines returns the last n non-empty lines of s, joined by " | ".
func lastLines(s string, n int) string {
	var lines []string
	for _, ln := range strings.Split(s, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			lines = append(lines, ln)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// launch starts the simulator of sdk. On macOS `open -a` hands the app to
// launchd (nil exit channel) with its output sent to a log in the state dir.
// Elsewhere the child's output tail is kept and the channel delivers an
// [ErrSimulatorExited] naming it once the child exits.
func launch(ctx context.Context, sdk ciqsdk.SDK) (launched, error) {
	bin := sdk.Bin()
	if runtime.GOOS == "darwin" {
		app := filepath.Join(bin, "ConnectIQ.app")
		args := []string{"-a", app}
		la := launched{path: app}
		if log, err := launchLog(); err == nil {
			args = append(args, "--stdout", log, "--stderr", log)
			la.output = func() string { return fileTail(log, 4<<10) }
		}
		if out, err := exec.CommandContext(ctx, "open", args...).CombinedOutput(); err != nil { //nolint:gosec // G204: fixed program; app is the resolved SDK's bundled ConnectIQ.app, log is under the state dir
			return launched{}, fmt.Errorf("open -a %s: %v: %s", app, err, strings.TrimSpace(string(out)))
		}
		return la, nil
	}
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return launched{}, ErrNoDisplay
	}
	return spawn(filepath.Join(bin, processName()), bin)
}

// launchLog returns an emptied sim-launch.log under the state dir for the
// output of a simulator started by `open -a`.
func launchLog() (string, error) {
	dir, err := state.DefaultDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, "sim-launch.log")
	f, err := os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) //nolint:gosec // G304: fixed name under the state dir
	if err != nil {
		return "", err
	}
	return p, f.Close()
}

// fileTail returns the last limit bytes of the file at p ("" if unreadable).
func fileTail(p string, limit int64) string {
	f, err := os.Open(p) //nolint:gosec // G304: the launch log under the state dir
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	if st, err := f.Stat(); err == nil && st.Size() > limit {
		if _, err := f.Seek(st.Size()-limit, io.SeekStart); err != nil {
			return ""
		}
	}
	b, _ := io.ReadAll(io.LimitReader(f, limit))
	return string(b)
}

// spawn starts exe in dir, detached from the request, and reports its exit
// with the last 4 KiB of its stdout and stderr.
func spawn(exe, dir string) (launched, error) {
	cmd := exec.Command(exe) //nolint:gosec // G204: exe is the simulator binary inside the resolved SDK's bin dir
	cmd.Dir = dir
	ownGroup(cmd)
	out := &tailBuffer{max: 4 << 10}
	cmd.Stdout, cmd.Stderr = out, out
	// A grandchild holding the output pipe must not hide the exit.
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return launched{}, fmt.Errorf("start %s: %w", exe, err)
	}
	exited := make(chan error, 1)
	go func() {
		status := "exit status 0"
		if err := cmd.Wait(); err != nil {
			status = err.Error()
		}
		tail := strings.TrimSpace(out.String())
		if tail == "" {
			tail = "no output"
		}
		exited <- fmt.Errorf("%w (%s): %s", ErrSimulatorExited, status, tail)
	}()
	return launched{exited: exited, output: out.String, path: exe}, nil
}

// tailBuffer keeps the last max bytes written to it.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.b = append(t.b, p...)
	if over := len(t.b) - t.max; over > 0 {
		t.b = append(t.b[:0], t.b[over:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.b)
}

// Stop kills m's monkeydo clients, then the simulator, and waits for the
// port to close. A simulator m did not launch is left running with an
// [ErrForeignSimulator], unless force.
func Stop(ctx context.Context, clk Clock, m *Manager, force bool) (killedClients int, wasRunning bool, err error) {
	return prodLauncher(m).stop(ctx, clk, force)
}

func (l launcher) stop(ctx context.Context, clk Clock, force bool) (killedClients int, wasRunning bool, err error) {
	killedClients = l.sessions.KillAll()
	pid, err := l.pid(ctx)
	if err != nil {
		return killedClients, false, err
	}
	if pid > 0 && !force && !l.owned.is(pid) {
		return killedClients, false, foreignErr(pid)
	}
	if pid > 0 {
		wasRunning = true
		l.kill(ctx, pid)
		l.owned.set(0)
	}
	dial := l.dial
	if dial == nil {
		dial = Dial
	}
	return killedClients, wasRunning, WaitClosed(ctx, clk, dial, Addr, 20*time.Second)
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
