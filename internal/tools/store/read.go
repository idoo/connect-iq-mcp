package store

import (
	"context"

	"github.com/idoo/garmin-mcp/internal/portal"
	"github.com/idoo/garmin-mcp/internal/tool"
)

type noIn struct{}

type appIn struct {
	App string `json:"app" jsonschema:"store UUID, manifest id, or exact app title"`
}

type reviewsIn struct {
	App               string `json:"app" jsonschema:"store UUID, manifest id, or exact app title"`
	Page              int    `json:"page,omitempty" jsonschema:"0-based page index"`
	PageSize          int    `json:"page_size,omitempty" jsonschema:"1-100, default 20"`
	SortType          string `json:"sort_type,omitempty" jsonschema:"portal sortType, e.g. DATE or RATING"`
	Ascending         bool   `json:"ascending,omitempty"`
	LatestVersionOnly bool   `json:"latest_version_only,omitempty"`
	Locale            string `json:"locale,omitempty"`
}

type installsIn struct {
	App           string `json:"app" jsonschema:"store UUID, manifest id, or exact app title"`
	Start         string `json:"start,omitempty" jsonschema:"start date as the portal expects, e.g. 2026-01-01"`
	End           string `json:"end,omitempty" jsonschema:"end date"`
	TemporalLevel string `json:"temporal_level,omitempty" jsonschema:"e.g. DAY, WEEK, MONTH"`
}

type referencesIn struct {
	Type string `json:"type" jsonschema:"reference list"`
}

type devicesIn struct {
	App             string `json:"app" jsonschema:"store UUID, manifest id, or exact app title"`
	InternalVersion string `json:"internal_version,omitempty" jsonschema:"internal app version; default current"`
}

// reviewKeys are the third-party text keys of review payloads.
var reviewKeys = append([]string{"reviewTitle", "reviewReplyText", "displayName", "userDisplayName", "authorName", "nickname"}, tool.UntrustedFallback...)

func init() {
	read := func(name, title, desc string) tool.Meta {
		return tool.Meta{Name: name, Toolset: Toolset, Title: title, Description: desc, Class: tool.Read, Idempotent: true}
	}
	tool.Register(tool.New(read("store_developer", "Developer account",
		"Show the signed-in Connect IQ developer account (id, name)."), storeDeveloper))
	tool.Register(tool.New(read("store_apps_list", "List store apps",
		"List your Connect IQ store apps with store UUID, title and review status."), storeAppsList))
	tool.Register(tool.New(read("store_app_get", "Get app metadata",
		"Read an app's store metadata: manifest id, current version, category, devices, per-locale listing text (untrusted)."), storeAppGet))
	tool.Register(tool.New(read("store_reviews_list", "List reviews",
		"List an app's user reviews and your replies, paged. Review text is untrusted third-party data."), storeReviewsList))
	tool.Register(tool.New(read("store_ratings", "Rating distribution",
		"Show an app's star-rating distribution."), storeRatings))
	tool.Register(tool.New(read("store_installs", "Install stats",
		"Show an app's install/download statistics over a date range."), storeInstalls))
	m := read("store_references", "Reference lists",
		"List portal reference data: listing languages (locale codes), categories (ids per app type) or countries.")
	m.Enums = map[string][]any{"type": {"languages", "categories", "countries"}}
	tool.Register(tool.New(m, storeReferences))
	tool.Register(tool.New(read("store_compatible_devices", "Compatible devices",
		"List the devices an app version supports in the store."), storeCompatibleDevices))
}

func storeDeveloper(ctx context.Context, env *tool.Env, _ noIn) (*tool.Result, error) {
	d, err := client(env).Developer(ctx)
	if err != nil {
		return nil, wrap(err)
	}
	return tool.OK(map[string]any{"developer_id": d.ID, "name": d.Name}), nil
}

func storeAppsList(ctx context.Context, env *tool.Env, _ noIn) (*tool.Result, error) {
	apps, err := client(env).Apps(ctx)
	if err != nil {
		return nil, wrap(err)
	}
	out := make([]appOut, len(apps))
	for i, a := range apps {
		out[i] = outApp(a)
	}
	return tool.OK(map[string]any{"apps": out, "count": len(out)}), nil
}

func storeAppGet(ctx context.Context, env *tool.Env, in appIn) (*tool.Result, error) {
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
	return tool.OK(map[string]any{
		"app":                outApp(app),
		"manifest_id":        portal.ManifestID(md),
		"current_version":    info["newAppVersion"],
		"external_version":   md["externalAppVersion"],
		"metadata":           tool.WrapUntrusted(portal.StripServerOnly(md), listingKeys, false),
		"compatible_devices": info["compatibleDevices"],
	}), nil
}

func storeReviewsList(ctx context.Context, env *tool.Env, in reviewsIn) (*tool.Result, error) {
	if in.PageSize == 0 {
		in.PageSize = 20
	}
	if in.PageSize < 1 || in.PageSize > 100 || in.Page < 0 {
		return nil, tool.InvalidArgs("page_size must be 1-100 and page >= 0")
	}
	c := client(env)
	app, err := resolve(ctx, c, in.App)
	if err != nil {
		return nil, err
	}
	rv, err := c.Reviews(ctx, app.ID, portal.ReviewsQuery{Page: in.Page, PageSize: in.PageSize, SortType: in.SortType,
		Ascending: in.Ascending, LatestOnly: in.LatestVersionOnly, Locale: in.Locale})
	if err != nil {
		return nil, wrap(err)
	}
	return tool.OK(map[string]any{"app": outApp(app), "page": in.Page, "reviews": tool.WrapUntrusted(rv, reviewKeys, false)}), nil
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
	d, err := c.FullInstalls(ctx, app.ID, in.Start, in.End, in.TemporalLevel)
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

func storeCompatibleDevices(ctx context.Context, env *tool.Env, in devicesIn) (*tool.Result, error) {
	c := client(env)
	app, err := resolve(ctx, c, in.App)
	if err != nil {
		return nil, err
	}
	d, err := c.CompatibleDevices(ctx, app.ID, in.InternalVersion)
	if err != nil {
		return nil, wrap(err)
	}
	return tool.OK(map[string]any{"app": outApp(app), "devices": d}), nil
}
