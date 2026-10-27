package credstore

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zalando/go-keyring"
)

// fakeKeyring is an in-memory keychain enforcing the Windows per-item limit.
type fakeKeyring struct {
	mu    sync.Mutex
	items map[string]string
	err   error         // returned by every call when set
	block chan struct{} // when set, calls wait on it (a locked keyring prompt)
}

func useFakeKeyring(t *testing.T) *fakeKeyring {
	t.Helper()
	k := &fakeKeyring{items: map[string]string{}}
	get, set, del, timeout := keyringGet, keyringSet, keyringDel, keyringTimeout
	t.Cleanup(func() { keyringGet, keyringSet, keyringDel, keyringTimeout = get, set, del, timeout })
	keyringGet = func(_, user string) (string, error) {
		if err := k.wait(); err != nil {
			return "", err
		}
		k.mu.Lock()
		defer k.mu.Unlock()
		v, ok := k.items[user]
		if !ok {
			return "", keyring.ErrNotFound
		}
		return v, nil
	}
	keyringSet = func(_, user, v string) error {
		if err := k.wait(); err != nil {
			return err
		}
		if len(v) > 2560 {
			return keyring.ErrSetDataTooBig
		}
		k.mu.Lock()
		defer k.mu.Unlock()
		k.items[user] = v
		return nil
	}
	keyringDel = func(_, user string) error {
		if err := k.wait(); err != nil {
			return err
		}
		k.mu.Lock()
		defer k.mu.Unlock()
		if _, ok := k.items[user]; !ok {
			return keyring.ErrNotFound
		}
		delete(k.items, user)
		return nil
	}
	return k
}

func (k *fakeKeyring) wait() error {
	k.mu.Lock()
	block, err := k.block, k.err
	k.mu.Unlock()
	if block != nil {
		<-block
	}
	return err
}

func (k *fakeKeyring) set(err error, block chan struct{}) {
	k.mu.Lock()
	k.err, k.block = err, block
	k.mu.Unlock()
}

func (k *fakeKeyring) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.items)
}

func bigCookies(n int) string {
	return cookies(time.Now().Add(30*24*time.Hour)) + "; big=" + strings.Repeat("x", n)
}

// A Fe26 portal session plus a Connect token exceeds one Windows credential
// (2560 bytes): it must still land in the keychain, split into chunks.
func TestKeychainChunkRoundTrip(t *testing.T) {
	k := useFakeKeyring(t)
	s := testStore(t, nil)
	big := bigCookies(7000) + "; uni=" + strings.Repeat("ж", 1500) // multi-byte runes across chunk edges
	if err := s.writeStored(blob{Portal: &portalCred{Cookies: big}, Connect: &connectCred{RefreshToken: "rt"}}); err != nil {
		t.Fatal(err)
	}
	if k.count() < 4 {
		t.Fatalf("items = %d, want chunked", k.count())
	}
	b, src, err := s.readStored()
	if err != nil || src != srcKeychain || b.Portal.Cookies != big || b.Connect.RefreshToken != "rt" {
		t.Fatalf("read = %v %q %v", b.Portal != nil, src, err)
	}
	// A shorter blob removes the stale chunks.
	if err := s.writeStored(blob{Portal: &portalCred{Cookies: "session=S"}}); err != nil {
		t.Fatal(err)
	}
	if k.count() != 1 {
		t.Fatalf("items after shrink = %d, want 1", k.count())
	}
	if err := s.deleteAll(); err != nil || k.count() != 0 {
		t.Fatalf("deleteAll: %v, items %d", err, k.count())
	}
}

func TestKeychainReadsLegacySingleItem(t *testing.T) {
	k := useFakeKeyring(t)
	k.items["test"] = `{"portal":{"cookies":"session=OLD"}}`
	s := testStore(t, nil)
	b, src, err := s.readStored()
	if err != nil || src != srcKeychain || b.Portal.Cookies != "session=OLD" {
		t.Fatalf("read = %+v %q %v", b.Portal, src, err)
	}
}

// A3/X3: a locked or denied keychain must not silently downgrade to a
// plaintext file; only an unavailable keychain may.
func TestKeychainFallbackRules(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		env      string
		wantFile bool
		reason   string
	}{
		{"unsupported platform", keyring.ErrUnsupportedPlatform, "", true, "keychain unavailable"},
		{"no dbus", errors.New("failed to open dbus connection: DBUS_SESSION_BUS_ADDRESS not set"), "", true, "keychain unavailable"},
		{"env off", nil, "off", true, "GARMIN_MCP_KEYRING=off"},
		{"locked keychain", errors.New("User interaction is not allowed."), "", false, ""}, //nolint:revive // verbatim macOS keychain message
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := useFakeKeyring(t)
			k.set(c.err, nil)
			s := testStore(t, map[string]string{"GARMIN_MCP_KEYRING": c.env})
			werr := s.writeStored(blob{Portal: &portalCred{Cookies: "session=S"}})
			_, src, rerr := s.readStored()
			if !c.wantFile {
				if werr == nil || rerr == nil {
					t.Fatalf("locked keychain must surface an error: write %v, read %v", werr, rerr)
				}
				if _, ok, _ := s.readFile(); ok {
					t.Fatal("credentials written to file despite a keychain error")
				}
				return
			}
			if werr != nil || rerr != nil {
				t.Fatalf("write %v, read %v", werr, rerr)
			}
			if !strings.HasPrefix(src, srcFile+" ("+c.reason) {
				t.Fatalf("source = %q", src)
			}
			if c.env == "off" && k.count() != 0 {
				t.Fatal("keychain used despite GARMIN_MCP_KEYRING=off")
			}
		})
	}
}

