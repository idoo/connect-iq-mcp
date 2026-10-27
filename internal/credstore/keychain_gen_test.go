package credstore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

func bigBlob(t *testing.T, s *Store, cookie string) string {
	t.Helper()
	if err := s.writeStored(blob{Portal: &portalCred{Cookies: cookie}}); err != nil {
		t.Fatal(err)
	}
	return cookie
}

// LC-1: a write that stored the new generation's chunks but never switched
// item 0 (crash, timeout) leaves the old blob readable.
func TestKeychainInterruptedWriteKeepsOldBlob(t *testing.T) {
	k := useFakeKeyring(t)
	s := testStore(t, nil)
	old := bigBlob(t, s, bigCookies(5000))
	before := k.count()

	// Crash after the chunks: a foreign generation lingers next to item 0.
	k.items["test#deadbeef.1"] = "garbage"
	k.items["test#deadbeef.2"] = "more garbage"
	if b, _, err := s.readStored(); err != nil || b.Portal.Cookies != old {
		t.Fatalf("after crash: %v", err)
	}
	delete(k.items, "test#deadbeef.1")
	delete(k.items, "test#deadbeef.2")

	// Item 0 write fails (timeout/denied): old blob stays, new chunks go.
	set := keyringSet
	keyringSet = func(svc, user, v string) error {
		if user == "test" {
			return errors.New("access denied")
		}
		return set(svc, user, v)
	}
	if err := s.writeStored(blob{Portal: &portalCred{Cookies: bigCookies(6000)}}); err == nil {
		t.Fatal("write succeeded despite item 0 failure")
	}
	keyringSet = set
	if b, _, err := s.readStored(); err != nil || b.Portal.Cookies != old {
		t.Fatalf("after failed write: %v", err)
	}
	if k.count() != before {
		t.Fatalf("items = %d, want %d (new chunks cleaned up)", k.count(), before)
	}
}

// r4: a crash or timeout after the new chunks but before item 0 leaves
// orphans; the "#pending" marker lets the next save and logout delete them.
func TestKeychainOrphanChunksRecovered(t *testing.T) {
	k := useFakeKeyring(t)
	s := testStore(t, nil)
	clk := markerClock(t)
	bigBlob(t, s, bigCookies(5000))
	if _, ok := k.items["test#pending"]; ok {
		t.Fatal("marker left after a clean save")
	}

	// "Crash": item 0 fails and the cleanup of the new chunks fails too.
	set, del := keyringSet, keyringDel
	keyringSet = func(svc, user, v string) error {
		if user == "test" {
			return errors.New("access denied")
		}
		return set(svc, user, v)
	}
	keyringDel = func(svc, user string) error {
		if strings.HasPrefix(user, "test#") && user != "test#pending" {
			return errors.New("access denied")
		}
		return del(svc, user)
	}
	if err := s.writeStored(blob{Portal: &portalCred{Cookies: bigCookies(6000)}}); err == nil {
		t.Fatal("write succeeded despite item 0 failure")
	}
	keyringSet, keyringDel = set, del
	head, _ := keyringGet(keyringService, "test")
	cur, _, _ := parseHead("test", head)
	var orphans []string
	for u := range k.items {
		if strings.HasPrefix(u, "test#") && u != "test#pending" && !strings.HasPrefix(u, "test#"+cur.gen+".") {
			orphans = append(orphans, u)
		}
	}
	if len(orphans) == 0 || k.items["test#pending"] == "" {
		t.Fatalf("crash left no orphans/marker: %v", k.items)
	}

	clk.add(pendingGrace + time.Second) // RC-2: young entries may be a peer's
	want := bigCookies(7000)
	bigBlob(t, s, want)
	head, _ = keyringGet(keyringService, "test")
	cur, _, _ = parseHead("test", head)
	for u := range k.items {
		if u != "test" && !strings.HasPrefix(u, "test#"+cur.gen+".") {
			t.Errorf("after the next save, %q remains", u)
		}
	}
	if b, _, err := s.readStored(); err != nil || b.Portal.Cookies != want {
		t.Fatalf("read after recovery: %v", err)
	}

	// A second crash, then logout: nothing remains.
	keyringSet = func(svc, user, v string) error {
		if user == "test" {
			return errors.New("access denied")
		}
		return set(svc, user, v)
	}
	keyringDel = func(svc, user string) error {
		if strings.HasPrefix(user, "test#") && user != "test#pending" {
			return errors.New("access denied")
		}
		return del(svc, user)
	}
	_ = s.writeStored(blob{Portal: &portalCred{Cookies: bigCookies(6500)}})
	keyringSet, keyringDel = set, del
	clk.add(pendingGrace + time.Second)
	if err := s.deleteAll(); err != nil {
		t.Fatal(err)
	}
	if k.count() != 0 {
		t.Fatalf("after logout: %v", k.items)
	}
}

