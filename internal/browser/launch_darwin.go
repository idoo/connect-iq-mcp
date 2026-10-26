//go:build darwin

package browser

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// canLaunchApp: on macOS the interactive window is started through
// LaunchServices (`open -n -a`) so it comes to the front; a process we exec
// ourselves stays behind the terminal under cooperative activation.
const canLaunchApp = true

const portFileWait = 15 * time.Second

// launchApp starts the bundle with the profile and returns the browser's
// DevTools websocket URL.
func launchApp(ctx context.Context, bundle, profile string, url string) (string, error) {
	portFile := filepath.Join(profile, "DevToolsActivePort")
	if err := os.Remove(portFile); err != nil && !os.IsNotExist(err) {
		return "", fmt.Errorf("browser: remove stale DevToolsActivePort: %w", err)
	}
	args := []string{
		"-n", "-a", bundle, "--args",
		"--user-data-dir=" + profile,
		"--remote-debugging-port=0",
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-blink-features=AutomationControlled",
		// Same cookie encryption as the headless exec launch (chromedp's
		// defaults); otherwise silent renewal can't decrypt this profile's
		// cookies and the user would see a keychain prompt.
		"--use-mock-keychain",
		"--password-store=basic",
		"--window-size=" + windowSize,
		url,
	}
	if out, err := exec.CommandContext(ctx, "open", args...).CombinedOutput(); err != nil { //nolint:gosec // fixed program, argv array (no shell); bundle is the browser resolved by Find
		return "", fmt.Errorf("browser: open %s: %v: %s", bundle, err, strings.TrimSpace(string(out)))
	}
	deadline := time.Now().Add(portFileWait)
	for {
		if b, err := os.ReadFile(portFile); err == nil { //nolint:gosec // fixed file name inside our own 0700 profile dir
			if ws, ok := parseDevToolsActivePort(b); ok {
				return ws, nil
			}
		}
		if time.Now().After(deadline) {
			killProfile(profile)
			return "", fmt.Errorf("browser: %s opened no DevTools port within %s (is this profile already open in another window?)", filepath.Base(bundle), portFileWait)
		}
		select {
		case <-ctx.Done():
			killProfile(profile)
			return "", ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// excludeFromBackup keeps the signed-in profile out of Time Machine. Best
// effort: errors are ignored.
func excludeFromBackup(dir string) {
	_ = exec.Command("tmutil", "addexclusion", dir).Run() //nolint:gosec // fixed program, argv array; dir is our own profile path
}
