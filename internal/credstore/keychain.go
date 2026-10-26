package credstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/zalando/go-keyring"
)

const (
	keyringService = "garmin-mcp"
	// keyringChunk keeps every item well under the smallest backend limit
	// (Windows Credential Manager: 2560 bytes per blob).
	keyringChunk = 2000
	// maxChunks bounds reads and stale-chunk cleanup.
	maxChunks = 64
	// chunkPrefix marks item 0 of a chunked blob: "gmcp1:<count>:<sha256 hex>:<data>".
	// An item 0 starting with "{" is a single-item blob from older versions.
	chunkPrefix = "gmcp1:"
)

// errKeyringUnavailable marks a keychain that cannot be used at all (no
// backend, no D-Bus, a call stuck past keyringTimeout, or
// GARMIN_MCP_KEYRING=off). Only then do credentials go to the file.
var errKeyringUnavailable = errors.New("keychain unavailable")

// Keychain calls; tests replace them.
var (
	keyringGet = keyring.Get
	keyringSet = keyring.Set
	keyringDel = keyring.Delete
	// keyringTimeout bounds one keychain call: a locked Secret Service
	// collection waits on a prompt nobody can see from stdio.
	keyringTimeout = 5 * time.Second
)

// keyringSem serializes keychain access within the process (the backends are
// not documented as goroutine-safe). A call stuck past its deadline keeps the
// slot; keyringStuck then fails later calls fast instead of queueing them.
var (
	keyringSem   = make(chan struct{}, 1)
	keyringStuck atomic.Bool
)

// keyringCall runs op with the keychain deadline and classifies its error.
func keyringCall(op func() error) error {
	if keyringStuck.Load() {
		return fmt.Errorf("%w: an earlier keychain request is still waiting (locked keyring prompt?)", errKeyringUnavailable)
	}
	timer := time.NewTimer(keyringTimeout)
	defer timer.Stop()
	select {
	case keyringSem <- struct{}{}:
	case <-timer.C:
		return fmt.Errorf("%w: timed out after %s", errKeyringUnavailable, keyringTimeout)
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
		return fmt.Errorf("%w: no answer within %s (locked keyring waiting for a prompt?)", errKeyringUnavailable, keyringTimeout)
	}
}

// classifyKeyring wraps errors meaning "there is no usable keychain here" in
// errKeyringUnavailable; anything else (locked keychain, denied access, data
// too big) is returned as is so it is never silently downgraded to a file.
func classifyKeyring(err error) error {
	if err == nil || errors.Is(err, keyring.ErrNotFound) {
		return err
	}
	if errors.Is(err, keyring.ErrUnsupportedPlatform) {
		return fmt.Errorf("%w: %w", errKeyringUnavailable, err)
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"dbus", "d-bus", "org.freedesktop.secrets", "secret service"} {
		if strings.Contains(msg, s) {
			return fmt.Errorf("%w: %w", errKeyringUnavailable, err)
		}
	}
	return err
}

func chunkUser(profile string, i int) string {
	if i == 0 {
		return profile
	}
	return profile + "#" + strconv.Itoa(i)
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

// keychainRead returns the stored blob text for profile ("" if none).
func keychainRead(profile string) (string, error) {
	var head string
	err := keyringCall(func() (err error) { head, err = keyringGet(keyringService, profile); return err })
	if errors.Is(err, keyring.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(head, chunkPrefix) {
		return head, nil // single-item blob from older versions
	}
	parts := strings.SplitN(strings.TrimPrefix(head, chunkPrefix), ":", 3)
	if len(parts) != 3 {
		return "", errors.New("credentials in keychain are unreadable: bad chunk header")
	}
	n, err := strconv.Atoi(parts[0])
	if err != nil || n < 1 || n > maxChunks {
		return "", errors.New("credentials in keychain are unreadable: bad chunk count")
	}
	var b strings.Builder
	b.WriteString(parts[2])
	for i := 1; i < n; i++ {
		var c string
		err := keyringCall(func() (err error) { c, err = keyringGet(keyringService, chunkUser(profile, i)); return err })
		if err != nil {
			return "", fmt.Errorf("credentials in keychain are incomplete (chunk %d of %d): %w", i+1, n, err)
		}
		b.WriteString(c)
	}
	data := b.String()
	if digest(data) != parts[1] {
		return "", errors.New("credentials in keychain are unreadable: chunk checksum mismatch")
	}
	return data, nil
}

// keychainWrite stores data in chunks. Item 0 (with the count and checksum)
// is written last, so a reader never sees a header for missing chunks; stale
// chunks from a longer previous blob are deleted afterwards.
func keychainWrite(profile, data string) error {
	chunks := splitChunks(data)
	if len(chunks) > maxChunks {
		return fmt.Errorf("credentials too large for the keychain (%d bytes)", len(data))
	}
	for i := len(chunks) - 1; i >= 1; i-- {
		user, c := chunkUser(profile, i), chunks[i]
		if err := keyringCall(func() error { return keyringSet(keyringService, user, c) }); err != nil {
			return err
		}
	}
	head := chunkPrefix + strconv.Itoa(len(chunks)) + ":" + digest(data) + ":" + chunks[0]
	if err := keyringCall(func() error { return keyringSet(keyringService, profile, head) }); err != nil {
		return err
	}
	for i := len(chunks); i < maxChunks; i++ {
		user := chunkUser(profile, i)
		err := keyringCall(func() error { return keyringDel(keyringService, user) })
		if errors.Is(err, keyring.ErrNotFound) {
			break
		}
		if err != nil {
			return fmt.Errorf("remove stale keychain chunk: %w", err)
		}
	}
	return nil
}

// keychainDelete removes item 0 and every chunk.
func keychainDelete(profile string) error {
	for i := range maxChunks {
		user := chunkUser(profile, i)
		err := keyringCall(func() error { return keyringDel(keyringService, user) })
		if errors.Is(err, keyring.ErrNotFound) {
			if i == 0 {
				continue // chunks may survive a lost item 0
			}
			return nil
		}
		if err != nil {
			return err
		}
	}
	return nil
}