// markerClock drives the "#pending" marker's clock.
func markerClock(t *testing.T) *fakeClock {
	t.Helper()
	clk := &fakeClock{t: time.Now()}
	old := keyringNow
	keyringNow = clk.now
	t.Cleanup(func() { keyringNow = old })
	return clk
}

// RC-1: gmcp1 chunks past the head's count (an interrupted gmcp1 write) go
// on migration and on logout.
func TestKeychainV1ChunksPastCountRemoved(t *testing.T) {
	seed := func(k *fakeKeyring) {
		d := `{"portal":{"cookies":"session=OLD` + strings.Repeat("x", 100) + `"}}`
		k.items["test"] = chunkPrefixV1 + "2:" + digest(d) + ":" + d[:30]
		k.items["test#1"] = d[30:]
		k.items["test#2"] = "STALE-1"
		k.items["test#3"] = "STALE-2"
	}
	k := useFakeKeyring(t)
	s := testStore(t, nil)
	seed(k)
	if err := s.writeStored(blob{Portal: &portalCred{Cookies: "session=NEW"}}); err != nil {
		t.Fatal(err)
	}
	if k.count() != 1 {
		t.Fatalf("after migration: %v", k.items)
	}
	if err := s.deleteAll(); err != nil || k.count() != 0 {
		t.Fatalf("logout: %v, %v", err, k.items)
	}
	seed(k)
	if err := s.deleteAll(); err != nil || k.count() != 0 {
		t.Fatalf("logout of a gmcp1 blob: %v, %v", err, k.items)
	}
}

// RC-2: a generation recorded less than 30 s ago may be another writer's in
// flight (one that does not share our lock): save and logout leave it, and
// its item 0 switch still reads; later cleanup removes it.
func TestPendingGraceSparesPeerWrite(t *testing.T) {
	k := useFakeKeyring(t)
	s := testStore(t, nil)
	clk := markerClock(t)
	bigBlob(t, s, bigCookies(5000))
	peer := func() (string, []string) {
		pd := `{"portal":{"cookies":"` + bigCookies(4500) + `"}}`
		pc := splitChunks(pd)
		k.items["test#pending"] = "deadbeef:" + strconv.Itoa(len(pc)) + ":" + strconv.FormatInt(clk.now().Unix(), 10)
		for i := 1; i < len(pc); i++ {
			k.items[genChunkUser("test", "deadbeef", i)] = pc[i]
		}
		return pd, pc
	}
	pd, pc := peer()
	bigBlob(t, s, bigCookies(5200)) // this process saves meanwhile
	if !strings.Contains(k.items["test#pending"], "deadbeef:") {
		t.Fatalf("young peer entry dropped: %q", k.items["test#pending"])
	}
	k.items["test"] = chunkPrefix + "deadbeef:" + strconv.Itoa(len(pc)) + ":" + digest(pd) + ":" + pc[0]
	if _, _, err := s.readStored(); err != nil {
		t.Fatalf("peer's committed blob: %v", err)
	}
	clk.add(pendingGrace + time.Second)
	bigBlob(t, s, bigCookies(5100)) // old enough: item 0 owns deadbeef now, then replaced
	if _, ok := k.items[genChunkUser("test", "deadbeef", 1)]; ok {
		t.Fatalf("replaced generation kept: %v", k.items)
	}
	if _, ok := k.items["test#pending"]; ok {
		t.Fatalf("marker left: %q", k.items["test#pending"])
	}

	// Logout with a young entry keeps it and the marker; later logout clears
	// all. Start clean: the simulated peer above replaced item 0 without
	// knowing our 5200 generation, which no record covers.
	clear(k.items)
	bigBlob(t, s, bigCookies(5000))
	peer()
	if err := s.deleteAll(); err != nil {
		t.Fatal(err)
	}
	if _, ok := k.items["test#pending"]; !ok || k.items[genChunkUser("test", "deadbeef", 1)] == "" {
		t.Fatalf("logout removed a young generation: %v", k.items)
	}
	clk.add(pendingGrace + time.Second)
	if err := s.deleteAll(); err != nil || k.count() != 0 {
		t.Fatalf("later logout: %v, %v", err, k.items)
	}
}

