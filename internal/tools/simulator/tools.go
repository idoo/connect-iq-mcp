package simulator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/idoo/garmin-mcp/internal/ciqsdk"
	"github.com/idoo/garmin-mcp/internal/sim"
	"github.com/idoo/garmin-mcp/internal/tool"
)

func init() {
	read := func(name, title, desc string) tool.Meta {
		return tool.Meta{Name: name, Toolset: Toolset, Title: title, Description: desc, Class: tool.Read, Idempotent: true, Local: true}
	}
	write := func(name, title, desc string) tool.Meta {
		// Local process control only: usable in the default read-only mode.
		return tool.Meta{Name: name, Toolset: Toolset, Title: title, Description: desc, Class: tool.Write, Local: true, AvailableReadOnly: true}
	}
	tool.Register(tool.New(write("sim_stop", "Stop simulator",
		"Kill monkeydo sessions, then the simulator, and wait for port 1234 to close. Also recovers a wedged simulator."), simStop))
	tool.Register(tool.New(write("sim_run", "Run app in simulator",
		"Run a PRG (or build project+device) via monkeydo, starting the simulator if needed; returns a session id once the client prints or exits, inlining the newest crash entry on a crash. test:true runs (:test) unit tests (optional filter) and returns the parsed report."), simRun))
	tool.Register(tool.New(read("sim_logs", "Session logs",
		"Simulator state (running, pid, SDK, device) and a monkeydo session's lines after a cursor (next_token), with exit and crash flags. Default: newest session. crash:true returns the newest parsed CIQ_LOG.YML entries."), simLogs))
	// Writes a PNG under the root (and may replace one with overwrite), so it
	// is a Write tool; it stays usable in read-only mode like the others.
	shot := write("sim_screenshot", "Screenshot",
		"macOS only. Capture the simulator window (works behind other windows; needs Screen Recording and an unlocked screen), crop to the device display at native size, save PNG under the root and return it as an image.")
	shot.Platforms = []string{"darwin"}
	tool.Register(tool.New(shot, simScreenshot))
	m := write("sim_input", "Simulator input",
		"macOS only. Drive the simulator in order: press a button (menu = long up), set the GPS position, or wait. Steals focus: requires allow_focus:true and Accessibility permission; stops if another app takes focus between steps.")
	m.Enums = map[string][]any{"steps.press": {"up", "down", "enter", "esc", "back", "menu"}}
	m.Platforms = []string{"darwin"}
	tool.Register(tool.New(m, simInput))
}

// waitCap bounds a wait to [tool.NoProgressWaitCap] when the client sent no
// progress token (it would cancel the request after about 60 s).
func waitCap(env *tool.Env, d time.Duration) time.Duration {
	if (env == nil || !env.HasProgress()) && d > tool.NoProgressWaitCap {
		return tool.NoProgressWaitCap
	}
	return d
}

func clock(env *tool.Env) sim.Clock {
	if env != nil && env.Clock != nil {
		return env.Clock
	}
	return tool.SystemClock{}
}

func resolveSDK(arg string) (string, ciqsdk.Resolved, error) {
	dr, err := ciqsdk.DataRoot(os.Getenv)
	if err != nil {
		return "", ciqsdk.Resolved{}, err
	}
	r, err := ciqsdk.Resolve(ciqsdk.ResolveOpts{Arg: arg, DataRoot: dr, Getenv: os.Getenv})
	if err != nil {
		return "", ciqsdk.Resolved{}, tool.InvalidArgs("%v", err)
	}
	return dr, r, nil
}

func lease(ctx context.Context) (func(), error) {
	c, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	rel, err := sim.Lease(c)
	if err != nil {
		return nil, &tool.Error{Code: "sim_busy", Message: "another simulator call holds the lease: " + err.Error(), Hint: "retry when it finishes"}
	}
	return rel, nil
}

