//go:build windows

package ciqsdk

import "os/exec"

// ownGroup is a no-op on Windows: monkeyc runs as java.exe directly (never a
// .bat wrapper), so CommandContext's default kill reaches the JVM itself.
func ownGroup(*exec.Cmd) {}
