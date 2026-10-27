// Package sim drives the Connect IQ simulator: its process, monkeydo
// sessions, input, screenshots and crash logs.
package sim

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// CrashMarker is printed by monkeydo when the app crashed; details are in
// CIQ_LOG.YML.
const CrashMarker = "Encountered an app crash"

// RingLines is the per-session line capacity.
const RingLines = 5000

// Session is one monkeydo client process.
type Session struct {
	ID      string
	PRG     string
	Device  string
	Tests   bool
	Started time.Time

	ring     *Ring
	cmd      *exec.Cmd
	first    chan struct{}
	firstOne sync.Once
	done     chan struct{}
	exitCode atomic.Int64
	crashed  atomic.Bool
}

// SessionInfo is the JSON view of a session.
type SessionInfo struct {
	ID          string `json:"session_id"`
	PRG         string `json:"prg"`
	Device      string `json:"device"`
	Tests       bool   `json:"tests,omitempty"`
	Started     string `json:"started"`
	Running     bool   `json:"running"`
	HasOutput   bool   `json:"has_output"`
	ExitCode    *int   `json:"exit_code,omitempty"`
	Crashed     bool   `json:"crashed"`
	LatestToken int64  `json:"latest_token"`
}

// Info returns the session state.
func (s *Session) Info() SessionInfo {
	_, _, latest, _ := s.ring.Since(0, 1)
	i := SessionInfo{
		ID: s.ID, PRG: s.PRG, Device: s.Device, Tests: s.Tests,
		Started: s.Started.UTC().Format(time.RFC3339), Running: !s.Exited(),
		HasOutput: s.HasOutput(), Crashed: s.crashed.Load(), LatestToken: latest,
	}
	if !i.Running {
		c := int(s.exitCode.Load())
		i.ExitCode = &c
	}
	return i
}

// Ring returns the session's output buffer.
func (s *Session) Ring() *Ring { return s.ring }

// Exited reports whether the process ended.
func (s *Session) Exited() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// HasOutput reports whether the client printed anything yet.
func (s *Session) HasOutput() bool {
	select {
	case <-s.first:
		return true
	default:
		return false
	}
}

// Crashed reports whether the crash marker was seen.
func (s *Session) Crashed() bool { return s.crashed.Load() }

// WaitFirst waits for the first output line or exit; false on timeout.
func (s *Session) WaitFirst(ctx context.Context, timeout time.Duration) bool {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-s.first:
	case <-s.done:
	case <-t.C:
		return false
	case <-ctx.Done():
		return false
	}
	return true
}

// WaitExit waits for the process to end; false on timeout.
func (s *Session) WaitExit(ctx context.Context, timeout time.Duration) bool {
	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case <-s.done:
		return true
	case <-t.C:
	case <-ctx.Done():
	}
	return false
}

// ErrNoOutputYet refuses to kill a client still pushing its PRG.
var ErrNoOutputYet = errors.New("monkeydo has produced no output yet; killing it mid-push wedges the simulator")

// Kill ends the client's process group. Without force it refuses while the
// client has not printed anything (it may still be pushing the PRG).
func (s *Session) Kill(force bool) error {
	if s.Exited() {
		return nil
	}
	if !force && !s.HasOutput() {
		return ErrNoOutputYet
	}
	return killGroup(s.cmd)
}

// Manager tracks sessions of this server process.
type Manager struct {
	mu       sync.Mutex
	seq      int
	sessions map[string]*Session
}

// Sessions is the process-wide session manager.
var Sessions = &Manager{sessions: map[string]*Session{}}

