package store

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/idoo/garmin-mcp/internal/portal"
	"github.com/idoo/garmin-mcp/internal/tool"
)

type noIn struct{}

type appGetIn struct {
	App     string `json:"app" jsonschema:"store UUID, manifest id, or exact app title"`
	Devices bool   `json:"devices,omitempty" jsonschema:"include the full compatible device list (default: count only)"`
}

type appIn struct {
	App string `json:"app" jsonschema:"store UUID, manifest id, or exact app title"`
}

type reviewsIn struct {
	App               string `json:"app" jsonschema:"store UUID, manifest id, or exact app title"`
	Page              int    `json:"page,omitempty" jsonschema:"0-based page index"`
	PageSize          int    `json:"page_size,omitempty" jsonschema:"1-30 (portal limit), default 20"`
	SortType          string `json:"sort_type,omitempty" jsonschema:"portal sortType passed through as is; accepted values were never verified (omit for the portal default order)"`
	Ascending         bool   `json:"ascending,omitempty" jsonschema:"oldest first instead of newest first"`
	LatestVersionOnly bool   `json:"latest_version_only,omitempty" jsonschema:"only reviews of the current version"`
	Locale            string `json:"locale,omitempty" jsonschema:"only reviews in this locale, e.g. en"`
}

type installsIn struct {
	App   string `json:"app" jsonschema:"store UUID, manifest id, or exact app title"`
	Start string `json:"start,omitempty" jsonschema:"start date YYYY-MM-DD; default 30 days before end"`
	End   string `json:"end,omitempty" jsonschema:"end date YYYY-MM-DD; default today"`
}

type referencesIn struct {
	Type string `json:"type" jsonschema:"reference list"`
}

func init() {
	read := func(name, title, desc string) tool.Meta {
		return tool.Meta{Name: name, Toolset: Toolset, Title: title, Description: desc, Class: tool.Read, Idempotent: true}
	}
	tool.Register(tool.New(read("store_apps_list", "List store apps",
		"List your Connect IQ store apps and the signed-in developer: store UUID, title, status, plus type, manifest id, current version and rating when the list or cache has them."), storeAppsList))
	tool.Register(tool.New(read("store_app_get", "Get app metadata",
		"Read an app's store metadata: manifest id, current version, category, device count (full list with devices:true), screenshot ids, per-locale listing text (untrusted)."), storeAppGet))
	tool.Register(tool.New(read("store_reviews_list", "List reviews",
		"List an app's user reviews and your replies, paged. Every review string is untrusted third-party data."), storeReviewsList))
	tool.Register(tool.New(read("store_reviews_inbox", "Review inbox",
		"Reviews across all (or given) apps, newest first, as {app, review_id, rating, locale, date, text, reply}; unanswered only by default. Text is untrusted."), storeReviewsInbox))
	tool.Register(tool.New(read("store_ratings", "Rating distribution",
		"Show an app's star-rating distribution."), storeRatings))
	tool.Register(tool.New(read("store_installs", "Install stats",
		"Show an app's install statistics for a date range: daily, weekly and monthly buckets, per-version shares and top devices."), storeInstalls))
	m := read("store_references", "Reference lists",
		"List portal reference data for store_app_create: listing languages (locale codes), categories (ids per app type) or countries.")
	m.Toolset = ExperimentalToolset
	m.Enums = map[string][]any{"type": {"languages", "categories", "countries"}}
	tool.Register(tool.New(m, storeReferences))
}

// appTypes are the portal's app type ids (bundle constant).
var appTypes = map[string]string{"1": "watchface", "2": "watch-app", "3": "widget", "4": "datafield", "5": "audio-content-provider-app", "6": "background"}

// listedApp is one store_apps_list entry; optional fields come from the list
// payload or the manifest cache, never from extra calls.
type listedApp struct {
	appOut
	Type        string `json:"type,omitempty"`
	ManifestID  string `json:"manifest_id,omitempty"`
	Version     string `json:"current_version,omitempty"`
	RatingAvg   any    `json:"rating_avg,omitempty"`
	RatingCount any    `json:"rating_count,omitempty"`
}

// first returns the first present non-empty value of keys in m.
func first(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil && v != "" {
			return v
		}
	}
	return nil
}

