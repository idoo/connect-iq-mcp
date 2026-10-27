package config

import "io/fs"

// fileGid: Windows has no Unix groups; permission checks are skipped there.
func fileGid(fs.FileInfo) (int, bool) { return 0, false }
