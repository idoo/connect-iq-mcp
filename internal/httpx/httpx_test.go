package httpx

import (
	"net/http"
	"testing"
	"time"
)

func TestRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	for v, want := range map[string]time.Duration{
		"":     0,
		" 7 ":  7 * time.Second,
		"-1":   0,
		"soon": 0,
		now.Add(90 * time.Second).Format(http.TimeFormat):  90 * time.Second,
		now.Add(-90 * time.Second).Format(http.TimeFormat): 0,
	} {
		if got := RetryAfter(v, now); got != want {
			t.Errorf("RetryAfter(%q) = %v, want %v", v, got, want)
		}
	}
}
