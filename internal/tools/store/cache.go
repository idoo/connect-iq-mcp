package store

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/idoo/garmin-mcp/internal/portal"
	"github.com/idoo/garmin-mcp/internal/tool"
)

// cacheTTL bounds how long the app list and manifest map are reused.
const cacheTTL = 5 * time.Minute

// appCache is the per-developer app list and manifest id → store UUID map.
// Any store write drops it (see client). Entries are read and written only
// under cacheMu; callers get copies.
type appCache struct {
	at        time.Time
	gen       uint64 // cacheGen when this entry was filled
	apps      []portal.App
	manifests map[string]string // normalized manifest id → app id; nil until built
}

var (
	cacheMu sync.Mutex
	caches  = map[string]*appCache{}
	// cacheGen is bumped by every invalidation and every fill. A fetch that
	// started at an older generation never stores its result, so a list read
	// before a write cannot repopulate the cache after it.
	cacheGen uint64
)

// invalidateCache drops every cached app list.
func invalidateCache() {
	cacheMu.Lock()
	clear(caches)
	cacheGen++
	cacheMu.Unlock()
}

// freshLocked returns the fresh entry for dev, or nil. Caller holds cacheMu.
func freshLocked(dev string) *appCache {
	e := caches[dev]
	if e == nil || time.Since(e.at) > cacheTTL {
		return nil
	}
	return e
}

// listApps returns a copy of the developer's apps, from the cache when fresh,
// and the generation of the entry they belong to (pass it to manifestMap).
func listApps(ctx context.Context, c *portal.Client) ([]portal.App, uint64, error) {
	dev, err := c.DeveloperID(ctx)
	if err != nil {
		return nil, 0, err
	}
	cacheMu.Lock()
	if e := freshLocked(dev); e != nil {
		apps, gen := slices.Clone(e.apps), e.gen
		cacheMu.Unlock()
		return apps, gen, nil
	}
	start := cacheGen
	cacheMu.Unlock()

	apps, err := c.Apps(ctx)
	if err != nil {
		return nil, 0, err
	}
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if cacheGen != start {
		// A write or another fill happened meanwhile: serve this result once,
		// never cache it over newer data. Generation 0 matches no entry.
		return apps, 0, nil
	}
	cacheGen++
	caches[dev] = &appCache{at: time.Now(), gen: cacheGen, apps: slices.Clone(apps)}
	return apps, cacheGen, nil
}

// manifestMap returns a copy of the manifest map for the app list of
// generation gen, building it (one metadata GET per app) at most once per
// cache entry. A map is stored only on the entry its app list came from.
func manifestMap(ctx context.Context, c *portal.Client, apps []portal.App, gen uint64) (map[string]string, error) {
	dev, err := c.DeveloperID(ctx)
	if err != nil {
		return nil, err
	}
	cacheMu.Lock()
	if e := freshLocked(dev); e != nil && e.gen == gen && e.manifests != nil {
		m := maps.Clone(e.manifests)
		cacheMu.Unlock()
		return m, nil
	}
	cacheMu.Unlock()

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
	if e := caches[dev]; e != nil && gen != 0 && e.gen == gen {
		e.manifests = maps.Clone(m)
	}
	cacheMu.Unlock()
	return m, nil
}

// cachedManifests returns a copy of the manifest map only if already built
// (no calls).
func cachedManifests(ctx context.Context, c *portal.Client) map[string]string {
	dev, err := c.DeveloperID(ctx)
	if err != nil {
		return nil
	}
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if e := freshLocked(dev); e != nil {
		return maps.Clone(e.manifests)
	}
	return nil
}

// resolveApp finds an app by store UUID, manifest id or exact title
// (case-insensitive).
func resolveApp(ctx context.Context, c *portal.Client, ref string) (portal.App, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return portal.App{}, tool.InvalidArgs("app is required (store UUID, manifest id or title)")
	}
	apps, gen, err := listApps(ctx, c)
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
		m, err := manifestMap(ctx, c, apps, gen)
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
	return portal.App{}, &portal.APIError{Status: http.StatusNotFound, Code: "APP_NOT_FOUND", Message: fmt.Sprintf("no app matches %q (store UUID, manifest id or exact title); store_apps_list shows your apps. A brand-new app needs its first upload on apps.garmin.com before versions can be published here", ref)}
}