// X2/LD-2: a keyring waiting on an invisible unlock prompt must not hang
// callers, and must not downgrade to the plaintext file either.
func TestKeychainTimeoutIsLocked(t *testing.T) {
	k := useFakeKeyring(t)
	keyringTimeout = 50 * time.Millisecond
	release := make(chan struct{})
	k.set(nil, release)
	s := testStore(t, nil)
	start := time.Now()
	if err := s.writeStored(blob{Portal: &portalCred{Cookies: "session=S"}}); !errors.Is(err, errKeyringLocked) {
		t.Fatalf("write = %v, want locked", err)
	}
	if _, ok, _ := s.readFile(); ok {
		t.Fatal("credentials written to file while the keychain was locked")
	}
	// The stuck call keeps its slot: later calls fail fast, not after 5 s each.
	if _, _, err := s.readStored(); !errors.Is(err, errKeyringLocked) || !strings.Contains(err.Error(), "still waiting") {
		t.Fatalf("read = %v", err)
	}
	if st := s.Status(context.Background()); UnreadableState(st.Portal) != "locked" {
		t.Fatalf("status note = %q", st.Portal.Note)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("took %v", d)
	}
	k.set(nil, nil)
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for keyringStuck.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if keyringStuck.Load() {
		t.Fatal("keychain still marked stuck after the call returned")
	}
}

// X3: the newer of the keychain and file copies wins, by saved_at.
func TestNewestCopyWins(t *testing.T) {
	useFakeKeyring(t)
	env := map[string]string{}
	s := testStore(t, env)
	t0 := time.Now()
	s.now = func() time.Time { return t0 }
	if err := s.writeStored(blob{Portal: &portalCred{Cookies: "session=KEYCHAIN"}}); err != nil {
		t.Fatal(err)
	}
	env["GARMIN_MCP_KEYRING"] = "off" // keychain went away; a newer save lands in the file
	s.now = func() time.Time { return t0.Add(time.Minute) }
	if err := s.writeStored(blob{Portal: &portalCred{Cookies: "session=FILE"}}); err != nil {
		t.Fatal(err)
	}
	delete(env, "GARMIN_MCP_KEYRING") // keychain is back with its stale copy
	b, src, err := s.readStored()
	if err != nil || b.Portal.Cookies != "session=FILE" || src != srcFile+" (newer than the keychain copy)" {
		t.Fatalf("read = %+v %q %v", b.Portal, src, err)
	}
	// The next save goes to the keychain and drops the file.
	s.now = func() time.Time { return t0.Add(2 * time.Minute) }
	if err := s.writeStored(blob{Portal: &portalCred{Cookies: "session=NEW"}}); err != nil {
		t.Fatal(err)
	}
	b, src, err = s.readStored()
	if err != nil || b.Portal.Cookies != "session=NEW" || src != srcKeychain {
		t.Fatalf("read = %+v %q %v", b.Portal, src, err)
	}
	if _, ok, _ := s.readFile(); ok {
		t.Fatal("stale file kept")
	}
}

// A4: a slow request's Set-Cookie must not overwrite a session another
// process renewed while the request was in flight.
func TestSavePortalCookiesKeepsConcurrentRenewal(t *testing.T) {
	useFakeKeyring(t)
	ctx := context.Background()
	old := cookies(time.Now().Add(2 * 24 * time.Hour))
	fresh := cookies(time.Now().Add(180 * 24 * time.Hour))
	a, b := testStore(t, nil), testStore(t, nil)
	b.dir = a.dir
	if err := a.savePortal(&portalCred{Cookies: old}); err != nil {
		t.Fatal(err)
	}
	sent := old                                                       // server A starts a slow upload with C_old
	if err := b.savePortal(&portalCred{Cookies: fresh}); err != nil { // server B renews
		t.Fatal(err)
	}
	if err := a.SavePortalCookies(ctx, sent, []string{"session=" + seal(time.Now().Add(200*24*time.Hour)) + "; Path=/"}); err != nil {
		t.Fatal(err)
	}
	got, _, _, _ := a.view()
	if got.Portal.Cookies != fresh {
		t.Fatalf("renewed session clobbered: %q", got.Portal.Cookies)
	}
	// A cookie other than `session` is not stored.
	if err := a.SavePortalCookies(ctx, fresh, []string{"CIQ_DI_session=D2; Path=/"}); err != nil {
		t.Fatal(err)
	}
	if got, _, _, _ = a.view(); got.Portal.Cookies != fresh {
		t.Fatalf("non-session cookie stored: %q", got.Portal.Cookies)
	}
	// Still derived from the request's cookie: the rotation is applied.
	rotated := cookies(time.Now().Add(200 * 24 * time.Hour))
	if err := a.SavePortalCookies(ctx, fresh, []string{rotated + "; Path=/"}); err != nil {
		t.Fatal(err)
	}
	got, _, _, _ = a.view()
	if got.Portal.Cookies != rotated {
		t.Fatalf("rotation not applied: %q", got.Portal.Cookies)
	}
	// Signed out meanwhile: nothing is written back.
	if err := a.deleteAll(); err != nil {
		t.Fatal(err)
	}
	if err := a.SavePortalCookies(ctx, got.Portal.Cookies, []string{"x=1"}); err != nil {
		t.Fatal(err)
	}
	if got, _, _, _ = a.view(); got.Portal != nil {
		t.Fatal("rotation resurrected a signed-out session")
	}
}
