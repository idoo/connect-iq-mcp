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
	"runtime"
	"time"

	"github.com/idoo/ciq-mcp/internal/ciqsdk"
	"github.com/idoo/ciq-mcp/internal/sim"
	"github.com/idoo/ciq-mcp/internal/tool"
)

func init() {
	read := func(name, title, desc string) tool.Meta {
		return tool.Meta{Name: name, Toolset: Toolset, Title: title, Description: desc, Class: tool.Read, Idempotent: true, Local: true}
	}
	write := func(name, title, desc string) tool.Meta {
		return tool.Meta{Name: name, Toolset: Toolset, Title: title, Description: desc, Class: tool.Write, Local: true}
	}
	tool.Register(tool.New(read("sim_status", "Simulator status",
		"Report whether the Connect IQ simulator runs (pid, SDK, port 1234 ready) and the monkeydo sessions of this server. No GUI or permissions needed."), simStatus))
	tool.Register(tool.New(write("sim_start", "Start simulator",
		"Start the Connect IQ simulator of the resolved SDK and wait until port 1234 is stable. Errors if a simulator of another SDK runs. Opens a GUI window; no focus or permissions needed."), simStart))
	tool.Register(tool.New(write("sim_stop", "Stop simulator",
		"Kill monkeydo sessions, then the simulator, and wait for port 1234 to close. Also recovers a wedged simulator."), simStop))
	tool.Register(tool.New(write("sim_run", "Run app in simulator",
		"Run a PRG (or build project+device first) on a device in the running simulator via monkeydo; returns a session id once the client prints or exits. Read output with sim_logs. No focus needed."), simRun))
	tool.Register(tool.New(read("sim_logs", "Session logs",
		"Read a monkeydo session's println/logger lines after a cursor (next_token), with latest_token, dropped count, exit and crash flags. Default: newest session."), simLogs))
	tool.Register(tool.New(read("sim_crash_log", "Crash log",
		"Parse the newest entries of the simulator's CIQ_LOG.YML (error, details, app, file:line stack). Use after a run reports a crash. Headless."), simCrashLog))
	tool.Register(tool.New(write("sim_test", "Run unit tests",
		"Run (:test) unit tests: build project with -t or take a test PRG, run monkeydo -t [filter] in the running simulator, parse the transcript (exit code ignored). No focus needed."), simTest))
	tool.Register(tool.New(read("sim_screenshot", "Screenshot",
		"macOS: capture the simulator window (works behind other windows; needs Screen Recording and an unlocked screen), crop to the device display at native size, save PNG under the root and return it as an image."), simScreenshot))
	m := write("sim_press", "Press button",
		"macOS: press/hold a simulator button via synthetic keys (menu = long up). Steals focus: requires allow_focus:true and Accessibility permission. Not on Linux/Windows.")
	m.Enums = map[string][]any{"button": {"up", "down", "enter", "esc", "back", "menu"}}
	tool.Register(tool.New(m, simPress))
	tool.Register(tool.New(write("sim_set_position", "Set GPS position",
		"macOS: set the simulated GPS position via Settings → Set Position. Steals focus: requires allow_focus:true and Accessibility permission. Not on Linux/Windows."), simSetPosition))
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
		return &tool.Error{Code: "sdk_mismatch", Message: err.Error(), Hint: "call sim_stop, then sim_start with the wanted sdk"}
	case errors.Is(err, sim.ErrAccessibility):
		return &tool.Error{Code: "accessibility_denied", Message: err.Error()}
	case errors.Is(err, sim.ErrScreenLocked):
		return &tool.Error{Code: "screen_locked", Message: err.Error(), Hint: "unlock the Mac (keep it awake with caffeinate -d) and retry"}
	case err != nil && err.Error() == "unsupported_on_"+runtime.GOOS:
		return &tool.Error{Code: err.Error(), Message: "not implemented on " + runtime.GOOS + "; GUI automation is macOS-only for now"}
	}
	return err
}

func sessionsOut() []sim.SessionInfo {
	out := []sim.SessionInfo{}
	for _, s := range sim.Sessions.List() {
		out = append(out, s.Info())
	}
	return out
}