// RC-4: an item 0 Set that reports failure but landed (a late timeout)
// keeps the new chunks; a failed read-back keeps them too.
func TestItem0LateLandingKeepsNewChunks(t *testing.T) {
	k := useFakeKeyring(t)
	s := testStore(t, nil)
	bigBlob(t, s, bigCookies(5000))
	set, get := keyringSet, keyringGet
	keyringSet = func(svc, user, v string) error {
		if user == "test" {
			_ = set(svc, user, v)
			return errors.New("timed out")
		}
		return set(svc, user, v)
	}
	want := bigCookies(6000)
	if err := s.writeStored(blob{Portal: &portalCred{Cookies: want}}); err == nil {
		t.Fatal("write reported success")
	}
	keyringSet = set
	if b, _, err := s.readStored(); err != nil || b.Portal.Cookies != want {
		t.Fatalf("landed blob unreadable: %v", err)
	}
	if k.items["test#pending"] == "" {
		t.Fatal("marker dropped")
	}

	// Set fails and the read-back fails: nothing is deleted.
	before := k.count()
	keyringSet = func(svc, user, v string) error {
		if user == "test" {
			return errors.New("timed out")
		}
		return set(svc, user, v)
	}
	heads := 0
	keyringGet = func(svc, user string) (string, error) {
		if user == "test" {
			if heads++; heads == 2 {
				return "", errors.New("timed out")
			}
		}
		return get(svc, user)
	}
	if err := s.writeStored(blob{Portal: &portalCred{Cookies: bigCookies(6100)}}); err == nil {
		t.Fatal("write reported success")
	}
	keyringSet, keyringGet = set, get
	if k.count() <= before {
		t.Fatalf("new chunks deleted without a read-back: %d <= %d", k.count(), before)
	}
	if b, _, err := s.readStored(); err != nil || b.Portal.Cookies != want {
		t.Fatalf("old blob: %v", err)
	}
}

// LC-7: a mismatching chunk set (another process mid-write) is read again
// once before it is reported unreadable.
func TestKeychainMismatchRetriedOnce(t *testing.T) {
	k := useFakeKeyring(t)
	s := testStore(t, nil)
	want := bigBlob(t, s, bigCookies(5000))
	good := k.items["test"]
	parts := strings.SplitN(good, ":", 5)
	parts[3] = "0000000000000000" // checksum of a set still being written
	k.items["test"] = strings.Join(parts, ":")

	heads := 0
	get := keyringGet
	keyringGet = func(svc, user string) (string, error) {
		if user == "test" {
			heads++
			if heads == 2 {
				k.mu.Lock()
				k.items["test"] = good // the writer finished
				k.mu.Unlock()
			}
		}
		return get(svc, user)
	}
	b, _, err := s.readStored()
	if err != nil || b.Portal.Cookies != want || heads != 2 {
		t.Fatalf("read = %v, heads %d", err, heads)
	}

	k.items["test#"+parts[1]+".1"] = "tampered"
	heads = 0
	keyringGet = func(svc, user string) (string, error) {
		if user == "test" {
			heads++
		}
		return get(svc, user)
	}
	if _, _, err := s.readStored(); !errors.Is(err, errStoredUnreadable) || heads != 2 {
		t.Fatalf("read = %v, heads %d", err, heads)
	}
	if st := s.Status(context.Background()); UnreadableState(st.Portal) != "unreadable" {
		t.Fatalf("status note = %q", st.Portal.Note)
	}
}

