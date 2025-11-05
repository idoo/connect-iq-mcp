package tool

import (
	"context"
	"sync"
	"time"
)

// Lifetime scopes work that outlives the tool call that started it (sdk
// builds) to the server: its context is cancelled when the server stops, and
// [Lifetime.Close] waits for that work to clean up (kill its processes,
// remove temp dirs). A nil *Lifetime is valid: work runs under
// context.Background and is never waited for (tests).
type Lifetime struct {
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

// NewLifetime returns a Lifetime that ends when parent does or on Close.
func NewLifetime(parent context.Context) *Lifetime {
	ctx, cancel := context.WithCancel(parent)
	return &Lifetime{ctx: ctx, cancel: cancel}
}

// Go runs fn in a goroutine with the server-lifetime context. After Close,
// fn still runs (so it can report its result) but with a cancelled context.
func (l *Lifetime) Go(fn func(ctx context.Context)) {
	if l == nil {
		go fn(context.Background())
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		go fn(l.ctx)
		return
	}
	l.wg.Add(1)
	go func() {
		defer l.wg.Done()
		fn(l.ctx)
	}()
}

// Close cancels the context and waits up to wait for running work to return.
func (l *Lifetime) Close(wait time.Duration) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	l.cancel()
	done := make(chan struct{})
	go func() { l.wg.Wait(); close(done) }()
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
	}
}