// --- status/start/stop ---

type noIn struct{}

func simStatus(ctx context.Context, _ *tool.Env, _ noIn) (*tool.Result, error) {
	st, err := sim.GetStatus(ctx)
	if err != nil {
		return nil, err
	}
	return tool.OK(map[string]any{"simulator": st, "sessions": sessionsOut()}), nil
}

type sdkIn struct {
	SDK string `json:"sdk,omitempty" jsonschema:"SDK path or installed version, e.g. 9.2.0; default CIQ_MCP_SDK/CIQ_SDK, current-sdk.cfg, newest"`
}

func simStart(ctx context.Context, env *tool.Env, in sdkIn) (*tool.Result, error) {
	_, r, err := resolveSDK(in.SDK)
	if err != nil {
		return nil, err
	}
	rel, err := lease(ctx)
	if err != nil {
		return nil, err
	}
	defer rel()
	st, already, err := sim.Start(ctx, clock(env), r.SDK)
	if err != nil {
		return nil, simErr(err)
	}
	out := map[string]any{"simulator": st, "sdk": r.SDK.Version, "sdk_source": r.Source}
	if already {
		return tool.Already(out), nil
	}
	return tool.OK(out), nil
}

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
	SDK     string `json:"sdk,omitempty" jsonschema:"SDK path or installed version, e.g. 9.2.0; default CIQ_MCP_SDK/CIQ_SDK, current-sdk.cfg, newest"`
	Device  string `json:"device" jsonschema:"device id, e.g. fenix7"`
	PRG     string `json:"prg,omitempty" jsonschema:"PRG inside the root; omit to build project"`
	Project string `json:"project,omitempty" jsonschema:"project dir inside the root to build (default: root)"`
	Key     string `json:"key,omitempty" jsonschema:"developer key (.der) inside the root; required when building"`
	WaitSec int    `json:"wait_seconds,omitempty" jsonschema:"max wait for first output, default 60 (device switch can take ~15 s)"`
}

type testIn struct {
	runIn
	Filter string `json:"filter,omitempty" jsonschema:"run only this test name"`
}

// prgFor returns a PRG: the given one, or a fresh build of project.
func prgFor(ctx context.Context, env *tool.Env, r ciqsdk.Resolved, in runIn, test bool) (string, *ciqsdk.BuildResult, error) {
	if in.PRG != "" {
		p, err := env.Root.Resolve(in.PRG)
		if err != nil {
			return "", nil, err
		}
		if _, err := os.Stat(p); err != nil {
			return "", nil, tool.InvalidArgs("prg: %v", err)
		}
		return p, nil, nil
	}
	if in.Key == "" {
		return "", nil, tool.InvalidArgs("give prg, or key (+ project) to build one")
	}
	proj := in.Project
	if proj == "" {
		proj = "."
	}
	pdir, err := env.Root.Resolve(proj)
	if err != nil {
		return "", nil, err
	}
	kabs, err := env.Root.Resolve(in.Key)
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
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return "", nil, err
	}
	var java string
	if j, err := ciqsdk.FindJava(ctx, os.Getenv); err == nil {
		java = j.Path
	}
	br, err := ciqsdk.Build(ctx, ciqsdk.BuildOpts{SDK: r.SDK, Java: java, ProjectDir: pdir, Device: in.Device, Key: kabs,
		Output: out, Test: test, Warnings: true})
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

