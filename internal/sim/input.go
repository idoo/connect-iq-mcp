package sim

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// KeyCodes maps simulator buttons to macOS virtual key codes.
var KeyCodes = map[string]int{"up": 126, "down": 125, "enter": 36, "esc": 53, "back": 53, "menu": 126}

// MenuHoldMS is the up-button hold that the simulator treats as MENU.
const MenuHoldMS = 1100

// ErrAccessibility means macOS refused synthetic input.
var ErrAccessibility = errors.New("macOS Accessibility permission missing: System Settings → Privacy & Security → Accessibility → enable the app running garmin-mcp (terminal or MCP client), then retry")

// ErrFocusLost means another app became frontmost while keys were being
// sent; the remaining keys were not posted.
var ErrFocusLost = errors.New("the simulator lost focus while pressing; remaining keys were not sent")

func mapAXError(out []byte, err error) error {
	s := string(out)
	if strings.Contains(s, "focus_lost") {
		return ErrFocusLost
	}
	if strings.Contains(s, "accessibility_denied") || strings.Contains(s, "assistive access") ||
		strings.Contains(s, "-1719") || strings.Contains(s, "-25211") || strings.Contains(s, "-1743") {
		return ErrAccessibility
	}
	return fmt.Errorf("%v: %s", err, strings.TrimSpace(s))
}

// MaxHoldMS bounds a press hold so a key is never held for long.
const MaxHoldMS = 5000

// pressPrelude binds the simulator process by pid and defines the
// primitives used by pressCore: front(), post(code, down), pause(seconds).
const pressPrelude = `ObjC.import('CoreGraphics'); ObjC.import('ApplicationServices');
if (!$.AXIsProcessTrusted()) { throw new Error('accessibility_denied'); }
var se = Application('System Events');
var p = se.processes.whose({unixId: %d});
if (p.length === 0) { throw new Error('simulator process not found'); }
p[0].frontmost = true; delay(0.3);
function front() { return p[0].frontmost(); }
function post(code, down) { $.CGEventPost(0, $.CGEventCreateKeyboardEvent($(), code, down)); }
function pause(s) { delay(s); }
`

// pressCore checks focus before every key-down and posts the key-up in a
// finally block, so no key (Shift included) is left held when the hold is
// interrupted.
const pressCore = `function press(code, hold) {
  if (!front()) { throw new Error('focus_lost'); }
  post(code, true);
  try { pause(hold); } finally { post(code, false); }
}
if (%t) { press(56, 0.06); pause(0.15); }
press(%d, %s); 'ok';`

// pressScriptCore returns the focus-checked press logic for code held
// holdMS, with an optional Shift warmup.
func pressScriptCore(code, holdMS int, warmup bool) string {
	return fmt.Sprintf(pressCore, warmup, code, strconv.FormatFloat(float64(holdMS)/1000, 'f', 3, 64))
}

// PressHold returns the effective hold of a press of button: menu is held at
// least [MenuHoldMS], every press at least 50 ms, 0 means 100 ms.
func PressHold(button string, holdMS int) int {
	if holdMS == 0 {
		holdMS = 100
	}
	if button == "menu" && holdMS < MenuHoldMS {
		holdMS = MenuHoldMS
	}
	return max(holdMS, 50)
}

// runScript runs osascript without killing it on ctx cancel (a kill
// mid-hold would skip the key-up); the script is bounded by limit instead,
// and ctx's error is reported after it ends.
func runScript(ctx context.Context, limit time.Duration, args ...string) ([]byte, error) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), limit)
	defer cancel()
	out, err := exec.CommandContext(rctx, "osascript", args...).CombinedOutput() //nolint:gosec // G204: fixed program; scripts are constant templates filled with integers and numbers
	if err != nil {
		return out, err
	}
	return out, ctx.Err()
}

// Press sends button with a hold of holdMS (at most [MaxHoldMS]) to the
// simulator pid (macOS; steals focus). warmup sends an inert Shift first
// (the first key after an app launch is swallowed).
func Press(ctx context.Context, pid int, button string, holdMS int, warmup bool) error {
	if runtime.GOOS != "darwin" {
		return ErrUnsupported()
	}
	code, ok := KeyCodes[button]
	if !ok {
		return fmt.Errorf("unknown button %q", button)
	}
	if holdMS > MaxHoldMS {
		return fmt.Errorf("hold_ms %d exceeds %d", holdMS, MaxHoldMS)
	}
	holdMS = PressHold(button, holdMS)
	script := fmt.Sprintf(pressPrelude, pid) + pressScriptCore(code, holdMS, warmup)
	out, err := runScript(ctx, time.Duration(holdMS)*time.Millisecond+20*time.Second, "-l", "JavaScript", "-e", script)
	if err != nil {
		return mapAXError(out, err)
	}
	return nil
}

const positionAS = `tell application "System Events" to tell (first process whose unix id is %d)
	set frontmost to true
	delay 0.3
	click (first menu item of menu 1 of menu bar item "Settings" of menu bar 1 whose name starts with "Set Position")
	repeat 50 times
		if exists text field 1 of front window then exit repeat
		delay 0.1
	end repeat
	set value of text field 1 of front window to %q
	if not frontmost then error "focus_lost"
	key code 36
end tell`

// SetPosition sets the simulated GPS position of the simulator pid via
// Settings → Set Position (macOS Accessibility UI scripting; steals focus).
func SetPosition(ctx context.Context, pid int, lat, lon float64) error {
	if runtime.GOOS != "darwin" {
		return ErrUnsupported()
	}
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return fmt.Errorf("lat/lon out of range")
	}
	v := strconv.FormatFloat(lat, 'f', -1, 64) + ", " + strconv.FormatFloat(lon, 'f', -1, 64)
	out, err := exec.CommandContext(ctx, "osascript", "-e", fmt.Sprintf(positionAS, pid, v)).CombinedOutput() //nolint:gosec // G204: fixed program; integer pid and two formatted floats, quoted with %q
	if err != nil {
		return mapAXError(out, err)
	}
	return nil
}

const frontmostAS = `tell application "System Events"
	if not (exists (first process whose unix id is %d)) then error "simulator process not found"
	return frontmost of (first process whose unix id is %d)
end tell`

// Frontmost reports whether the simulator pid is the frontmost app,
// without raising it (macOS).
func Frontmost(ctx context.Context, pid int) (bool, error) {
	if runtime.GOOS != "darwin" {
		return false, ErrUnsupported()
	}
	out, err := exec.CommandContext(ctx, "osascript", "-e", fmt.Sprintf(frontmostAS, pid, pid)).CombinedOutput() //nolint:gosec // G204: fixed program; integer pid
	if err != nil {
		return false, mapAXError(out, err)
	}
	return strings.TrimSpace(string(out)) == "true", nil
}
