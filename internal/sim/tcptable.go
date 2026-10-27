package sim

import "encoding/binary"

// Row sizes of iphlpapi's MIB_TCPROW_OWNER_PID (state, local addr, local
// port, remote addr, remote port, pid) and MIB_TCP6ROW_OWNER_PID (local
// addr[16], scope, port, remote addr[16], scope, port, state, pid).
const (
	tcpRowSize  = 6 * 4
	tcp6RowSize = 16 + 4 + 4 + 16 + 4 + 4 + 4 + 4
)

// parseTCPTable reads a GetExtendedTcpTable(TCP_TABLE_OWNER_PID_LISTENER)
// buffer (dwNumEntries, then rows) and returns the owning pids of rows whose
// local port is port. The OS fills it in host (little-endian) order except
// the port, which is in network order in the low 16 bits.
func parseTCPTable(buf []byte, v6 bool, port uint16) []int {
	if len(buf) < 4 {
		return nil
	}
	n := int(binary.LittleEndian.Uint32(buf))
	size, portOff, pidOff := tcpRowSize, 8, 20
	if v6 {
		size, portOff, pidOff = tcp6RowSize, 20, 52
	}
	var pids []int
	for i := range n {
		row := 4 + i*size
		if row+size > len(buf) {
			break
		}
		p := binary.LittleEndian.Uint32(buf[row+portOff:])
		if uint16(p&0xff)<<8|uint16(p>>8&0xff) != port {
			continue
		}
		pids = append(pids, int(binary.LittleEndian.Uint32(buf[row+pidOff:])))
	}
	return pids
}
