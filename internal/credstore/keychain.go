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
// then item 0 pointing at them, then (best effort) the previous generation's
// chunks are deleted. A crash or timeout before item 0 leaves the old blob
// intact and readable.
func keychainWrite(profile, data string) error {
	chunks := splitChunks(data)
	if len(chunks) > maxChunks {
		return fmt.Errorf("credentials too large for the keychain (%d bytes)", len(data))
	}
	var stale []string
	staleV1 := false
	head, err := keychainGet(profile)
	switch {
	case errors.Is(err, keyring.ErrNotFound):
	case err != nil:
		return err
	default:
		// A damaged old head still gets replaced; its chunks are just unknown.
		if cs, _, perr := parseHead(profile, head); perr == nil {
			stale, staleV1 = cs.users, cs.v1
		}
	}
	gen, err := newGen()
	if err != nil {
		return fmt.Errorf("keychain generation: %w", err)
	}
	written := make([]string, 0, len(chunks)-1)
	for i := 1; i < len(chunks); i++ {
		user, c := genChunkUser(profile, gen, i), chunks[i]
		if err := keyringCall(func() error { return keyringSet(keyringService, user, c) }); err != nil {
			deleteBestEffort(written)
			return err
		}
		written = append(written, user)
	}
	newHead := chunkPrefix + gen + ":" + strconv.Itoa(len(chunks)) + ":" + digest(data) + ":" + chunks[0]
	if err := keyringCall(func() error { return keyringSet(keyringService, profile, newHead) }); err != nil {
		deleteBestEffort(written)
		return err
	}
	deleteBestEffort(stale)
	if staleV1 {
		deleteV1Chunks(profile) //nolint:errcheck // best effort: the new generation is already in place
	}
	return nil
}

func deleteBestEffort(users []string) {
	for _, u := range users {
		_ = keychainDel(u) // leftovers are unreferenced and never read
	}
}

// deleteV1Chunks removes gmcp1 chunks "<profile>#1", "#2", … up to the first
// missing one.
func deleteV1Chunks(profile string) error {
	for i := 1; i < maxChunks; i++ {
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

// keychainDelete removes item 0, the chunks it points at, and any gmcp1
// chunks.
func keychainDelete(profile string) error {
	head, err := keychainGet(profile)
	if err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return err
	}
	var errs []error
	if err == nil {
		if cs, _, perr := parseHead(profile, head); perr == nil {
			for _, u := range cs.users {
				if err := keychainDel(u); err != nil && !errors.Is(err, keyring.ErrNotFound) {
					errs = append(errs, err)
				}
			}
		}
		if err := keychainDel(profile); err != nil && !errors.Is(err, keyring.ErrNotFound) {
			errs = append(errs, err)
		}
	}
	// gmcp1 chunks may survive a lost item 0.
	if err := deleteV1Chunks(profile); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}
