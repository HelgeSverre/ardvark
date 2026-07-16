# Media-type parsing & classification Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a standalone `internal/mediatype` package that parses ARD entry media types into base + suffix + params, classifies them by semantic Kind and encoding Format, and rewire verify + crawler to use it — closing the unsuffixed-pointer crawl gap and silencing spurious warnings on recognized types.

**Architecture:** One new pure package `internal/mediatype` (no deps beyond `strings`). `internal/ard/verify.go` and `internal/crawler/{handlers,engine}.go` delegate recognition and pointer-detection to it. No DB schema change: the format hint is surfaced in verify messages/logs only; the `media_type` column stays the raw string.

**Tech Stack:** Go 1.26, standard library only. Tests are `go test` table-driven; existing crawler tests use `httptest` + `newTestEngine`.

## Global Constraints

- Module path: `github.com/helgesverre/ardvark`.
- Standard library only for `internal/mediatype` (import `strings` only).
- Strict allowlist classification: any type not explicitly mapped is `KindUnknown`. No heuristics.
- `KindGeneric` counts as known (`IsKnown() == true`) and must NOT emit a verify warning; only `KindUnknown` warns.
- `Parse` is a total function: never panics; garbage input yields `KindUnknown` / `FormatUnknown`.
- No DB migration. Do not touch `internal/store` models.
- Commit after each task. Run `just check` (vet + gofmt + full test) before the final task's commit.
- Design reference: `docs/superpowers/specs/2026-07-16-media-type-parsing-design.md`.

---

### Task 1: `mediatype.Parse` — fields, params, FullType

**Files:**
- Create: `internal/mediatype/mediatype.go`
- Test: `internal/mediatype/mediatype_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces:
  - `type MediaType struct { Raw, Type, Base, Suffix string; Params map[string]string }`
  - `func Parse(s string) MediaType`
  - `func (m MediaType) FullType() string` — returns `Type + "/" + Base`

- [ ] **Step 1: Write the failing test**

```go
package mediatype

import (
	"reflect"
	"testing"
)

