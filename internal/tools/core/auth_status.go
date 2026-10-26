package core

import (
	"context"
	"time"

	"github.com/idoo/ciq-mcp/internal/auth"
	"github.com/idoo/ciq-mcp/internal/tool"
)

type authStatusIn struct{}

// credState is one credential's status as reported to the agent.
type credState struct {
	Present   bool   `json:"present"`
	Source    string `json:"source,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	Renewable bool   `json:"renewable"`
	Note      string `json:"note,omitempty"`
}

type authStatusOut struct {
	Configured bool      `json:"configured"`
	Profile    string    `json:"profile"`
	Portal     credState `json:"portal"`
	Connect    credState `json:"connect"`
	Hint       string    `json:"hint,omitempty"`
}

const loginHint = "ask the user to run `ciq-mcp auth login` in a terminal"

func init() {
	tool.Register(tool.New(tool.Meta{
		Name:        "auth_status",
		Toolset:     tool.Core,
		Title:       "Auth status",
		Description: "Report whether developer-portal cookies and the Garmin Connect token are present, their source and expiry. Never returns secrets.",
		Class:       tool.Read,
		Idempotent:  true,
		Local:       true,
	}, authStatus))
}

func authStatus(ctx context.Context, env *tool.Env, _ authStatusIn) (*tool.Result, error) {
	out := authStatusOut{Profile: env.Profile.Name}
	if env.Credentials == nil {
		out.Portal.Note = "not configured"
		out.Connect.Note = "not configured"
		out.Hint = loginHint
		return tool.OK(out), nil
	}
	st := env.Credentials.Status(ctx)
	out.Configured = true
	out.Portal, out.Connect = state(st.Portal), state(st.Connect)
	if !st.Portal.Present || !st.Connect.Present {
		out.Hint = loginHint
	}
	return tool.OK(out), nil
}

func state(c auth.CredStatus) credState {
	s := credState{Present: c.Present, Source: c.Source, Renewable: c.Renewable, Note: c.Note}
	if !c.ExpiresAt.IsZero() {
		s.ExpiresAt = c.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return s
}
