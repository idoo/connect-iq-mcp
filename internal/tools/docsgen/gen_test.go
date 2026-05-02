package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/idoo/connect-iq-mcp/internal/tool"
)

// Session-built tools are documented from their own Meta, and a forced
// human gate (HumanApproval) is documented as "human".
func TestEntriesUseRealMeta(t *testing.T) {
	entries, err := Entries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][2]string{
		"toolsets":              {tool.Write.String(), "none"},
		"store_version_publish": {tool.Destructive.String(), "human"},
	}
	for _, e := range entries {
		if w, ok := want[e.Name]; ok {
			if e.Class != w[0] || e.Confirm != w[1] {
				t.Errorf("%s: class %s confirm %s, want %s %s", e.Name, e.Class, e.Confirm, w[0], w[1])
			}
			delete(want, e.Name)
		}
	}
	if len(want) > 0 {
		t.Errorf("missing entries: %v", want)
	}
}

func TestGeneratedDocsAreFresh(t *testing.T) {
	j, md, err := Generate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string][]byte{"tools.json": j, "TOOLS.md": md} {
		got, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", name))
		if err != nil {
			t.Fatalf("%s: %v (run go run ./internal/tools/docsgen)", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("docs/%s is stale; run go run ./internal/tools/docsgen", name)
		}
	}
}

func TestGenerateIsDeterministic(t *testing.T) {
	j1, m1, err := Generate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	j2, m2, err := Generate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(j1, j2) || !bytes.Equal(m1, m2) {
		t.Fatal("docsgen output differs between runs")
	}
}
