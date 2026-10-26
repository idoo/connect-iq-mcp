//go:build !darwin

package browser

import (
	"context"
	"errors"
	"time"
)

// canLaunchApp is false: elsewhere the exec allocator's window is fine.
const canLaunchApp = false

func launchApp(context.Context, string, string, string) (string, error) {
	return "", errors.New("browser: app launch is macOS-only")
}

func waitProfileExit(string, time.Duration) bool { return true }

func killProfile(string) {}
