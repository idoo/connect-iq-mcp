package credstore

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/zalando/go-keyring"
)

const (
	keyringService = "garmin-mcp"
	// keyringChunk keeps every item well under the smallest backend limit
	// (Windows Credential Manager: 2560 bytes per blob).
	keyringChunk = 2000
	// maxChunks bounds reads and legacy chunk cleanup.
	maxChunks = 64
	// chunkPrefix marks item 0 of a chunked blob:
	// "gmcp2:<gen>:<count>:<sha256 hex>:<data>"; chunk i>0 lives under
	// "<profile>#<gen>.<i>". A new generation is written beside the old one
	// and item 0 switches to it last, so an interrupted write leaves the old
	// blob readable.
	chunkPrefix = "gmcp2:"
	// chunkPrefixV1 ("gmcp1:<count>:<sha>:<data>", chunks "<profile>#<i>")
	// and a bare "{" item 0 are older formats, still read.
	chunkPrefixV1 = "gmcp1:"
	// pendingSuffix names the marker item "<profile>#pending" listing chunk
	// generations ("<gen>:<count>,…") a write may leave behind: the new one
	// before its chunks are written and the replaced one until its chunks are
	// deleted. go-keyring cannot enumerate, so the next write and logout find
	// orphans (a crash or timeout before item 0) through it.
	pendingSuffix = "#pending"
)

// errKeyringUnavailable marks a keychain that cannot be used at all (no
// backend, no D-Bus session, a dead bus socket, no Windows logon session).
// Only then (or with GARMIN_MCP_KEYRING=off) do credentials go to the file.
var errKeyringUnavailable = errors.New("keychain unavailable")

// errKeyringLocked marks a keychain that exists but cannot be used right now
// (locked, or its unlock prompt cannot be shown). It never falls back to the
// file, on read or write.
var errKeyringLocked = errors.New("the system keychain is locked or cannot show its unlock prompt; unlock it (Linux: log in to the desktop or `gnome-keyring-daemon --unlock`) and retry, or set GARMIN_MCP_KEYRING=off to use a 0600 file")

// errStoredUnreadable marks a stored credential blob that was read but is
// damaged (incomplete chunk set, checksum mismatch, bad JSON). Saves replace
// it instead of failing.
var errStoredUnreadable = errors.New("stored credentials are unreadable")

// Keychain calls; tests replace them.
var (
	keyringGet = keyring.Get
	keyringSet = keyring.Set
	keyringDel = keyring.Delete
	// keyringTimeout bounds one keychain call: a locked Secret Service
	// collection waits on a prompt nobody can see from stdio.
	keyringTimeout = 5 * time.Second
	// keyringRetryDelay is the pause before re-reading a chunk set that
	// another process may be rewriting.
	keyringRetryDelay = 200 * time.Millisecond
	// keyringPrecheck reports errKeyringUnavailable before any backend call
	// when the keychain clearly cannot work here.
	keyringPrecheck = noSessionBus
)

// noSessionBus: on Linux and the BSDs the keychain is the Secret Service on
// the D-Bus session bus. Without DBUS_SESSION_BUS_ADDRESS or
// /run/user/<uid>/bus, godbus would autolaunch a dbus-daemon that never exits,
// so the keychain is treated as unavailable without calling it.
func noSessionBus() error {
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" {
		return nil
	}
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") != "" {
		return nil
	}
	bus := "/run/user/" + strconv.Itoa(os.Getuid()) + "/bus"
	if _, err := os.Stat(bus); err == nil {
		return nil
	}
	return fmt.Errorf("%w: no D-Bus session (DBUS_SESSION_BUS_ADDRESS unset, no %s)", errKeyringUnavailable, bus)
}

// keyringSem serializes keychain access within the process (the backends are
// not documented as goroutine-safe). A call stuck past its deadline keeps the
// slot; keyringStuck then fails later calls fast instead of queueing them.
var (
	keyringSem   = make(chan struct{}, 1)
	keyringStuck atomic.Bool
)

