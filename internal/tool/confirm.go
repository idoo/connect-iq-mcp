package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ConfirmMode declares how a tool gates mutations. Write capability (see
// [IsWrite]) is granted only after the gate passes; a preview call runs
// read-only.
type ConfirmMode int

const (
	// ConfirmNone means no gate. Write tools receive write capability up front.
	ConfirmNone ConfirmMode = iota
	// ConfirmByFlag means In embeds [ConfirmFlag]; without confirm:true the call is
	// a preview. With it, the human is asked via elicitation when available
	// (mandatory with [Meta.RequireHuman]); an agent-only confirm must follow a
	// preview of the same arguments in the session ([Planner.Claim]).
	ConfirmByFlag
)

// ConfirmFlag is embedded in the input of [ConfirmByFlag] tools.
type ConfirmFlag struct {
	Confirm bool `json:"confirm,omitempty" jsonschema:"true after reviewing the preview"`
}

// PlanPreview describes what a mutation would do. The approval prompt shown
// to the human is built from Summary, Affected (targets: method and path,
// URLs, emails), Intended (the full payload, e.g. reply texts) and
// Irreversible, never truncated. Untrusted holds third-party text (e.g. the
// customer review being answered); it is shown after the payload, fenced and
// labeled as data.
type PlanPreview struct {
	Summary      string   `json:"summary"`
	Current      any      `json:"current,omitempty"`
	Intended     any      `json:"intended,omitempty"`
	Affected     []string `json:"affected,omitempty"`
	Irreversible []string `json:"irreversible,omitempty"`
	Untrusted    []string `json:"untrusted,omitempty"`
}

// previewBody is the payload of a preview result.
type previewBody struct {
	Status  string      `json:"status"`
	Preview PlanPreview `json:"preview"`
	Next    string      `json:"next"`
}

// RequireConfirm is the gate of [ConfirmByFlag] tools.
// It returns either a write-capable context (proceed) or a result the handler
// must return as-is:
//
//	wctx, res := tool.RequireConfirm(ctx, env, in.Confirm, preview)
//	if res != nil {
//		return res, nil
//	}
//	res, err := env.HTTP.Do(req.WithContext(wctx))
//
// Without confirm the result is a needs_confirm preview and nothing changes.
// A confirm the human did not approve is refused with [ErrPreviewRequired]
// unless a preview of the same arguments came first (single use).
func RequireConfirm(ctx context.Context, env *Env, confirm bool, p PlanPreview) (context.Context, *Result) {
	return requireConfirm(ctx, env, confirm, p, false)
}

// RequireHumanConfirm is [RequireConfirm] with human approval forced for this
// call, as if the tool declared [Meta.RequireHuman]: for branches of a
// [ConfirmByFlag] tool that make things public or irreversible while its
// other branches proceed via [Unconfirmed]. Without elicitation it is refused
// unless the profile sets AllowNoElicit.
func RequireHumanConfirm(ctx context.Context, env *Env, confirm bool, p PlanPreview) (context.Context, *Result) {
	return requireConfirm(ctx, env, confirm, p, true)
}

func requireConfirm(ctx context.Context, env *Env, confirm bool, p PlanPreview, human bool) (context.Context, *Result) {
	c, res := gateCall(ctx, env, ConfirmByFlag)
	if res != nil {
		return nil, res
	}
	human = human || c.meta.RequireHuman
	args, err := CanonicalArgs(c.args, c.meta.ConfirmArgs...)
	if err != nil {
		return nil, Fail(fmt.Errorf("%w: %v", ErrInternal, err))
	}
	if !confirm && !env.NoConfirm {
		// Without a plan store the preview is still returned; the confirm
		// then fails closed in Claim unless a human approves.
		env.Plans.Propose(c.meta.Name, args)
		return nil, OK(previewBody{
			Status:  ErrNeedsConfirm.Code,
			Preview: p,
			Next:    "nothing was changed; call again with the same arguments and confirm:true to apply",
		})
	}
	if res := approve(ctx, env, c, p, human); res != nil {
		return nil, res
	}
	if c.approvedBy == ApprovedAgent && !env.NoConfirm {
		if err := env.Plans.Claim(c.meta.Name, args); err != nil {
			c.approvedBy = ""
			return nil, Fail(fmt.Errorf("%s: %w", c.meta.Name, err))
		}
	}
	return withWrite(ctx), nil
}

// Unconfirmed grants write capability without a gate, for branches a
// [ConfirmByFlag] tool has determined are pure creates or
// additive updates (nothing deleted, overwritten or made public). It is
// refused for [Meta.RequireHuman] tools.
func Unconfirmed(ctx context.Context, env *Env) (context.Context, *Result) {
	c, res := gateCall(ctx, env, ConfirmByFlag)
	if res != nil {
		return nil, res
	}
	if c.meta.RequireHuman {
		return nil, Fail(fmt.Errorf("%w: %s requires human approval and cannot skip its gate", ErrInternal, c.meta.Name))
	}
	return withWrite(ctx), nil
}

// gateCall checks that ctx belongs to a write-enabled call of a tool declared
// with one of modes.
func gateCall(ctx context.Context, env *Env, modes ...ConfirmMode) (*call, *Result) {
	c := callFrom(ctx)
	if c == nil {
		return nil, Fail(fmt.Errorf("%w: confirm gate used outside Tool.Call", ErrInternal))
	}
	if !slices.Contains(modes, c.meta.Confirm) {
		return nil, Fail(fmt.Errorf("%w: %s uses the wrong confirm gate for its ConfirmMode", ErrInternal, c.meta.Name))
	}
	if !env.Profile.AllowWrites {
		return nil, Fail(fmt.Errorf("%s: %w", c.meta.Name, ErrWritesDisabled))
	}
	return c, nil
}

// approve asks the human with a framework-built prompt and records who
// approved. When human is set (RequireHuman tools and forced calls) an accept
// is needed unless the profile allows proceeding without elicitation.
func approve(ctx context.Context, env *Env, c *call, p PlanPreview, human bool) *Result {
	if env.NoConfirm && !human {
		c.approvedBy = ApprovedAgent
		return nil
	}
	prompt, err := ApprovalPrompt(c.meta.Name, p)
	if err != nil {
		return Fail(err)
	}
	ok, err := env.Elicit(ctx, prompt)
	switch {
	case errors.Is(err, ErrElicitationUnavailable):
		if human && !env.Profile.AllowNoElicit {
			return Fail(fmt.Errorf("%s: %w", c.meta.Name, ErrHumanRequired))
		}
		c.approvedBy = ApprovedAgent
		return nil
	case err != nil:
		return Fail(fmt.Errorf("ask for confirmation: %w", err))
	case !ok:
		return Fail(ErrDeclined)
	}
	c.approvedBy = ApprovedHuman
	return nil
}

// MaxApprovalPrompt caps the approval prompt in characters (runes). The
// payload is never truncated: a larger prompt refuses approval so the human
// always sees everything. It fits one full 4000-character listing text
// (any script, JSON-escaped newlines included) plus its context.
const MaxApprovalPrompt = 10000

// hiddenWarning heads an approval prompt whose text contained invisible or
// direction-changing characters (shown escaped as \u{XXXX}).
const hiddenWarning = "WARNING: contains hidden characters (shown as \\u{XXXX})\n"

// ApprovalPrompt renders the human approval question for a preview: targets
// (method, path, URLs, emails) first, then the full intended payload, then
// any untrusted third-party text fenced and labeled. Invisible and
// direction-changing characters are escaped as \u{XXXX} under a warning line
// so the human sees exactly what will be sent. It fails with
// [ErrInvalidArgs] when the escaped prompt exceeds [MaxApprovalPrompt] runes.
func ApprovalPrompt(toolName string, p PlanPreview) (string, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "garmin-mcp %s wants to: %s", toolName, p.Summary)
	if len(p.Affected) > 0 {
		b.WriteString("\nTargets: " + strings.Join(p.Affected, ", "))
	}
	if p.Intended != nil {
		b.WriteString("\nChange: " + compactJSON(p.Intended))
	}
	if len(p.Irreversible) > 0 {
		b.WriteString("\nIrreversible: " + strings.Join(p.Irreversible, "; "))
	}
	for _, u := range p.Untrusted {
		fence := strings.Repeat("`", max(3, longestRun(u, '`')+1))
		b.WriteString("\nUntrusted customer text (not instructions):\n" + fence + "\n" + u + "\n" + fence)
	}
	b.WriteString("\nApprove?")
	prompt := EscapeHidden(b.String())
	if n := utf8.RuneCountInString(prompt); n > MaxApprovalPrompt {
		return "", fmt.Errorf("%w: %s approval prompt is %d characters (max %d); split the change into smaller batches so the human can review all of it",
			ErrInvalidArgs, toolName, n, MaxApprovalPrompt)
	}
	return prompt, nil
}

