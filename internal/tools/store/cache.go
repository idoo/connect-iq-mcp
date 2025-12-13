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

	"github.com/idoo/connect-iq-mcp/internal/portal"
	"github.com/idoo/connect-iq-mcp/internal/tool"
	"github.com/idoo/connect-iq-mcp/internal/upstream"
)

// cacheTTL bounds how long the app list and manifest map are reused.
const cacheTTL = 5 * time.Minute

// cacheEntry is the per-developer app list and manifest id → store UUID map.
type cacheEntry struct {
	at        time.Time
	gen       uint64 // appCache.gen when this entry was filled
	apps      []portal.App
	manifests map[string]string // normalized manifest id → app id; nil until built
}

// appCache is the server's app cache (one per [tool.Shared]). Any store
// write drops it (see client). Entries are read and written only under mu;
// callers get copies.
type appCache struct {
	mu      sync.Mutex
	entries map[string]*cacheEntry
	// gen is bumped by every invalidation and every fill. A fetch that
	// started at an older generation never stores its result, so a list read
	// before a write cannot repopulate the cache after it.
	gen uint64
}

func newAppCache() *appCache { return &appCache{entries: map[string]*cacheEntry{}} }

// cacheOf returns the server's app cache.
func cacheOf(env *tool.Env) *appCache { return tool.SharedValue(env.Shared, newAppCache) }

// storeClient is a portal client paired with the server's app cache.
type storeClient struct {
	*portal.Client
	cache *appCache
}

// invalidate drops every cached app list.
func (a *appCache) invalidate() {
	a.mu.Lock()
	clear(a.entries)
	a.gen++
	a.mu.Unlock()
}

// freshLocked returns the fresh entry for dev, or nil. Caller holds a.mu.
func (a *appCache) freshLocked(dev string) *cacheEntry {
	e := a.entries[dev]
	if e == nil || time.Since(e.at) > cacheTTL {
		return nil
	}
	return e
}

// listApps returns a copy of the developer's apps, from the cache when fresh,
// and the generation of the entry they belong to (pass it to manifestMap).
func listApps(ctx context.Context, c *storeClient) ([]portal.App, uint64, error) {
	dev, err := c.DeveloperID(ctx)
	if err != nil {
		return nil, 0, err
	}
	a := c.cache
	a.mu.Lock()
	if e := a.freshLocked(dev); e != nil {
		apps, gen := slices.Clone(e.apps), e.gen
		a.mu.Unlock()
		return apps, gen, nil
	}
	start := a.gen
	a.mu.Unlock()

	apps, err := c.Apps(ctx)
	if err != nil {
		return nil, 0, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.gen != start {
		// A write or another fill happened meanwhile: serve this result once,
		// never cache it over newer data. Generation 0 matches no entry.
		return apps, 0, nil
	}
	a.gen++
	a.entries[dev] = &cacheEntry{at: time.Now(), gen: a.gen, apps: slices.Clone(apps)}
	return apps, a.gen, nil
}

// manifestMap returns a copy of the manifest map for the app list of
// generation gen, building it (one metadata GET per app) at most once per
// cache entry. A map is stored only on the entry its app list came from.
func manifestMap(ctx context.Context, c *storeClient, apps []portal.App, gen uint64) (map[string]string, error) {
	dev, err := c.DeveloperID(ctx)
	if err != nil {
		return nil, err
	}
	a := c.cache
	a.mu.Lock()
	if e := a.freshLocked(dev); e != nil && e.gen == gen && e.manifests != nil {
		m := maps.Clone(e.manifests)
		a.mu.Unlock()
		return m, nil
	}
	a.mu.Unlock()

	m := make(map[string]string, len(apps))
	for _, app := range apps {
		md, err := c.AppMetadata(ctx, app.ID)
		if err != nil {
			return nil, err
		}
		if id := portal.NormalizeID(portal.ManifestID(md)); id != "" {
			m[id] = app.ID
		}
	}
	a.mu.Lock()
	if e := a.entries[dev]; e != nil && gen != 0 && e.gen == gen {
		e.manifests = maps.Clone(m)
	}
	a.mu.Unlock()
	return m, nil
}

// cachedManifests returns a copy of the manifest map only if already built
// (no calls).
func cachedManifests(ctx context.Context, c *storeClient) map[string]string {
	dev, err := c.DeveloperID(ctx)
	if err != nil {
		return nil
	}
	a := c.cache
	a.mu.Lock()
	defer a.mu.Unlock()
	if e := a.freshLocked(dev); e != nil {
		return maps.Clone(e.manifests)
	}
	return nil
}

// resolveApp finds an app by store UUID, manifest id or exact title
// (case-insensitive).
func resolveApp(ctx context.Context, c *storeClient, ref string) (portal.App, error) {
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
	return portal.App{}, &upstream.HTTPError{Service: upstream.Portal, Status: http.StatusNotFound, Code: "APP_NOT_FOUND", Message: fmt.Sprintf("no app matches %q (store UUID, manifest id or exact title); store_apps_list shows your apps. A brand-new app needs its first upload on apps.garmin.com before versions can be published here", ref)}
}
