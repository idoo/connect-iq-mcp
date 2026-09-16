//go:build windows

package sim

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procGetExtendedTCPTable = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")

const tcpTableOwnerPIDListener = 3 // TCP_TABLE_OWNER_PID_LISTENER

// osListenerPIDs returns the pids listening on TCP port (IPv4 and IPv6) from
// GetExtendedTcpTable. Unlike netstat output, nothing in it is localized.
func osListenerPIDs(port uint16) ([]int, error) {
	var pids []int
	for _, fam := range []struct {
		af uint32
		v6 bool
	}{{windows.AF_INET, false}, {windows.AF_INET6, true}} {
		buf, err := tcpTable(fam.af)
		if err != nil {
			return nil, err
		}
		pids = append(pids, parseTCPTable(buf, fam.v6, port)...)
	}
	return pids, nil
}

func tcpTable(af uint32) ([]byte, error) {
	size := uint32(16 << 10)
	for range 4 {
		buf := make([]byte, size)
		r, _, _ := procGetExtendedTCPTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0, uintptr(af), tcpTableOwnerPIDListener, 0) //nolint:gosec // G103: documented iphlpapi call; buf and size outlive it
		switch windows.Errno(r) {
		case 0:
			return buf[:size], nil
		case windows.ERROR_INSUFFICIENT_BUFFER:
			continue // size now holds the needed length
		default:
			return nil, fmt.Errorf("GetExtendedTcpTable: %w", windows.Errno(r))
		}
	}
	return nil, errors.New("GetExtendedTcpTable: table keeps growing")
}

// osExecutable returns pid's full image path, or "" when it can't be read
// (exited, another user's). The UTF-16 API is immune to the console code page.
func osExecutable(pid int) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid)) //nolint:gosec // G115: pids fit in uint32
	if err != nil {
		return ""
	}
	defer func() { _ = windows.CloseHandle(h) }()
	var buf [windows.MAX_LONG_PATH]uint16
	n := uint32(windows.MAX_LONG_PATH)
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return ""
	}
	return windows.UTF16ToString(buf[:n])
}