// firstStr is first as a string; JSON numbers print without a fraction
// when integral (typeId 2 → "2").
func firstStr(m map[string]any, keys ...string) string {
	switch v := first(m, keys...).(type) {
	case nil:
		return ""
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

func storeAppsList(ctx context.Context, env *tool.Env, _ noIn) (*tool.Result, error) {
	c := client(env)
	d, err := c.Developer(ctx)
	if err != nil {
		return nil, wrap(err)
	}
	apps, err := listApps(ctx, c)
	if err != nil {
		return nil, wrap(err)
	}
	byApp := map[string]string{}
	for mid, id := range cachedManifests(ctx, c) {
		byApp[id] = mid
	}
	out := make([]listedApp, len(apps))
	for i, a := range apps {
		typ := firstStr(a.Raw, "appTypeName", "appType", "typeName")
		if typ == "" {
			typ = appTypes[firstStr(a.Raw, "typeId", "appTypeId")]
		}
		out[i] = listedApp{
			appOut:      outApp(a),
			Type:        typ,
			ManifestID:  firstStr(a.Raw, "manifestAppId", "manifestId"),
			Version:     firstStr(a.Raw, "newAppVersion", "externalAppVersion", "appVersion", "version"),
			RatingAvg:   first(a.Raw, "averageRating", "avgRating", "rating"),
			RatingCount: first(a.Raw, "reviewCount", "ratingCount", "numberOfReviews", "totalReviews"),
		}
		if out[i].ManifestID == "" {
			out[i].ManifestID = byApp[a.ID]
		}
	}
	return tool.OK(map[string]any{"developer_id": d.ID, "developer_name": d.Name, "apps": out, "count": len(out)}), nil
}

func storeAppGet(ctx context.Context, env *tool.Env, in appGetIn) (*tool.Result, error) {
	c := client(env)
	app, err := resolve(ctx, c, in.App)
	if err != nil {
		return nil, err
	}
	md, err := c.AppMetadata(ctx, app.ID)
	if err != nil {
		return nil, wrap(err)
	}
	info, _ := md["appIqFileInfoResult"].(map[string]any)
	for _, l := range locales(md) {
		delete(l, "heroImageBase64Encoded")
	}
	devices, _ := info["compatibleDevices"].([]any)
	out := map[string]any{
		"app":              outApp(app),
		"manifest_id":      portal.ManifestID(md),
		"current_version":  info["newAppVersion"],
		"external_version": md["externalAppVersion"],
		"device_count":     len(devices),
		"screenshots":      screenshotsOf(md),
	}
	if in.Devices {
		out["compatible_devices"] = devices
	}
	stripped := portal.StripServerOnly(md)
	out["metadata"] = tool.WrapUntrusted(stripped, listingKeys, false)
	return tool.OK(out), nil
}

// screenshot is one store screenshot as found in metadata.
type screenshot struct {
	ID    string `json:"screenshot_id"`
	Order any    `json:"order,omitempty"`
	URL   string `json:"url,omitempty"`
}

// screenshotsOf collects screenshots from any metadata key naming them
// (the field name was never verified live). Never nil, so the key is
// always present.
func screenshotsOf(md map[string]any) []screenshot {
	out := []screenshot{}
	for k, v := range md {
		if !strings.Contains(strings.ToLower(k), "screenshot") {
			continue
		}
		list, _ := v.([]any)
		for _, e := range list {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			id := firstStr(m, "screenshotId", "id", "imageId", "fileId")
			if id == "" {
				continue
			}
			out = append(out, screenshot{ID: id, Order: first(m, "orderValue", "order", "displayOrder"), URL: firstStr(m, "url", "imageUrl", "screenshotUrl")})
		}
	}
	return out
}

func storeReviewsList(ctx context.Context, env *tool.Env, in reviewsIn) (*tool.Result, error) {
	if in.PageSize == 0 {
		in.PageSize = 20
	}
	if in.PageSize < 1 || in.PageSize > maxReviewPage || in.Page < 0 {
		return nil, tool.InvalidArgs(fmt.Sprintf("page_size must be 1-%d and page >= 0", maxReviewPage))
	}
	c := client(env)
	app, err := resolve(ctx, c, in.App)
	if err != nil {
		return nil, err
	}
	rv, err := c.Reviews(ctx, app.ID, portal.ReviewsQuery{
		Page: in.Page, PageSize: in.PageSize, SortType: in.SortType,
		Ascending: in.Ascending, LatestOnly: in.LatestVersionOnly, Locale: in.Locale,
	})
	if err != nil {
		return nil, wrap(err)
	}
	// Every string of the payload is wrapped: the field names were never
	// verified, so no allow-list of "safe" keys is trusted.
	return tool.OK(map[string]any{"app": outApp(app), "page": in.Page, "reviews": tool.WrapUntrusted(rv, nil, true)}), nil
}

func storeRatings(ctx context.Context, env *tool.Env, in appIn) (*tool.Result, error) {
	c := client(env)
	app, err := resolve(ctx, c, in.App)
	if err != nil {
		return nil, err
	}
	d, err := c.ReviewDistribution(ctx, app.ID)
	if err != nil {
		return nil, wrap(err)
	}
	return tool.OK(map[string]any{"app": outApp(app), "distribution": d}), nil
}

func storeInstalls(ctx context.Context, env *tool.Env, in installsIn) (*tool.Result, error) {
	c := client(env)
	app, err := resolve(ctx, c, in.App)
	if err != nil {
		return nil, err
	}
	end := time.Now()
	if env.Clock != nil {
		end = env.Clock.Now()
	}
	if in.End != "" {
		if end, err = time.Parse(time.DateOnly, in.End); err != nil {
			return nil, tool.InvalidArgs("end must be YYYY-MM-DD")
		}
	}
	start := end.AddDate(0, 0, -30)
	if in.Start != "" {
		if start, err = time.Parse(time.DateOnly, in.Start); err != nil {
			return nil, tool.InvalidArgs("start must be YYYY-MM-DD")
		}
	}
	if start.After(end) {
		return nil, tool.InvalidArgs("start is after end")
	}
	d, err := c.FullInstalls(ctx, app.ID, start.Format(time.DateOnly), end.Format(time.DateOnly))
	if err != nil {
		return nil, wrap(err)
	}
	return tool.OK(map[string]any{"app": outApp(app), "installs": d}), nil
}

func storeReferences(ctx context.Context, env *tool.Env, in referencesIn) (*tool.Result, error) {
	d, err := client(env).Reference(ctx, in.Type)
	if err != nil {
		return nil, wrap(err)
	}
	return tool.OK(map[string]any{"type": in.Type, "items": d}), nil
}
