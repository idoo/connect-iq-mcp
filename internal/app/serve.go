package app

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/idoo/garmin-mcp/internal/config"
	"github.com/idoo/garmin-mcp/internal/mcpserver"
	"github.com/idoo/garmin-mcp/internal/sim"
	"github.com/idoo/garmin-mcp/internal/tool"
)

// ServeOptions configures [Serve].
type ServeOptions struct {
	// Resolved is the effective configuration.
	Resolved *config.Resolved
	// Version is the garmin-mcp build version.
	Version string
	// HTTPAddr serves Streamable HTTP when set; empty serves stdio.
	HTTPAddr string
	// HTTP holds the HTTP transport options (NewServer is filled in by Serve).
	HTTP mcpserver.HTTPOptions
	// Logger receives server logs on stderr; nil uses a stderr text logger.
	Logger *slog.Logger
	// Root confines file tools to this directory (--root); empty falls back
	// to GARMIN_MCP_ROOT, then the client's MCP roots, then the git root of the cwd.
	Root string
}

// Serve builds the environment and runs the MCP server until ctx ends: one
// server over stdio, or one server per HTTP session.
func Serve(ctx context.Context, opts ServeOptions) error {
	// monkeydo clients run in their own process group; never leave them
	// running after the server (stdio or HTTP) returns.
	defer sim.Sessions.KillAll()
	r := opts.Resolved
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}
	for _, w := range r.Warnings {
		logger.Warn(w)
	}
	for _, p := range r.Profile.Problems {
		logger.Warn(p)
	}
	root := opts.Root
	if root == "" {
		root = os.Getenv(config.EnvRoot)
	}
	env, err := newEnv(ctx, r, opts.Version, root)
	if err != nil {
		return err
	}
	if pf, ok := env.Root.(*ProjectFiles); ok {
		defer func() { _ = pf.Close() }()
	}
	// Background builds end with the server (stdio EOF, HTTP shutdown,
	// signal): cancel kills monkeyc's process group, and the short wait lets
	// each build remove its temp dir.
	life := tool.NewLifetime(ctx)
	defer life.Close(5 * time.Second)
	env.Lifetime = life
	if env.Credentials == nil {
		logger.Warn("no Connect IQ credentials configured; run `garmin-mcp auth login`")
	}
	deps := mcpserver.Deps{
		Env:            *env,
		Profile:        env.Profile,
		WritesLocked:   r.WritesLocked,
		Toolsets:       r.Toolsets,
		Discovery:      r.Discovery,
		Version:        opts.Version,
		Logger:         logger,
		MaxResultBytes: r.MaxResponseBytes,
	}
	if opts.HTTPAddr == "" {
		srv, err := mcpserver.Build(ctx, deps)
		if err != nil {
			return fmt.Errorf("app: build server: %w", err)
		}
		return mcpserver.RunStdio(ctx, srv)
	}
	// Validate once up front so misconfiguration fails before listening.
	if _, err := mcpserver.Build(ctx, deps); err != nil {
		return fmt.Errorf("app: build server: %w", err)
	}
	h := opts.HTTP
	h.Logger = logger
	h.NewServer = mcpserver.SessionFactory(deps)
	return mcpserver.ServeHTTP(ctx, opts.HTTPAddr, h)
}
