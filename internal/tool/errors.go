package tool

import (
	"errors"
	"fmt"
	"strings"

	"github.com/idoo/connect-iq-mcp/internal/auth"
	"github.com/idoo/connect-iq-mcp/internal/upstream"
)

// Error is a typed connect-iq-mcp error with a stable machine-readable code. The
// package-level Err* values are sentinels: wrap them with fmt.Errorf("...: %w")
// and test with errors.Is.
type Error struct {
	Code    string
	Message string
	Hint    string
}

// Error implements error.
func (e *Error) Error() string { return e.Message }

// EnableWritesHint is the one way connect-iq-mcp tells agents to turn writes on: the
// toolsets tool asks the human in the client and, when the client cannot
// ask, returns the restart steps for that client.
const EnableWritesHint = `call toolsets {"action":"enable_writes"} so the user can approve writes for this session (if the client cannot show the approval, it returns the restart steps)`

// LoginCommand is the one CLI sign-in command every human-facing hint names:
// "connect-iq-mcp auth login" plus flags, plus "--profile <name>" unless the
// profile is "" or "default" (a bare command in the user's terminal signs in
// "default", not the profile the server runs as).
func LoginCommand(profile string, flags ...string) string {
	cmd := "connect-iq-mcp auth login"
	for _, f := range flags {
		cmd += " " + f
	}
	if profile != "" && profile != "default" {
		cmd += " --profile " + profile
	}
	return cmd
}

// AuthRequiredHint is the auth_required hint for profile.
func AuthRequiredHint(profile string) string {
	return "call the auth_login tool (it opens a browser window for the user to sign in), or ask the user to run `" + LoginCommand(profile) + "` in a terminal, then retry"
}

// Sentinel errors shared by every tool.
var (
	ErrWritesDisabled = &Error{
		Code:    "writes_disabled",
		Message: "writes are disabled",
		Hint:    EnableWritesHint,
	}
	ErrNeedsConfirm = &Error{
		Code:    "needs_confirm",
		Message: "confirmation required",
		Hint:    "review the preview, then call again with confirm",
	}
	ErrPreviewRequired = &Error{
		Code:    "preview_required",
		Message: "confirm:true needs a preview of the same arguments in this session first",
		Hint:    "call again without confirm, show the preview to the user, then repeat with the same arguments and confirm:true",
	}
	ErrOutcomeUnknown = &Error{
		Code:    "outcome_unknown",
		Message: "the request may or may not have been applied",
		Hint:    "read the object back before retrying; connect-iq-mcp never replays mutations",
	}
	ErrDeclined = &Error{
		Code:    "declined",
		Message: "the user declined the confirmation",
	}
	ErrInvalidArgs = &Error{
		Code:    "invalid_arguments",
		Message: "invalid arguments",
	}
	// ErrOutsideRoot is a path argument that resolves outside the confined
	// root. It is an argument error, so it shares the invalid_arguments code.
	ErrOutsideRoot = &Error{
		Code:    "invalid_arguments",
		Message: "path is outside the allowed root",
		Hint:    "paths are relative to the client's workspace root (or the directory connect-iq-mcp started in when the client has no roots); pass a path inside it, or open the project folder as a workspace root",
	}
	ErrRateLimited = &Error{
		Code:    "rate_limited",
		Message: "upstream rate limit reached",
		Hint:    "wait before retrying",
	}
	ErrInternal = &Error{
		Code:    "internal_error",
		Message: "internal error",
		Hint:    "this is a connect-iq-mcp bug; please report it",
	}
	ErrWriteGuard = &Error{
		Code:    "write_guard",
		Message: "refused a non-GET request outside an approved write call",
		Hint:    "this is a connect-iq-mcp bug; please report it",
	}
	ErrAuthRequired = &Error{
		Code:    "auth_required",
		Message: "Connect IQ credentials are missing or expired",
		Hint:    AuthRequiredHint(""),
	}
	ErrUnavailable = &Error{
		Code:    "unavailable",
		Message: "capability unavailable in this session",
	}
	ErrElicitationUnavailable = &Error{
		Code:    "elicitation_unavailable",
		Message: "the MCP client does not support elicitation",
	}
	ErrHumanRequired = &Error{
		Code:    "human_approval_required",
		Message: "this action needs explicit human approval, but the client cannot ask",
		Hint:    "use a client with elicitation support, or set allow_no_elicit = true under the profile in connect-iq-mcp's config.toml (~/.config/connect-iq-mcp/config.toml, $XDG_CONFIG_HOME/connect-iq-mcp/config.toml if set, %AppData%\\connect-iq-mcp\\config.toml on Windows)",
	}
)

// APIError is a decoded upstream HTTP error (Garmin portal or Connect API)
// enriched with a hint for the agent.
type APIError struct {
	Status int    `json:"status"`
	Code   string `json:"code,omitempty"`
	Detail string `json:"detail,omitempty"`
	Hint   string `json:"hint,omitempty"`
}

// Error implements error.
func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "HTTP %d", e.Status)
	if e.Code != "" {
		b.WriteString(" " + e.Code)
	}
	if e.Detail != "" {
		b.WriteString(": " + e.Detail)
	}
	return b.String()
}