func simErr(err error) error {
	switch {
	case errors.Is(err, sim.ErrSDKMismatch):
		return &tool.Error{Code: "sdk_mismatch", Message: err.Error(), Hint: "call sim_stop, then sim_run with the wanted sdk"}
	case errors.Is(err, sim.ErrPortNotOurs):
		return &tool.Error{Code: "port_in_use", Message: err.Error(), Hint: "stop whatever holds 127.0.0.1:1234 (or sim_stop), then retry"}
	case errors.Is(err, sim.ErrFocusLost):
		return &tool.Error{Code: "focus_lost", Message: err.Error(), Hint: "keep the simulator frontmost and retry sim_input"}
	case errors.Is(err, sim.ErrAccessibility):
		return &tool.Error{Code: "accessibility_denied", Message: err.Error()}
	case errors.Is(err, sim.ErrScreenLocked):
		return &tool.Error{Code: "screen_locked", Message: err.Error(), Hint: "unlock the Mac (keep it awake with caffeinate -d) and retry"}
	case err != nil && err.Error() == "unsupported_on_"+runtime.GOOS:
		return &tool.Error{Code: err.Error(), Message: "not implemented on " + runtime.GOOS + "; GUI automation is macOS-only for now"}
	case errors.Is(err, sim.ErrNoDisplay):
		return &tool.Error{Code: tool.ErrUnavailable.Code, Message: err.Error(), Hint: "run garmin-mcp inside a desktop session (or under xvfb-run; on WSL use WSLg), then retry"}
	case errors.Is(err, sim.ErrSimulatorExited):
		return &tool.Error{Code: "sim_exited", Message: err.Error(), Hint: "fix what the simulator output names (display, missing libraries such as libwebkit2gtk), then retry sim_run"}
	case errors.Is(err, sim.ErrTooManySessions):
		return &tool.Error{Code: "sim_busy", Message: err.Error(), Hint: "call sim_stop to end the running sessions, then retry"}
	}
	return err
}

// findJava returns the absolute java for monkeyc/monkeydo; without one the
// call fails (the SDK's wrapper scripts would run a bare `java`).
func findJava(ctx context.Context) (string, error) {
	j, err := ciqsdk.FindJava(ctx, os.Getenv)
	if err != nil {
		return "", fmt.Errorf("%w: java: %v; install a JDK 17+ and set JAVA_HOME to its absolute path (or put java on PATH), then retry", tool.ErrUnavailable, err)
	}
	return j.Path, nil
}

func sessionsOut() []sim.SessionInfo {
	out := []sim.SessionInfo{}
	for _, s := range sim.Sessions.List() {
		out = append(out, s.Info())
	}
	return out
}

// --- stop ---

type noIn struct{}

func simStop(ctx context.Context, env *tool.Env, _ noIn) (*tool.Result, error) {
	rel, err := lease(ctx)
	if err != nil {
		return nil, err
	}
	defer rel()
	killed, was, err := sim.Stop(ctx, clock(env))
	if err != nil {
		return nil, err
	}
	out := map[string]any{"stopped": was, "killed_sessions": killed}
	if !was && killed == 0 {
		return tool.Already(out), nil
	}
	return tool.OK(out), nil
}

// --- run / test ---

type runIn struct {
	SDK     string `json:"sdk,omitempty" jsonschema:"installed SDK version or dir name; custom SDK only via GARMIN_MCP_SDK"`
	Device  string `json:"device" jsonschema:"device id, e.g. fenix7"`
	PRG     string `json:"prg,omitempty" jsonschema:"PRG inside the root; omit to build project"`
	Project string `json:"project,omitempty" jsonschema:"project dir inside the root to build (default: root)"`
	Key     string `json:"key,omitempty" jsonschema:"developer key (.der) inside the root; default GARMIN_MCP_KEY; needed when building"`
	Test    bool   `json:"test,omitempty" jsonschema:"run (:test) unit tests: build with -t or take a test PRG, run monkeydo -t, return the parsed report (exit code ignored)"`
	Filter  string `json:"filter,omitempty" jsonschema:"with test: run only this test name (letters, digits, _ . * : -; no leading -)"`
}

