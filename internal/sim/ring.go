package sim

import "sync"

// Line is one captured output line. Cursor increases by one per line,
// starting at 1, and never repeats within a session.
type Line struct {
	Cursor int64  `json:"cursor"`
	Stream string `json:"stream"`
	Text   string `json:"text"`
}

// RingBytes is a ring's default text budget: the oldest lines go once the
// retained text exceeds it, so one chatty app cannot pin gigabytes.
const RingBytes = 8 << 20

// Ring keeps the newest lines of a session; older lines are dropped.
type Ring struct {
	mu       sync.Mutex
	cap      int
	maxBytes int
	bytes    int
	buf      []Line
	last     int64
}

// NewRing returns a ring holding at most n lines and [RingBytes] of text.
func NewRing(n int) *Ring {
	if n < 1 {
		n = 1
	}
	return &Ring{cap: n, maxBytes: RingBytes}
}

// Append adds a line, evicting the oldest lines past the line or byte cap
// (the newest line is always kept).
func (r *Ring) Append(stream, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.last++
	r.buf = append(r.buf, Line{Cursor: r.last, Stream: stream, Text: text})
	r.bytes += len(text)
	drop := 0
	for drop < len(r.buf)-1 && (len(r.buf)-drop > r.cap || r.bytes > r.maxBytes) {
		r.bytes -= len(r.buf[drop].Text)
		drop++
	}
	if drop > 0 {
		n := copy(r.buf, r.buf[drop:])
		clear(r.buf[n:])
		r.buf = r.buf[:n]
	}
}

// Since returns up to limit lines (0 = all) with Cursor > cursor. next is the
// cursor to pass to the following call, latest the newest cursor written and
// dropped the number of lines after cursor already evicted from the ring.
func (r *Ring) Since(cursor int64, limit int) (lines []Line, next, latest, dropped int64) {
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
		if limit > 0 && len(lines) == limit {
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