func startSession(ctx context.Context, env *tool.Env, in runIn, test bool, extra ...string) (*sim.Session, *ciqsdk.BuildResult, *tool.Result, error) {
	if !ciqsdk.ValidDeviceID(in.Device) {
		return nil, nil, nil, tool.InvalidArgs("invalid device id %q", in.Device)
	}
	_, r, err := resolveSDK(in.SDK)
	if err != nil {
		return nil, nil, nil, err
	}
	prg, br, err := prgFor(ctx, env, r, in, test)
	if err != nil {
		return nil, nil, nil, err
	}
	if br != nil && !br.OK {
		return nil, br, buildFailed(br), nil
	}
	st, err := sim.GetStatus(ctx)
	if err != nil {
		return nil, br, nil, err
	}
	if !st.Ready {
		return nil, br, nil, &tool.Error{Code: "simulator_not_running", Message: "the simulator is not running", Hint: "call sim_start"}
	}
	if st.SDKPath != "" && !sim.SameSDK(st.SDKPath, r.SDK.Path) {
		return nil, br, nil, simErr(fmt.Errorf("%w: running %s, requested %s", sim.ErrSDKMismatch, st.SDKPath, r.SDK.Path))
	}
	var java string
	if j, err := ciqsdk.FindJava(ctx, os.Getenv); err == nil {
		java = j.Path
	}
	s, err := sim.Sessions.Start(r.SDK.MonkeydoArgv(java, prg, in.Device, extra...), prg, in.Device, test)
	if err != nil {
		return nil, br, nil, err
	}
	return s, br, nil, nil
}

func simRun(ctx context.Context, env *tool.Env, in runIn) (*tool.Result, error) {
	rel, err := lease(ctx)
	if err != nil {
		return nil, err
	}
	defer rel()
	s, br, fail, err := startSession(ctx, env, in, false)
	if fail != nil || err != nil {
		return fail, err
	}
	wait := time.Duration(in.WaitSec) * time.Second
	if wait <= 0 {
		wait = 60 * time.Second
	}
	out := map[string]any{"session": nil}
	if br != nil {
		out["build_ms"] = br.DurationMS
		out["build_warnings"] = br.Warnings
	}
	if !s.WaitFirst(ctx, wait) {
		out["session"] = s.Info()
		// Never kill a client that has not connected yet: it would wedge the simulator.
		return tool.StillProcessing(out, "monkeydo has not printed yet; poll sim_logs with session_id"), nil
	}
	out["session"] = s.Info()
	r := tool.OK(out)
	if s.Crashed() {
		r.Warn("app crashed; see sim_crash_log")
	}
	return r, nil
}

func simTest(ctx context.Context, env *tool.Env, in testIn) (*tool.Result, error) {
	rel, err := lease(ctx)
	if err != nil {
		return nil, err
	}
	defer rel()
	extra := []string{"-t"}
	if in.Filter != "" {
		extra = append(extra, in.Filter)
	}
	s, _, fail, err := startSession(ctx, env, in.runIn, true, extra...)
	if fail != nil || err != nil {
		return fail, err
	}
	wait := time.Duration(in.WaitSec) * time.Second
	if wait <= 0 {
		wait = 5 * time.Minute
	}
	if !s.WaitExit(ctx, wait) {
		return tool.StillProcessing(map[string]any{"session": s.Info()}, "tests still running; read sim_logs with session_id"), nil
	}
	rep := sim.ParseTestTranscript(s.Ring().Text())
	r := tool.OK(map[string]any{"report": rep, "session": s.Info()})
	if !rep.Consistent {
		r.Warn("transcript blocks, RESULTS table and summary disagree or are incomplete; check sim_logs")
	}
	if s.Crashed() {
		r.Warn("app crashed; see sim_crash_log")
	}
	return r, nil
}

// --- logs / crash log ---

type logsIn struct {
	SessionID string `json:"session_id,omitempty" jsonschema:"default: newest session"`
	After     int64  `json:"after,omitempty" jsonschema:"cursor: return lines after this (next_token of the previous call)"`
	Limit     int    `json:"limit,omitempty" jsonschema:"max lines, default 500"`
}

func simLogs(_ context.Context, _ *tool.Env, in logsIn) (*tool.Result, error) {
	var s *sim.Session
	var ok bool
	if in.SessionID == "" {
		s, ok = sim.Sessions.Latest()
	} else {
		s, ok = sim.Sessions.Get(in.SessionID)
	}
	if !ok {
		return nil, &tool.Error{Code: "not_found", Message: "no such session in this server process", Hint: "sim_run starts one; sim_status lists them"}
	}
	if in.Limit <= 0 {
		in.Limit = 500
	}
	lines, next, latest, dropped := s.Ring().Since(in.After, in.Limit)
	if lines == nil {
		lines = []sim.Line{}
	}
	return tool.OK(map[string]any{"session": s.Info(), "lines": lines, "next_token": next, "latest_token": latest,
		"dropped": dropped, "more": next < latest}), nil
}

