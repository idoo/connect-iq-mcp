package tool

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
)

// decodeArgs parses raw tool arguments into a JSON object, coerces
// stringified JSON where the schema expects an object or array (a common
// client bug), and validates the result against the resolved schema.
func decodeArgs(raw json.RawMessage, s *jsonschema.Schema, rs *jsonschema.Resolved) (map[string]any, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		raw = []byte("{}")
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("arguments are not JSON: %w", err)
	}
	if str, ok := v.(string); ok { // whole argument object sent as a string
		if err := json.Unmarshal([]byte(str), &v); err != nil {
			return nil, fmt.Errorf("arguments are not a JSON object: %w", err)
		}
	}
	args, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("arguments must be a JSON object")
	}
	coerce(s, args)
	if err := rs.Validate(args); err != nil {
		return nil, err
	}
	if name := blankRequired(s, args, ""); name != "" {
		return nil, fmt.Errorf("%s is required and must not be blank", name)
	}
	return args, nil
}

// blankRequired returns the path of the first required string property that
// is empty or only whitespace, recursively, or "".
func blankRequired(s *jsonschema.Schema, v any, path string) string {
	if s == nil {
		return ""
	}
	switch x := v.(type) {
	case map[string]any:
		for _, name := range s.Required {
			if str, ok := x[name].(string); ok && strings.TrimSpace(str) == "" {
				return join(path, name)
			}
		}
		for _, k := range slices.Sorted(maps.Keys(x)) {
			if p := blankRequired(s.Properties[k], x[k], join(path, k)); p != "" {
				return p
			}
		}
	case []any:
		for i, el := range x {
			if p := blankRequired(s.Items, el, fmt.Sprintf("%s[%d]", path, i)); p != "" {
				return p
			}
		}
	}
	return ""
}

func join(path, name string) string {
	if path == "" {
		return name
	}
	return path + "." + name
}

// coerce replaces string values with their parsed JSON where the schema
// expects an object or array, recursively.
func coerce(s *jsonschema.Schema, v any) any {
	if s == nil {
		return v
	}
	if str, ok := v.(string); ok && wantsContainer(s) {
		var parsed any
		if json.Unmarshal([]byte(str), &parsed) == nil {
			switch parsed.(type) {
			case map[string]any, []any:
				v = parsed
			}
		}
	}
	switch x := v.(type) {
	case map[string]any:
		for k, el := range x {
			if ps := s.Properties[k]; ps != nil {
				x[k] = coerce(ps, el)
			} else if s.AdditionalProperties != nil {
				x[k] = coerce(s.AdditionalProperties, el)
			}
		}
	case []any:
		for i, el := range x {
			x[i] = coerce(s.Items, el)
		}
	}
	return v
}

func wantsContainer(s *jsonschema.Schema) bool {
	types := s.Types
	if s.Type != "" {
		types = append(types, s.Type)
	}
	if slices.Contains(types, "string") {
		return false
	}
	return slices.Contains(types, "object") || slices.Contains(types, "array")
}
