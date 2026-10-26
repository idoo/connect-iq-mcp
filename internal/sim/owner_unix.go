//go:build !windows

package sim

import (
	"io/fs"
	"os"
	"syscall"
)

// ownedByMe reports whether fi belongs to the current user, so files under a
// shared temp dir planted by another account are never trusted.
func ownedByMe(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}
