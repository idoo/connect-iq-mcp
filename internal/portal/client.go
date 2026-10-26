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
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/idoo/ciq-mcp/internal/auth"
)

// maxBody caps how much of a portal response is read.
const maxBody = 32 << 20

// ServerOnlyFields are returned by GET …/metadata but must not be sent back on
// PUT …/apps/{appId}.
var ServerOnlyFields = []string{"appIqFileInfoResult", "appFileCoordinates", "appTypeName", "isFeatured", "permissionNotification", "typeId"}

// APIError is a non-auth portal error decoded from {code,message} or
// {validationResult:{…}}. HTML bodies are never kept (they may echo cookies).
type APIError struct {
	Status  int
	Code    string
	Message string
	// Fields is validationResult (field or code → code).
	Fields map[string]string
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "portal HTTP %d", e.Status)
	if e.Code != "" {
		b.WriteString(" " + e.Code)
	}
	if e.Message != "" && e.Message != e.Code {
		b.WriteString(": " + e.Message)
	}
	return b.String()
}

// HasCode reports whether err is an APIError carrying code (as Code or in Fields).
func HasCode(err error, code string) bool {
	var ae *APIError
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

func statusError(res *http.Response, body []byte) error {
	c := res.StatusCode
	if (c >= 300 && c < 400) || c == http.StatusUnauthorized || c == http.StatusForbidden {
		return fmt.Errorf("%w: the developer portal answered HTTP %d (not signed in or session expired)", auth.ErrAuthRequired, c)
	}
	if c < 400 {
		return nil
	}
	ae := &APIError{Status: c}
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

	mu    sync.Mutex
	devID string
}

// New returns a client over hc (the write-guarded env.HTTP) and creds.
func New(hc *http.Client, creds auth.Store) *Client {
	return &Client{hc: noRedirect(hc), creds: creds}
}

// File is an in-memory upload part.
type File struct {
	Name string
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
	res, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("portal %s %s: %w", r.method, r.path, err)
	}
	defer res.Body.Close()
	if sc := res.Header.Values("Set-Cookie"); len(sc) > 0 {
		if merged := MergeSetCookie(cookie, sc); merged != cookie {
			if err := c.creds.SavePortalCookies(ctx, merged); err != nil {
				return fmt.Errorf("saving rotated portal cookies: %w", err)
			}
		}
	}
	data, err := io.ReadAll(io.LimitReader(res.Body, maxBody))
	if err != nil {
		return fmt.Errorf("portal %s %s: reading response: %w", r.method, r.path, err)
	}
	if err := statusError(res, data); err != nil {
		return err
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

// ResolveApp finds an app by store UUID, manifest id or exact title
// (case-insensitive) and returns it.
func (c *Client) ResolveApp(ctx context.Context, ref string) (App, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return App{}, errors.New("app is required (store UUID, manifest id or title)")
	}
	apps, err := c.Apps(ctx)
	if err != nil {
		return App{}, err
	}
	norm := NormalizeID(ref)
	for _, a := range apps {
		if NormalizeID(a.ID) == norm {
			return a, nil
		}
	}
	var byName []App
	for _, a := range apps {
		if strings.EqualFold(strings.TrimSpace(a.Name), ref) {
			byName = append(byName, a)
		}
	}
	if len(byName) == 1 {
		return byName[0], nil
	}
	if len(byName) > 1 {
		return App{}, fmt.Errorf("%d apps are titled %q; pass the store UUID", len(byName), ref)
	}
	if len(norm) == 32 {
		for _, a := range apps {
			md, err := c.AppMetadata(ctx, a.ID)
			if err != nil {
				return App{}, err
			}
			if NormalizeID(ManifestID(md)) == norm {
				return a, nil
			}
		}
	}
	return App{}, &APIError{Status: http.StatusNotFound, Code: "APP_NOT_FOUND", Message: fmt.Sprintf("no app matches %q (store UUID, manifest id or exact title)", ref)}
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

// FullInstalls returns install statistics.
func (c *Client) FullInstalls(ctx context.Context, appID, start, end, temporalLevel string) (any, error) {
	q := url.Values{"locale": {"en"}}
	for k, v := range map[string]string{"start": start, "end": end, "temporalLevel": temporalLevel} {
		if v != "" {
			q.Set(k, v)
		}
	}
	var out any
	err := c.do(ctx, request{method: http.MethodGet, path: asw + "/" + p(appID) + "/fullInstalls", query: q}, &out)
	return out, err
}

// CompatibleDevices lists the devices of an app version ("" = current).
func (c *Client) CompatibleDevices(ctx context.Context, appID, internalVersion string) (any, error) {
	var q url.Values
	if internalVersion != "" {
		q = url.Values{"internalAppVersion": {internalVersion}}
	}
	var out any
	err := c.do(ctx, request{method: http.MethodGet, path: asw + "/apps/" + p(appID) + "/compatibleDevices", query: q}, &out)
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
