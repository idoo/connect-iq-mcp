//go:build !windows

package ciqsdk

import (
	"os/exec"
	"syscall"
)

// ownGroup starts cmd in its own process group and makes cancellation
// (timeout, server shutdown) kill the whole group, so no JVM child survives.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
