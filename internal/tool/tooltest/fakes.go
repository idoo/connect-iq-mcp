package tooltest

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/idoo/connect-iq-mcp/internal/tool"
)

// FakeStore is an in-memory [tool.StateStore].
type FakeStore struct {
	mu      sync.Mutex
	cursors map[string]string
	records map[string][]byte
}

// NewFakeStore returns an empty FakeStore.
func NewFakeStore() *FakeStore {
	return &FakeStore{cursors: map[string]string{}, records: map[string][]byte{}}
}

// Cursor implements tool.StateStore.
func (s *FakeStore) Cursor(profile, app, name string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cursors[profile+"/"+app+"/"+name], nil
}

// SetCursor implements tool.StateStore.
func (s *FakeStore) SetCursor(profile, app, name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cursors[profile+"/"+app+"/"+name] = value
	return nil
}

// Load implements tool.StateStore.
func (s *FakeStore) Load(kind, key string, v any) (bool, error) {
	s.mu.Lock()
	b, ok := s.records[kind+"/"+key]
	s.mu.Unlock()
	if !ok {
		return false, nil
	}
	if err := json.Unmarshal(b, v); err != nil {
		return true, fmt.Errorf("load %s/%s: %w", kind, key, err)
	}
	return true, nil
}

// Save implements tool.StateStore.
func (s *FakeStore) Save(kind, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("save %s/%s: %w", kind, key, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[kind+"/"+key] = b
	return nil
}

// Delete implements tool.StateStore.
func (s *FakeStore) Delete(kind, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, kind+"/"+key)
	return nil
}

// FakeFiles is a [tool.Files] confined to a temporary directory (Root).
type FakeFiles struct {
	Root string
}

// NewFakeFiles returns FakeFiles rooted at t.TempDir() (symlinks resolved).
func NewFakeFiles(t testing.TB) *FakeFiles {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	return &FakeFiles{Root: root}
}

// Resolve implements tool.Files: relative paths are joined to Root, and the
// resolved path (symlinks resolved on the longest existing prefix) must stay
// inside Root.
func (f *FakeFiles) Resolve(path string) (string, error) {
	p := path
	if !filepath.IsAbs(p) {
		p = filepath.Join(f.Root, p)
	}
	p = filepath.Clean(p)
	existing, rest := p, ""
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		rest = filepath.Join(filepath.Base(existing), rest)
		existing = parent
	}
	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", path, err)
	}
	resolved = filepath.Join(resolved, rest)
	rel, err := filepath.Rel(f.Root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %s is outside the allowed root", path)
	}
	return resolved, nil
}

// ReadFile implements tool.Files.
func (f *FakeFiles) ReadFile(path string) ([]byte, error) {
	p, err := f.Resolve(path)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p) //nolint:gosec // confined by Resolve
}

// WriteFile implements tool.Files.
func (f *FakeFiles) WriteFile(path string, data []byte, perm fs.FileMode) error {
	p, err := f.Resolve(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return os.WriteFile(p, data, perm)
}

// Open implements tool.Files.
func (f *FakeFiles) Open(path string) (*os.File, error) {
	p, err := f.Resolve(path)
	if err != nil {
		return nil, err
	}
	return os.Open(p) //nolint:gosec // confined by Resolve
}

// Stat implements tool.Files.
func (f *FakeFiles) Stat(path string) (fs.FileInfo, error) {
	p, err := f.Resolve(path)
	if err != nil {
		return nil, err
	}
	return os.Stat(p)
}

// WalkDir implements tool.Files.
func (f *FakeFiles) WalkDir(root string, fn fs.WalkDirFunc) error {
	p, err := f.Resolve(root)
	if err != nil {
		return err
	}
	return filepath.WalkDir(p, fn)
}

// FakeToolsets is a [tool.Toolsets] over the global registry.
type FakeToolsets struct {
	mu      sync.Mutex
	enabled map[tool.Toolset]bool
}

// NewFakeToolsets returns toolsets with the defaults enabled.
func NewFakeToolsets() *FakeToolsets {
	e := map[tool.Toolset]bool{}
	for _, ts := range tool.DefaultToolsets {
		e[ts] = true
	}
	return &FakeToolsets{enabled: e}
}

// List implements tool.Toolsets.
func (f *FakeToolsets) List() []tool.ToolsetInfo {
	counts := map[tool.Toolset]int{}
	for _, t := range tool.All() {
		counts[t.Meta().Toolset]++
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]tool.ToolsetInfo, 0, len(tool.AllToolsets))
	for _, ts := range tool.AllToolsets {
		out = append(out, tool.ToolsetInfo{Name: ts, Enabled: f.enabled[ts], Default: ts.IsDefault(), Tools: counts[ts]})
	}
	return out
}

// Enable implements tool.Toolsets.
func (f *FakeToolsets) Enable(_ context.Context, names ...tool.Toolset) error {
	return f.set(true, names)
}

// Disable implements tool.Toolsets.
func (f *FakeToolsets) Disable(_ context.Context, names ...tool.Toolset) error {
	return f.set(false, names)
}

func (f *FakeToolsets) set(on bool, names []tool.Toolset) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, n := range names {
		if !n.Valid() {
			return fmt.Errorf("%w: unknown toolset %q", tool.ErrInvalidArgs, n)
		}
		if n == tool.Core && !on {
			return fmt.Errorf("%w: core cannot be disabled", tool.ErrInvalidArgs)
		}
		f.enabled[n] = on
	}
	return nil
}

// FakeClock is a [tool.Clock] whose Sleep advances time instantly.
type FakeClock struct {
	mu  sync.Mutex
	now time.Time
	// Slept accumulates every Sleep duration.
	Slept time.Duration
}

// NewFakeClock starts at 2026-01-02T03:04:05Z.
func NewFakeClock() *FakeClock {
	return &FakeClock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

// Now implements tool.Clock.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Sleep implements tool.Clock.
func (c *FakeClock) Sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	c.Slept += d
	return nil
}

// Set moves the clock to t.
func (c *FakeClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = t
}
