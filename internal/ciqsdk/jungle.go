package ciqsdk

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"
)

// JunglePath is one path a monkey.jungle makes monkeyc read: the key that
// names it and the path as written (relative to the jungle's directory unless
// absolute), with $(var) references expanded.
type JunglePath struct {
	Key  string
	Path string
}

// IsJunglePathKey reports whether a jungle key's value is a ;-separated list
// of paths: *.sourcePath, *.resourcePath, *.barrelPath, *.manifest,
// *.personality and *.lang.<code> (resource folders). excludeAnnotations and
// the other keys hold names, not paths.
func IsJunglePathKey(key string) bool {
	parts := strings.Split(key, ".")
	last := parts[len(parts)-1]
	for _, k := range []string{"sourcePath", "resourcePath", "barrelPath", "manifest", "personality"} {
		if strings.EqualFold(last, k) {
			return true
		}
	}
	return len(parts) >= 2 && strings.EqualFold(parts[len(parts)-2], "lang")
}

// maxJunglePaths bounds the expansion of $(var) references.
const maxJunglePaths = 10000

// JunglePaths parses a jungle file (`key = a;b` lines, # comments) and
// returns every path its path-valued keys name. A $(var) reference expands
// to every value assigned to var; a whole-entry reference to a key the file
// never sets (a compiler default such as $(base.sourcePath)) or to the key
// being defined (appending) is skipped. A reference inside a longer entry
// must expand to exactly one value, else it is an error, since the path it
// names could not be checked.
func JunglePaths(data []byte) ([]JunglePath, error) {
	type assign struct{ key, val string }
	var as []assign
	vals := map[string][]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if k == "" {
			continue
		}
		as = append(as, assign{k, v})
		vals[k] = append(vals[k], v)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	var out []JunglePath
	for _, a := range as {
		if !IsJunglePathKey(a.key) {
			continue
		}
		for _, tok := range strings.Split(a.val, ";") {
			ps, err := expandJungle(strings.TrimSpace(tok), vals, map[string]bool{a.key: true})
			if err != nil {
				return nil, fmt.Errorf("%s: %w", a.key, err)
			}
			for _, p := range ps {
				out = append(out, JunglePath{Key: a.key, Path: p})
			}
			if len(out) > maxJunglePaths {
				return nil, fmt.Errorf("%s: too many paths after expanding $(...) references", a.key)
			}
		}
	}
	return out, nil
}

func expandJungle(tok string, vals map[string][]string, seen map[string]bool) ([]string, error) {
	if tok == "" {
		return nil, nil
	}
	i := strings.Index(tok, "$(")
	if i < 0 {
		return []string{tok}, nil
	}
	j := strings.IndexByte(tok[i:], ')')
	if j < 0 {
		return nil, fmt.Errorf("unterminated $( in %q", tok)
	}
	name := strings.TrimSpace(tok[i+2 : i+j])
	defs, known := vals[name]
	known = known && !seen[name]
	var parts []string
	if known {
		for _, d := range defs {
			for _, p := range strings.Split(d, ";") {
				if p = strings.TrimSpace(p); p != "" {
					parts = append(parts, p)
				}
			}
		}
	}
	whole := i == 0 && j == len(tok)-1
	if !whole && len(parts) != 1 {
		return nil, fmt.Errorf("cannot check %q: $(%s) must be a whole entry or name exactly one value", tok, name)
	}
	next := make(map[string]bool, len(seen)+1)
	for k := range seen {
		next[k] = true
	}
	next[name] = true
	var out []string
	for _, p := range parts {
		if !whole {
			p = tok[:i] + p + tok[i+j+1:]
		}
		ps, err := expandJungle(p, vals, next)
		if err != nil {
			return nil, err
		}
		out = append(out, ps...)
		if len(out) > maxJunglePaths {
			return nil, fmt.Errorf("too many paths after expanding $(%s)", name)
		}
	}
	return out, nil
}
