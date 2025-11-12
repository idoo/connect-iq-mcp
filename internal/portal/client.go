package portal

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/idoo/connect-iq-mcp/internal/auth"
	"github.com/idoo/connect-iq-mcp/internal/httpx"
	"github.com/idoo/connect-iq-mcp/internal/upstream"
)

// maxBody caps how much of a portal response is read.
const maxBody = 32 << 20

// ServerOnlyFields are returned by GET …/metadata but must not be sent back on
// PUT …/apps/{appId}.
var ServerOnlyFields = []string{"appIqFileInfoResult", "appFileCoordinates", "appTypeName", "isFeatured", "permissionNotification", "typeId"}

// HasCode reports whether err is an upstream.HTTPError carrying code (as Code or in Fields).
func HasCode(err error, code string) bool {
	var ae *upstream.HTTPError
	if !errors.As(err, &ae) {
		return false
	}
	if ae.Code == code {
		return true
	}
	for k, v := range ae.Fields {
		if k == code || v == code {
			return true
		}
	}
	return false
}

// idRE is the shape of every portal path segment: UUIDs, numeric ids,
// developer ids, reference names and the fixed route words.
var idRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

// ErrInvalidID marks an id that cannot be a single portal path segment.
var ErrInvalidID = errors.New("invalid portal id")

// CheckID rejects id unless it is one safe path segment (so "", ".", "..",
// "a/b" and "a?b" never reach a URL). name is the argument name for the error.
func CheckID(name, id string) error {
	if !idRE.MatchString(id) {
		return fmt.Errorf("%w: %s %.64q must match %s", ErrInvalidID, name, id, idRE)
	}
	return nil
}

// checkPath validates every segment of a request path before it is sent.
func checkPath(path string) error {
	for seg := range strings.SplitSeq(strings.TrimPrefix(path, "/"), "/") {
		if !idRE.MatchString(seg) {
			if u, err := url.PathUnescape(seg); err == nil {
				seg = u
			}
			return fmt.Errorf("%w: path segment %.64q must match %s", ErrInvalidID, seg, idRE)
		}
	}
	return nil
}

// ErrBlocked is a Cloudflare block page (WAF rule, bot check, IP ban) in
// front of the portal. The session is fine; signing in again does not help.
var ErrBlocked = errors.New("blocked by Cloudflare in front of the developer portal (the session itself is fine; signing in again will not help); wait a few minutes and retry, or open apps.garmin.com in a browser to clear the check")

// ErrNoResponse marks a request that was sent (or may have been) but got no
// HTTP status back: timeout, connection reset, cancelled context. For a
// write the outcome is unknown.
var ErrNoResponse = errors.New("no response from the developer portal")

// developersPath is the session probe: a 403 here means "not signed in".
const developersPath = devServices + "/developers"

// cloudflareCode returns the Cloudflare "error code: 10xx" of a block page,
// "challenge" for a bot check or Attention Required page, or "" when the
// response is not a Cloudflare page.
func cloudflareCode(res *http.Response, body []byte) string {
	if res.Header.Get("Cf-Ray") == "" && !strings.EqualFold(res.Header.Get("Server"), "cloudflare") {
		return ""
	}
	if res.Header.Get("Cf-Mitigated") != "" {
		return "challenge"
	}
	if m := cfCodeRE.FindSubmatch(body); m != nil {
		return string(m[1])
	}
	if bytes.Contains(body, []byte("Attention Required")) {
		return "challenge"
	}
	return ""
}

var cfCodeRE = regexp.MustCompile(`(?i)error code:?\s*(10\d\d)\b`)