// checkPRG accepts only a regular file named *.prg, so no other file (a
// key, a directory, a FIFO) is ever pushed to the simulator.
func checkPRG(p string) error {
	if !strings.EqualFold(filepath.Ext(p), ".prg") {
		return tool.InvalidArgs("prg must be a .prg file")
	}
	fi, err := os.Stat(p)
	if err != nil {
		return tool.InvalidArgs("prg: %v", err)
	}
	if !fi.Mode().IsRegular() {
		return tool.InvalidArgs("prg must be a regular file")
	}
	return nil
}

// simKey returns the build key: an agent-supplied key confined to the root,
// else the human-set GARMIN_MCP_KEY (read only, may be outside the root).
func simKey(env *tool.Env, key string) (string, error) {
	if key != "" {
		return env.Root.Resolve(key)
	}
	if p, ok := ciqsdk.EnvKey(); ok {
		return p, nil
	}
	if v := os.Getenv(ciqsdk.EnvKeyVar); v != "" {
		return "", tool.InvalidArgs("%s=%q is not an absolute path to an existing key file; ask the human to fix it", ciqsdk.EnvKeyVar, v)
	}
	return "", tool.InvalidArgs("give prg, or build one: ask the human to set %s to the app's developer key, or pass key inside the root", ciqsdk.EnvKeyVar)
}

// prgFor returns a PRG: the given one, or a fresh build of project with java.
func prgFor(ctx context.Context, env *tool.Env, r ciqsdk.Resolved, java string, in runIn, test bool) (string, *ciqsdk.BuildResult, error) {
	if in.PRG != "" {
		p, err := env.Root.Resolve(in.PRG)
		if err != nil {
			return "", nil, err
		}
		if err := checkPRG(p); err != nil {
			return "", nil, err
		}
		return p, nil, nil
	}
	kabs, err := simKey(env, in.Key)
	if err != nil {
		return "", nil, err
	}
	proj := in.Project
	if proj == "" {
		proj = "."
	}
	pdir, err := env.Root.Resolve(proj)
	if err != nil {
		return "", nil, err
	}
	if _, err := ciqsdk.CheckKey(kabs); err != nil {
		return "", nil, tool.InvalidArgs("key: %v", err)
	}
	name := in.Device + ".prg"
	if test {
		name = in.Device + "-test.prg"
	}
	out, err := env.Root.Resolve(filepath.Join(pdir, "bin", name))
	if err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil { //nolint:gosec // G301: build output dir in the user's project; normal project-dir permissions
		return "", nil, err
	}
	br, err := ciqsdk.Build(ctx, ciqsdk.BuildOpts{
		SDK: r.SDK, Java: java, ProjectDir: pdir, Device: in.Device, Key: kabs,
		Output: out, Test: test, Warnings: true,
	})
	if err != nil {
		return "", nil, err
	}
	if !br.OK {
		return "", &br, nil
	}
	return out, &br, nil
}

func buildFailed(br *ciqsdk.BuildResult) *tool.Result {
	ds := br.Diagnostics
	if len(ds) > 50 {
		ds = ds[:50]
	}
	return tool.Fail(&tool.Error{Code: "build_failed", Message: fmt.Sprintf("build failed: %d errors", br.Errors), Hint: "fix the diagnostics; sdk_build shows the full list"}).
		Warn(fmt.Sprintf("diagnostics: %v; tail: %s", ds, br.Tail))
}

type started struct {
	s          *sim.Session
	br         *ciqsdk.BuildResult
	simStarted bool
}

