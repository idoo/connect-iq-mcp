// Package app wires configuration, credentials, the HTTP client and local
// state into a [tool.Env], and runs the MCP server.
package app

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/idoo/garmin-mcp/internal/auth"
	"github.com/idoo/garmin-mcp/internal/config"
	"github.com/idoo/garmin-mcp/internal/state"
	"github.com/idoo/garmin-mcp/internal/tool"
)

// httpTimeout bounds one outbound request.
const httpTimeout = 2 * time.Minute

// NewEnv builds the base tool environment for resolved. Without configured
// credentials Env.Credentials stays nil and tools report auth_required. The
// file root comes from GARMIN_MCP_ROOT when set (see [ProjectFiles]).
func NewEnv(ctx context.Context, r *config.Resolved, version string) (*tool.Env, error) {
	return newEnv(ctx, r, version, os.Getenv(config.EnvRoot))
}

func newEnv(ctx context.Context, r *config.Resolved, version, root string) (*tool.Env, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("app: cwd: %w", err)
	}
	dir, err := state.DefaultDir()
	if err != nil {
		return nil, fmt.Errorf("app: state dir: %w", err)
	}
	store, err := state.New(dir)
	if err != nil {
		return nil, fmt.Errorf("app: state: %w", err)
	}
	home, _ := os.UserHomeDir()
	files, err := NewProjectFiles(root, cwd, home)
	if err != nil {
		return nil, err
	}
	env := &tool.Env{
		HTTP: &http.Client{
			Transport: userAgent{base: tool.GuardWrites(nil), ua: "garmin-mcp/" + version},
			Timeout:   httpTimeout,
		},
		State:     store,
		Root:      files,
		Clock:     tool.SystemClock{},
		Profile:   r.Profile.Info(),
		Version:   version,
		NoConfirm: r.NoConfirm && r.CI,
	}
	if auth.Open != nil {
		creds, err := auth.Open(ctx, r.Profile.Name)
		if err != nil {
			return nil, fmt.Errorf("app: credentials: %w", err)
		}
		if creds != nil {
			env.Credentials = creds
		}
	}
	return env, nil
}

// userAgent sets the User-Agent header on requests that lack one.
type userAgent struct {
	base http.RoundTripper
	ua   string
}

func (u userAgent) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("User-Agent") == "" {
		req = req.Clone(req.Context())
		req.Header.Set("User-Agent", u.ua)
	}
	return u.base.RoundTrip(req)
}
