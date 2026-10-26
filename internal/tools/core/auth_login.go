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

	"github.com/idoo/garmin-mcp/internal/auth"
	"github.com/idoo/garmin-mcp/internal/browser"
	"github.com/idoo/garmin-mcp/internal/config"
	"github.com/idoo/garmin-mcp/internal/tool"
)

type authLoginIn struct {
	Connect        bool `json:"connect,omitempty" jsonschema:"also sign in to Garmin Connect (default false)"`
	TimeoutMinutes int  `json:"timeout_minutes,omitempty" jsonschema:"how long to wait for the user to finish signing in, 1-15 (default 10)"`
}

const (
	defaultLoginTimeout = 10
	maxLoginTimeout     = 15
	loginProgressEvery  = 5 * time.Second
	// loginCooldown follows a window that closed without a sign-in, so a
	// looping (or prompt-injected) agent can't keep popping windows.
	loginCooldown = 10 * time.Minute
)

// getenv and loginNow are replaced in tests.
var (
	getenv   = os.Getenv
	loginNow = time.Now
)

// loginGate allows one interactive window per process; failedAt starts the
// cooldown. The CLI `auth login` does not go through it.
var loginGate struct {
	mu       sync.Mutex
	busy     bool
	failedAt time.Time
}

// enterLogin claims the window slot or explains why not.
func enterLogin() error {
	loginGate.mu.Lock()
	defer loginGate.mu.Unlock()
	if loginGate.busy {
		return &tool.Error{
			Code:    "login_unavailable",
			Message: "a Garmin sign-in window is already open",
			Hint:    "wait for the user to finish signing in there",
		}
	}
	if !loginGate.failedAt.IsZero() {
		if left := loginCooldown - loginNow().Sub(loginGate.failedAt); left > 0 {
			return &tool.Error{
				Code:    "login_unavailable",
				Message: fmt.Sprintf("the last sign-in window closed without a sign-in; auth_login can open another in %s", left.Round(time.Second)),
				Hint:    "ask the user to run `garmin-mcp auth login` in a terminal, or call auth_login again later",
			}
		}
	}
	loginGate.busy = true
	return nil
}

// leaveLogin frees the slot; a failed sign-in starts the cooldown.
func leaveLogin(failed bool) {
	loginGate.mu.Lock()
	defer loginGate.mu.Unlock()
	loginGate.busy = false
	if failed {
		loginGate.failedAt = loginNow()
	} else {
		loginGate.failedAt = time.Time{}
	}
}

func init() {
	tool.Register(tool.New(tool.Meta{
		Name:    "auth_login",
		Toolset: tool.Core,
		Title:   "Sign in to Garmin",
		Description: "Sign the user in to Garmin: opens a browser window, waits up to timeout_minutes (progress every 5 s). " +
			"Returns auth_status at once if credentials still work. connect=true adds Garmin Connect. No secrets returned. " +
			"One window at a time; 10 min cooldown after one closes unsigned.",
		Class:             tool.Write,
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
			Hint:    "provide credentials through GARMIN_MCP_PORTAL_COOKIES / GARMIN_MCP_CONNECT_REFRESH_TOKEN in CI",
		}
	}
	lg, ok := env.Credentials.(auth.Loginer)
	if env.Credentials == nil || !ok {
		return nil, &tool.Error{
			Code:    "login_unavailable",
			Message: "this garmin-mcp build has no interactive sign-in",
			Hint:    "ask the user to run `garmin-mcp auth login` in a terminal",
		}
	}
	if in.Connect && !env.Credentials.Status(ctx).Connect.Present {
		if err := connectConsent(ctx, env); err != nil {
			return nil, err
		}
	}

	if err := enterLogin(); err != nil {
		return nil, err
	}

	// MCP: progress must increase with every notification; total unknown.
	var mu sync.Mutex
	var n float64
	last := "waiting for you to sign in in the browser window"
	report := func(msg string) {
		mu.Lock()
		defer mu.Unlock() // held while sending, so values reach the client in order
		n++
		env.Progress(n, 0, msg)
	}
	logw := progressWriter(func(line string) {
		mu.Lock()
		last = line
		mu.Unlock()
		report(line)
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
				report(fmt.Sprintf("%s (%s elapsed)", msg, time.Since(start).Round(time.Second)))
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
	// No browser means no window was shown, so there is nothing to cool down from.
	leaveLogin(err != nil && !errors.Is(err, browser.ErrNoBrowser))
	if err != nil {
		var te *tool.Error
		if errors.As(err, &te) {
			return nil, err
		}
		return nil, &tool.Error{
			Code:    "login_failed",
			Message: err.Error(),
			Hint:    "ask the user to run `garmin-mcp auth login` in a terminal; auth_login itself waits 10 minutes before opening another window",
		}
	}
	return tool.OK(statusOut(ctx, env)), nil
}

// connectConsentPrompt is the human question before Garmin Connect (health
// data) access is enabled for the first time.
const connectConsentPrompt = "auth_login wants to sign in to Garmin Connect, giving this AI agent read access to your health and activity data (sleep, heart rate, activities). Allow?"

// connectConsentHint is the way to enable Connect without elicitation.
const connectConsentHint = "ask the user to run `garmin-mcp auth login --connect` in a terminal"

// connectConsent asks the human to enable Garmin Connect. An agent alone can
// never enable health-data access: without elicitation it is refused, even
// when the profile allows proceeding without elicitation elsewhere.
func connectConsent(ctx context.Context, env *tool.Env) error {
	ok, err := false, error(tool.ErrElicitationUnavailable)
	if env.Elicit != nil {
		ok, err = env.Elicit(ctx, connectConsentPrompt)
	}
	switch {
	case errors.Is(err, tool.ErrElicitationUnavailable):
		return &tool.Error{Code: tool.ErrHumanRequired.Code, Message: "enabling Garmin Connect needs the user's approval, but the client cannot ask", Hint: connectConsentHint}
	case err != nil:
		return fmt.Errorf("ask for confirmation: %w", err)
	case !ok:
		return &tool.Error{Code: tool.ErrDeclined.Code, Message: "the user declined enabling Garmin Connect", Hint: connectConsentHint}
	}
	return nil
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
