package app

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/idoo/connect-iq-mcp/internal/tool"
)

// J7: "~/k.der" is refused with a hint, never resolved as a "~" folder
// inside the root (even when one exists).
func TestResolveRefusesTilde(t *testing.T) {
	f, _ := newTestFiles(t)
	if err := os.MkdirAll(filepath.Join(f.Root(), "~"), 0o750); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"~/k.der", "~", "~ivan/k.der"} {
		_, err := f.Resolve(p)
		b := tool.BodyFor(err)
		if !errors.Is(err, ErrOutsideRoot) || b.Error != "invalid_arguments" || !strings.Contains(err.Error(), "~ is not expanded; use a path relative to the project or an absolute path inside the root") {
			t.Errorf("Resolve(%q) = %v (%+v)", p, err, b)
		}
	}
}
