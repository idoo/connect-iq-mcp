package sim

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
)

// Key codes (macOS virtual key codes) of simulator buttons.
var KeyCodes = map[string]int{"up": 126, "down": 125, "enter": 36, "esc": 53, "back": 53, "menu": 126}

// MenuHoldMS is the up-button hold that the simulator treats as MENU.
const MenuHoldMS = 1100

// ErrAccessibility means macOS refused synthetic input.
var ErrAccessibility = errors.New("macOS Accessibility permission missing: System Settings → Privacy & Security → Accessibility → enable the app running ciq-mcp (terminal or MCP client), then retry")

func mapAXError(out []byte, err error) error {
	s := string(out)
	if strings.Contains(s, "accessibility_denied") || strings.Contains(s, "assistive access") ||
		strings.Contains(s, "-1719") || strings.Contains(s, "-25211") || strings.Contains(s, "-1743") {
		return ErrAccessibility
	}
	return fmt.Errorf("%v: %s", err, strings.TrimSpace(s))
}

const pressJXA = `ObjC.import('CoreGraphics'); ObjC.import('ApplicationServices');
if (!$.AXIsProcessTrusted()) { throw new Error('accessibility_denied'); }
var se = Application('System Events');
var p = se.processes.whose({name: 'simulator'});
if (p.length === 0) { throw new Error('simulator process not found'); }
p[0].frontmost = true; delay(0.3);
function key(code, down) { var e = $.CGEventCreateKeyboardEvent($(), code, down); $.CGEventPost(0, e); }
if (%t) { key(56, true); delay(0.06); key(56, false); delay(0.15); }
key(%d, true); delay(%s); key(%d, false); 'ok';`

// Press sends button with a hold of holdMS to the frontmost simulator
// (macOS; steals focus). warmup sends an inert Shift first (the first key
// after an app launch is swallowed).
func Press(ctx context.Context, button string, holdMS int, warmup bool) error {
	if runtime.GOOS != "darwin" {
		return ErrUnsupported()
	}
	code, ok := KeyCodes[button]
	if !ok {
		return fmt.Errorf("unknown button %q", button)
	}
	if button == "menu" && holdMS < MenuHoldMS {
		holdMS = MenuHoldMS
	}
	if holdMS < 50 {
		holdMS = 50
	}
	script := fmt.Sprintf(pressJXA, warmup, code, strconv.FormatFloat(float64(holdMS)/1000, 'f', 3, 64), code)
	out, err := exec.CommandContext(ctx, "osascript", "-l", "JavaScript", "-e", script).CombinedOutput()
	if err != nil {
		return mapAXError(out, err)
	}
	return nil
}

const positionAS = `tell application "System Events" to tell process "simulator"
	set frontmost to true
	delay 0.3
	click (first menu item of menu 1 of menu bar item "Settings" of menu bar 1 whose name starts with "Set Position")
	repeat 50 times
		if exists text field 1 of front window then exit repeat
		delay 0.1
	end repeat
	set value of text field 1 of front window to %q
	key code 36
end tell`

// SetPosition sets the simulated GPS position via Settings → Set Position
// (macOS Accessibility UI scripting; steals focus).
func SetPosition(ctx context.Context, lat, lon float64) error {
	if runtime.GOOS != "darwin" {
		return ErrUnsupported()
	}
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return fmt.Errorf("lat/lon out of range")
	}
	v := strconv.FormatFloat(lat, 'f', -1, 64) + ", " + strconv.FormatFloat(lon, 'f', -1, 64)
	out, err := exec.CommandContext(ctx, "osascript", "-e", fmt.Sprintf(positionAS, v)).CombinedOutput()
	if err != nil {
		return mapAXError(out, err)
	}
	return nil
}
