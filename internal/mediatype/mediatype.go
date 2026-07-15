// Package mediatype parses and classifies ARD entry media types. The base
// (e.g. "application/ai-skill") identifies what a resource is; the RFC 6839
// structured-syntax suffix (+json, +md, +gzip) hints at how it's encoded,
// like an HTTP content type. Recognition keys on the base so suffix and
// parameter variants of the same resource are treated as one.
package mediatype

import "strings"

// MediaType is a parsed, normalized media type.
type MediaType struct {
	Raw    string            // untouched original input
	Type   string            // lowercased top-level type: "application", "text"
	Base   string            // lowercased subtype minus suffix: "ai-skill-archive"
	Suffix string            // lowercased suffix minus '+': "json", "md", "gzip", ""
	Params map[string]string // lowercased parameter names; values unquoted, as-is
}

// Parse normalizes s into a MediaType. It never panics; unparseable input
// yields zero-value Type/Base/Suffix and an empty Params map.
func Parse(s string) MediaType {
	m := MediaType{Raw: s, Params: map[string]string{}}
	head := strings.TrimSpace(s)

	if i := strings.IndexByte(head, ';'); i >= 0 {
		for _, p := range strings.Split(head[i+1:], ";") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			name, val, _ := strings.Cut(p, "=")
			name = strings.ToLower(strings.TrimSpace(name))
			val = strings.Trim(strings.TrimSpace(val), `"`)
			if name != "" {
				m.Params[name] = val
			}
		}
		head = strings.TrimSpace(head[:i])
	}

	subtype := head
	if i := strings.IndexByte(head, '/'); i >= 0 {
		m.Type = strings.ToLower(strings.TrimSpace(head[:i]))
		subtype = strings.TrimSpace(head[i+1:])
	}
	if i := strings.LastIndexByte(subtype, '+'); i >= 0 {
		m.Suffix = strings.ToLower(strings.TrimSpace(subtype[i+1:]))
		subtype = subtype[:i]
	}
	m.Base = strings.ToLower(strings.TrimSpace(subtype))
	return m
}

// FullType is Type + "/" + Base — the identity used for classification.
func (m MediaType) FullType() string {
	return m.Type + "/" + m.Base
}