func statusError(res *http.Response, body []byte) error {
	c := res.StatusCode
	if c >= 400 {
		switch cloudflareCode(res, body) {
		case "":
		case "1015":
			return &upstream.HTTPError{Service: upstream.Portal, Status: c, RateLimited: true, RetryAfter: httpx.RetryAfter(res.Header.Get("Retry-After"), time.Now()), Cloudflare: true}
		default:
			return fmt.Errorf("%w (HTTP %d)", ErrBlocked, c)
		}
	}
	if (c >= 300 && c < 400) || c == http.StatusUnauthorized || (c == http.StatusForbidden && res.Request != nil && strings.HasSuffix(res.Request.URL.Path, developersPath)) {
		return fmt.Errorf("%w: the developer portal answered HTTP %d (not signed in or session expired)", auth.ErrAuthRequired, c)
	}
	if c == http.StatusForbidden {
		return &upstream.HTTPError{Service: upstream.Portal, Status: c, Code: "forbidden", Message: "forbidden for this endpoint: the session is valid but the portal refused this action (no permission for this app or operation)"}
	}
	if ra := res.Header.Get("Retry-After"); c == http.StatusTooManyRequests || (c == http.StatusServiceUnavailable && ra != "") {
		return &upstream.HTTPError{Service: upstream.Portal, Status: c, RateLimited: true, RetryAfter: httpx.RetryAfter(ra, time.Now())}
	}
	if c < 400 {
		return nil
	}
	// A non-auth error decoded from {code,message} or {validationResult:{…}}.
	// HTML bodies are never kept (they may echo cookies).
	ae := &upstream.HTTPError{Service: upstream.Portal, Status: c}
	var raw struct {
		Code             any               `json:"code"`
		Message          string            `json:"message"`
		ValidationResult map[string]string `json:"validationResult"`
	}
	if json.Unmarshal(body, &raw) == nil {
		if raw.Code != nil {
			ae.Code = fmt.Sprint(raw.Code)
		}
		ae.Message = raw.Message
		ae.Fields = raw.ValidationResult
		if ae.Code == "" && raw.Message != "" {
			if p, _, ok := strings.Cut(raw.Message, ":"); ok && p == strings.ToUpper(p) && !strings.Contains(p, " ") {
				ae.Code = p
			}
		}
		if ae.Code == "" && len(ae.Fields) > 0 {
			keys := make([]string, 0, len(ae.Fields))
			for k := range ae.Fields {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			ae.Code = ae.Fields[keys[0]]
			if ae.Message == "" {
				parts := make([]string, len(keys))
				for i, k := range keys {
					parts[i] = k + "=" + ae.Fields[k]
				}
				ae.Message = strings.Join(parts, ", ")
			}
		}
	}
	if ae.Message == "" && ae.Code == "" {
		ae.Message = http.StatusText(c)
	}
	return ae
}

// Client calls the developer portal with the stored cookies. It never
// follows redirects and persists Set-Cookie rotations.
type Client struct {
	hc    *http.Client
	creds auth.Store

	// OnWrite, when set, runs after every non-GET request is attempted
	// (success or not), so callers can drop cached portal state.
	OnWrite func()

	mu       sync.Mutex
	devID    string
	warnings []string
}

// Warnings returns and clears the non-fatal problems seen by writes (a
// rotated cookie that could not be saved after a successful write).
func (c *Client) Warnings() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	w := c.warnings
	c.warnings = nil
	return w
}

// New returns a client over hc (the write-guarded env.HTTP) and creds.
func New(hc *http.Client, creds auth.Store) *Client {
	return &Client{hc: noRedirect(hc), creds: creds}
}

// File is an in-memory upload part.
type File struct {
	// Name is the multipart file name (the base name).
	Name string
	// Path is the root-relative path shown in approvals ("" when unknown).
	Path string
	Data []byte
}

type request struct {
	method, path string
	query        url.Values
	body         []byte
	ctype        string
	accept       string
}

func (c *Client) do(ctx context.Context, r request, out any) error {
	if err := checkPath(r.path); err != nil {
		return err
	}
	if c.creds == nil {
		return fmt.Errorf("%w: no credentials configured", auth.ErrAuthRequired)
	}
	cookie, err := c.creds.PortalCookies(ctx)
	if err != nil {
		return err
	}
	u := baseURL + r.path
	if len(r.query) > 0 {
		u += "?" + r.query.Encode()
	}
	var body io.Reader
	if r.body != nil {
		body = bytes.NewReader(r.body)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, u, body)
	if err != nil {
		return err
	}
	if r.accept != "" {
		req.Header.Set("Accept", r.accept)
	}
	setHeaders(req, cookie)
	if r.ctype != "" {
		req.Header.Set("Content-Type", r.ctype)
	}
	if r.method != http.MethodGet && c.OnWrite != nil {
		defer c.OnWrite()
	}
	res, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("portal %s %s: %w: %w", r.method, r.path, ErrNoResponse, err)
	}
	defer res.Body.Close()
	var saveErr error
	if sc := res.Header.Values("Set-Cookie"); len(sc) > 0 && MergeSetCookie(cookie, sc) != cookie {
		saveErr = c.creds.SavePortalCookies(ctx, cookie, sc)
		// A GET may fail on it (nothing changed upstream); a write has
		// already happened, so failing it would invite a duplicate retry.
		if saveErr != nil && r.method == http.MethodGet {
			return fmt.Errorf("saving rotated portal cookies: %w", saveErr)
		}
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, maxBody))
	if err != nil {
		return fmt.Errorf("portal %s %s: reading response: %w", r.method, r.path, err)
	}
	if err := statusError(res, data); err != nil {
		return err
	}
	if saveErr != nil {
		c.mu.Lock()
		c.warnings = append(c.warnings, "the portal rotated its session cookie but saving it failed ("+saveErr.Error()+"); the write itself went through, do not repeat it. If later calls fail with auth_required, call auth_login")
		c.mu.Unlock()
	}
	switch o := out.(type) {
	case nil:
		return nil
	case *string:
		*o = strings.TrimSpace(string(data))
		return nil
	default:
		if len(bytes.TrimSpace(data)) == 0 {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("portal %s %s: unexpected response (%d bytes): %w", r.method, r.path, len(data), err)
		}
		return nil
	}
}