// keyringCall runs op with the keychain deadline and classifies its error.
func keyringCall(op func() error) error {
	if err := keyringPrecheck(); err != nil {
		return err
	}
	if keyringStuck.Load() {
		return fmt.Errorf("%w (an earlier keychain request is still waiting for an answer)", errKeyringLocked)
	}
	timer := time.NewTimer(keyringTimeout)
	defer timer.Stop()
	select {
	case keyringSem <- struct{}{}:
	case <-timer.C:
		return fmt.Errorf("%w (no answer within %s)", errKeyringLocked, keyringTimeout)
	}
	done := make(chan error, 1)
	go func() {
		defer func() { <-keyringSem }()
		done <- op()
	}()
	select {
	case err := <-done:
		return classifyKeyring(err)
	case <-timer.C:
		// Only one call holds the slot, so this is the only watcher.
		keyringStuck.Store(true)
		go func() {
			<-done //nolint:errcheck // a late answer is dropped; the next call asks the keychain again
			keyringStuck.Store(false)
		}()
		return fmt.Errorf("%w (no answer within %s)", errKeyringLocked, keyringTimeout)
	}
}

// errNoLogonSession is Windows ERROR_NO_SUCH_LOGON_SESSION (OpenSSH key
// login, S4U scheduled tasks): Credential Manager has no session to use.
const errNoLogonSession = syscall.Errno(1312)

// classifyKeyring sorts backend errors into three classes: unavailable
// (wrapped in errKeyringUnavailable; the file is used), locked (wrapped in
// errKeyringLocked; never downgraded to the file) and anything else (denied
// access, data too big), returned as is.
func classifyKeyring(err error) error {
	if err == nil || errors.Is(err, keyring.ErrNotFound) ||
		errors.Is(err, errKeyringUnavailable) || errors.Is(err, errKeyringLocked) {
		return err
	}
	msg := strings.ToLower(err.Error())
	// Checked first: go-keyring's "failed to unlock correct collection
	// '/org/freedesktop/secrets/…'" also names the Secret Service.
	for _, s := range []string{"failed to unlock", "user interaction is not allowed", "interaction not allowed"} {
		if strings.Contains(msg, s) {
			return fmt.Errorf("%w (%v)", errKeyringLocked, err)
		}
	}
	var opErr *net.OpError
	if errors.Is(err, keyring.ErrUnsupportedPlatform) || errors.As(err, &opErr) ||
		errors.Is(err, syscall.ENOENT) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, errNoLogonSession) {
		return fmt.Errorf("%w: %w", errKeyringUnavailable, err)
	}
	for _, s := range []string{
		"dbus", "d-bus", "org.freedesktop.secrets", "secret service",
		"dial unix", "connect: no such file or directory", "connection refused",
		"logon session does not exist",
	} {
		if strings.Contains(msg, s) {
			return fmt.Errorf("%w: %w", errKeyringUnavailable, err)
		}
	}
	return err
}

// splitChunks cuts data into pieces of at most keyringChunk bytes on rune
// boundaries.
func splitChunks(data string) []string {
	var out []string
	for len(data) > keyringChunk {
		n := keyringChunk
		for n > 0 && !utf8.RuneStart(data[n]) {
			n--
		}
		out = append(out, data[:n])
		data = data[n:]
	}
	return append(out, data)
}

func digest(data string) string {
	sum := sha256.Sum256([]byte(data))
	return hex.EncodeToString(sum[:8])
}

// chunkSet is a parsed item 0.
type chunkSet struct {
	gen   string   // gmcp2 generation ("" for gmcp1)
	users []string // keychain users of chunks 1..n-1, in order
	sum   string
	data0 string
	v1    bool // gmcp1: chunks may also linger past n
}

