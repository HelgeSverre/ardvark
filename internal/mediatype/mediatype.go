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

// Kind is the semantic category of a media type — what the resource is,
// independent of how it's serialized.
type Kind int

const (
	KindUnknown   Kind = iota
	KindCatalog        // application/ai-catalog[...]  — pointer to a nested catalog
	KindRegistry       // application/ai-registry[...] — pointer to a registry endpoint
	KindSkill          // ai-skill, agent-skills, ai-skill-archive
	KindMCPServer      // mcp-server-card, mcp-server
	KindA2AAgent       // a2a-agent-card, a2a-agent
	KindGeneric        // recognized non-ARD artifact (openapi, linkset, json, markdown, …)
)

func (k Kind) String() string {
	switch k {
	case KindCatalog:
		return "catalog"
	case KindRegistry:
		return "registry"
	case KindSkill:
		return "skill"
	case KindMCPServer:
		return "mcp-server"
	case KindA2AAgent:
		return "a2a-agent"
	case KindGeneric:
		return "generic"
	default:
		return "unknown"
	}
}

// kindByFullType maps a normalized "type/base" to its Kind. Strict allowlist:
// anything absent is KindUnknown. Adding a newly-observed base is a one-line
// edit here.
var kindByFullType = map[string]Kind{
	"application/ai-skill":         KindSkill,
	"application/agent-skills":     KindSkill,
	"application/ai-skill-archive": KindSkill,
	"application/mcp-server-card":  KindMCPServer,
	"application/mcp-server":       KindMCPServer,
	"application/a2a-agent-card":   KindA2AAgent,
	"application/a2a-agent":        KindA2AAgent,
	"application/ai-catalog":       KindCatalog,
	"application/ai-registry":      KindRegistry,
	"application/json":             KindGeneric,
	"application/vnd.oai.openapi":  KindGeneric,
	"application/linkset":          KindGeneric,
	"text/markdown":                KindGeneric,
	"text/plain":                   KindGeneric,
	"text/html":                    KindGeneric,
}

// Kind classifies the media type. A profile parameter of "urn:air:agent-skills"
// marks a skill regardless of the carrier type (e.g. text/markdown).
func (m MediaType) Kind() Kind {
	base := kindByFullType[m.FullType()]
	if base != KindCatalog && base != KindRegistry && m.Profile() == "urn:air:agent-skills" {
		return KindSkill
	}
	return base
}

// IsPointer reports whether the entry points at another ARD document to crawl
// (a nested catalog or a registry endpoint).
func (m MediaType) IsPointer() bool {
	k := m.Kind()
	return k == KindCatalog || k == KindRegistry
}

// IsKnown reports whether the media type classified to any recognized Kind
// (KindGeneric counts as known).
func (m MediaType) IsKnown() bool {
	return m.Kind() != KindUnknown
}

// Profile returns the "profile" media-type parameter, or "".
func (m MediaType) Profile() string {
	return m.Params["profile"]
}

// Format is the concrete serialization/encoding hint — how the artifact is
// encoded — derived from the suffix, the base tail, or the top-level type.
type Format int

const (
	FormatUnknown Format = iota
	FormatJSON
	FormatMarkdown
	FormatZip
	FormatGzip
	FormatHTML
	FormatText
)

func (f Format) String() string {
	switch f {
	case FormatJSON:
		return "json"
	case FormatMarkdown:
		return "markdown"
	case FormatZip:
		return "zip"
	case FormatGzip:
		return "gzip"
	case FormatHTML:
		return "html"
	case FormatText:
		return "text"
	default:
		return "unknown"
	}
}

// Format derives the encoding hint. Suffix wins; otherwise text/* subtypes
// map to their obvious formats; otherwise Unknown.
func (m MediaType) Format() Format {
	switch m.Suffix {
	case "json":
		return FormatJSON
	case "md":
		return FormatMarkdown
	case "zip":
		return FormatZip
	case "gzip":
		return FormatGzip
	}
	if m.Type == "text" {
		switch m.Base {
		case "markdown":
			return FormatMarkdown
		case "html":
			return FormatHTML
		case "plain":
			return FormatText
		}
	}
	return FormatUnknown
}
