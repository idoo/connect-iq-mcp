package state

import (
	"os"
	"time"
)

// renameRetryFor bounds Rename's retries.
const renameRetryFor = 2 * time.Second

// Rename is os.Rename that retries for about 2 s while Windows reports the
// target busy (access denied, sharing or lock violation): another process
// holds it open, or an antivirus scanner is reading a fresh file. Off
// Windows it is a single os.Rename.
func Rename(oldpath, newpath string) error {
	return retryWhile(func() error { return os.Rename(oldpath, newpath) }, isBusy, renameRetryFor, 50*time.Millisecond)
}

// retryWhile runs fn until it succeeds, fails with an error retry rejects,
// or total has elapsed, sleeping step between attempts.
func retryWhile(fn func() error, retry func(error) bool, total, step time.Duration) error {
	end := time.Now().Add(total)
	for {
		err := fn()
		if err == nil || !retry(err) || time.Now().After(end) {
			return err
		}
		time.Sleep(step)
	}
}