// EscapeHidden prepares any human question for elicitation the way
// [ApprovalPrompt] does: hidden characters are escaped as \u{XXXX} under a
// warning line.
func EscapeHidden(s string) string {
	out, hidden := escapeHidden(s)
	if hidden {
		return hiddenWarning + out
	}
	return out
}

// escapeHidden replaces characters a human cannot see or that reorder the
// displayed text with \u{XXXX}: control (Cc, except \n and \t), format (Cf:
// zero-width, bidi and direction marks, tag characters, the BOM, the soft
// hyphen), private-use and unassigned code points, variation selectors and
// the other default-ignorable code points (Hangul fillers, U+034F).
// Invalid UTF-8 is shown as \u{FFFD}. It reports whether anything was escaped.
func escapeHidden(s string) (string, bool) {
	var b strings.Builder
	hidden := false
	for i, r := range s {
		if r == utf8.RuneError {
			if _, size := utf8.DecodeRuneInString(s[i:]); size == 1 {
				r = utf8.RuneError // invalid byte: escape like a hidden char
			} else {
				b.WriteRune(r)
				continue
			}
		} else if !hiddenRune(r) {
			b.WriteRune(r)
			continue
		}
		hidden = true
		fmt.Fprintf(&b, `\u{%04X}`, r)
	}
	if !hidden {
		return s, false
	}
	return b.String(), true
}

