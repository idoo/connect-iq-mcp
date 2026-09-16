//go:build windows

package sim

import "io/fs"

// ownedByMe: the Windows temp dir is per user, so no extra check.
func ownedByMe(fs.FileInfo) bool { return true }
