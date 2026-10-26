package tool_test

import (
	"errors"
	"testing"
	"time"

	"github.com/idoo/garmin-mcp/internal/tool"
)

func TestPlannerClaim(t *testing.T) {
	args := []byte(`{"a":1}`)
	tests := []struct {
		name    string
		tool    string
		args    []byte
		advance time.Duration
		ok      bool
	}{
		{"match", "t", args, 0, true},
		{"just before ttl", "t", args, tool.PreviewTTL - time.Nanosecond, true},
		{"other tool", "u", args, 0, false},
		{"other args", "t", []byte(`{"a":2}`), 0, false},
		{"expired", "t", args, tool.PreviewTTL, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Unix(1000, 0)
			p := tool.NewPlanner(func() time.Time { return now })
			p.Propose("t", args)
			now = now.Add(tt.advance)
			err := p.Claim(tt.tool, tt.args)
			if tt.ok != (err == nil) {
				t.Fatalf("claim err = %v, want ok=%v", err, tt.ok)
			}
			if err != nil && !errors.Is(err, tool.ErrPreviewRequired) {
				t.Fatalf("claim err = %v", err)
			}
			if tt.ok {
				if err := p.Claim(tt.tool, tt.args); !errors.Is(err, tool.ErrPreviewRequired) {
					t.Fatalf("preview claimed twice: %v", err)
				}
			}
		})
	}
}

func TestPlannerEvictsOldest(t *testing.T) {
	p := tool.NewPlanner(nil)
	for i := 0; i <= tool.PlanCapacity; i++ {
		p.Propose("t", []byte{byte(i)})
	}
	if err := p.Claim("t", []byte{0}); err == nil {
		t.Fatal("oldest preview survived eviction")
	}
	if err := p.Claim("t", []byte{byte(tool.PlanCapacity)}); err != nil {
		t.Fatalf("newest preview lost: %v", err)
	}
}

func TestNilPlannerFailsClosed(t *testing.T) {
	var p *tool.Planner
	p.Propose("t", nil)
	if err := p.Claim("t", nil); !errors.Is(err, tool.ErrPreviewRequired) {
		t.Fatalf("nil planner claim = %v", err)
	}
}

func TestCanonicalArgsDropsConfirmArgs(t *testing.T) {
	a, err := tool.CanonicalArgs(map[string]any{"path": "x.iq"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := tool.CanonicalArgs(map[string]any{"path": "x.iq", "confirm": true, "iq_sha256": "ab"}, "iq_sha256")
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("%s != %s", a, b)
	}
}
