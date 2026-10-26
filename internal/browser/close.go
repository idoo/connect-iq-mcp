package browser

import "path/filepath"

// CloseProfile terminates every process still using dir as its
// --user-data-dir (an interactive window left open, a headless browser
// orphaned by a killed server). Call it before deleting a profile so no
// survivor rewrites its cookie database or holds its SingletonLock.
// Best effort: it never fails.
func CloseProfile(dir string) {
	if dir == "" {
		return
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	killProfile(dir)
}
