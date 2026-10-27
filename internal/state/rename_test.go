package state

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// WD-10: a busy error is retried until it clears; any other error returns at once.
func TestRetryWhile(t *testing.T) {
	busy := errors.New("busy")
	n := 0
	err := retryWhile(func() error {
		n++
		if n < 3 {
			return busy
		}
		return nil
	}, func(err error) bool { return errors.Is(err, busy) }, time.Second, time.Millisecond)
	if err != nil || n != 3 {
		t.Fatalf("err=%v attempts=%d, want nil after 3", err, n)
	}

	other := errors.New("other")
	n = 0
	err = retryWhile(func() error { n++; return other }, func(err error) bool { return errors.Is(err, busy) }, time.Second, time.Millisecond)
	if !errors.Is(err, other) || n != 1 {
		t.Fatalf("err=%v attempts=%d, want other after 1", err, n)
	}

	n = 0
	start := time.Now()
	err = retryWhile(func() error { n++; return busy }, func(error) bool { return true }, 20*time.Millisecond, time.Millisecond)
	if !errors.Is(err, busy) || n < 2 || time.Since(start) > time.Second {
		t.Fatalf("err=%v attempts=%d elapsed=%v, want busy after bounded retries", err, n, time.Since(start))
	}
}

func TestRenameReplacesTarget(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	if err := os.WriteFile(src, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Rename(src, dst); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(dst); err != nil || string(b) != "new" {
		t.Fatalf("dst = %q, %v", b, err)
	}
	if err := Rename(src, dst); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing source: %v, want ErrNotExist without retrying", err)
	}
}