type crashIn struct {
	Limit int `json:"limit,omitempty" jsonschema:"newest entries to return, default 3"`
}

func simCrashLog(_ context.Context, _ *tool.Env, in crashIn) (*tool.Result, error) {
	if in.Limit <= 0 {
		in.Limit = 3
	}
	p := sim.CrashLogPath()
	tail, size, err := sim.ReadCrashLogTail(p, 256*1024)
	if errors.Is(err, sim.ErrNoCrashLog) {
		return tool.OK(map[string]any{"path": p, "exists": false, "entries": []sim.CrashEntry{}, "note": err.Error()}), nil
	}
	if err != nil {
		return nil, err
	}
	es := sim.ParseCrashLog(tail)
	total := len(es)
	if len(es) > in.Limit {
		es = es[len(es)-in.Limit:]
	}
	if es == nil {
		es = []sim.CrashEntry{}
	}
	return tool.OK(map[string]any{"path": p, "exists": true, "size": size, "entries": es, "parsed_in_tail": total}), nil
}

// --- screenshot ---

type shotIn struct {
	Device string `json:"device,omitempty" jsonschema:"device whose display to crop; default newest session's, else simulator.ini LastUsedDevice"`
	Output string `json:"output,omitempty" jsonschema:"PNG path inside the root (default sim-screenshots/<device>-<time>.png)"`
	Full   bool   `json:"full_window,omitempty" jsonschema:"return the whole window instead of the cropped display"`
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
	if !in.Full && dev != "" {
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

type pressIn struct {
	Button     string `json:"button" jsonschema:"simulator button"`
	HoldMS     int    `json:"hold_ms,omitempty" jsonschema:"hold time, default 100 (min 50; menu >= 1100)"`
	Warmup     bool   `json:"warmup,omitempty" jsonschema:"send an inert Shift first (first key after an app launch is swallowed)"`
	AllowFocus bool   `json:"allow_focus" jsonschema:"must be true: brings the simulator to front and sends keys"`
}

var errFocus = &tool.Error{Code: "focus_required", Message: "this tool steals keyboard focus", Hint: "ask the user, then call again with allow_focus:true"}

func simPress(ctx context.Context, _ *tool.Env, in pressIn) (*tool.Result, error) {
	if !in.AllowFocus {
		return nil, errFocus
	}
	if in.HoldMS == 0 {
		in.HoldMS = 100
	}
	rel, err := lease(ctx)
	if err != nil {
		return nil, err
	}
	defer rel()
	if err := sim.Press(ctx, in.Button, in.HoldMS, in.Warmup); err != nil {
		return nil, simErr(err)
	}
	return tool.OK(map[string]any{"pressed": in.Button, "hold_ms": in.HoldMS}), nil
}

type positionIn struct {
	Lat        float64 `json:"lat" jsonschema:"latitude, decimal degrees"`
	Lon        float64 `json:"lon" jsonschema:"longitude, decimal degrees"`
	AllowFocus bool    `json:"allow_focus" jsonschema:"must be true: brings the simulator to front and drives its menu"`
}

func simSetPosition(ctx context.Context, _ *tool.Env, in positionIn) (*tool.Result, error) {
	if !in.AllowFocus {
		return nil, errFocus
	}
	if in.Lat < -90 || in.Lat > 90 || in.Lon < -180 || in.Lon > 180 {
		return nil, tool.InvalidArgs("lat must be -90..90 and lon -180..180")
	}
	rel, err := lease(ctx)
	if err != nil {
		return nil, err
	}
	defer rel()
	if err := sim.SetPosition(ctx, in.Lat, in.Lon); err != nil {
		return nil, simErr(err)
	}
	return tool.OK(map[string]any{"lat": in.Lat, "lon": in.Lon}), nil
}
