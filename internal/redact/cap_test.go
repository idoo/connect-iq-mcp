package redact

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func sizeOf(t *testing.T, v any) int {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return len(b)
}

func TestCapResultGeneric(t *testing.T) {
	builds := make([]any, 8)
	for i := range builds {
		builds[i] = map[string]any{"id": i, "v": strings.Repeat("b", 30)}
	}
	obj := func() map[string]any {
		return map[string]any{
			"app":      map[string]any{"name": "Hjul"},
			"tags":     []any{"a", "b"},
			"builds":   builds,
			"included": []any{map[string]any{"id": "x"}, map[string]any{"id": "y"}},
		}
	}
	want3 := map[string]any{"app": map[string]any{"name": "Hjul"}, "tags": []any{"a", "b"}, "builds": builds[:3]}
	noIncl := obj()
	delete(noIncl, "included")

	tests := []struct {
		name        string
		in          any
		max         int
		wantDropped int
		check       func(t *testing.T, got any)
	}{
		{
			name: "object drops included then largest array tail", in: obj(), max: sizeOf(t, want3), wantDropped: 2 + 5,
			check: func(t *testing.T, got any) {
				m := got.(map[string]any)
				if _, ok := m["included"]; ok || len(m["builds"].([]any)) != 3 || len(m["tags"].([]any)) != 2 {
					t.Errorf("got %v", m)
				}
			},
		},
		{
			name: "object fixes returned", in: map[string]any{"builds": builds, "returned": 8, "page": 1},
			max: sizeOf(t, map[string]any{"builds": builds[:3], "returned": 8, "page": 1}), wantDropped: 5,
			check: func(t *testing.T, got any) {
				m := got.(map[string]any)
				if m["returned"] != 3 || fmt.Sprint(m["page"]) != "1" || len(m["builds"].([]any)) != 3 {
					t.Errorf("got %v, want 3 builds, returned 3, page kept", m)
				}
			},
		},
		{
			name: "object only included", in: obj(), max: sizeOf(t, noIncl), wantDropped: 2,
			check: func(t *testing.T, got any) {
				if len(got.(map[string]any)["builds"].([]any)) != 8 {
					t.Errorf("builds trimmed unnecessarily")
				}
			},
		},
		{
			name: "top-level array tail", in: builds, max: sizeOf(t, builds[:2]), wantDropped: 6,
			check: func(t *testing.T, got any) {
				if len(got.([]any)) != 2 {
					t.Errorf("got %d items", len(got.([]any)))
				}
			},
		},
		{
			name: "struct converted generically", in: struct {
				Items []string `json:"items"`
			}{Items: []string{"aaaa", "bbbb", "cccc"}}, max: len(`{"items":["aaaa"]}`), wantDropped: 2,
			check: func(t *testing.T, got any) {
				if sizeOf(t, got) != len(`{"items":["aaaa"]}`) {
					t.Errorf("got %v", got)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, tr, err := CapResult(tt.in, tt.max)
			if err != nil {
				t.Fatal(err)
			}
			if tr == nil || tr.Dropped != tt.wantDropped {
				t.Fatalf("truncation = %+v, want dropped %d", tr, tt.wantDropped)
			}
			if !strings.Contains(tr.Hint, "lower page_size or request the next page") || strings.Contains(tr.Hint, "cursor") {
				t.Errorf("hint = %q", tr.Hint)
			}
			if sizeOf(t, got) > tt.max {
				t.Errorf("result exceeds cap")
			}
			tt.check(t, got)
		})
	}
}

func TestCapResultMarshalError(t *testing.T) {
	if _, _, err := CapResult(map[string]any{"f": func() {}}, 10); err == nil {
		t.Fatal("want marshal error")
	}
}
