package tool

// UntrustedFallback lists common free-text keys written by third parties
// (store reviewers, users) or copied from them. [WrapUntrusted] uses it when
// the caller passes no keys.
var UntrustedFallback = []string{
	"body",
	"comment",
	"description",
	"name",
	"notes",
	"response",
	"reviewText",
	"text",
	"title",
	"userName",
	"whatsNew",
}

// WrapUntrusted replaces, in place, third-party text in v with [Untrusted] so
// it marshals as {"untrusted": text}. Strings under any of keys (nil means
// [UntrustedFallback]) are wrapped wherever they appear, including inside
// nested objects and arrays; all=true wraps every string. Already wrapped
// values are left as they are, so WrapUntrusted is idempotent.
func WrapUntrusted(v any, keys []string, all bool) any {
	if all {
		return wrapAll(v)
	}
	if keys == nil {
		keys = UntrustedFallback
	}
	set := make(map[string]bool, len(keys))
	for _, k := range keys {
		set[k] = true
	}
	return wrapKeys("", v, set)
}

// wrapKeys wraps v entirely when key is free text, otherwise it descends
// into objects and arrays looking for free-text keys.
func wrapKeys(key string, v any, set map[string]bool) any {
	if set[key] {
		return wrapAll(v)
	}
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = wrapKeys(k, e, set)
		}
	case []any:
		for i, e := range x {
			x[i] = wrapKeys(key, e, set)
		}
	}
	return v
}

// wrapAll wraps every string in v, descending into objects and arrays.
func wrapAll(v any) any {
	switch x := v.(type) {
	case string:
		return Untrusted(x)
	case map[string]any:
		for k, e := range x {
			x[k] = wrapAll(e)
		}
	case []any:
		for i, e := range x {
			x[i] = wrapAll(e)
		}
	}
	return v
}

// Unwrap returns a copy of v with every [Untrusted] value, at any depth,
// replaced by its plain string. Use it before decoding or comparing
// attributes the client wrapped; never to echo text back to the agent.
func Unwrap(v any) any {
	switch x := v.(type) {
	case Untrusted:
		return string(x)
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[k] = Unwrap(e)
		}
		return m
	case []any:
		s := make([]any, len(x))
		for i, e := range x {
			s[i] = Unwrap(e)
		}
		return s
	}
	return v
}