// parseHead parses item 0. ok is false for a single-item (pre-chunk) blob.
func parseHead(profile, head string) (cs chunkSet, ok bool, err error) {
	var gen, rest string
	switch {
	case strings.HasPrefix(head, chunkPrefix):
		var found bool
		gen, rest, found = strings.Cut(strings.TrimPrefix(head, chunkPrefix), ":")
		if !found || !validGen(gen) {
			return cs, true, fmt.Errorf("%w: bad chunk header in the keychain", errStoredUnreadable)
		}
	case strings.HasPrefix(head, chunkPrefixV1):
		rest, cs.v1 = strings.TrimPrefix(head, chunkPrefixV1), true
	default:
		return cs, false, nil
	}
	parts := strings.SplitN(rest, ":", 3)
	if len(parts) != 3 {
		return cs, true, fmt.Errorf("%w: bad chunk header in the keychain", errStoredUnreadable)
	}
	n, err := strconv.Atoi(parts[0])
	if err != nil || n < 1 || n > maxChunks {
		return cs, true, fmt.Errorf("%w: bad chunk count in the keychain", errStoredUnreadable)
	}
	cs.gen = gen
	for i := 1; i < n; i++ {
		if cs.v1 {
			cs.users = append(cs.users, v1ChunkUser(profile, i))
		} else {
			cs.users = append(cs.users, genChunkUser(profile, gen, i))
		}
	}
	cs.sum, cs.data0 = parts[1], parts[2]
	return cs, true, nil
}

func validGen(gen string) bool {
	if len(gen) != 8 {
		return false
	}
	_, err := hex.DecodeString(gen)
	return err == nil
}

func genChunkUser(profile, gen string, i int) string {
	return profile + "#" + gen + "." + strconv.Itoa(i)
}

func v1ChunkUser(profile string, i int) string { return profile + "#" + strconv.Itoa(i) }

// pendingGen is one generation recorded in the "#pending" marker, with the
// time it was recorded (zero: unknown, treated as old).
type pendingGen struct {
	gen string
	n   int
	at  time.Time
}

// pendingGrace: cleanup leaves a recorded generation alone while it is
// younger than this, so a writer that does not share our profile lock (a
// different state directory) can still switch item 0 to it. It is more than
// twice keyringTimeout plus slack.
const pendingGrace = 30 * time.Second

// keyringNow is the marker clock; tests replace it.
var keyringNow = time.Now

func pendingUser(profile string) string { return profile + pendingSuffix }

