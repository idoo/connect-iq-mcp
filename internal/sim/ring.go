package sim

import "sync"

// Line is one captured output line. Cursor increases by one per line,
// starting at 1, and never repeats within a session.
type Line struct {
	Cursor int64  `json:"cursor"`
	Stream string `json:"stream"`
	Text   string `json:"text"`
}

// Ring keeps the newest lines of a session; older lines are dropped.
type Ring struct {
	mu   sync.Mutex
	cap  int
	buf  []Line
	last int64
}

// NewRing returns a ring holding at most n lines.
func NewRing(n int) *Ring {
	if n < 1 {
		n = 1
	}
	return &Ring{cap: n}
}

// Append adds a line.
func (r *Ring) Append(stream, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last++
	if len(r.buf) == r.cap {
		copy(r.buf, r.buf[1:])
		r.buf = r.buf[:len(r.buf)-1]
	}
	r.buf = append(r.buf, Line{Cursor: r.last, Stream: stream, Text: text})
}

// Since returns up to max lines (0 = all) with Cursor > cursor. next is the
// cursor to pass to the following call, latest the newest cursor written and
// dropped the number of lines after cursor already evicted from the ring.
func (r *Ring) Since(cursor int64, max int) (lines []Line, next, latest, dropped int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	latest = r.last
	if cursor < 0 {
		cursor = 0
	}
	next = cursor
	if len(r.buf) == 0 {
		return nil, next, latest, 0
	}
	if oldest := r.buf[0].Cursor; cursor < oldest-1 {
		dropped = oldest - 1 - cursor
	}
	for _, l := range r.buf {
		if l.Cursor <= cursor {
			continue
		}
		if max > 0 && len(lines) == max {
			break
		}
		lines = append(lines, l)
		next = l.Cursor
	}
	if next < cursor+dropped {
		next = cursor + dropped
	}
	return lines, next, latest, dropped
}

// Text returns all retained lines joined by newlines.
func (r *Ring) Text() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, l := range r.buf {
		n += len(l.Text) + 1
	}
	b := make([]byte, 0, n)
	for _, l := range r.buf {
		b = append(b, l.Text...)
		b = append(b, '\n')
	}
	return string(b)
}
