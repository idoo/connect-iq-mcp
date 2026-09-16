package state

import (
	"errors"

	"golang.org/x/sys/windows"
)

// isBusy reports a transient Windows error from MoveFileEx while another
// process (or Defender) has the source or target open.
func isBusy(err error) bool {
	return errors.Is(err, windows.ERROR_ACCESS_DENIED) ||
		errors.Is(err, windows.ERROR_SHARING_VIOLATION) ||
		errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
