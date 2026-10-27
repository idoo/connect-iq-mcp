//go:build !windows

package sim

import "errors"

// osListenerPIDs and osExecutable are the Windows API lookups; other OSes
// use lsof/ss and /proc or ps (see listenerPIDs, executable).
func osListenerPIDs(uint16) ([]int, error) { return nil, errors.ErrUnsupported }

func osExecutable(int) string { return "" }