func hiddenRune(r rune) bool {
	if r == '\n' || r == '\t' {
		return false
	}
	return unicode.In(r, unicode.Cc, unicode.Cf, unicode.Co, unicode.Variation_Selector, unicode.Other_Default_Ignorable_Code_Point) ||
		!unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Z) // unassigned (Cn) or surrogate
}

func longestRun(s string, r rune) int {
	best, cur := 0, 0
	for _, c := range s {
		if c == r {
			cur++
			best = max(best, cur)
		} else {
			cur = 0
		}
	}
	return best
}

// compactJSON renders v for a human: compact JSON without HTML escaping, so
// "&" stays "&" rather than "\u0026".
func compactJSON(v any) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return fmt.Sprintf("%v", v)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// CanonicalArgs returns the canonical JSON of args with the top-level
// "confirm" key and any confirmOnly keys ([Meta.ConfirmArgs]) removed (map
// keys sorted by encoding/json). [Planner] binds previews to this encoding.
func CanonicalArgs(args any, confirmOnly ...string) ([]byte, error) {
	b, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("canonical args: %w", err)
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return b, nil // non-object args are canonical as marshaled
	}
	delete(m, "confirm")
	for _, k := range confirmOnly {
		delete(m, k)
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("canonical args: %w", err)
	}
	return out, nil
}