// LC-1: a save over an unreadable blob replaces it (with a warning) instead
// of failing with "nothing was saved".
func TestSaveReplacesUnreadableBlob(t *testing.T) {
	for name, seed := range map[string]map[string]string{
		"missing chunk": {"test": "gmcp2:deadbeef:2:0000000000000000:{"},
		"bad json":      {"test": "{not json"},
	} {
		t.Run(name, func(t *testing.T) {
			k := useFakeKeyring(t)
			for u, v := range seed {
				k.items[u] = v
			}
			s := testStore(t, nil)
			if err := s.savePortal(&portalCred{Cookies: "session=NEW"}); err != nil {
				t.Fatalf("save: %v", err)
			}
			if w := s.takeSaveWarnings(); len(w) != 1 || !strings.Contains(w[0], "unreadable") {
				t.Fatalf("warnings = %q", w)
			}
			if b, _, err := s.readStored(); err != nil || b.Portal.Cookies != "session=NEW" {
				t.Fatalf("read = %+v, %v", b.Portal, err)
			}
		})
	}
}

// A locked keychain is not "unreadable": saves fail instead of overwriting.
func TestSaveLockedKeychainFails(t *testing.T) {
	k := useFakeKeyring(t)
	k.set(errors.New("failed to unlock correct collection '/org/freedesktop/secrets/collection/login'"), nil)
	s := testStore(t, nil)
	if err := s.savePortal(&portalCred{Cookies: "session=NEW"}); !errors.Is(err, errKeyringLocked) {
		t.Fatalf("save = %v, want locked", err)
	}
	if _, ok, _ := s.readFile(); ok {
		t.Fatal("locked keychain fell back to the file")
	}
	var out bytes.Buffer
	if err := s.cliStatus(context.Background(), &out, false); err == nil || !strings.Contains(out.String(), "locked") {
		t.Fatalf("auth status = %v\n%s", err, out.String())
	}
}

// LD-2..LD-5, WD-9: each backend error lands in the right class.
func TestClassifyKeyring(t *testing.T) {
	const unavailable, locked, asIs = "unavailable", "locked", "as is"
	cases := []struct {
		err  error
		want string
	}{
		{keyring.ErrUnsupportedPlatform, unavailable},
		{&net.OpError{Op: "dial", Net: "unix", Err: syscall.ENOENT}, unavailable},
		{fmt.Errorf("dial: %w", syscall.ECONNREFUSED), unavailable},
		{errors.New("dial unix /nonexistent: connect: no such file or directory"), unavailable},
		{errors.New("failed to open dbus connection"), unavailable},
		{syscall.Errno(1312), unavailable},
		{errors.New("A specified logon session does not exist. It may already have been terminated."), unavailable}, //nolint:revive // verbatim Windows message
		{errors.New("failed to unlock correct collection '/org/freedesktop/secrets/collection/login'"), locked},
		{errors.New("User interaction is not allowed."), locked}, //nolint:revive // verbatim macOS message
		{errors.New("access denied"), asIs},
		{keyring.ErrSetDataTooBig, asIs},
		{keyring.ErrNotFound, asIs},
	}
	for _, c := range cases {
		got := classifyKeyring(c.err)
		class := asIs
		switch {
		case errors.Is(got, errKeyringUnavailable):
			class = unavailable
		case errors.Is(got, errKeyringLocked):
			class = locked
		}
		if class != c.want || !errors.Is(got, c.err) && class == asIs {
			t.Errorf("%v: class %s, want %s", c.err, class, c.want)
		}
	}
}

