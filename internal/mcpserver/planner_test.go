package mcpserver

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/idoo/garmin-mcp/internal/tool"
)

func TestPlannerClaim(t *testing.T) {
	ctx := context.Background()
	args := map[string]any{"a": 1, "confirm": true}
	if _, err := NewPlanner(nil).Propose(ctx, "t", args, "", tool.PlanPreview{}); err == nil {
		t.Fatal("empty fingerprint accepted")
	}
	tests := []struct {
		name    string
		tool    string
		args    any
		fp      string
		advance time.Duration
		ok      bool
	}{
		{"match", "t", map[string]any{"a": 1}, tool.FlagFingerprint, 0, true},
		{"just before ttl", "t", args, tool.FlagFingerprint, tool.PreviewTTL - time.Nanosecond, true},
		{"other tool", "u", args, tool.FlagFingerprint, 0, false},
		{"other args", "t", map[string]any{"a": 2}, tool.FlagFingerprint, 0, false},
		{"not a flag preview", "t", args, "fp", 0, false},
		{"expired", "t", args, tool.FlagFingerprint, tool.PreviewTTL, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Unix(1000, 0)
			p := NewPlanner(func() time.Time { return now })
			if _, err := p.Propose(ctx, "t", args, tt.fp, tool.PlanPreview{}); err != nil {
				t.Fatal(err)
			}
			now = now.Add(tt.advance)
			err := p.Claim(ctx, tt.tool, tt.args)
			if tt.ok != (err == nil) {
				t.Fatalf("claim err = %v, want ok=%v", err, tt.ok)
			}
			if err != nil && !errors.Is(err, tool.ErrPreviewRequired) {
				t.Fatalf("error %v does not wrap ErrPreviewRequired", err)
			}
			if tt.ok {
				if err := p.Claim(ctx, tt.tool, tt.args); !errors.Is(err, tool.ErrPreviewRequired) {
					t.Fatalf("preview claimed twice: %v", err)
				}
			}
		})
	}
}

func TestPlannerEvictsOldest(t *testing.T) {
	ctx := context.Background()
	p := NewPlanner(nil)
	for i := 0; i <= PlanCapacity; i++ {
		if _, err := p.Propose(ctx, "t", i, tool.FlagFingerprint, tool.PlanPreview{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Claim(ctx, "t", 0); err == nil {
		t.Fatal("oldest preview survived eviction")
	}
	if err := p.Claim(ctx, "t", PlanCapacity); err != nil {
		t.Fatalf("newest preview lost: %v", err)
	}
}