func p(s string) string { return url.PathEscape(s) }

// Developer returns the signed-in developer.
func (c *Client) Developer(ctx context.Context) (Developer, error) {
	var d Developer
	if err := c.do(ctx, request{method: http.MethodGet, path: devServices + "/developers"}, &d); err != nil {
		return d, err
	}
	if d.ID == "" {
		return d, fmt.Errorf("%w: the portal did not return a developer account", auth.ErrAuthRequired)
	}
	c.mu.Lock()
	c.devID = d.ID
	c.mu.Unlock()
	return d, nil
}

// DeveloperID returns the cached developer id, fetching it once.
func (c *Client) DeveloperID(ctx context.Context) (string, error) {
	c.mu.Lock()
	id := c.devID
	c.mu.Unlock()
	if id != "" {
		return id, nil
	}
	d, err := c.Developer(ctx)
	return d.ID, err
}

func (c *Client) devPath(ctx context.Context, suffix string) (string, error) {
	id, err := c.DeveloperID(ctx)
	if err != nil {
		return "", err
	}
	return devServices + "/developers/" + p(id) + suffix, nil
}

// App is one entry of the developer's apps list.
type App struct {
	ID     string         `json:"app_id"`
	Name   string         `json:"name"`
	Status string         `json:"status,omitempty"`
	Raw    map[string]any `json:"-"`
}

// Apps lists the developer's apps (all statuses).
func (c *Client) Apps(ctx context.Context) ([]App, error) {
	path, err := c.devPath(ctx, "/apps")
	if err != nil {
		return nil, err
	}
	var raw []map[string]any
	q := url.Values{"locale": {"en"}, "displayOnlyApprovedApps": {"false"}}
	if err := c.do(ctx, request{method: http.MethodGet, path: path, query: q}, &raw); err != nil {
		return nil, err
	}
	out := make([]App, 0, len(raw))
	for _, m := range raw {
		out = append(out, App{ID: str(m["appId"]), Name: str(m["localizedAppName"]), Status: str(m["status"]), Raw: m})
	}
	return out, nil
}

func str(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

// AppMetadata returns the editable metadata of an app.
func (c *Client) AppMetadata(ctx context.Context, appID string) (map[string]any, error) {
	path, err := c.devPath(ctx, "/apps/"+p(appID)+"/metadata")
	if err != nil {
		return nil, err
	}
	var md map[string]any
	err = c.do(ctx, request{method: http.MethodGet, path: path, query: url.Values{"isShadowApp": {"false"}}}, &md)
	return md, err
}

// NormalizeID lowercases a UUID and strips dashes (manifest ids appear both ways).
func NormalizeID(s string) string {
	return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "-", ""))
}

// ManifestID returns appIqFileInfoResult.manifestAppId of metadata.
func ManifestID(md map[string]any) string {
	info, _ := md["appIqFileInfoResult"].(map[string]any)
	return str(info["manifestAppId"])
}

// Validation is the result of POST /iqFiles/validate.
type Validation struct {
	TempFileID string         `json:"tempFileId"`
	Info       map[string]any `json:"iqFileInfoResult"`
}

// ManifestAppID is the validated file's manifest id.
func (v Validation) ManifestAppID() string { return str(v.Info["manifestAppId"]) }

// SignatureOK reports signatureCheckSuccessFul (false when absent).
func (v Validation) SignatureOK() bool { b, _ := v.Info["signatureCheckSuccessFul"].(bool); return b }

// Devices is compatibleDevices.
func (v Validation) Devices() []any { d, _ := v.Info["compatibleDevices"].([]any); return d }