// startSession builds (or takes) the PRG, starts the simulator of the
// resolved SDK when none is ready, and launches monkeydo.
func startSession(ctx context.Context, env *tool.Env, in runIn, extra ...string) (started, *tool.Result, error) {
	var out started
	if !ciqsdk.ValidDeviceID(in.Device) {
		return out, nil, tool.InvalidArgs("invalid device id %q", in.Device)
	}
	if err := sim.Sessions.CheckRoom(); err != nil {
		return out, nil, simErr(err)
	}
	_, r, err := resolveSDK(in.SDK)
	if err != nil {
		return out, nil, err
	}
	// monkeydo needs java too: fail before building or starting anything.
	java, err := findJava(ctx)
	if err != nil {
		return out, nil, err
	}
	prg, br, err := prgFor(ctx, env, r, java, in, in.Test)
	out.br = br
	if err != nil {
		return out, nil, err
	}
	if br != nil && !br.OK {
		return out, buildFailed(br), nil
	}
	_, already, err := sim.Start(ctx, clock(env), r.SDK)
	if err != nil {
		return out, nil, simErr(err)
	}
	out.simStarted = !already
	out.s, err = sim.Sessions.Start(r.SDK.MonkeydoArgv(java, prg, in.Device, extra...), prg, in.Device, in.Test)
	return out, nil, simErr(err)
}

// testFilterRe bounds test filters to test-name characters so a filter
// can never become a monkeydo flag or a second argument.
var testFilterRe = regexp.MustCompile(`^[A-Za-z0-9_.*:][A-Za-z0-9_.*:-]{0,199}$`)

func simRun(ctx context.Context, env *tool.Env, in runIn) (*tool.Result, error) {
	if in.Filter != "" && (!in.Test || !testFilterRe.MatchString(in.Filter)) {
		return nil, tool.InvalidArgs("filter %q: needs test:true and a test name (letters, digits, _ . * : -, max 200, no leading -)", in.Filter)
	}
	begin := time.Now()
	// First output (device switch can take ~15 s) or, for tests, exit.
	budget := 60 * time.Second
	extra := []string(nil)
	if in.Test {
		budget = 5 * time.Minute
		extra = []string{"-t"}
		if in.Filter != "" {
			extra = append(extra, in.Filter)
		}
	}
	budget = waitCap(env, budget)
	rel, err := lease(ctx)
	if err != nil {
		return nil, err
	}
	defer rel()
	st, fail, err := startSession(ctx, env, in, extra...)
	if fail != nil || err != nil {
		return fail, err
	}
	s := st.s
	wait := budget - time.Since(begin)
	if wait < 5*time.Second {
		wait = 5 * time.Second
	}
	out := map[string]any{}
	if st.simStarted {
		out["simulator_started"] = true
	}
	if st.br != nil {
		out["build_ms"] = st.br.DurationMS
		out["build_warnings"] = st.br.Warnings
	}
	var warns []string
	if in.Test {
		if !s.WaitExit(ctx, wait) {
			out["session"] = s.Info()
			return tool.StillProcessing(out, "tests still running; read sim_logs with session_id"), nil
		}
		rep := sim.ParseTestTranscript(s.Ring().Text())
		out["report"] = rep
		if !rep.Consistent {
			warns = append(warns, "transcript blocks, RESULTS table and summary disagree or are incomplete; check sim_logs")
		}
	} else if !s.WaitFirst(ctx, wait) {
		out["session"] = s.Info()
		// Never kill a client that has not connected yet: it would wedge the simulator.
		return tool.StillProcessing(out, "monkeydo has not printed yet; poll sim_logs with session_id"), nil
	}
	out["session"] = s.Info()
	if s.Crashed() {
		if e, ok := crashSince(s.Started); ok {
			out["crash"] = e
		} else {
			warns = append(warns, "app crashed but CIQ_LOG.YML has no entry from this run yet; read sim_logs with crash:true")
		}
	}
	r := tool.OK(out)
	for _, w := range warns {
		r.Warn(w)
	}
	return r, nil
}

// Bounds of a crash entry inlined into sim_run.
const (
	maxCrashText   = 2000
	maxCrashFrames = 20
)

// crashSince returns the newest crash-log entry, bounded in size, if the log
// was written at or after since (so a stale entry is never blamed on a run).
func crashSince(since time.Time) (sim.CrashEntry, bool) {
	p := sim.CrashLogPath()
	if fi, err := os.Stat(p); err != nil || fi.ModTime().Before(since.Add(-2*time.Second)) {
		return sim.CrashEntry{}, false
	}
	tail, _, err := sim.ReadCrashLogTail(p, 256*1024)
	if err != nil {
		return sim.CrashEntry{}, false
	}
	es := sim.ParseCrashLog(tail)
	if len(es) == 0 {
		return sim.CrashEntry{}, false
	}
	return boundCrash(es[len(es)-1]), true
}

