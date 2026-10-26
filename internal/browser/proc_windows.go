//go:build windows

package browser

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// profilePIDs lists processes whose command line carries this
// --user-data-dir. The path travels in an environment variable, never in
// the PowerShell source.
func profilePIDs(profile string) []int {
	const script = `Get-CimInstance Win32_Process | Where-Object { $_.CommandLine -and ($_.CommandLine.Contains('--user-data-dir=' + $env:GARMIN_MCP_PROFILE + ' ') -or $_.CommandLine.Contains('--user-data-dir="' + $env:GARMIN_MCP_PROFILE + '"') -or $_.CommandLine.EndsWith('--user-data-dir=' + $env:GARMIN_MCP_PROFILE)) } | ForEach-Object { $_.ProcessId }`
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Env = append(os.Environ(), "GARMIN_MCP_PROFILE="+profile)
	out, err := cmd.Output()
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
		time.Sleep(200 * time.Millisecond)
	}
}

// killProfile terminates every process using profile. Windows has no
// SIGTERM; processes are killed outright.
func killProfile(profile string) {
	for _, p := range profilePIDs(profile) {
		if pr, err := os.FindProcess(p); err == nil {
			_ = pr.Kill()
		}
	}
}

// killOrphans is a no-op on Windows: a child browser has no parent-death
// re-parenting to detect, and the profile lock is released when it exits.
func killOrphans(string) {}
