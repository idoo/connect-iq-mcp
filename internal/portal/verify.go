package portal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/idoo/ciq-mcp/internal/auth"
)

// baseURL is BaseURL; tests point it at a fake server.
var baseURL = BaseURL

const (
	devServices = "/api/ciq-developerservices"
	asw         = "/api/appsLibraryExternalServices/api/asw"
	referer     = BaseURL + "/en-US/developer/dashboard"
)

// Developer is the signed-in portal developer (GET /developers).
type Developer struct {
	ID        string `json:"userId"`
	Name      string `json:"name,omitempty"`
	Developer bool   `json:"developer"`
}

// Verify calls GET /api/ciq-developerservices/developers with cookieHeader,
// following no redirects. It returns the developer and the header with any
// Set-Cookie rotations applied. 3xx/401/403 wrap auth.ErrAuthRequired.
func Verify(ctx context.Context, hc *http.Client, cookieHeader string) (Developer, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+devServices+"/developers", nil)
	if err != nil {
		return Developer{}, cookieHeader, err
	}
	setHeaders(req, cookieHeader)
	res, err := noRedirect(hc).Do(req)
	if err != nil {
		return Developer{}, cookieHeader, fmt.Errorf("portal request failed: %w", err)
	}
	defer res.Body.Close()
	header := MergeSetCookie(cookieHeader, res.Header.Values("Set-Cookie"))
	body, _ := io.ReadAll(io.LimitReader(res.Body, maxBody))
	if err := statusError(res, body); err != nil {
		return Developer{}, header, err
	}
	var d Developer
	if err := json.Unmarshal(body, &d); err != nil || d.ID == "" {
		return Developer{}, header, fmt.Errorf("%w: the portal did not return a developer account", auth.ErrAuthRequired)
	}
	return d, header, nil
}

func setHeaders(req *http.Request, cookieHeader string) {
	req.Header.Set("Cookie", cookieHeader)
	req.Header.Set("Origin", BaseURL)
	req.Header.Set("Referer", referer)
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json")
	}
}

// noRedirect returns a shallow copy of hc that never follows redirects, so
// cookies never leave apps.garmin.com and a dead session shows up as 3xx.
func noRedirect(hc *http.Client) *http.Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	c := *hc
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}