// LD-4: with no D-Bus session the keychain is not called at all (no
// dbus-launch), and credentials go to the file.
func TestNoSessionBusSkipsKeychain(t *testing.T) {
	k := useFakeKeyring(t)
	s := testStore(t, nil)
	keyringPrecheck = func() error { return fmt.Errorf("%w: no D-Bus session", errKeyringUnavailable) }
	calls := 0
	get := keyringGet
	keyringGet = func(svc, user string) (string, error) { calls++; return get(svc, user) }
	if err := s.writeStored(blob{Portal: &portalCred{Cookies: "session=S"}}); err != nil {
		t.Fatal(err)
	}
	if _, src, err := s.readStored(); err != nil || !strings.Contains(src, "no D-Bus session") {
		t.Fatalf("read = %q, %v", src, err)
	}
	if calls != 0 || k.count() != 0 {
		t.Fatalf("keychain called %d times, %d items", calls, k.count())
	}
}

func TestNoSessionBus(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	err := noSessionBus()
	switch runtime.GOOS {
	case "darwin", "windows":
		if err != nil {
			t.Fatal(err)
		}
	default:
		if _, statErr := os.Stat("/run/user/" + strconv.Itoa(os.Getuid()) + "/bus"); statErr == nil {
			t.Skip("a session bus socket exists")
		}
		if !errors.Is(err, errKeyringUnavailable) {
			t.Fatalf("err = %v", err)
		}
	}
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/1/bus")
	if err := noSessionBus(); err != nil {
		t.Fatal(err)
	}
}

// gmcp1 blobs from older versions still read, and the next save removes
// their chunks.
func TestKeychainReadsV1Chunks(t *testing.T) {
	k := useFakeKeyring(t)
	raw, _ := json.Marshal(blob{Portal: &portalCred{Cookies: bigCookies(3000)}})
	chunks := splitChunks(string(raw))
	if len(chunks) != 2 {
		t.Fatalf("chunks = %d", len(chunks))
	}
	k.items["test"] = chunkPrefixV1 + "2:" + digest(string(raw)) + ":" + chunks[0]
	k.items["test#1"] = chunks[1]
	s := testStore(t, nil)
	b, _, err := s.readStored()
	if err != nil || b.Portal == nil {
		t.Fatalf("read v1: %v", err)
	}
	if err := s.writeStored(blob{Portal: &portalCred{Cookies: "session=S"}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := k.items["test#1"]; ok || k.count() != 1 {
		t.Fatalf("v1 chunk survived: %v", k.items)
	}
}

// LC-10: logout clears the keychain even with GARMIN_MCP_KEYRING=off;
// unavailable is ignored, other errors are reported.
func TestLogoutAlwaysClearsKeychain(t *testing.T) {
	k := useFakeKeyring(t)
	env := map[string]string{}
	s := testStore(t, env)
	bigBlob(t, s, bigCookies(5000))
	env["GARMIN_MCP_KEYRING"] = "off"
	if err := s.deleteAll(); err != nil || k.count() != 0 {
		t.Fatalf("deleteAll = %v, items %d", err, k.count())
	}
	k.set(keyring.ErrUnsupportedPlatform, nil)
	if err := s.deleteAll(); err != nil {
		t.Fatalf("unavailable: %v", err)
	}
	k.set(errors.New("access denied"), nil)
	if err := s.deleteAll(); err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("denied: %v", err)
	}
}

// LC-8: the constructor refuses names that would escape or collide.
func TestNewStoreRejectsBadProfileNames(t *testing.T) {
	for _, name := range []string{"", "..", ".", "a#1", "Work", "a/b", "-x", strings.Repeat("a", 64)} {
		if _, err := newStore(name, t.TempDir(), true); err == nil {
			t.Errorf("newStore(%q) accepted", name)
		}
	}
	if _, err := newStore("work-1_x", t.TempDir(), true); err != nil {
		t.Fatal(err)
	}
}
