// Package ciqsdktest provides a Connect IQ developer key for tests without
// paying for a 4096-bit RSA generation in every test.
package ciqsdktest

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/idoo/garmin-mcp/internal/ciqsdk"
)

var (
	once   sync.Once
	keyDER []byte
	keyErr error
)

// Key writes this test binary's developer key (a real ciqsdk.GenerateKey
// key, generated once per process) to path with mode 0600 and returns its
// info. Every call returns the same key bytes.
func Key(t testing.TB, path string) ciqsdk.KeyInfo {
	t.Helper()
	once.Do(func() {
		dir, err := os.MkdirTemp("", "ciqsdktest-")
		if err != nil {
			keyErr = err
			return
		}
		defer func() { _ = os.RemoveAll(dir) }()
		p := filepath.Join(dir, "developer_key.der")
		if _, keyErr = ciqsdk.GenerateKey(p); keyErr == nil {
			keyDER, keyErr = os.ReadFile(p) //nolint:gosec // G304: our own temp file
		}
	})
	if keyErr != nil {
		t.Fatalf("test key: %v", keyErr)
	}
	if err := os.WriteFile(path, keyDER, 0o600); err != nil {
		t.Fatal(err)
	}
	ki, err := ciqsdk.CheckKey(path)
	if err != nil {
		t.Fatal(err)
	}
	return ki
}
