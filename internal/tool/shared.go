package tool

import (
	"reflect"
	"sync"
)

// Shared holds the state one server keeps across calls and sessions: the
// store app cache, the sdk build jobs, the simulator sessions. Tool packages
// own their state types and reach this server's single value of each through
// [SharedValue]; none of it lives in package-level variables. The server
// makes one per process; tooltest.New makes a fresh one per Kit.
type Shared struct {
	mu   sync.Mutex
	vals map[reflect.Type]any
}

// NewShared returns an empty Shared.
func NewShared() *Shared { return &Shared{vals: map[reflect.Type]any{}} }

// SharedValue returns s's value of type T, making it with mk on first use.
func SharedValue[T any](s *Shared, mk func() T) T {
	k := reflect.TypeFor[T]()
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.vals[k]; ok {
		return v.(T)
	}
	v := mk()
	s.vals[k] = v
	return v
}
