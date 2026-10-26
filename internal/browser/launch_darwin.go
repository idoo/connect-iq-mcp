//go:build darwin

package browser

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
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
	args := []string{"-n", "-a", bundle, "--args",
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
	if out, err := exec.CommandContext(ctx, "open", args...).CombinedOutput(); err != nil {
		return "", fmt.Errorf("browser: open %s: %v: %s", bundle, err, strings.TrimSpace(string(out)))
	}
	deadline := time.Now().Add(portFileWait)
	for {
		if b, err := os.ReadFile(portFile); err == nil {
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

// profilePIDs lists processes started with exactly this --user-data-dir.
func profilePIDs(profile string) []int {
	pat := "--user-data-dir=" + regexp.QuoteMeta(profile) + "( |$)"
	out, err := exec.Command("pgrep", "-f", "--", pat).Output()
	if err != nil {
		return nil
	}
	var pids []int
	for _, f := range strings.Fields(string(out)) {
		if n, err := strconv.Atoi(f); err == nil && n != os.Getpid() {
			pids = append(pids, n)
		}
	}
	return pids
}

// waitProfileExit waits up to d for every process of the profile to exit.
func waitProfileExit(profile string, d time.Duration) bool {
	for end := time.Now().Add(d); ; {
		if len(profilePIDs(profile)) == 0 {
			return true
		}
		if time.Now().After(end) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// killProfile terminates the browser we launched for profile (we don't own
// its pid because LaunchServices started it).
func killProfile(profile string) {
	for _, p := range profilePIDs(profile) {
		_ = syscall.Kill(p, syscall.SIGTERM)
	}
	if waitProfileExit(profile, 3*time.Second) {
		return
	}
	for _, p := range profilePIDs(profile) {
		_ = syscall.Kill(p, syscall.SIGKILL)
	}
}