func boundCrash(e sim.CrashEntry) sim.CrashEntry {
	clip := func(s string) string {
		if len(s) > maxCrashText {
			return s[:maxCrashText] + "…"
		}
		return s
	}
	e.Error, e.Details = clip(e.Error), clip(e.Details)
	if len(e.Stack) > maxCrashFrames {
		e.Stack = e.Stack[:maxCrashFrames]
	}
	fs := make(map[string]string, len(e.Fields))
	for k, v := range e.Fields {
		if len(fs) == maxCrashFrames {
			break
		}
		fs[k] = clip(v)
	}
	e.Fields = fs
	return e
}

// --- logs / crash log ---

type logsIn struct {
	SessionID string `json:"session_id,omitempty" jsonschema:"default: newest session"`
	After     int64  `json:"after,omitempty" jsonschema:"cursor: return lines after this (next_token of the previous call)"`
	Limit     int    `json:"limit,omitempty" jsonschema:"max lines, default 500; with crash: newest entries, default 3"`
	Crash     bool   `json:"crash,omitempty" jsonschema:"return the newest parsed CIQ_LOG.YML entries (error, details, app, file:line stack) instead of session lines"`
}

func simLogs(ctx context.Context, _ *tool.Env, in logsIn) (*tool.Result, error) {
	st, err := sim.GetStatus(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"simulator": st, "sessions": sessionsOut()}
	if in.Crash {
		return crashLog(out, in.Limit)
	}
	var s *sim.Session
	var ok bool
	if in.SessionID == "" {
		s, ok = sim.Sessions.Latest()
	} else {
		s, ok = sim.Sessions.Get(in.SessionID)
	}
	if !ok && in.SessionID != "" {
		return nil, &tool.Error{Code: "not_found", Message: "no such session in this server process", Hint: "call sim_logs without session_id to list sessions"}
	}
	if !ok {
		if dr, err := ciqsdk.DataRoot(os.Getenv); err == nil {
			out["device"] = sim.LastUsedDevice(dr)
		}
		out["session"], out["lines"] = nil, []sim.Line{}
		return tool.OK(out), nil
	}
	if in.Limit <= 0 {
		in.Limit = 500
	}
	lines, next, latest, dropped := s.Ring().Since(in.After, in.Limit)
	if lines == nil {
		lines = []sim.Line{}
	}
	out["device"], out["session"], out["lines"] = s.Device, s.Info(), lines
	out["next_token"], out["latest_token"], out["dropped"], out["more"] = next, latest, dropped, next < latest
	return tool.OK(out), nil
}

func crashLog(out map[string]any, limit int) (*tool.Result, error) {
	if limit <= 0 {
		limit = 3
	}
	p := sim.CrashLogPath()
	out["path"] = p
	tail, size, err := sim.ReadCrashLogTail(p, 256*1024)
	if errors.Is(err, sim.ErrNoCrashLog) {
		out["exists"], out["entries"], out["note"] = false, []sim.CrashEntry{}, err.Error()
		return tool.OK(out), nil
	}
	if err != nil {
		return nil, err
	}
	es := sim.ParseCrashLog(tail)
	out["parsed_in_tail"] = len(es)
	if len(es) > limit {
		es = es[len(es)-limit:]
	}
	if es == nil {
		es = []sim.CrashEntry{}
	}
	out["exists"], out["size"], out["entries"] = true, size, es
	return tool.OK(out), nil
}

// --- screenshot ---

type shotIn struct {
	Device    string `json:"device,omitempty" jsonschema:"device whose display to crop; default newest session's, else simulator.ini LastUsedDevice"`
	Output    string `json:"output,omitempty" jsonschema:"PNG path inside the root, must end in .png (default sim-screenshots/<device>-<time>.png)"`
	Overwrite bool   `json:"overwrite,omitempty" jsonschema:"replace an existing .png at output"`
}

