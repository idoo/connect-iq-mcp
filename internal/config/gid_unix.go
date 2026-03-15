//go:build !windows

package config

import (
	"io/fs"
	"syscall"
)

// fileGid returns the file's group id.
func fileGid(info fs.FileInfo) (int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Gid), true
}
