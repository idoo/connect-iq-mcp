package tool

import (
	"context"
	"io/fs"
	"net/http"
	"os"
	"time"

	"github.com/idoo/connect-iq-mcp/internal/auth"
)

// ProfileInfo describes the active profile (never secrets).
type ProfileInfo struct {
	Name        string `json:"name"`
	AllowWrites bool   `json:"allow_writes"`
	// PinSource is the path of the .connect-iq-mcp.toml pin that selected the profile, if any.
	PinSource string `json:"pin_source,omitempty"`
	// AllowNoElicit lets RequireHuman tools proceed on an agent confirmation
	// when the client cannot elicit (config opt-in).
	AllowNoElicit bool `json:"allow_no_elicit,omitempty"`
}

// StateStore is persistent local state (XDG state dir).
type StateStore interface {
	// Cursor returns the stored "since" cursor for profile/app/name, or "".
	Cursor(profile, app, name string) (string, error)
	// SetCursor stores a cursor value.
	SetCursor(profile, app, name, value string) error
	// Load decodes the JSON record kind/key into v, reporting whether it existed.
	Load(kind, key string, v any) (bool, error)
	// Save stores v as the JSON record kind/key.
	Save(kind, key string, v any) error
	// Delete removes a record; missing records are not an error.
	Delete(kind, key string) error
}

// ToolsetInfo describes one toolset for the toolsets tool.
type ToolsetInfo struct {
	Name    Toolset `json:"name"`
	Enabled bool    `json:"enabled"`
	Default bool    `json:"default"`
	// Tools counts tools visible in the current mode; Withheld counts tools
	// hidden by read-only mode or the key kind.
	Tools    int `json:"tools"`
	Withheld int `json:"withheld,omitempty"`
}

// Toolsets controls the session's enabled toolsets (sends tools/list_changed).
type Toolsets interface {
	List() []ToolsetInfo
	Enable(ctx context.Context, names ...Toolset) error
	Disable(ctx context.Context, names ...Toolset) error
}

// Files gives tools confined filesystem access: an explicit --root/CIQ_MCP_ROOT,
// else the MCP client's roots, else (only when the client sent none) the git
// root of the cwd. A fallback root of / or the home directory is refused.
// Symlink escapes are refused.
// All methods accept paths relative to the root or absolute paths inside it.
type Files interface {
	// Resolve returns the confined absolute path or an error.
	Resolve(path string) (string, error)
	ReadFile(path string) ([]byte, error)
	// WriteFile writes data, creating parent directories.
	WriteFile(path string, data []byte, perm fs.FileMode) error
	Open(path string) (*os.File, error)
	Stat(path string) (fs.FileInfo, error)
	// WalkDir walks root like [fs.WalkDir]; callback paths are absolute.
	WalkDir(root string, fn fs.WalkDirFunc) error
}

// Clock abstracts time so wait loops are testable.
type Clock interface {
	Now() time.Time
	// Sleep waits d or until ctx is done (returning ctx.Err()).
	Sleep(ctx context.Context, d time.Duration) error
}

// SystemClock is the real-time [Clock].
type SystemClock struct{}

// Now returns time.Now().
func (SystemClock) Now() time.Time { return time.Now() }

// Sleep waits d or until ctx is done.
func (SystemClock) Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Env is everything a tool handler may use. The server builds one per
// session; tests use tooltest.New. Inside a handler every member except
// Plans is non-nil: [Tool.Call] substitutes stand-ins that fail with
// [ErrUnavailable] (and a no-op Progress, an Elicit returning
// [ErrElicitationUnavailable], the system clock). A nil Plans fails every
// agent-only confirm closed.
type Env struct {
	// Credentials provides portal cookies and the Connect token; nil means
	// none configured (tools report auth_required).
	Credentials auth.Store
	// HTTP is the outbound client. Its transport is wrapped by [GuardWrites]
	// so non-GET requests fail unless the context carries write capability.
	HTTP     *http.Client
	Plans    *Planner
	State    StateStore
	Toolsets Toolsets
	// Root is the confined project file root (os.Root based).
	Root    Files
	Clock   Clock
	Profile ProfileInfo
	// Version is the connect-iq-mcp build version.
	Version string
	// NoConfirm skips the preview step (CI only: --no-confirm with CIQ_MCP_CI=1).
	// RequireHuman tools still require approval.
	NoConfirm bool
	// SetWrites flips the session's write capability below the tool layer.
	// The server calls it when toolsets enable_writes succeeds; nil is a no-op.
	SetWrites func(bool)
	// Roots lists the MCP client's roots (file:// URIs). nil means the client
	// declared no roots capability. [Tool.Call] passes it to a [RootedFiles]
	// Files, which asks the client only when a tool first touches a file.
	Roots func(ctx context.Context) ([]string, error)
	// Progress reports progress to the client.
	Progress func(done, total float64, msg string)
	// Elicit asks the human a yes/no question. It returns false when the human
	// declines and an error wrapping [ErrElicitationUnavailable] when the
	// client cannot ask.
	Elicit func(ctx context.Context, msg string) (bool, error)
	// Lifetime runs work that outlives one call (sdk builds) until the
	// server stops; nil runs it under context.Background (tests).
	Lifetime *Lifetime
	// Shared is the server's cross-call state (see [Shared]). The server and
	// tooltest.New set it; a hand-built Env with none gets a fresh,
	// call-private one.
	Shared *Shared

	// noProgress records that Progress was nil before [Env.complete].
	noProgress bool
}

// HasProgress reports whether the client asked for progress notifications
// (sent a progressToken). Long waits clamp to [NoProgressWaitCap] without it,
// since such clients typically cancel a request after about 60 s.
func (e *Env) HasProgress() bool { return e.Progress != nil && !e.noProgress }

// NoProgressWaitCap bounds a tool's wait when the client sent no progress token.
const NoProgressWaitCap = 50 * time.Second

// complete returns a copy of e with every nil member replaced by a stand-in.
func (e *Env) complete() *Env {
	c := *e
	if c.HTTP == nil {
		c.HTTP = &http.Client{Transport: GuardWrites(nil), Timeout: 60 * time.Second}
	}
	if c.State == nil {
		c.State = noStore{}
	}
	if c.Toolsets == nil {
		c.Toolsets = noToolsets{}
	}
	if c.Root == nil {
		c.Root = noFiles{}
	}
	if c.Clock == nil {
		c.Clock = SystemClock{}
	}
	if c.Shared == nil {
		c.Shared = NewShared()
	}
	if c.SetWrites == nil {
		c.SetWrites = func(bool) {}
	}
	if c.Progress == nil {
		c.noProgress = true
		c.Progress = func(float64, float64, string) {}
	}
	if c.Elicit == nil {
		c.Elicit = func(context.Context, string) (bool, error) {
			return false, ErrElicitationUnavailable
		}
	}
	return &c
}
