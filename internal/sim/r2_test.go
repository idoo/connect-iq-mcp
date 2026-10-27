package sim

import (
	"bufio"
	"encoding/binary"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// runPressCore executes the generated press logic in JavaScriptCore with
// stub primitives and returns the posted key events, e.g. "56d,56u".
func runPressCore(t *testing.T, stubs string, code, holdMS int, warmup bool) string {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("needs osascript JavaScript")
	}
	script := "var log = [];\nfunction post(c, d) { log.push(c + (d ? 'd' : 'u')); }\n" + stubs +
		"\ntry {\n" + pressScriptCore(code, holdMS, warmup) + "\n} catch (e) { log.push('err:' + e.message); }\nlog.join(',');"
	out, err := exec.Command("osascript", "-l", "JavaScript", "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("osascript: %v: %s", err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestPressReleasesKeyWhenHoldIsInterrupted(t *testing.T) {
	// The hold throws (focus loss/cancel mid-hold): key-up must still be posted.
	got := runPressCore(t, "function front() { return true; }\nfunction pause(s) { throw new Error('interrupted'); }", 126, 600, false)
	if got != "126d,126u,err:interrupted" {
		t.Fatalf("events = %s", got)
	}
	// Same for the Shift warmup.
	got = runPressCore(t, "function front() { return true; }\nfunction pause(s) { throw new Error('interrupted'); }", 126, 600, true)
	if got != "56d,56u,err:interrupted" {
		t.Fatalf("warmup events = %s", got)
	}
}

func TestPressNeverPostsDownWithoutFocusAndAlwaysPairs(t *testing.T) {
	// Focus lost after the warmup: the main key is never pressed.
	got := runPressCore(t, "var n = 0; function front() { return n++ === 0; }\nfunction pause(s) {}", 36, 100, true)
	if got != "56d,56u,err:focus_lost" {
		t.Fatalf("events = %s", got)
	}
	got = runPressCore(t, "function front() { return true; }\nfunction pause(s) {}", 36, 100, true)
	if got != "56d,56u,36d,36u" {
		t.Fatalf("events = %s", got)
	}
}

func TestPressHold(t *testing.T) {
	for _, c := range []struct {
		button   string
		in, want int
	}{{"up", 0, 100}, {"up", 10, 50}, {"menu", 0, MenuHoldMS}, {"down", 5000, 5000}} {
		if got := PressHold(c.button, c.in); got != c.want {
			t.Errorf("PressHold(%s,%d) = %d, want %d", c.button, c.in, got, c.want)
		}
	}
}

func TestPressRejectsLongHold(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	if err := Press(t.Context(), 1, "up", MaxHoldMS+1, false); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err = %v", err)
	}
}

func TestListenerParsers(t *testing.T) {
	if got := parseLsofPIDs("p3095\nf8\np42\n"); len(got) != 2 || got[0] != 3095 || got[1] != 42 {
		t.Errorf("lsof = %v", got)
	}
	ss := `LISTEN 0 50 127.0.0.1:1234 0.0.0.0:* users:(("simulator",pid=77,fd=8))
LISTEN 0 50 127.0.0.1:1234 0.0.0.0:*
`
	if got := parseSSPIDs(ss); len(got) != 2 || got[0] != 77 || got[1] != 0 {
		t.Errorf("ss = %v (another user's listener must yield 0)", got)
	}
}

