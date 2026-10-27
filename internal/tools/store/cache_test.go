package store

import (
	"strings"
	"sync"
	"testing"

	"github.com/idoo/connect-iq-mcp/internal/tool/tooltest"
)

// Parallel lookups by manifest id race with a store write on the cache
// entry; run with -race.
func TestCacheConcurrentGetAndWrite(t *testing.T) {
	k, _ := setup(t)
	get := lookup(t, "store_app_get")
	write := lookup(t, "store_listing_update")
	args := `{"app":"` + strings.ReplaceAll(manifest2, "-", "") + `"}`
	for range 10 {
		cacheOf(k.Env).invalidate()
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() { tooltest.Call(t, get, k.Env, args) })
		}
		wg.Go(func() {
			tooltest.Call(t, write, k.Env, `{"app":"Bike Guide","locales":[{"locale":"en","whats_new":"x"}],"confirm":true}`)
		})
		wg.Wait()
	}
}

// A list fetched before a write must not be cached after it.
func TestStaleListFillDropped(t *testing.T) {
	k, f := setup(t)
	once := sync.Once{}
	f.onAppsList = func() { once.Do(cacheOf(k.Env).invalidate) } // a write lands mid-fetch
	list := lookup(t, "store_apps_list")
	tooltest.MustOK(t, tooltest.Call(t, list, k.Env, `{}`))
	tooltest.MustOK(t, tooltest.Call(t, list, k.Env, `{}`))
	if n := f.count("GET " + devBase + "/apps"); n != 2 {
		t.Fatalf("pre-write list served from cache: %d listings, want 2", n)
	}
}

func TestBlankAppIsInvalidArguments(t *testing.T) {
	k, f := setup(t)
	tooltest.MustFail(t, tooltest.Call(t, lookup(t, "store_app_get"), k.Env, `{"app":"  "}`), "invalid_arguments")
	if f.called("/apps") {
		t.Fatalf("blank app reached the portal: %v", f.calls)
	}
}
