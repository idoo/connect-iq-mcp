package store

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/idoo/garmin-mcp/internal/portal"
)

// cacheTTL bounds how long the app list and manifest map are reused.
const cacheTTL = 5 * time.Minute

// appCache is the per-developer app list and manifest id → store UUID map.
// Any store write drops it (see client).
type appCache struct {
	at        time.Time
	apps      []portal.App
	manifests map[string]string // normalized manifest id → app id; nil until built
}

var (
	cacheMu sync.Mutex
	caches  = map[string]*appCache{}
)

// invalidateCache drops every cached app list.
func invalidateCache() {
	cacheMu.Lock()
	clear(caches)
	cacheMu.Unlock()
}

func cached(dev string) *appCache {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	e := caches[dev]
	if e == nil || time.Since(e.at) > cacheTTL {
		return nil
	}
	return e
}

// listApps returns the developer's apps, from the cache when fresh.
func listApps(ctx context.Context, c *portal.Client) ([]portal.App, error) {
	dev, err := c.DeveloperID(ctx)
	if err != nil {
		return nil, err
	}
	if e := cached(dev); e != nil {
		return e.apps, nil
	}
	apps, err := c.Apps(ctx)
	if err != nil {
		return nil, err
	}
	cacheMu.Lock()
	caches[dev] = &appCache{at: time.Now(), apps: apps}
	cacheMu.Unlock()
	return apps, nil
}

// manifestMap returns the cached manifest map, building it (one metadata GET
// per app) at most once per cache lifetime.
func manifestMap(ctx context.Context, c *portal.Client, apps []portal.App) (map[string]string, error) {
	dev, err := c.DeveloperID(ctx)
	if err != nil {
		return nil, err
	}
	if e := cached(dev); e != nil && e.manifests != nil {
		return e.manifests, nil
	}
	m := make(map[string]string, len(apps))
	for _, a := range apps {
		md, err := c.AppMetadata(ctx, a.ID)
		if err != nil {
			return nil, err
		}
		if id := portal.NormalizeID(portal.ManifestID(md)); id != "" {
			m[id] = a.ID
		}
	}
	cacheMu.Lock()
	if e := caches[dev]; e != nil {
		e.manifests = m
	}
	cacheMu.Unlock()
	return m, nil
}

// cachedManifests returns the manifest map only if already built (no calls).
func cachedManifests(ctx context.Context, c *portal.Client) map[string]string {
	dev, err := c.DeveloperID(ctx)
	if err != nil {
		return nil
	}
	if e := cached(dev); e != nil {
		return e.manifests
	}
	return nil
}

// resolveApp finds an app by store UUID, manifest id or exact title
// (case-insensitive).
func resolveApp(ctx context.Context, c *portal.Client, ref string) (portal.App, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return portal.App{}, fmt.Errorf("app is required (store UUID, manifest id or title)")
	}
	apps, err := listApps(ctx, c)
	if err != nil {
		return portal.App{}, err
	}
	norm := portal.NormalizeID(ref)
	for _, a := range apps {
		if portal.NormalizeID(a.ID) == norm {
			return a, nil
		}
	}
	var byName []portal.App
	for _, a := range apps {
		if strings.EqualFold(strings.TrimSpace(a.Name), ref) {
			byName = append(byName, a)
		}
	}
	if len(byName) == 1 {
		return byName[0], nil
	}
	if len(byName) > 1 {
		return portal.App{}, fmt.Errorf("%d apps are titled %q; pass the store UUID", len(byName), ref)
	}
	if len(norm) == 32 {
		m, err := manifestMap(ctx, c, apps)
		if err != nil {
			return portal.App{}, err
		}
		if id, ok := m[norm]; ok {
			for _, a := range apps {
				if a.ID == id {
					return a, nil
				}
			}
		}
	}
	return portal.App{}, &portal.APIError{Status: http.StatusNotFound, Code: "APP_NOT_FOUND", Message: fmt.Sprintf("no app matches %q (store UUID, manifest id or exact title)", ref)}
}