// GetExtendedTcpTable rows: ports are in network order in the low 16 bits,
// everything else little-endian; nothing depends on the UI language (WD-1).
func TestParseTCPTable(t *testing.T) {
	row4 := func(port uint16, pid uint32) []byte {
		r := make([]byte, tcpRowSize)
		binary.LittleEndian.PutUint32(r[0:], 2)                                    // LISTEN
		binary.LittleEndian.PutUint32(r[4:], 0x0100007f)                           // 127.0.0.1
		binary.LittleEndian.PutUint32(r[8:], uint32(port>>8)|uint32(port&0xff)<<8) // htons
		binary.LittleEndian.PutUint32(r[20:], pid)
		return r
	}
	row6 := func(port uint16, pid uint32) []byte {
		r := make([]byte, tcp6RowSize)
		binary.LittleEndian.PutUint32(r[20:], uint32(port>>8)|uint32(port&0xff)<<8)
		binary.LittleEndian.PutUint32(r[52:], pid)
		return r
	}
	table := func(rows ...[]byte) []byte {
		b := binary.LittleEndian.AppendUint32(nil, uint32(len(rows)))
		for _, r := range rows {
			b = append(b, r...)
		}
		return b
	}
	if got := parseTCPTable(table(row4(1234, 4242), row4(12345, 1), row4(53764, 9)), false, 1234); len(got) != 1 || got[0] != 4242 {
		t.Errorf("v4 = %v", got)
	}
	if got := parseTCPTable(table(row6(80, 1), row6(1234, 77)), true, 1234); len(got) != 1 || got[0] != 77 {
		t.Errorf("v6 = %v", got)
	}
	if got := parseTCPTable(table(row4(1234, 1))[:10], false, 1234); len(got) != 0 {
		t.Errorf("truncated = %v", got)
	}
}

func exitedSession(id string, started time.Time) *Session {
	s := &Session{ID: id, Started: started, ring: NewRing(1), first: make(chan struct{}), done: make(chan struct{})}
	close(s.done)
	return s
}

func TestManagerKeepsLastFinishedSessions(t *testing.T) {
	m := &Manager{sessions: map[string]*Session{}}
	base := time.Unix(1_700_000_000, 0)
	running := &Session{ID: "run", Started: base.Add(-time.Hour), ring: NewRing(1), first: make(chan struct{}), done: make(chan struct{})}
	m.add(running)
	for i := range 50 {
		m.add(exitedSession("s"+strings.Repeat("x", i), base.Add(time.Duration(i)*time.Second)))
	}
	l := m.List()
	if len(l) != MaxFinishedSessions+1 {
		t.Fatalf("kept %d sessions", len(l))
	}
	if l[0] != running {
		t.Fatal("running session evicted")
	}
	if l[1].Started != base.Add(30*time.Second) || l[len(l)-1].Started != base.Add(49*time.Second) {
		t.Fatalf("kept wrong sessions: %v .. %v", l[1].Started, l[len(l)-1].Started)
	}
}

func TestPumpDrainsOversizedLine(t *testing.T) {
	s := &Session{ring: NewRing(10), first: make(chan struct{})}
	in := strings.Repeat("x", 3*MaxLineBytes) + "\r\nok\nlast"
	var wg sync.WaitGroup
	wg.Add(1)
	s.pump(&wg, "stdout", strings.NewReader(in))
	lines, _, _, _ := s.ring.Since(0, 10)
	if len(lines) != 3 {
		t.Fatalf("got %d lines", len(lines))
	}
	if !strings.HasPrefix(lines[0].Text, strings.Repeat("x", MaxLineBytes)+" [line truncated") || lines[1].Text != "ok" || lines[2].Text != "last" {
		t.Fatalf("lines: %q.. %q %q", lines[0].Text[len(lines[0].Text)-40:], lines[1].Text, lines[2].Text)
	}
}

func TestReadLineExactLimit(t *testing.T) {
	br := bufio.NewReaderSize(strings.NewReader("abcd\nefghij\n"), 16)
	if l, tr, err := readLine(br, 4); string(l) != "abcd" || tr || err != nil {
		t.Fatalf("%q %v %v", l, tr, err)
	}
	if l, tr, err := readLine(br, 4); string(l) != "efgh" || !tr || err != nil {
		t.Fatalf("%q %v %v", l, tr, err)
	}
}

func TestKillAllEndsRunningClients(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sleep")
	}
	m := &Manager{sessions: map[string]*Session{}}
	s, err := m.Start([]string{"sleep", "30"}, "x.prg", "fenix7", false)
	if err != nil {
		t.Fatal(err)
	}
	if n := m.KillAll(); n != 1 {
		t.Fatalf("killed %d", n)
	}
	if !s.WaitExit(t.Context(), 5*time.Second) {
		t.Fatal("client survived KillAll")
	}
}
