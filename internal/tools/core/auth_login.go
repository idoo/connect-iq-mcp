package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/idoo/ciq-mcp/internal/auth"
	"github.com/idoo/ciq-mcp/internal/config"
	"github.com/idoo/ciq-mcp/internal/tool"
)

type authLoginIn struct {
	Connect        bool `json:"connect,omitempty" jsonschema:"also sign in to Garmin Connect (default false)"`
	TimeoutMinutes int  `json:"timeout_minutes,omitempty" jsonschema:"how long to wait for the user to finish signing in, 1-15 (default 10)"`
}

const (
	defaultLoginTimeout = 10
	maxLoginTimeout     = 15
	loginProgressEvery  = 5 * time.Second
)

// getenv is replaced in tests.
var getenv = os.Getenv

func init() {
	tool.Register(tool.New(tool.Meta{
		Name:    "auth_login",
		Toolset: tool.Core,
		Title:   "Sign in to Garmin",
		Description: "Sign the user in to Garmin: opens a browser window, waits up to timeout_minutes (progress every 5 s; clients without progress may cancel after ~60 s). " +
			"Returns auth_status at once if credentials still work. connect=true adds Garmin Connect. No secrets returned.",
		Class:             tool.Write,
		Local:             true,
		AvailableReadOnly: true,
	}, authLogin))
}

func authLogin(ctx context.Context, env *tool.Env, in authLoginIn) (*tool.Result, error) {
	timeout := in.TimeoutMinutes
	if timeout == 0 {
		timeout = defaultLoginTimeout
	}
	if timeout < 1 || timeout > maxLoginTimeout {
		return nil, fmt.Errorf("%w: timeout_minutes must be 1-%d", tool.ErrInvalidArgs, maxLoginTimeout)
	}
	if ci, _ := strconv.ParseBool(strings.TrimSpace(getenv(config.EnvCI))); ci {
		return nil, &tool.Error{
			Code:    "login_unavailable",
			Message: config.EnvCI + " is set: no human is present to sign in",
			Hint:    "provide credentials through CIQ_MCP_PORTAL_COOKIES / CIQ_MCP_CONNECT_REFRESH_TOKEN in CI",
		}
	}
	lg, ok := env.Credentials.(auth.Loginer)
	if env.Credentials == nil || !ok {
		return nil, &tool.Error{
			Code:    "login_unavailable",
			Message: "this ciq-mcp build has no interactive sign-in",
			Hint:    "ask the user to run `ciq-mcp auth login` in a terminal",
		}
	}

	var mu sync.Mutex
	last := "waiting for you to sign in in the browser window"
	logw := progressWriter(func(line string) {
		mu.Lock()
		last = line
		mu.Unlock()
		env.Progress(0, 0, line)
	})
	done := make(chan struct{})
	go func() {
		start := time.Now()
		t := time.NewTicker(loginProgressEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				mu.Lock()
				msg := last
				mu.Unlock()
				env.Progress(0, 0, fmt.Sprintf("%s (%s elapsed)", msg, time.Since(start).Round(time.Second)))
			}
		}
	}()
	_, err := lg.Login(ctx, auth.LoginOptions{
		Connect:     in.Connect,
		Timeout:     time.Duration(timeout) * time.Minute,
		SkipIfValid: true,
		Log:         logw,
	})
	close(done)
	if err != nil {
		var te *tool.Error
		if errors.As(err, &te) {
			return nil, err
		}
		return nil, &tool.Error{
			Code:    "login_failed",
			Message: err.Error(),
			Hint:    "call auth_login again when the user is ready, or ask them to run `ciq-mcp auth login` in a terminal",
		}
	}
	return tool.OK(statusOut(ctx, env)), nil
}

// progressWriter turns browser log lines into progress notifications.
type progressWriter func(line string)

func (p progressWriter) Write(b []byte) (int, error) {
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			p(line)
		}
	}
	return len(b), nil
}