// readPending returns the generations recorded in profile's marker
// ("<gen>:<count>:<unix seconds>,…"). A missing marker is nil; malformed
// entries are dropped.
func readPending(profile string) ([]pendingGen, error) {
	v, err := keychainGet(pendingUser(profile))
	if errors.Is(err, keyring.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := []pendingGen{} // non-nil: a marker item exists and must be removed
	for _, e := range strings.Split(v, ",") {
		f := strings.Split(e, ":")
		if len(f) < 2 || len(f) > 3 || !validGen(f[0]) {
			continue
		}
		n, err := strconv.Atoi(f[1])
		if err != nil || n < 1 || n > maxChunks {
			continue
		}
		g := pendingGen{gen: f[0], n: n}
		if len(f) == 3 {
			if sec, err := strconv.ParseInt(f[2], 10, 64); err == nil && sec > 0 {
				g.at = time.Unix(sec, 0)
			}
		}
		out = append(out, g)
	}
	return out, nil
}

// writePending stores gens as the marker, deleting it when gens is empty.
func writePending(profile string, gens []pendingGen) error {
	if len(gens) == 0 {
		err := keychainDel(pendingUser(profile))
		if errors.Is(err, keyring.ErrNotFound) {
			return nil
		}
		return err
	}
	parts := make([]string, len(gens))
	for i, g := range gens {
		parts[i] = g.gen + ":" + strconv.Itoa(g.n)
		if !g.at.IsZero() {
			parts[i] += ":" + strconv.FormatInt(g.at.Unix(), 10)
		}
	}
	v := strings.Join(parts, ",")
	return keyringCall(func() error { return keyringSet(keyringService, pendingUser(profile), v) })
}

// deleteGens deletes the chunks of gens, except generation keep (the one
// item 0 points at) and generations recorded less than pendingGrace ago
// (possibly another writer's, still in flight). It returns the generations
// still to clean (skipped or not fully deleted), whether any was skipped,
// and the first delete error.
func deleteGens(profile string, gens []pendingGen, keep string) (left []pendingGen, skipped bool, first error) {
	now := keyringNow()
	for _, g := range gens {
		if g.gen == keep {
			continue
		}
		if age := now.Sub(g.at); !g.at.IsZero() && age >= 0 && age < pendingGrace {
			left, skipped = append(left, g), true
			continue
		}
		ok := true
		for i := 1; i < g.n; i++ {
			if err := keychainDel(genChunkUser(profile, g.gen, i)); err != nil && !errors.Is(err, keyring.ErrNotFound) {
				ok = false
				if first == nil {
					first = err
				}
			}
		}
		if !ok {
			left = append(left, g)
		}
	}
	return left, skipped, first
}

func newGen() (string, error) {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// keychainRead returns the stored blob text for profile ("" if none). A
// damaged chunk set may be another process mid-write, so it is read again
// once after keyringRetryDelay before errStoredUnreadable is returned.
func keychainRead(profile string) (string, error) {
	data, err := keychainReadOnce(profile)
	if errors.Is(err, errStoredUnreadable) {
		time.Sleep(keyringRetryDelay)
		data, err = keychainReadOnce(profile)
	}
	return data, err
}

func keychainReadOnce(profile string) (string, error) {
	head, err := keychainGet(profile)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	cs, chunked, err := parseHead(profile, head)
	if err != nil {
		return "", err
	}
	if !chunked {
		return head, nil
	}
	var b strings.Builder
	b.WriteString(cs.data0)
	for i, user := range cs.users {
		c, err := keychainGet(user)
		if errors.Is(err, keyring.ErrNotFound) {
			return "", fmt.Errorf("%w: keychain chunk %d of %d is missing", errStoredUnreadable, i+2, len(cs.users)+1)
		}
		if err != nil {
			return "", err
		}
		b.WriteString(c)
	}
	data := b.String()
	if digest(data) != cs.sum {
		return "", fmt.Errorf("%w: keychain chunk checksum mismatch", errStoredUnreadable)
	}
	return data, nil
}

func keychainGet(user string) (v string, err error) {
	err = keyringCall(func() (err error) { v, err = keyringGet(keyringService, user); return err })
	return v, err
}

func keychainDel(user string) error {
	return keyringCall(func() error { return keyringDel(keyringService, user) })
}

// keychainWrite stores data as a new chunk generation: chunks 1..n-1 first,
// then item 0 pointing at them, then the previous generation's chunks are
// deleted. A crash or timeout before item 0 leaves the old blob intact and
// readable. While chunks are in flight (new or replaced), the "#pending"
// marker records their generations; each write first deletes the chunks of
// any recorded generation item 0 does not point at.
func keychainWrite(profile, data string) error {
	chunks := splitChunks(data)
	if len(chunks) > maxChunks {
		return fmt.Errorf("credentials too large for the keychain (%d bytes)", len(data))
	}
	var stale []string
	staleV1 := false
	curGen := ""
	head, err := keychainGet(profile)
	switch {
	case errors.Is(err, keyring.ErrNotFound):
	case err != nil:
		return err
	default:
		// A damaged old head still gets replaced; its chunks are just unknown.
		if cs, _, perr := parseHead(profile, head); perr == nil {
			stale, staleV1, curGen = cs.users, cs.v1, cs.gen
		}
	}
	recorded, err := readPending(profile)
	if err != nil {
		return err
	}
	orphans, _, _ := deleteGens(profile, recorded, curGen) // failures and young entries stay recorded
	gen, err := newGen()
	if err != nil {
		return fmt.Errorf("keychain generation: %w", err)
	}
	now := keyringNow()
	pending := append([]pendingGen(nil), orphans...)
	if curGen != "" && len(stale) > 0 {
		pending = append(pending, pendingGen{curGen, len(stale) + 1, now})
	}
	if len(chunks) > 1 {
		pending = append(pending, pendingGen{gen, len(chunks), now})
	}
	// Skip the marker write when nothing is in flight and it already matches.
	if len(pending) > 0 || recorded != nil {
		if err := writePending(profile, pending); err != nil {
			return err
		}
	}
	written := make([]string, 0, len(chunks)-1)
	for i := 1; i < len(chunks); i++ {
		user, c := genChunkUser(profile, gen, i), chunks[i]
		if err := keyringCall(func() error { return keyringSet(keyringService, user, c) }); err != nil {
			abortWrite(profile, written, orphans)
			return err
		}
		written = append(written, user)
	}
	newHead := chunkPrefix + gen + ":" + strconv.Itoa(len(chunks)) + ":" + digest(data) + ":" + chunks[0]
	if err := keyringCall(func() error { return keyringSet(keyringService, profile, newHead) }); err != nil {
		// A timed-out Set may still land: if item 0 now points at the new
		// generation (or cannot be read), its chunks and the marker stay.
		if h, gerr := keychainGet(profile); gerr == nil {
			if cs, _, perr := parseHead(profile, h); perr != nil || cs.gen != gen {
				abortWrite(profile, written, orphans)
			}
		}
		return err
	}
	left := orphans
	if curGen != "" && len(stale) > 0 {
		// Our own replaced generation: no grace (zero time).
		old, _, _ := deleteGens(profile, []pendingGen{{gen: curGen, n: len(stale) + 1}}, gen)
		left = append(left, old...)
	}
	if staleV1 {
		deleteBestEffort(stale)
		deleteV1Chunks(profile, len(stale)+1) //nolint:errcheck // best effort: the new generation is already in place
	}
	if len(pending) > 0 {
		_ = writePending(profile, left) // a stale entry is cleaned by the next write or logout
	}
	return nil
}

// abortWrite removes a failed write's new chunks and, if that worked,
// restores the marker to the generations still orphaned before it.
func abortWrite(profile string, written []string, orphans []pendingGen) {
	for _, u := range written {
		if err := keychainDel(u); err != nil && !errors.Is(err, keyring.ErrNotFound) {
			return // the marker keeps the new generation for the next write or logout
		}
	}
	_ = writePending(profile, orphans)
}

func deleteBestEffort(users []string) {
	for _, u := range users {
		_ = keychainDel(u) // leftovers are unreferenced and never read
	}
}

// deleteV1Chunks removes gmcp1 chunks "<profile>#from", "#from+1", … up to
// the first missing one. from is the old head's chunk count (its chunks
// below that are deleted by the caller), or 1 when no gmcp1 head is known;
// an interrupted gmcp1 write can leave chunks past the count.
func deleteV1Chunks(profile string, from int) error {
	for i := max(from, 1); i < maxChunks; i++ {
		err := keychainDel(v1ChunkUser(profile, i))
		if errors.Is(err, keyring.ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// keychainDelete removes item 0, the chunks it points at, any gmcp1 chunks,
// and the chunks of every generation recorded in the "#pending" marker. A
// generation recorded less than pendingGrace ago may be another writer's in
// flight: it and the marker are left for the next save or logout.
func keychainDelete(profile string) error {
	head, err := keychainGet(profile)
	if err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return err
	}
	var errs []error
	v1From := 1
	if err == nil {
		if cs, _, perr := parseHead(profile, head); perr == nil {
			for _, u := range cs.users {
				if err := keychainDel(u); err != nil && !errors.Is(err, keyring.ErrNotFound) {
					errs = append(errs, err)
				}
			}
			if cs.v1 {
				v1From = len(cs.users) + 1
			}
		}
		if err := keychainDel(profile); err != nil && !errors.Is(err, keyring.ErrNotFound) {
			errs = append(errs, err)
		}
	}
	recorded, err := readPending(profile)
	switch {
	case err != nil:
		errs = append(errs, err)
	case recorded != nil:
		left, skipped, derr := deleteGens(profile, recorded, "")
		if derr != nil {
			errs = append(errs, derr)
		}
		if !skipped {
			if err := writePending(profile, left); err != nil {
				errs = append(errs, err)
			}
		}
	}
	// gmcp1 chunks may survive a lost item 0, or lie past the head's count.
	if err := deleteV1Chunks(profile, v1From); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
