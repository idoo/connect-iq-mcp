package store

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/idoo/garmin-mcp/internal/auth"
	"github.com/idoo/garmin-mcp/internal/browser"
	"github.com/idoo/garmin-mcp/internal/portal"
	"github.com/idoo/garmin-mcp/internal/tool"
)

// Listing limits in UTF-16 code units (portal form limits).
const (
	maxTitle       = 50
	maxDescription = 4000
	maxWhatsNew    = 4000
	maxVersion     = 20
	maxIQ          = 400 << 20
	maxScreenshot  = 150 << 10
	maxIcon        = 300 << 10
	maxScreenshots = 5
)

const appDesc = "store UUID, manifest id, or exact app title"

var versionRE = regexp.MustCompile(`^[vV]?\d+(\.\d+)*([-+][0-9A-Za-z.-]+)?$`)

// listingKeys are the listing text keys of app metadata.
var listingKeys = []string{"appTitle", "appDescription", "appWhatsNew", "localizedAppName"}

// client returns a portal client whose writes drop the app cache.
func client(env *tool.Env) *portal.Client {
	c := portal.New(env.HTTP, env.Credentials)
	c.OnWrite = invalidateCache
	return c
}

// wrap maps portal errors to tool errors (auth_required, api_error with the
// portal code). Messages never carry cookies.
func wrap(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, browser.ErrRateLimited) {
		return fmt.Errorf("%w: %w", tool.ErrRateLimited, &tool.APIError{Code: "rate_limited", Detail: err.Error(), Hint: browser.ErrRateLimited.Error()})
	}
	if errors.Is(err, auth.ErrAuthRequired) {
		return fmt.Errorf("%w: %s", tool.ErrAuthRequired, err.Error())
	}
	if errors.Is(err, portal.ErrInvalidID) {
		return tool.InvalidArgs("%s", err.Error())
	}
	var rl *portal.RateLimitError
	if errors.As(err, &rl) {
		hint := "wait before retrying; garmin-mcp does not retry by itself"
		if s := int(rl.RetryAfter.Seconds()); s > 0 {
			hint = fmt.Sprintf("the portal asked to wait %d seconds (Retry-After) before retrying", s)
		}
		return fmt.Errorf("%w: %w", tool.ErrRateLimited, &tool.APIError{Status: rl.Status, Code: "rate_limited", Detail: rl.Error(), Hint: hint})
	}
	var ae *portal.APIError
	if errors.As(err, &ae) {
		return &tool.APIError{Status: ae.Status, Code: ae.Code, Detail: ae.Message, Hint: hintFor(ae.Code)}
	}
	return err
}

func hintFor(code string) string {
	switch code {
	case "APP_VERSION_ALREADY_EXISTS":
		return "this version is already uploaded; bump the version"
	case "SIGNATURE_CHECK_FAILED", "SIGNATURE_CHECK_FAILED_PREVIOUS_KEY_DIFFERENT":
		return "sign the .iq with the same developer key as the published version"
	case "APP_NOT_FOUND":
		return "call store_apps_list for the app ids and titles"
	case "TITLE_TOO_LONG", "DESCRIPTION_TOO_LONG", "WHATS_NEW_TOO_LONG":
		return "limits: title 50, description 4000, what's new 4000 UTF-16 units"
	}
	return ""
}

func resolve(ctx context.Context, c *portal.Client, ref string) (portal.App, error) {
	a, err := resolveApp(ctx, c, ref)
	return a, wrap(err)
}

func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		n++
		if r >= 0x10000 {
			n++
		}
	}
	return n
}

func checkVersion(v string) error {
	if len(v) > maxVersion || !versionRE.MatchString(v) {
		return tool.InvalidArgs("version %q must look like 1.2.3 and be at most %d characters", v, maxVersion)
	}
	return nil
}

// checkNewerVersion rejects v unless its dotted numbers are greater than
// current's (the live version). The portal never lets a version string be
// reused, and lower ones are older releases, so this runs before any upload.
// Suffixes such as -beta are ignored: 1.2.0-rc and 1.2.0 compare equal.
func checkNewerVersion(v, current string) error {
	if err := checkVersion(v); err != nil {
		return err
	}
	if strings.TrimSpace(current) == "" || !versionRE.MatchString(current) {
		return nil
	}
	if compareVersions(v, current) <= 0 {
		return tool.InvalidArgs("version %s must be greater than the live version %s; a published version string can never be reused", v, current)
	}
	return nil
}

// compareVersions compares the dotted numeric cores of two versions matched
// by versionRE (missing parts count as 0) and returns -1, 0 or 1.
func compareVersions(a, b string) int {
	pa, pb := versionCore(a), versionCore(b)
	for i := range max(len(pa), len(pb)) {
		x, y := "0", "0"
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		if c := cmp.Compare(len(x), len(y)); c != 0 {
			return c
		}
		if c := strings.Compare(x, y); c != 0 {
			return c
		}
	}
	return 0
}

// versionCore returns the numeric parts without leading zeros (so they
// compare by length, then lexically, without overflow).
func versionCore(v string) []string {
	v = strings.TrimLeft(v, "vV")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		v = v[:i]
	}
	parts := strings.Split(v, ".")
	for i, p := range parts {
		if p = strings.TrimLeft(p, "0"); p == "" {
			p = "0"
		}
		parts[i] = p
	}
	return parts
}

// readFile reads a project file through the confined root.
func readFile(env *tool.Env, path string, limit int64, exts ...string) (portal.File, error) {
	if strings.TrimSpace(path) == "" {
		return portal.File{}, tool.InvalidArgs("file path is empty")
	}
	ext := strings.ToLower(filepath.Ext(path))
	if len(exts) > 0 && !contains(exts, ext) {
		return portal.File{}, tool.InvalidArgs("%s: expected %s", filepath.Base(path), strings.Join(exts, ", "))
	}
	st, err := env.Root.Stat(path)
	if err != nil {
		return portal.File{}, err
	}
	if st.Size() > limit {
		return portal.File{}, tool.InvalidArgs("%s is %d bytes; the limit is %d", filepath.Base(path), st.Size(), limit)
	}
	data, err := env.Root.ReadFile(path)
	if err != nil {
		return portal.File{}, err
	}
	return portal.File{Name: filepath.Base(path), Data: data}, nil
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// locales returns metadata's localizedAppModel entries (shared maps).
func locales(md map[string]any) []map[string]any {
	raw, _ := md["localizedAppModel"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, e := range raw {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func localeOf(m map[string]any) string { s, _ := m["appLocale"].(string); return s }

func textOf(m map[string]any, key string) string { s, _ := m[key].(string); return s }

type appOut struct {
	ID     string `json:"app_id"`
	Name   string `json:"name"`
	Status string `json:"status,omitempty"`
}

func outApp(a portal.App) appOut { return appOut{ID: a.ID, Name: a.Name, Status: a.Status} }