// ErrorBody is the structured payload of an error result.
type ErrorBody struct {
	Error   string     `json:"error"`
	Code    string     `json:"code,omitempty"`
	Hint    string     `json:"hint,omitempty"`
	Message string     `json:"message"`
	API     *APIError  `json:"api,omitempty"`
	Next    []NextStep `json:"next,omitempty"`
}

// NextStep is a machine-readable follow-up suggestion: call Tool with Args.
// When Tool is hidden in the session (writes disabled) the server sets Locked
// and Unlock, the call that makes Tool available.
type NextStep struct {
	Tool   string         `json:"tool"`
	Args   map[string]any `json:"args,omitempty"`
	Locked string         `json:"locked,omitempty"`
	Unlock *NextStep      `json:"unlock,omitempty"`
}

// NextStepper is implemented by errors that suggest follow-up calls.
type NextStepper interface {
	NextSteps() []NextStep
}

type nextError struct {
	err  error
	next []NextStep
}

func (e *nextError) Error() string         { return e.err.Error() }
func (e *nextError) Unwrap() error         { return e.err }
func (e *nextError) NextSteps() []NextStep { return e.next }

// WithNext attaches follow-up suggestions to err (nil stays nil).
func WithNext(err error, next ...NextStep) error {
	if err == nil {
		return nil
	}
	return &nextError{err: err, next: next}
}

// BodyFor converts any error into the structured error payload.
func BodyFor(err error) ErrorBody {
	b := ErrorBody{Error: "error", Message: err.Error()}
	var api *APIError
	if errors.As(err, &api) {
		b.Error = "api_error"
		b.Code = api.Code
		b.Hint = api.Hint
		b.API = api
	}
	var te *Error
	if errors.As(err, &te) {
		b.Error = te.Code
		if b.Hint == "" {
			b.Hint = te.Hint
		}
	}
	var ns NextStepper
	if errors.As(err, &ns) {
		b.Next = ns.NextSteps()
	}
	return b
}

// InvalidArgs returns an error wrapping [ErrInvalidArgs] with the formatted
// message ("invalid arguments: <msg>"). %w in format wraps as in fmt.Errorf.
func InvalidArgs(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrInvalidArgs}, args...)...)
}

// UpstreamHints is the toolset-specific part of [FromUpstream]: the agent
// hints for its upstream's errors and the client's own sentinels.
type UpstreamHints struct {
	// Codes maps an upstream error code to its hint.
	Codes map[string]string
	// Statuses maps an HTTP status to its hint when Codes has none.
	Statuses map[int]string
	// Blocked is the client's Cloudflare-block sentinel (nil: none). It maps
	// to rate_limited with an api body of code "blocked" and BlockedHint.
	Blocked     error
	BlockedHint string
	// InvalidID is the client's sentinel for an id it refused to send (nil:
	// none). It maps to invalid_arguments.
	InvalidID error
	// RateLimitAPI attaches an api body (code rate_limited plus a wait hint)
	// to rate limits; without it they are plain rate_limited errors.
	RateLimitAPI bool
}

// FromUpstream maps an upstream client error to a tool error: auth_required,
// rate_limited, invalid_arguments, or api_error carrying the upstream code
// and a hint from h. Other errors pass through. nil stays nil.
func FromUpstream(err error, h UpstreamHints) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, auth.ErrRateLimited) {
		if !h.RateLimitAPI {
			return fmt.Errorf("%w: %s", ErrRateLimited, err.Error())
		}
		return fmt.Errorf("%w: %w", ErrRateLimited, &APIError{Code: "rate_limited", Detail: err.Error(), Hint: auth.ErrRateLimited.Error()})
	}
	if h.Blocked != nil && errors.Is(err, h.Blocked) {
		return fmt.Errorf("%w: %w", ErrRateLimited, &APIError{Code: "blocked", Detail: err.Error(), Hint: h.BlockedHint})
	}
	if errors.Is(err, auth.ErrAuthRequired) || errors.Is(err, auth.ErrInteractionRequired) {
		return fmt.Errorf("%w: %s", ErrAuthRequired, err.Error())
	}
	if h.InvalidID != nil && errors.Is(err, h.InvalidID) {
		return InvalidArgs("%s", err.Error())
	}
	var he *upstream.HTTPError
	if !errors.As(err, &he) {
		return err
	}
	if he.RateLimited {
		if !h.RateLimitAPI {
			return fmt.Errorf("%w: %s", ErrRateLimited, he.Error())
		}
		hint := "wait before retrying; connect-iq-mcp does not retry by itself"
		if he.Cloudflare {
			hint = fmt.Sprintf("Cloudflare is rate-limiting requests to the %s (error 1015); wait several minutes before retrying and do not call auth_login", he.Service)
		}
		if s := int(he.RetryAfter.Seconds()); s > 0 {
			hint = fmt.Sprintf("the %s asked to wait %d seconds (Retry-After) before retrying", he.Service, s)
		}
		return fmt.Errorf("%w: %w", ErrRateLimited, &APIError{Status: he.Status, Code: "rate_limited", Detail: he.Error(), Hint: hint})
	}
	hint := h.Codes[he.Code]
	if hint == "" {
		hint = h.Statuses[he.Status]
	}
	return &APIError{Status: he.Status, Code: he.Code, Detail: he.Detail(), Hint: hint}
}
