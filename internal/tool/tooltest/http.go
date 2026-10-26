package tooltest

import (
	"context"
	"fmt"
	"net/http"

	"github.com/idoo/ciq-mcp/internal/tool"
)

// Delete sends DELETE https://example.test+path through env.HTTP, the way a
// mutating tool would; it fails with [tool.ErrWriteGuard] outside a write.
func Delete(ctx context.Context, env *tool.Env, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, "https://example.test"+path, nil)
	if err != nil {
		return err
	}
	res, err := env.HTTP.Do(req)
	if err != nil {
		return err
	}
	_ = res.Body.Close()
	if res.StatusCode >= 300 {
		return &tool.APIError{Status: res.StatusCode, Detail: fmt.Sprintf("DELETE %s", path)}
	}
	return nil
}