func multipartBody(build func(w *multipart.Writer) error) ([]byte, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := build(w); err != nil {
		return nil, "", err
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

func filePart(w *multipart.Writer, field, name, ctype string, data []byte) error {
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name=%q; filename=%q`, field, name))
	h.Set("Content-Type", ctype)
	pw, err := w.CreatePart(h)
	if err != nil {
		return err
	}
	_, err = pw.Write(data)
	return err
}

func jsonPart(w *multipart.Writer, field string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return filePart(w, field, "blob", "application/json", b)
}

// ImageType returns the MIME type for an image file name.
func ImageType(name string) string {
	n := strings.ToLower(name)
	switch {
	case strings.HasSuffix(n, ".png"):
		return "image/png"
	case strings.HasSuffix(n, ".gif"):
		return "image/gif"
	default:
		return "image/jpeg"
	}
}

// ValidateIQ uploads a .iq for validation (a temp upload; nothing public).
// appID is empty for a brand-new app.
func (c *Client) ValidateIQ(ctx context.Context, appID, version string, iq File) (Validation, error) {
	body, ct, err := multipartBody(func(w *multipart.Writer) error {
		return filePart(w, "appBinaryFile", iq.Name, "application/octet-stream", iq.Data)
	})
	if err != nil {
		return Validation{}, err
	}
	q := url.Values{"isNewAppVersion": {strconv.FormatBool(appID != "")}, "appVersion": {version}}
	if appID != "" {
		q.Set("appId", appID)
	}
	var v Validation
	err = c.do(ctx, request{method: http.MethodPost, path: devServices + "/iqFiles/validate", query: q, body: body, ctype: ct}, &v)
	if err == nil && v.TempFileID == "" {
		err = errors.New("validate returned no tempFileId")
	}
	return v, err
}

// PostVersion publishes a validated file as a new version of appID.
func (c *Client) PostVersion(ctx context.Context, appID, tempFileID, version string) error {
	path, err := c.devPath(ctx, "/apps/"+p(appID)+"/version")
	if err != nil {
		return err
	}
	b, _ := json.Marshal(map[string]string{"tempFileId": tempFileID, "appVersion": version})
	q := url.Values{"locale": {"en"}, "juniorAppUpdate": {"false"}}
	return c.do(ctx, request{method: http.MethodPost, path: path, query: q, body: b, ctype: "application/json"}, nil)
}

// StripServerOnly returns a copy of md without server-only fields.
func StripServerOnly(md map[string]any) map[string]any {
	out := make(map[string]any, len(md))
	for k, v := range md {
		if !slices.Contains(ServerOnlyFields, k) {
			out[k] = v
		}
	}
	return out
}

// UpdateAppMetadata PUTs the full metadata (the listing is replaced as a
// whole: always GET → modify → PUT). Server-only fields are stripped.
func (c *Client) UpdateAppMetadata(ctx context.Context, appID string, md map[string]any) error {
	path, err := c.devPath(ctx, "/apps/"+p(appID))
	if err != nil {
		return err
	}
	body, ct, err := multipartBody(func(w *multipart.Writer) error { return jsonPart(w, "appMetadata", StripServerOnly(md)) })
	if err != nil {
		return err
	}
	return c.do(ctx, request{method: http.MethodPut, path: path, body: body, ctype: ct}, nil)
}

// ReviewsQuery pages GET …/asw/apps/{appId}/reviews.
type ReviewsQuery struct {
	Page, PageSize int
	SortType       string
	Ascending      bool
	LatestOnly     bool
	Locale         string
}

// Reviews lists an app's reviews (third-party text).
func (c *Client) Reviews(ctx context.Context, appID string, rq ReviewsQuery) (any, error) {
	q := url.Values{
		"startPageIndex":    {strconv.Itoa(rq.Page)},
		"pageSize":          {strconv.Itoa(rq.PageSize)},
		"ascending":         {strconv.FormatBool(rq.Ascending)},
		"latestVersionOnly": {strconv.FormatBool(rq.LatestOnly)},
	}
	if rq.SortType != "" {
		q.Set("sortType", rq.SortType)
	}
	if rq.Locale != "" {
		q.Set("locale", rq.Locale)
	}
	var out any
	err := c.do(ctx, request{method: http.MethodGet, path: asw + "/apps/" + p(appID) + "/reviews", query: q}, &out)
	return out, err
}

// ReviewDistribution returns the rating histogram.
func (c *Client) ReviewDistribution(ctx context.Context, appID string) (any, error) {
	var out any
	err := c.do(ctx, request{method: http.MethodGet, path: asw + "/apps/" + p(appID) + "/reviews/distribution"}, &out)
	return out, err
}

// CreateReply posts a public developer reply to a review.
func (c *Client) CreateReply(ctx context.Context, appID, reviewID, text string) (any, error) {
	b, _ := json.Marshal(map[string]string{"reviewReplyText": text})
	var out any
	err := c.do(ctx, request{method: http.MethodPost, path: asw + "/apps/" + p(appID) + "/reviews/" + p(reviewID) + "/reviewreplies", body: b, ctype: "application/json"}, &out)
	return out, err
}

// DeleteReply deletes a developer reply.
func (c *Client) DeleteReply(ctx context.Context, appID, replyID string) error {
	var s string
	return c.do(ctx, request{method: http.MethodDelete, path: asw + "/apps/" + p(appID) + "/reviewreplies/" + p(replyID), query: url.Values{"locale": {"en"}}, accept: "*/*"}, &s)
}

// FullInstalls returns install statistics between start and end
// (YYYY-MM-DD, both required). The response always carries daily, weekly
// and monthly buckets.
func (c *Client) FullInstalls(ctx context.Context, appID, start, end string) (any, error) {
	q := url.Values{"locale": {"en"}, "start": {start}, "end": {end}}
	var out any
	err := c.do(ctx, request{method: http.MethodGet, path: asw + "/" + p(appID) + "/fullInstalls", query: q}, &out)
	return out, err
}

// Reference returns GET /references/{typ} (languages, categories, countries…).
func (c *Client) Reference(ctx context.Context, typ string) (any, error) {
	var out any
	err := c.do(ctx, request{method: http.MethodGet, path: devServices + "/references/" + p(typ)}, &out)
	return out, err
}

func juniorQ(order int) url.Values {
	q := url.Values{"juniorAppUpdate": {"false"}}
	if order >= 0 {
		q.Set("orderValue", strconv.Itoa(order))
	}
	return q
}

// AddScreenshot uploads a screenshot at position order.
func (c *Client) AddScreenshot(ctx context.Context, appID string, order int, f File) (any, error) {
	path, err := c.devPath(ctx, "/apps/"+p(appID)+"/screenshots")
	if err != nil {
		return nil, err
	}
	body, ct, err := multipartBody(func(w *multipart.Writer) error {
		return filePart(w, "screenshotFile", f.Name, ImageType(f.Name), f.Data)
	})
	if err != nil {
		return nil, err
	}
	var out any
	err = c.do(ctx, request{method: http.MethodPost, path: path, query: juniorQ(order), body: body, ctype: ct}, &out)
	return out, err
}

// ReorderScreenshot moves a screenshot to position order.
func (c *Client) ReorderScreenshot(ctx context.Context, appID, shotID string, order int) error {
	path, err := c.devPath(ctx, "/apps/"+p(appID)+"/screenshots/"+p(shotID))
	if err != nil {
		return err
	}
	return c.do(ctx, request{method: http.MethodPut, path: path, query: juniorQ(order)}, nil)
}

// DeleteScreenshot removes a screenshot.
func (c *Client) DeleteScreenshot(ctx context.Context, appID, shotID string) error {
	path, err := c.devPath(ctx, "/apps/"+p(appID)+"/screenshots/"+p(shotID))
	if err != nil {
		return err
	}
	return c.do(ctx, request{method: http.MethodDelete, path: path, query: juniorQ(-1)}, nil)
}

// CreateApp submits a brand-new app (goes to review) and returns its store UUID.
func (c *Client) CreateApp(ctx context.Context, md map[string]any, icon File, shots []File) (string, error) {
	path, err := c.devPath(ctx, "/apps")
	if err != nil {
		return "", err
	}
	body, ct, err := multipartBody(func(w *multipart.Writer) error {
		if err := jsonPart(w, "appMetadata", md); err != nil {
			return err
		}
		if err := filePart(w, "iconFile", icon.Name, ImageType(icon.Name), icon.Data); err != nil {
			return err
		}
		for _, s := range shots {
			if err := filePart(w, "screenshotFiles", s.Name, ImageType(s.Name), s.Data); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	var id string
	err = c.do(ctx, request{method: http.MethodPost, path: path, query: url.Values{"locale": {"en"}}, body: body, ctype: ct, accept: "*/*"}, &id)
	return strings.Trim(id, `"`), err
}

// DeleteApp deletes an app from the store.
func (c *Client) DeleteApp(ctx context.Context, appID string) error {
	path, err := c.devPath(ctx, "/apps/"+p(appID))
	if err != nil {
		return err
	}
	return c.do(ctx, request{method: http.MethodDelete, path: path, query: url.Values{"locale": {"en"}, "juniorAppDelete": {"false"}}}, nil)
}
