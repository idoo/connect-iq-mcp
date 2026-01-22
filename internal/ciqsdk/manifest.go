package ciqsdk

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// Manifest is the parsed manifest.xml.
type Manifest struct {
	ID          string   `json:"id"`
	Type        string   `json:"type"`
	Name        string   `json:"name,omitempty"`
	Entry       string   `json:"entry,omitempty"`
	MinAPILevel string   `json:"min_api_level,omitempty"`
	Version     string   `json:"version,omitempty"`
	Products    []string `json:"products"`
	Permissions []string `json:"permissions,omitempty"`
	Languages   []string `json:"languages,omitempty"`
}

// ParseManifest parses manifest.xml (namespace-agnostic local names).
// minApiLevel falls back to the older minSdkVersion attribute.
func ParseManifest(b []byte) (Manifest, error) {
	var m Manifest
	dec := xml.NewDecoder(bytes.NewReader(b))
	var stack []string
	found := false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return Manifest{}, fmt.Errorf("manifest.xml: %w", err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			parent := ""
			if len(stack) > 0 {
				parent = stack[len(stack)-1]
			}
			stack = append(stack, t.Name.Local)
			switch t.Name.Local {
			case "application", "barrel":
				found = true
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "id":
						m.ID = a.Value
					case "type":
						m.Type = a.Value
					case "name":
						m.Name = a.Value
					case "entry":
						m.Entry = a.Value
					case "version":
						m.Version = a.Value
					case "minApiLevel":
						m.MinAPILevel = a.Value
					case "minSdkVersion":
						if m.MinAPILevel == "" {
							m.MinAPILevel = a.Value
						}
					}
				}
				if t.Name.Local == "barrel" && m.Type == "" {
					m.Type = "barrel"
				}
			case "product":
				if parent == "products" {
					m.Products = append(m.Products, attr(t, "id"))
				}
			case "uses":
				if parent == "permissions" {
					m.Permissions = append(m.Permissions, attr(t, "id"))
				}
			}
		case xml.CharData:
			if len(stack) > 0 && stack[len(stack)-1] == "language" {
				if s := strings.TrimSpace(string(t)); s != "" {
					m.Languages = append(m.Languages, s)
				}
			}
		case xml.EndElement:
			stack = stack[:len(stack)-1]
		}
	}
	if !found {
		return Manifest{}, fmt.Errorf("manifest.xml has no <iq:application> element")
	}
	if m.Products == nil {
		m.Products = []string{}
	}
	return m, nil
}

func attr(t xml.StartElement, name string) string {
	for _, a := range t.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

var (
	productsOpenRE = regexp.MustCompile(`<(\w+:)?products\s*>`)
	productsSelfRE = regexp.MustCompile(`<(\w+:)?products\s*/>`)
	productLineRE  = regexp.MustCompile(`(?m)^([ \t]*)<(\w+:)?product\s+id\s*=\s*["']([^"']+)["']\s*/>[ \t]*\r?\n?`)
	appCloseRE     = regexp.MustCompile(`</(\w+:)?(application|barrel)\s*>`)
)

// SetProducts adds and removes <iq:product> entries with a text edit of the
// products block; bytes outside the touched lines are unchanged (comments
// included). New products go before </iq:products>, sorted, using the
// indentation of an existing product line. It returns the products actually
// added and removed.
func SetProducts(b []byte, add, remove []string) (out []byte, added, removed []string, err error) {
	m, err := ParseManifest(b)
	if err != nil {
		return nil, nil, nil, err
	}
	have := map[string]bool{}
	for _, p := range m.Products {
		have[p] = true
	}
	rm := map[string]bool{}
	for _, p := range remove {
		if have[p] {
			rm[p] = true
		}
	}
	for _, p := range uniqSorted(add) {
		if !ValidDeviceID(p) {
			return nil, nil, nil, fmt.Errorf("invalid product id %q", p)
		}
		if rm[p] {
			return nil, nil, nil, fmt.Errorf("product %q is both added and removed", p)
		}
		if !have[p] {
			added = append(added, p)
		}
	}
	s := string(b)
	// Ensure an open/close products block exists.
	if productsOpenRE.FindStringIndex(s) == nil {
		if loc := productsSelfRE.FindStringSubmatchIndex(s); loc != nil {
			pfx := s[loc[2]:loc[3]]
			s = s[:loc[0]] + "<" + pfx + "products>\n" + lineIndent(s, loc[0]) + "</" + pfx + "products>" + s[loc[1]:]
		} else if len(added) > 0 {
			loc := appCloseRE.FindStringSubmatchIndex(s)
			if loc == nil {
				return nil, nil, nil, fmt.Errorf("manifest.xml has no closing application element")
			}
			pfx := ""
			if loc[2] >= 0 {
				pfx = s[loc[2]:loc[3]]
			}
			ind := lineIndent(s, loc[0])
			inner := ind + "    "
			s = s[:loc[0]] + "    <" + pfx + "products>\n" + inner + "</" + pfx + "products>\n" + ind + s[loc[0]:]
		}
	}
	open := productsOpenRE.FindStringSubmatchIndex(s)
	if open == nil {
		return b, nil, nil, nil // nothing to add, no block to remove from
	}
	pfx := ""
	if open[2] >= 0 {
		pfx = s[open[2]:open[3]]
	}
	closeTag := "</" + pfx + "products>"
	ci := strings.Index(s[open[1]:], closeTag)
	if ci < 0 {
		return nil, nil, nil, fmt.Errorf("manifest.xml: unterminated products block")
	}
	start, end := open[1], open[1]+ci
	block := s[start:end]
	indent := ""
	block = productLineRE.ReplaceAllStringFunc(block, func(line string) string {
		sm := productLineRE.FindStringSubmatch(line)
		if indent == "" {
			indent = sm[1]
		}
		if rm[sm[3]] {
			removed = append(removed, sm[3])
			return ""
		}
		return line
	})
	if indent == "" {
		indent = lineIndent(s, open[0]) + "    "
	}
	if len(added) > 0 {
		var nb strings.Builder
		for _, p := range added {
			nb.WriteString(indent + "<" + pfx + "product id=\"" + p + "\"/>\n")
		}
		// Insert at the start of the closing tag's line.
		cut := strings.LastIndexByte(block, '\n') + 1
		if cut == 0 { // close tag on the same line as the open tag
			block += "\n" + nb.String() + lineIndent(s, open[0])
		} else {
			block = block[:cut] + nb.String() + block[cut:]
		}
	}
	sort.Strings(removed)
	return []byte(s[:start] + block + s[end:]), added, removed, nil
}

// lineIndent returns the leading whitespace of the line containing offset i.
func lineIndent(s string, i int) string {
	ls := strings.LastIndexByte(s[:i], '\n') + 1
	j := ls
	for j < len(s) && (s[j] == ' ' || s[j] == '\t') {
		j++
	}
	return s[ls:j]
}

func uniqSorted(xs []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		x = strings.TrimSpace(x)
		if x != "" && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	sort.Strings(out)
	return out
}
