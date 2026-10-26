package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/idoo/ciq-mcp/internal/auth"
	"github.com/idoo/ciq-mcp/internal/portal"
	"github.com/idoo/ciq-mcp/internal/tool"
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

func client(env *tool.Env) *portal.Client { return portal.New(env.HTTP, env.Credentials) }

// wrap maps portal errors to tool errors (auth_required, api_error with the
// portal code). Messages never carry cookies.
func wrap(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, auth.ErrAuthRequired) {
		return fmt.Errorf("%w: %s", tool.ErrAuthRequired, err.Error())
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
	a, err := c.ResolveApp(ctx, ref)
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

// readFile reads a project file through the confined root.
func readFile(env *tool.Env, path string, max int64, exts ...string) (portal.File, error) {
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
	if st.Size() > max {
		return portal.File{}, tool.InvalidArgs("%s is %d bytes; the limit is %d", filepath.Base(path), st.Size(), max)
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
