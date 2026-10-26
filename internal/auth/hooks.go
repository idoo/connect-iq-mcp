package auth

import (
	"context"
	"fmt"
	"io"
)

// Placeholders owned by the auth worker, who replaces both.

// CLI runs `garmin-mcp auth ARGS` and returns the exit code.
var CLI = func(_ []string, _, stderr io.Writer) int {
	_, _ = fmt.Fprintln(stderr, "auth: not built yet")
	return 1
}

// Open returns the credential store for profile. nil Open, or a nil Store,
// means no credentials are configured; tools then report auth_required.
var Open func(ctx context.Context, profile string) (Store, error)
