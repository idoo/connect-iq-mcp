package sim

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// CheckDest vets a resolved output path before a tool writes generated content
// there: the extension must be one of exts (lower case, with dot), symlinks
// are refused, and an existing file is replaced only with overwrite (its
// extension already matches, so unrelated files such as signing keys are never
// clobbered). Errors describe the problem; callers wrap them as invalid args.
func CheckDest(abs string, overwrite bool, exts ...string) error {
	ext := strings.ToLower(filepath.Ext(abs))
	if !slices.Contains(exts, ext) {
		return fmt.Errorf("path %q must end in %s", abs, strings.Join(exts, " or "))
	}
	if fi, err := os.Lstat(filepath.Dir(abs)); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return fmt.Errorf("path %q: parent directory is a symlink", abs)
	}
	fi, err := os.Lstat(abs)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	case fi.Mode()&fs.ModeSymlink != 0:
		return fmt.Errorf("path %q is a symlink", abs)
	case !fi.Mode().IsRegular():
		return fmt.Errorf("path %q exists and is not a regular file", abs)
	case !overwrite:
		return fmt.Errorf("path %q exists; pass overwrite:true to replace it", abs)
	}
	return nil
}
