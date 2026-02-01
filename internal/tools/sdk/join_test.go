package sdk

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/idoo/connect-iq-mcp/internal/tool/tooltest"
)

// Two identical builds join one job; each caller then writes its own result
// Meta. Run with -race: a shared *tool.Result is a data race.
func TestJoinedBuildCallersGetOwnResult(t *testing.T) {
	runs := filepath.Join(t.TempDir(), "runs")
	k, _, _ := exportFixtureWith(t, "echo x >> '"+runs+"'; sleep 1", ` version="1.2.3"`)
	setBuildWait(k, 20*time.Second)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			tooltest.MustOK(t, callBuild(t, k, `{"project":"app","device":"all","key":"dev.der"}`))
		})
	}
	wg.Wait()
	b, _ := os.ReadFile(runs)
	if n := strings.Count(string(b), "x"); n != 1 {
		t.Fatalf("compiler ran %d times, want 1 (the callers must join one job)", n)
	}
}