func TestParse_Fields(t *testing.T) {
	tests := []struct {
		in                        string
		wantType, wantBase, wantSuffix string
		wantParams                map[string]string
		wantFullType              string
	}{
		{"application/ai-catalog+json", "application", "ai-catalog", "json", map[string]string{}, "application/ai-catalog"},
		{"application/ai-registry", "application", "ai-registry", "", map[string]string{}, "application/ai-registry"},
		{"application/ai-skill-archive+gzip", "application", "ai-skill-archive", "gzip", map[string]string{}, "application/ai-skill-archive"},
		{"APPLICATION/AI-Catalog+JSON", "application", "ai-catalog", "json", map[string]string{}, "application/ai-catalog"},
		{"  application/ai-skill  ", "application", "ai-skill", "", map[string]string{}, "application/ai-skill"},
		{`text/markdown; profile="urn:air:agent-skills"`, "text", "markdown", "", map[string]string{"profile": "urn:air:agent-skills"}, "text/markdown"},
		{"application/ai-catalog+json; charset=utf-8", "application", "ai-catalog", "json", map[string]string{"charset": "utf-8"}, "application/ai-catalog"},
		{"application/vnd.oai.openapi", "application", "vnd.oai.openapi", "", map[string]string{}, "application/vnd.oai.openapi"},
		{"garbage", "", "garbage", "", map[string]string{}, "/garbage"},
		{"application/", "application", "", "", map[string]string{}, "application/"},
		{"", "", "", "", map[string]string{}, "/"},
	}
	for _, tt := range tests {
		m := Parse(tt.in)
		if m.Raw != tt.in {
			t.Errorf("Parse(%q).Raw = %q, want %q", tt.in, m.Raw, tt.in)
		}
		if m.Type != tt.wantType || m.Base != tt.wantBase || m.Suffix != tt.wantSuffix {
			t.Errorf("Parse(%q) = {Type:%q Base:%q Suffix:%q}, want {%q %q %q}", tt.in, m.Type, m.Base, m.Suffix, tt.wantType, tt.wantBase, tt.wantSuffix)
		}
		if !reflect.DeepEqual(m.Params, tt.wantParams) {
			t.Errorf("Parse(%q).Params = %v, want %v", tt.in, m.Params, tt.wantParams)
		}
		if m.FullType() != tt.wantFullType {
			t.Errorf("Parse(%q).FullType() = %q, want %q", tt.in, m.FullType(), tt.wantFullType)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mediatype/`
Expected: FAIL — `undefined: Parse` / `undefined: MediaType`.

- [ ] **Step 3: Write minimal implementation**

```go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/mediatype/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/mediatype/mediatype.go internal/mediatype/mediatype_test.go
git commit -m "feat(mediatype): parse media types into type/base/suffix/params"
```

---

### Task 2: Kind classification

**Files:**
- Modify: `internal/mediatype/mediatype.go`
- Test: `internal/mediatype/mediatype_test.go`

**Interfaces:**
- Consumes: `MediaType`, `FullType()` from Task 1.
- Produces:
  - `type Kind int` with `KindUnknown, KindCatalog, KindRegistry, KindSkill, KindMCPServer, KindA2AAgent, KindGeneric`
  - `func (k Kind) String() string`
  - `func (m MediaType) Kind() Kind`
  - `func (m MediaType) IsPointer() bool` — true for Catalog/Registry
  - `func (m MediaType) IsKnown() bool` — true unless `KindUnknown`
  - `func (m MediaType) Profile() string` — `Params["profile"]`

- [ ] **Step 1: Write the failing test**

```go
func TestKindClassification(t *testing.T) {
	tests := []struct {
		in        string
		wantKind  Kind
		pointer   bool
		known     bool
	}{
		{"application/ai-catalog+json", KindCatalog, true, true},
		{"application/ai-catalog", KindCatalog, true, true},
		{"application/ai-registry+json", KindRegistry, true, true},
		{"application/ai-registry", KindRegistry, true, true},
		{"application/ai-skill", KindSkill, false, true},
		{"application/ai-skill+md", KindSkill, false, true},
		{"application/agent-skills+md", KindSkill, false, true},
		{"application/agent-skills+gzip", KindSkill, false, true},
		{"application/ai-skill-archive+gzip", KindSkill, false, true},
		{`text/markdown; profile="urn:air:agent-skills"`, KindSkill, false, true},
		{"application/mcp-server-card+json", KindMCPServer, false, true},
		{"application/mcp-server+json", KindMCPServer, false, true},
		{"application/a2a-agent-card+json", KindA2AAgent, false, true},
		{"application/a2a-agent+json", KindA2AAgent, false, true},
		{"application/json", KindGeneric, false, true},
		{"application/vnd.oai.openapi", KindGeneric, false, true},
		{"application/linkset+json", KindGeneric, false, true},
		{"text/markdown", KindGeneric, false, true},
		{"text/plain", KindGeneric, false, true},
		{"text/html", KindGeneric, false, true},
		{"application/octet-stream", KindUnknown, false, false},
		{"garbage", KindUnknown, false, false},
		{"", KindUnknown, false, false},
	}
	for _, tt := range tests {
		m := Parse(tt.in)
		if got := m.Kind(); got != tt.wantKind {
			t.Errorf("Parse(%q).Kind() = %v, want %v", tt.in, got, tt.wantKind)
		}
		if got := m.IsPointer(); got != tt.pointer {
			t.Errorf("Parse(%q).IsPointer() = %v, want %v", tt.in, got, tt.pointer)
		}
		if got := m.IsKnown(); got != tt.known {
			t.Errorf("Parse(%q).IsKnown() = %v, want %v", tt.in, got, tt.known)
		}
	}
}

func TestProfile(t *testing.T) {
	if got := Parse(`text/markdown; profile="urn:air:agent-skills"`).Profile(); got != "urn:air:agent-skills" {
		t.Errorf("Profile() = %q, want urn:air:agent-skills", got)
	}
	if got := Parse("application/ai-skill").Profile(); got != "" {
		t.Errorf("Profile() = %q, want empty", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mediatype/ -run 'TestKindClassification|TestProfile'`
Expected: FAIL — `undefined: KindCatalog` etc.

- [ ] **Step 3: Write minimal implementation** (append to `mediatype.go`)

```go
// Kind is the semantic category of a media type — what the resource is,
// independent of how it's serialized.
type Kind int

const (
	KindUnknown Kind = iota
	KindCatalog   // application/ai-catalog[...]  — pointer to a nested catalog
	KindRegistry  // application/ai-registry[...] — pointer to a registry endpoint
	KindSkill     // ai-skill, agent-skills, ai-skill-archive
	KindMCPServer // mcp-server-card, mcp-server
	KindA2AAgent  // a2a-agent-card, a2a-agent
	KindGeneric   // recognized non-ARD artifact (openapi, linkset, json, markdown, …)
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
	if m.Profile() == "urn:air:agent-skills" {
		return KindSkill
	}
	return kindByFullType[m.FullType()]
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/mediatype/`
Expected: PASS (all tests, including Task 1).

- [ ] **Step 5: Commit**

```bash
git add internal/mediatype/mediatype.go internal/mediatype/mediatype_test.go
git commit -m "feat(mediatype): classify by semantic Kind with pointer/known helpers"
```

---

### Task 3: Format hint

**Files:**
- Modify: `internal/mediatype/mediatype.go`
- Test: `internal/mediatype/mediatype_test.go`

**Interfaces:**
- Consumes: `MediaType` fields from Task 1.
- Produces:
  - `type Format int` with `FormatUnknown, FormatJSON, FormatMarkdown, FormatZip, FormatGzip, FormatHTML, FormatText`
  - `func (f Format) String() string`
  - `func (m MediaType) Format() Format`

- [ ] **Step 1: Write the failing test**

```go
func TestFormat(t *testing.T) {
	tests := []struct {
		in   string
		want Format
	}{
		{"application/ai-catalog+json", FormatJSON},
		{"application/ai-skill+md", FormatMarkdown},
		{"application/agent-skills+md", FormatMarkdown},
		{"application/agent-skills+zip", FormatZip},
		{"application/ai-skill-archive+gzip", FormatGzip},
		{"text/markdown", FormatMarkdown},
		{"text/html", FormatHTML},
		{"text/plain", FormatText},
		{"application/vnd.oai.openapi", FormatUnknown},
		{"application/ai-skill", FormatUnknown},
		{"garbage", FormatUnknown},
	}
	for _, tt := range tests {
		if got := Parse(tt.in).Format(); got != tt.want {
			t.Errorf("Parse(%q).Format() = %v, want %v", tt.in, got, tt.want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/mediatype/ -run TestFormat`
Expected: FAIL — `undefined: FormatJSON` etc.

- [ ] **Step 3: Write minimal implementation** (append to `mediatype.go`)

```go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/mediatype/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/mediatype/mediatype.go internal/mediatype/mediatype_test.go
git commit -m "feat(mediatype): derive encoding Format hint from suffix/type"
```

---

### Task 4: Wire `internal/ard/verify.go` to the parser

**Files:**
- Modify: `internal/ard/verify.go` (imports; delete `mediaType*` consts + `knownEntryMediaTypes` map lines 98–130; delete `isPointerMediaType` lines 572–576; rewrite the entry loop tail in `semanticChecks` ~lines 430–434; rewrite `checkMediaType` ~lines 593–599)
- Test: `internal/ard/verify_test.go`

**Interfaces:**
- Consumes: `mediatype.Parse`, `MediaType.IsPointer`, `IsKnown`, `Kind`, `Format` from Tasks 1–3.
- Produces: unchanged public `Verify` behavior except recognition is now base-aware.

- [ ] **Step 1: Write the failing test** (append to `verify_test.go`)

```go
func TestVerify_MediaType_KnownFamiliesNoWarning(t *testing.T) {
	raw := []byte(`{"specVersion":"1.0","host":{"displayName":"H"},"entries":[
		{"identifier":"urn:air:example.com:skills:s","displayName":"S","type":"application/agent-skills+md","url":"https://example.com/s.md"},
		{"identifier":"urn:air:example.com:arch:a","displayName":"A","type":"application/ai-skill-archive+gzip","url":"https://example.com/a.tar.gz"},
		{"identifier":"urn:air:example.com:api:o","displayName":"O","type":"application/vnd.oai.openapi","url":"https://example.com/o.yaml"}
	]}`)
	report := Verify(raw, "example.com")
	for _, c := range report.Checks {
		if c.CheckID == "entry.media_type" && !c.Passed {
			t.Errorf("unexpected failed entry.media_type check: %+v", c)
		}
	}
}

func TestVerify_MediaType_UnsuffixedRegistryIsPointer(t *testing.T) {
	raw := []byte(`{"specVersion":"1.0","host":{"displayName":"H"},"entries":[
		{"identifier":"urn:air:example.com:registry:r","displayName":"R","type":"application/ai-registry","url":"https://example.com/api"}
	]}`)
	report := Verify(raw, "example.com")
	// Pointer entries must not be nagged about representativeQueries.
	if _, ok := checkByID(report.Checks, "queries.count", "urn:air:example.com:registry:r"); ok {
		t.Error("queries.count should be skipped for an unsuffixed application/ai-registry pointer")
	}
}

func TestVerify_MediaType_UnknownStillWarns(t *testing.T) {
	raw := []byte(`{"specVersion":"1.0","host":{"displayName":"H"},"entries":[
		{"identifier":"urn:air:example.com:x:y","displayName":"Y","type":"application/octet-stream","url":"https://example.com/y.bin"}
	]}`)
	report := Verify(raw, "example.com")
	c, ok := checkByID(report.Checks, "entry.media_type", "urn:air:example.com:x:y")
	if !ok || c.Passed {
		t.Errorf("expected failed entry.media_type warning for unknown type, got ok=%v check=%+v", ok, c)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/ard/ -run TestVerify_MediaType`
Expected: FAIL — `agent-skills+md` / `ai-skill-archive+gzip` / `vnd.oai.openapi` currently warn (not in the old allowlist), and the unsuffixed registry is not treated as a pointer.

- [ ] **Step 3: Delete the old media-type consts and map**

In `internal/ard/verify.go`, delete the entire block from the comment `// ARD entry media types, as of specVersion 1.0.` through the closing `}` of `var knownEntryMediaTypes = map[string]bool{ ... }` (lines ~98–131). Also delete `isPointerMediaType` (lines ~572–576):

```go
// (delete this function)
func isPointerMediaType(mediaType string) bool {
	return mediaType == mediaTypeAICatalog || mediaType == mediaTypeAIRegistry
}
```

- [ ] **Step 4: Add the import**

In the `import (` block of `internal/ard/verify.go`, add:

```go
	"github.com/helgesverre/ardvark/internal/mediatype"
```

- [ ] **Step 5: Rewrite the entry loop tail in `semanticChecks`**

Replace:

```go
		// representativeQueries describe a callable capability; they are not
		// meaningful for container/pointer entries (a nested catalog or a
		// registry endpoint), so don't warn on their absence there.
		if !isPointerMediaType(e.Type) {
			checks = append(checks, checkQueriesCount(e, subject))
		}
		checks = append(checks, checkMediaType(e, subject))
```

with:

```go
		mt := mediatype.Parse(e.Type)

		// representativeQueries describe a callable capability; they are not
		// meaningful for container/pointer entries (a nested catalog or a
		// registry endpoint), so don't warn on their absence there.
		if !mt.IsPointer() {
			checks = append(checks, checkQueriesCount(e, subject))
		}
		checks = append(checks, checkMediaType(e, mt, subject))
```

- [ ] **Step 6: Rewrite `checkMediaType`**

Replace:

```go
// checkMediaType: entry.media_type (warning) — unrecognized ARD media type.
func checkMediaType(e Entry, subject string) Check {
	passed := knownEntryMediaTypes[e.Type]
	return newCheck("entry.media_type", SeverityWarning, subject, passed,
		fmt.Sprintf("type %q is a recognized ARD media type", e.Type),
		fmt.Sprintf("type %q is not a recognized ARD media type (spec does not enforce this strictly)", e.Type))
}
```

with:

```go
// checkMediaType: entry.media_type (warning) — unrecognized media type. A type
// classifies as known when it maps to any Kind (including a recognized non-ARD
// artifact type); the message reports the classified kind and encoding hint.
func checkMediaType(e Entry, mt mediatype.MediaType, subject string) Check {
	passed := mt.IsKnown()
	return newCheck("entry.media_type", SeverityWarning, subject, passed,
		fmt.Sprintf("type %q recognized as %s (format: %s)", e.Type, mt.Kind(), mt.Format()),
		fmt.Sprintf("type %q is not a recognized ARD media type (spec does not enforce this strictly)", e.Type))
}
```

- [ ] **Step 7: Build and run tests**

Run: `go build ./... && go test ./internal/ard/`
Expected: build succeeds (no dangling references to the deleted consts), all `internal/ard` tests PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/ard/verify.go internal/ard/verify_test.go
git commit -m "feat(ard): recognize media types via mediatype parser, base-aware pointers"
```

---

### Task 5: Wire the crawler pointer-follow to Kind (closes the coverage gap)

**Files:**
- Modify: `internal/crawler/handlers.go` (import; `enqueueEntryFollowups` switch ~lines 273–300)
- Modify: `internal/crawler/engine.go` (delete unused `mediaTypeAICatalog` / `mediaTypeAIRegistry` consts ~lines 68–72)
- Test: `internal/crawler/crawler_test.go`

**Interfaces:**
- Consumes: `mediatype.Parse`, `MediaType.Kind`, `KindCatalog`, `KindRegistry` from Tasks 1–2.
- Produces: crawler now follows any Kind-Catalog / Kind-Registry pointer regardless of suffix.

- [ ] **Step 1: Write the failing test** (append to `crawler_test.go`)

```go
func TestEnqueueFollowups_UnsuffixedRegistryPointerIsFollowed(t *testing.T) {
	mux := http.NewServeMux()
	catalog := `{"specVersion":"1.0","host":{"displayName":"H"},"entries":[
		{"identifier":"urn:air:example.com:registry:r","displayName":"R","type":"application/ai-registry","url":"https://registry.example/api"}
	]}`
	mux.HandleFunc("/reg.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/ai-catalog+json")
		w.Write([]byte(catalog))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	eng, st := newTestEngine(t, testCrawlerConfig())
	host := strings.TrimPrefix(srv.URL, "http://")
	item := store.FrontierItem{URL: srv.URL + "/reg.json", Host: host, Depth: 0}
	if err := eng.handleCatalogFetch(context.Background(), item); err != nil {
		t.Fatalf("handleCatalogFetch: %v", err)
	}

	var harvests int64
	st.DB.Model(&store.FrontierItem{}).Where("kind = ?", store.KindRegistryHarvest).Count(&harvests)
	if harvests != 1 {
		t.Fatalf("expected 1 registry_harvest for unsuffixed application/ai-registry, got %d", harvests)
	}
	var regRows int64
	st.DB.Model(&store.Registry{}).Count(&regRows)
	if regRows != 1 {
		t.Fatalf("expected 1 registries row, got %d", regRows)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/crawler/ -run TestEnqueueFollowups_UnsuffixedRegistryPointerIsFollowed`
Expected: FAIL — `expected 1 registry_harvest …, got 0` (exact match on `application/ai-registry+json` misses the unsuffixed form).

- [ ] **Step 3: Add the import to `handlers.go`**

In the `import (` block of `internal/crawler/handlers.go`, add:

```go
	"github.com/helgesverre/ardvark/internal/mediatype"
```

- [ ] **Step 4: Rewrite the `enqueueEntryFollowups` switch**

Replace the loop body head + the three `case` conditions:

```go
	for i, en := range entries {
		entryID := cat.Entries[i].ID

		switch {
		case en.Type == mediaTypeAICatalog && en.URL != "":
```
```go
		case en.Type == mediaTypeAICatalog && hasEmbeddedData(en.Data):
```
```go
		case en.Type == mediaTypeAIRegistry && en.URL != "" && e.cfg.Registry.Harvest:
```

with:

```go
	for i, en := range entries {
		entryID := cat.Entries[i].ID
		kind := mediatype.Parse(en.Type).Kind()

		switch {
		case kind == mediatype.KindCatalog && en.URL != "":
```
```go
		case kind == mediatype.KindCatalog && hasEmbeddedData(en.Data):
```
```go
		case kind == mediatype.KindRegistry && en.URL != "" && e.cfg.Registry.Harvest:
```

(Leave the bodies of each case unchanged.)

- [ ] **Step 5: Delete the now-unused consts in `engine.go`**

Remove from `internal/crawler/engine.go`:

```go
// ARD entry media types the engine treats specially (see
// processCatalog).
const (
	mediaTypeAICatalog  = "application/ai-catalog+json"
	mediaTypeAIRegistry = "application/ai-registry+json"
)
```

- [ ] **Step 6: Build and run the test**

Run: `go build ./... && go test ./internal/crawler/ -run TestEnqueueFollowups_UnsuffixedRegistryPointerIsFollowed`
Expected: build succeeds; test PASSES.

- [ ] **Step 7: Run the full crawler suite (no regressions)**

Run: `go test ./internal/crawler/`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add internal/crawler/handlers.go internal/crawler/engine.go internal/crawler/crawler_test.go
git commit -m "feat(crawler): follow catalog/registry pointers by Kind, not exact suffix"
```

---

### Task 6: Full regression guard + real-catalog verification

**Files:**
- None modified. This task verifies the whole change against the official conformance fixtures and our own catalog.

**Interfaces:**
- Consumes: the built `ardvark` binary and `website/.well-known/ai-catalog.json`.

- [ ] **Step 1: Run the full gate**

Run: `just check`
Expected: `go vet` clean, gofmt clean, all packages PASS.

- [ ] **Step 2: Build the binary**

Run: `go build -o bin/ardvark ./cmd/ardvark`
Expected: builds with no error.

- [ ] **Step 3: Verify our own catalog still passes**

Run: `./bin/ardvark verify website/.well-known/ai-catalog.json`
Expected: `verdict: valid`, exit 0, and every `entry.media_type` line shows `✓ … recognized as <kind> (format: <fmt>)`.

- [ ] **Step 4: Confirm no media-type warnings on the skill/generic families**

Run: `./bin/ardvark verify website/.well-known/ai-catalog.json --json`
Expected: no check object with `"CheckID":"entry.media_type"` and `"Passed":false`.

- [ ] **Step 5: Commit (if `bin/` is gitignored, nothing to commit — record completion)**

```bash
git status --short
# bin/ is ignored per .gitignore; no commit needed. If any tracked file changed, commit it:
# git commit -am "test: regression guard for media-type parsing"
```

---

## Self-Review

**Spec coverage:**
- Package `internal/mediatype` with `MediaType`/`Parse`/`FullType`/`Kind`/`Format`/`IsPointer`/`IsKnown`/`Profile` → Tasks 1–3. ✓
- Parse algorithm (params, first `/`, last `+`, lowercase, total function) → Task 1. ✓
- Kind + Format classification tables (incl. profile→Skill special case) → Tasks 2–3. ✓
- verify integration; delete allowlist; Generic suppresses warning; Unknown warns → Task 4. ✓
- crawler pointer-follow by Kind; remove duplicate consts; unsuffixed-pointer gap → Task 5. ✓
- No DB migration → honored (no `internal/store` edits anywhere). ✓
- Test matrix: spec forms, wild forms, conformance forms, params, case/whitespace, junk, unsuffixed pointers → Tasks 1–5. ✓
- Regression guard (conformance fixtures + own catalog) → Task 6. ✓
- Decisions: strict allowlist (Task 2 map + octet-stream/garbage→Unknown tests), Generic no-warn (Task 4 tests) → ✓.

**Placeholder scan:** No TBD/TODO; every code step shows complete code and exact commands. ✓

**Type consistency:** `Parse`, `MediaType{Raw,Type,Base,Suffix,Params}`, `FullType()`, `Kind()`/`Kind` enum, `Format()`/`Format` enum, `IsPointer()`, `IsKnown()`, `Profile()` are named identically across Tasks 1–5. `checkMediaType(e Entry, mt mediatype.MediaType, subject string)` matches its one call site in Task 4 Step 5. `kind == mediatype.KindCatalog/KindRegistry` matches the enum from Task 2. ✓
