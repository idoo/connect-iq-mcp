package store

import (
	"cmp"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/idoo/connect-iq-mcp/internal/portal"
	"github.com/idoo/connect-iq-mcp/internal/tool"
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

var versionRE = regexp.MustCompile(`^[vV]?\d+(\.\d+)*([-+][0-9A-Za-z.-]+)?$`)

// listingKeys are the listing text keys of app metadata.
var listingKeys = []string{"appTitle", "appDescription", "appWhatsNew", "localizedAppName"}

// client returns a portal client, paired with the server's app cache, whose
// writes drop that cache.
func client(env *tool.Env) *storeClient {
	c := &storeClient{Client: portal.New(env.HTTP, env.Credentials), cache: cacheOf(env)}
	c.OnWrite = c.cache.invalidate
	return c
}

// upstreamHints maps portal errors to tool errors (auth_required,
// rate_limited, api_error with the portal code) through [tool.FromUpstream].
// Messages never carry cookies.
var upstreamHints = tool.UpstreamHints{
	Codes:        codeHints,
	Blocked:      portal.ErrBlocked,
	BlockedHint:  "Cloudflare blocked the request; do not call auth_login (the session is valid). Wait a few minutes, then retry; if it persists, ask the user to open apps.garmin.com in their browser",
	InvalidID:    portal.ErrInvalidID,
	RateLimitAPI: true,
}

// codeHints are the agent hints for portal error codes.
var codeHints = map[string]string{
	"APP_VERSION_ALREADY_EXISTS":                    "this version is already uploaded; bump the version",
	"SIGNATURE_CHECK_FAILED":                        "sign the .iq with the same developer key as the published version",
	"SIGNATURE_CHECK_FAILED_PREVIOUS_KEY_DIFFERENT": "sign the .iq with the same developer key as the published version",
	"APP_NOT_FOUND":                                 "call store_apps_list for the app ids and titles",
	"forbidden":                                     "the session is valid (auth_status shows it), so do not call auth_login; the portal refused this action for this app or account. Check the app id with store_apps_list, or ask the user to try it on apps.garmin.com",
	"TITLE_TOO_LONG":                                "limits: title 50, description 4000, what's new 4000 UTF-16 units",
	"DESCRIPTION_TOO_LONG":                          "limits: title 50, description 4000, what's new 4000 UTF-16 units",
	"WHATS_NEW_TOO_LONG":                            "limits: title 50, description 4000, what's new 4000 UTF-16 units",
}

func resolve(ctx context.Context, c *storeClient, ref string) (portal.App, error) {
	a, err := resolveApp(ctx, c, ref)
	return a, tool.FromUpstream(err, upstreamHints)
}

// written is the result of a successful write. It carries the client's
// non-fatal warnings (a rotated cookie that could not be saved).
func written(c *storeClient, data any) *tool.Result {
	return withWarnings(c, tool.OK(data))
}

func withWarnings(c *storeClient, res *tool.Result) *tool.Result {
	for _, w := range c.Warnings() {
		res.Warn(w)
	}
	return res
}

// outcomeUnknown reports a write that got no HTTP response (timeout, reset,
// cancelled): Garmin may or may not have applied it. A request refused by
// the local write guard never left the process, so it is not unknown.
func outcomeUnknown(err error) bool {
	return errors.Is(err, portal.ErrNoResponse) && !errors.Is(err, tool.ErrWriteGuard)
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
	switch compareVersions(v, current) {
	case 0:
		return tool.InvalidArgs("version %s is already the live version: it may already be published by you (for example by an earlier call whose response was lost); check store_app_get before choosing a new version, and do not publish the same binary again under a bumped one", v)
	case -1:
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

// readFile reads a project file for upload through the confined root. Every
// component below the root must be a real directory or regular file (no
// symlinks, as for writes), and the content must start with the signature
// of its extension, so a link or a renamed secret is never uploaded under an
// image or .iq name. File.Path is the root-relative path for approvals.
func readFile(env *tool.Env, path string, limit int64, exts ...string) (portal.File, error) {
	if strings.TrimSpace(path) == "" {
		return portal.File{}, tool.InvalidArgs("file path is empty")
	}
	ext := strings.ToLower(filepath.Ext(path))
	if len(exts) > 0 && !contains(exts, ext) {
		return portal.File{}, tool.InvalidArgs("%s: expected %s", filepath.Base(path), strings.Join(exts, ", "))
	}
	rel, err := uploadPath(env.Root, path)
	if err != nil {
		return portal.File{}, err
	}
	st, err := env.Root.Stat(path)
	if err != nil {
		return portal.File{}, err
	}
	if st.Size() > limit {
		return portal.File{}, tool.InvalidArgs("%s is %d bytes; the limit is %d", rel, st.Size(), limit)
	}
	data, err := env.Root.ReadFile(path)
	if err != nil {
		return portal.File{}, err
	}
	if sigs, ok := fileMagic[ext]; ok && !slices.ContainsFunc(sigs, func(s string) bool { return strings.HasPrefix(string(data[:min(len(data), len(s))]), s) }) {
		return portal.File{}, tool.InvalidArgs("%s is not a %s file (its content does not start with the %s signature); pass the real file", rel, strings.TrimPrefix(ext, "."), strings.TrimPrefix(ext, "."))
	}
	return portal.File{Name: filepath.Base(rel), Path: rel, Data: data}, nil
}

// fileMagic holds the accepted leading bytes per upload extension (.iq is a
// ZIP container).
var fileMagic = map[string][]string{
	".png":  {"\x89PNG\r\n\x1a\n"},
	".jpg":  {"\xff\xd8\xff"},
	".jpeg": {"\xff\xd8\xff"},
	".gif":  {"GIF87a", "GIF89a"},
	".iq":   {"PK\x03\x04"},
}

// uploadPath returns path relative to the root (slash-separated) after
// checking with Lstat that no component below the root is a symlink (or
// junction) and that the last one is a regular file. Links above the root
// (macOS /tmp → /private/tmp) are fine: the root is found as the shortest
// lexical prefix of path that is the root directory.
func uploadPath(files tool.Files, path string) (string, error) {
	if _, err := files.Resolve(path); err != nil {
		return "", err
	}
	root, err := files.Resolve(".")
	if err != nil {
		return "", err
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	lex := path
	if !filepath.IsAbs(lex) {
		lex = filepath.Join(root, lex)
	}
	lex = filepath.Clean(lex)
	var prefixes []string
	for p := lex; ; p = filepath.Dir(p) {
		prefixes = append(prefixes, p)
		if filepath.Dir(p) == p {
			break
		}
	}
	rel := ""
	for _, p := range slices.Backward(prefixes) {
		if fi, err := os.Stat(p); err == nil && os.SameFile(fi, rootInfo) {
			rel, err = filepath.Rel(p, lex)
			if err != nil {
				return "", err
			}
			break
		}
	}
	if rel == "" || rel == "." {
		return "", tool.InvalidArgs("%s reaches the project root only through a symlink; pass a path inside the root", path)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	cur := root
	for i, part := range parts {
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			return "", err
		}
		shown := filepath.ToSlash(filepath.Join(parts[:i+1]...))
		switch {
		case fi.Mode()&fs.ModeSymlink != 0:
			return "", tool.InvalidArgs("%s is a symlink; uploads must be regular files inside the root, not links (pass the real file)", shown)
		case i < len(parts)-1 && !fi.IsDir():
			return "", tool.InvalidArgs("%s is not a directory", shown)
		case i == len(parts)-1 && !fi.Mode().IsRegular():
			return "", tool.InvalidArgs("%s is not a regular file", shown)
		}
	}
	return filepath.ToSlash(rel), nil
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