func simScreenshot(ctx context.Context, env *tool.Env, in shotIn) (*tool.Result, error) {
	if runtime.GOOS != "darwin" {
		return nil, simErr(sim.ErrUnsupported())
	}
	dr, err := ciqsdk.DataRoot(os.Getenv)
	if err != nil {
		return nil, err
	}
	dev := in.Device
	if dev == "" {
		if s, ok := sim.Sessions.Latest(); ok {
			dev = s.Device
		} else {
			dev = sim.LastUsedDevice(dr)
		}
	}
	if dev != "" && !ciqsdk.ValidDeviceID(dev) {
		return nil, tool.InvalidArgs("invalid device id %q", dev)
	}
	out := in.Output
	if out == "" {
		name := dev
		if name == "" {
			name = "window"
		}
		out = filepath.Join("sim-screenshots", fmt.Sprintf("%s-%s.png", name, time.Now().Format("20060102-150405.000")))
	}
	abs, err := env.Root.Resolve(out)
	if err != nil {
		return nil, err
	}
	if err := sim.CheckDest(abs, in.Overwrite, ".png"); err != nil {
		return nil, tool.InvalidArgs("%v", err)
	}
	w, err := sim.SimulatorWindow(ctx)
	if err != nil {
		return nil, simErr(err)
	}
	tmp, err := os.CreateTemp("", "ciq-sim-*.png")
	if err != nil {
		return nil, err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	if err := sim.CaptureWindow(ctx, w.ID, tmp.Name()); err != nil {
		return nil, simErr(err)
	}
	raw, err := os.ReadFile(tmp.Name())
	if err != nil {
		return nil, err
	}
	data, cropped := raw, false
	var warn string
	if dev != "" {
		if cimg, err := crop(raw, w, dr, dev); err != nil {
			warn = "returned the full window: " + err.Error()
		} else {
			var b bytes.Buffer
			if err := png.Encode(&b, cimg); err != nil {
				return nil, err
			}
			data, cropped = b.Bytes(), true
		}
	}
	if err := env.Root.WriteFile(abs, data, 0o644); err != nil {
		return nil, err
	}
	r := tool.OK(map[string]any{"path": abs, "device": dev, "cropped": cropped, "window_id": w.ID, "bytes": len(data)}).WithImage("image/png", data)
	if warn != "" {
		r.Warn(warn)
	}
	return r, nil
}

func crop(raw []byte, w sim.Window, dataRoot, dev string) (image.Image, error) {
	img, err := png.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	scale := 1
	if w.Width > 0 {
		scale = int(float64(img.Bounds().Dx())/w.Width + 0.5)
	}
	art, err := sim.LoadDeviceArt(ciqsdk.DevicesDir(dataRoot), dev)
	if err != nil {
		return nil, err
	}
	return sim.CropDisplay(img, scale, art)
}

// --- input ---

type position struct {
	Lat float64 `json:"lat" jsonschema:"latitude, decimal degrees"`
	Lon float64 `json:"lon" jsonschema:"longitude, decimal degrees"`
}

type inputStep struct {
	Press    string    `json:"press,omitempty" jsonschema:"simulator button to press"`
	HoldMS   int       `json:"hold_ms,omitempty" jsonschema:"with press: hold time, default 100 (min 50; menu >= 1100; max 5000)"`
	Position *position `json:"position,omitempty" jsonschema:"set the simulated GPS position via Settings → Set Position"`
	WaitMS   int       `json:"wait_ms,omitempty" jsonschema:"pause in milliseconds"`
}

type inputIn struct {
	Steps      []inputStep `json:"steps" jsonschema:"1-50 steps run in order; each has exactly one of press, position, wait_ms"`
	AllowFocus bool        `json:"allow_focus" jsonschema:"must be true: brings the simulator to front and sends keys/menu clicks"`
}

const maxInputSteps = 50

var errFocus = &tool.Error{Code: "focus_required", Message: "this tool steals keyboard focus", Hint: "ask the user, then call again with allow_focus:true"}

// checkSteps validates steps and returns their total wait, holds included.
func checkSteps(steps []inputStep) (time.Duration, error) {
	if len(steps) == 0 || len(steps) > maxInputSteps {
		return 0, tool.InvalidArgs("steps: give 1-%d", maxInputSteps)
	}
	var total time.Duration
	for i, st := range steps {
		n := 0
		if st.Press != "" {
			n++
			if _, ok := sim.KeyCodes[st.Press]; !ok {
				return 0, tool.InvalidArgs("steps[%d]: unknown button %q", i, st.Press)
			}
		}
		if st.Position != nil {
			n++
			if p := st.Position; p.Lat < -90 || p.Lat > 90 || p.Lon < -180 || p.Lon > 180 {
				return 0, tool.InvalidArgs("steps[%d]: lat must be -90..90 and lon -180..180", i)
			}
		}
		if st.WaitMS != 0 {
			n++
			if st.WaitMS < 0 {
				return 0, tool.InvalidArgs("steps[%d]: wait_ms must be positive", i)
			}
			total += time.Duration(st.WaitMS) * time.Millisecond
		}
		if n != 1 {
			return 0, tool.InvalidArgs("steps[%d]: give exactly one of press, position, wait_ms", i)
		}
		if st.HoldMS != 0 && st.Press == "" {
			return 0, tool.InvalidArgs("steps[%d]: hold_ms needs press", i)
		}
		if st.HoldMS < 0 || st.HoldMS > sim.MaxHoldMS {
			return 0, tool.InvalidArgs("steps[%d]: hold_ms must be 0-%d", i, sim.MaxHoldMS)
		}
		if st.Press != "" {
			total += time.Duration(sim.PressHold(st.Press, st.HoldMS)) * time.Millisecond
		}
	}
	return total, nil
}

func simInput(ctx context.Context, env *tool.Env, in inputIn) (*tool.Result, error) {
	if !in.AllowFocus {
		return nil, errFocus
	}
	total, err := checkSteps(in.Steps)
	if err != nil {
		return nil, err
	}
	if c := waitCap(env, time.Minute); total > c {
		return nil, tool.InvalidArgs("total wait_ms + hold_ms %s exceeds %s; split the steps across calls", total, c)
	}
	rel, err := lease(ctx)
	if err != nil {
		return nil, err
	}
	defer rel()
	pid, err := sim.FindPID(ctx)
	if err != nil {
		return nil, simErr(err)
	}
	if pid == 0 {
		return nil, &tool.Error{Code: "not_running", Message: "no Connect IQ SDK simulator of this user is running", Hint: "start one with sim_run"}
	}
	warmed := false
	for i, st := range in.Steps {
		// Every press/position raises the simulator again; refuse if the
		// user took focus since the previous step rather than steal it back.
		if i > 0 && st.WaitMS == 0 {
			front, err := sim.Frontmost(ctx, pid)
			if err != nil {
				return nil, simErr(err)
			}
			if !front {
				return nil, simErr(fmt.Errorf("%w (before step %d; %d done)", sim.ErrFocusLost, i, i))
			}
		}
		switch {
		case st.Press != "":
			// The first key after an app launch is swallowed: send an inert
			// Shift before the first press of each call. sim.Press applies
			// the default and minimum hold.
			err = sim.Press(ctx, pid, st.Press, st.HoldMS, !warmed)
			warmed = true
		case st.Position != nil:
			err = sim.SetPosition(ctx, pid, st.Position.Lat, st.Position.Lon)
		default:
			select {
			case <-ctx.Done():
				err = ctx.Err()
			case <-time.After(time.Duration(st.WaitMS) * time.Millisecond):
			}
		}
		if err != nil {
			return nil, simErr(fmt.Errorf("step %d: %w", i, err))
		}
	}
	return tool.OK(map[string]any{"steps_done": len(in.Steps)}), nil
}
