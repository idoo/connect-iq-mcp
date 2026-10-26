package tool

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// Error is a typed ciq-mcp error with a stable machine-readable code. The
// package-level Err* values are sentinels: wrap them with fmt.Errorf("...: %w")
// and test with errors.Is.
type Error struct {
	Code    string
	Message string
	Hint    string
}

// Error implements error.
func (e *Error) Error() string { return e.Message }

// EnableWritesHint is the one way ciq-mcp tells agents to turn writes on: the
// toolsets tool asks the human in the client and, when the client cannot
// ask, returns the restart steps for that client.
const EnableWritesHint = `call toolsets {"action":"enable_writes"} so the user can approve writes for this session (if the client cannot show the approval, it returns the restart steps)`

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
	ErrStalePlan = &Error{
		Code:    "stale_plan",
		Message: "plan is unknown, expired, or the remote state changed since it was made",
		Hint:    "re-run the planning call (release_status / *_plan, or this tool without plan_id/confirm) for a fresh plan",
	}
	ErrPreviewRequired = &Error{
		Code:    "preview_required",
		Message: "confirm:true needs a preview of the same arguments in this session first",
		Hint:    "call again without confirm, show the preview to the user, then repeat with the same arguments and confirm:true",
	}
	ErrOutcomeUnknown = &Error{
		Code:    "outcome_unknown",
		Message: "the request may or may not have been applied",
		Hint:    "read the object back before retrying; ciq-mcp never replays mutations",
	}
	ErrDeclined = &Error{
		Code:    "declined",
		Message: "the user declined the confirmation",
	}
	ErrInvalidArgs = &Error{
		Code:    "invalid_arguments",
		Message: "invalid arguments",
	}
	ErrRateLimited = &Error{
		Code:    "rate_limited",
		Message: "upstream rate limit reached",
		Hint:    "wait before retrying",
	}
	ErrInternal = &Error{
		Code:    "internal_error",
		Message: "internal error",
		Hint:    "this is a ciq-mcp bug; please report it",
	}
	ErrWriteGuard = &Error{
		Code:    "write_guard",
		Message: "refused a non-GET request outside an approved write call",
		Hint:    "this is a ciq-mcp bug; please report it",
	}
	ErrAuthRequired = &Error{
		Code:    "auth_required",
		Message: "Connect IQ credentials are missing or expired",
		Hint:    "run `ciq-mcp auth login` in a terminal, then retry",
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
		Hint:    "use a client with elicitation support, or set allow_no_elicit=true in the profile",
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

// APIStatus returns the HTTP status of the [APIError] in err's chain, or 0.
func APIStatus(err error) int {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

// IsNotFound reports an upstream 404.
func IsNotFound(err error) bool {
	return APIStatus(err) == http.StatusNotFound
}

// IsConflict reports an upstream 409. Callers must read the state back before
// trusting it.
func IsConflict(err error) bool {
	return APIStatus(err) == http.StatusConflict
}