// Start launches argv (monkeydo) and captures its output. It refuses while
// [MaxRunningSessions] clients run.
func (m *Manager) Start(argv []string, prg, device string, tests bool) (*Session, error) {
	if len(argv) == 0 {
		return nil, errors.New("empty command")
	}
	if err := m.CheckRoom(); err != nil {
		return nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // G204: argv is built by callers from the resolved SDK's monkeydo and validated project paths; no shell
	ownGroup(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.seq++
	id := fmt.Sprintf("s%d-%d", m.seq, time.Now().Unix()%100000)
	m.mu.Unlock()
	s := &Session{
		ID: id, PRG: prg, Device: device, Tests: tests, Started: time.Now(),
		ring: NewRing(RingLines), cmd: cmd, first: make(chan struct{}), done: make(chan struct{}),
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go s.pump(&wg, "stdout", stdout)
	go s.pump(&wg, "stderr", stderr)
	go func() {
		wg.Wait()
		err := cmd.Wait()
		code := 0
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else if err != nil {
			code = -1
		}
		s.exitCode.Store(int64(code))
		close(s.done)
	}()
	m.add(s)
	return s, nil
}

// MaxFinishedSessions is how many exited sessions a [Manager] keeps.
const MaxFinishedSessions = 20

// add registers s and forgets all but the newest [MaxFinishedSessions]
// exited sessions; running sessions are always kept.
func (m *Manager) add(s *Session) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[s.ID] = s
	var done []*Session
	for _, x := range m.sessions {
		if x.Exited() {
			done = append(done, x)
		}
	}
	if len(done) <= MaxFinishedSessions {
		return
	}
	sort.Slice(done, func(i, j int) bool { return done[i].Started.Before(done[j].Started) })
	for _, x := range done[:len(done)-MaxFinishedSessions] {
		delete(m.sessions, x.ID)
	}
}

// MaxLineBytes is the longest output line kept; the rest of a longer line
// is read and discarded so the client never blocks on a full pipe.
const MaxLineBytes = 64 << 10

// MaxRunningSessions is how many monkeydo clients may run at once.
const MaxRunningSessions = 4

// ErrTooManySessions refuses a new client while [MaxRunningSessions] run.
var ErrTooManySessions = fmt.Errorf("%d monkeydo sessions are already running; call sim_stop to end them, then retry", MaxRunningSessions)

// CheckRoom returns [ErrTooManySessions] when no other client may start.
func (m *Manager) CheckRoom() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, s := range m.sessions {
		if !s.Exited() {
			n++
		}
	}
	if n >= MaxRunningSessions {
		return ErrTooManySessions
	}
	return nil
}

func (s *Session) pump(wg *sync.WaitGroup, stream string, r io.Reader) {
	defer wg.Done()
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, truncated, err := readLine(br, MaxLineBytes)
		if err == nil || len(line) > 0 {
			t := strings.TrimRight(string(line), "\r")
			if truncated {
				t += fmt.Sprintf(" [line truncated at %d bytes]", MaxLineBytes)
			}
			if strings.Contains(t, CrashMarker) {
				s.crashed.Store(true)
			}
			s.ring.Append(stream, t)
			s.firstOne.Do(func() { close(s.first) })
		}
		if err != nil {
			return
		}
	}
}

// readLine reads one line without its newline, keeping at most limit bytes
// and discarding the rest of the line.
func readLine(br *bufio.Reader, limit int) (line []byte, truncated bool, err error) {
	for {
		chunk, err := br.ReadSlice('\n')
		body := bytes.TrimSuffix(chunk, []byte("\n"))
		if room := limit - len(line); len(body) > room {
			body, truncated = body[:max(room, 0)], true
		}
		line = append(line, body...)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, truncated, err
		}
	}
}

// Get returns a session by id.
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

// List returns all sessions, oldest first.
func (m *Manager) List() []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.Before(out[j].Started) })
	return out
}

// Latest returns the newest session.
func (m *Manager) Latest() (*Session, bool) {
	l := m.List()
	if len(l) == 0 {
		return nil, false
	}
	return l[len(l)-1], true
}

// KillAll force-kills every running client (used when the simulator itself
// is being stopped, so a mid-push kill cannot wedge anything that survives).
func (m *Manager) KillAll() int {
	n := 0
	for _, s := range m.List() {
		if !s.Exited() {
			_ = s.Kill(true)
			n++
		}
	}
	return n
}
