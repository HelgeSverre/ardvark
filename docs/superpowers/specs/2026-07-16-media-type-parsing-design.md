# Media-type parsing & classification design

**Date:** 2026-07-16
**Status:** Approved (brainstorm), pending implementation plan
**Branch:** feat/distributed-crawling

## Problem

Ardvark recognizes ARD entry media types (`entry.type`) by **exact string match**
against a hand-maintained allowlist (`internal/ard/verify.go`
`knownEntryMediaTypes`), and follows nested-catalog / registry **pointers** by
exact match against two suffixed constants (`crawler/handlers.go`,
`crawler/engine.go`). This is wrong in two spec-backed ways and is demonstrably
lossy against real catalogs.

### The spec treats base + suffix as equivalent

`entry.type` is a **free-form string** in every normative artifact — CDDL
(`type: tstr`), JSON Schema (`"type": "string"`, no `enum`), OpenAPI. There is
no normative enumeration; recognition is advisory. The spec normatively pairs
the unsuffixed base and the `+json` form as alternatives for the same thing:

- CDDL: `type: "application/ai-registry" / "application/ai-registry+json"`
- OpenAPI: `enum: [application/ai-registry, application/ai-registry+json]`
- Prose uses both `application/ai-skill` and `application/ai-skill+md`.

The IANA note in `spec/ard.md` instructs intermediaries to "omit strict
verification of these types … the format may change." A crawler is an
intermediary. The structured-syntax suffix (`+json`, `+md`, `+gzip`, RFC 6839)
is an **encoding hint**, not part of the resource's identity — exactly like an
HTTP content type. The base says *what it is*; the suffix says *how it's
encoded*.

### Two concrete defects

1. **Crawl-coverage gap (functional bug, not cosmetic).** Pointer-following in
   `crawler/handlers.go` matches `en.Type == "application/ai-catalog+json"` and
   `en.Type == "application/ai-registry+json"` exactly. The spec explicitly
   allows the unsuffixed `application/ai-registry`. A catalog publishing an
   unsuffixed (or otherwise-suffixed) pointer is **silently not followed** — we
   miss sub-catalogs and registries during discovery.
2. **Brittle recognition.** Exact match is case-sensitive and breaks on
   media-type parameters (`; profile="…"`, `; charset=…`), which RFC 2045
   permits and the ARD skill form `text/markdown; profile="urn:air:agent-skills"`
   uses.

### Real-world vocabulary (live crawl, 2026-07-16, seeds/adopters.txt)

Distinct `entry.type` values observed in the wild — far richer than the spec
prose or our allowlist:

| media type | count | family | note |
|---|---|---|---|
| `application/agent-skills+md` | 9 | skill | base `agent-skills` (neon.tech) — not in our list |
| `application/ai-skill-archive+gzip` | 8 | skill | base `ai-skill-archive`, real gzip hint (clickhouse) |
| `application/mcp-server-card+json` | 5 | mcp | spec form |
| `text/markdown` | 6 | generic | non-`application`, bare (zapier) |
| `application/json` | 2 | generic | zapier |
| `application/ai-skill+md` | 1 | skill | spec form (clickhouse) |
| `application/linkset+json` | 1 | generic | RFC 9264 linkset (clickhouse) |
| `application/mcp-server+json` | 1 | mcp | `-card`-less wild form (unlimit) |
| `application/ai-registry+json` | 1 | registry | pointer (huggingface) |
| `application/vnd.oai.openapi` | 1 | generic | vendor tree, no suffix (zapier) |
| `text/plain` | 1 | generic | likely mislabel (zapier) |
| `text/html` | 1 | generic | likely mislabel (zapier) |

Takeaways that shape the design:
- The **skill family is fragmented across bases** (`ai-skill`, `agent-skills`,
  `ai-skill-archive`) — classification needs a base→family map, not
  suffix-splitting alone.
- Suffixes genuinely encode **compression** (`+gzip`) — the hint is load-bearing.
- Many legitimate **non-ARD artifact types** (`openapi`, `linkset`, `json`,
  `markdown`) should be *recognized as generic*, not flagged as suspicious.
- No `profile=` parameter appears in the wild **yet**; the ARD/conformance skill
  form defines it, so we support it forward-lookingly.

## Goals

- Parse media types robustly (case, whitespace, parameters, base/suffix split).
- Classify the **semantic kind** ("what it is") independent of encoding.
- Derive a **format hint** ("how it's encoded") from suffix / base tail / top type.
- Follow pointers by **kind**, closing the unsuffixed-pointer coverage gap.
- Stop warning on recognized non-ARD artifact types.

## Non-goals (YAGNI, deferred)

- **No DB schema migration.** The `media_type` column stays the raw string. The
  format hint is computed and surfaced in `verify` output/logs but not
  persisted. Persisting Kind/Format is the clean follow-up.
- **No `--strict` mode.** Generic types are accepted silently for now; a future
  `--strict` may warn on non-ARD-native types.
- **No heuristic classification.** Anything not explicitly known is
  `KindUnknown` (see Decisions).

## Design

### New package `internal/mediatype`

Standalone so `ard`, `crawler`, and (future) `httpx`/`store` share one parser
instead of duplicating pointer-type constants (today `crawler/engine.go:70-71`
duplicates `verify.go`).

One file, `internal/mediatype/mediatype.go`:

```go
package mediatype

// Kind is the semantic category of an entry's media type — "what it is",
// independent of serialization.
type Kind int

const (
	KindUnknown Kind = iota
	KindCatalog   // application/ai-catalog[...]   — pointer
	KindRegistry  // application/ai-registry[...]  — pointer
	KindSkill     // ai-skill, agent-skills, ai-skill-archive
	KindMCPServer // mcp-server-card, mcp-server
	KindA2AAgent  // a2a-agent-card, a2a-agent
	KindGeneric   // recognized non-ARD artifact (openapi, linkset, json, markdown, …)
)

// Format is the concrete serialization/encoding hint — "how it's encoded" —
// derived from the structured-syntax suffix, the base tail, or the top type.
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

// MediaType is a parsed, normalized media type.
type MediaType struct {
	Raw    string            // untouched original input
	Type   string            // lowercased top-level type: "application", "text"
	Base   string            // lowercased subtype minus suffix: "ai-skill-archive"
	Suffix string            // lowercased suffix minus '+': "json","md","gzip",""
	Params map[string]string // lowercased parameter names; values unquoted, as-is
}

func Parse(s string) MediaType

func (m MediaType) FullType() string // Type + "/" + Base, e.g. "application/ai-skill-archive"
func (m MediaType) Kind() Kind
func (m MediaType) Format() Format
func (m MediaType) IsPointer() bool // Kind == KindCatalog || KindRegistry
func (m MediaType) IsKnown() bool   // Kind != KindUnknown (KindGeneric counts as known)
func (m MediaType) Profile() string // Params["profile"]
```

### Parse algorithm (total function — garbage in → `KindUnknown`, never panics)

1. Trim outer whitespace; keep `Raw` = original.
2. Split on `;`: head = type/subtype, tail = parameters. For each `name=value`:
   lowercase `name`, strip surrounding quotes from `value`, store in `Params`.
3. Split head on the **first** `/` → `Type` / subtype. Lowercase `Type`.
   If no `/`, `Type=""`, subtype = head.
4. Split subtype on the **last** `+` → `Base` / `Suffix`. Lowercase + trim both.
   If no `+`, `Suffix=""`.

### Classification tables

`Kind` — exact match on `FullType()`, plus one special case: any type whose
`profile` param is `urn:air:agent-skills` → `KindSkill` (covers
`text/markdown; profile="urn:air:agent-skills"`).

| Kind | FullType() values |
|---|---|
| Skill | `application/ai-skill`, `application/agent-skills`, `application/ai-skill-archive` |
| MCPServer | `application/mcp-server-card`, `application/mcp-server` |
| A2AAgent | `application/a2a-agent-card`, `application/a2a-agent` |
| Catalog *(pointer)* | `application/ai-catalog` |
| Registry *(pointer)* | `application/ai-registry` |
| Generic | `application/json`, `application/vnd.oai.openapi`, `application/linkset`, `text/markdown`, `text/plain`, `text/html` |
| Unknown | everything else |

`Format` — first match wins:

| condition | Format |
|---|---|
| suffix `json` | JSON |
| suffix `md` | Markdown |
| suffix `zip` | Zip |
| suffix `gzip` | Gzip |
| `Type=="text"` && `Base=="markdown"` | Markdown |
| `Type=="text"` && `Base=="html"` | HTML |
| `Type=="text"` && `Base=="plain"` | Text |
| else | Unknown |

### Integration (no persistence changes)

- **`internal/ard/verify.go`**
  - `isPointerMediaType(t)` → `mediatype.Parse(t).IsPointer()`.
  - `checkMediaType` → passes when `mediatype.Parse(e.Type).IsKnown()`; message
    reports the classified Kind and Format hint. Warning severity unchanged.
  - Delete `knownEntryMediaTypes` and the wild-form `mediaType*` constants.
  - Effect: clickhouse (`ai-skill-archive+gzip`), neon (`agent-skills+md`), and
    zapier's generic types no longer emit spurious `entry.media_type` warnings.
- **`internal/crawler/handlers.go` + `engine.go`**
  - `enqueueEntryFollowups` switches from `en.Type == mediaTypeAICatalog` /
    `mediaTypeAIRegistry` to `mt.Kind() == KindCatalog` / `KindRegistry` (parse
    once per entry). Closes the unsuffixed-pointer coverage gap.
  - Remove the duplicate `mediaTypeAICatalog` / `mediaTypeAIRegistry` consts in
    `engine.go:70-71`.
- **Format hint surfacing:** included in `verify` per-entry check messages and
  debug logs only. DB `media_type` column unchanged.

## Testing (TDD, red-first)

### `internal/mediatype/mediatype_test.go` — table-driven

One case table asserting `(Type, Base, Suffix, Params, Kind, Format,
IsPointer, IsKnown)` for:

- Every spec form: `application/ai-catalog+json`, `application/ai-registry+json`,
  `application/mcp-server-card+json`, `application/a2a-agent-card+json`,
  `application/ai-skill`, `application/ai-skill+md`, and unsuffixed
  `application/ai-catalog`, `application/ai-registry`.
- Every wild form from the crawl (table above), including `agent-skills+md`,
  `ai-skill-archive+gzip`, `mcp-server+json`, `linkset+json`,
  `vnd.oai.openapi`, `text/markdown`, `text/plain`, `text/html`, `json`.
- Conformance-tool forms: `application/agent-skills+zip`,
  `application/agent-skills+gzip`, `text/markdown; profile="urn:air:agent-skills"`.
- Parameter forms: `application/ai-catalog+json; charset=utf-8`, quoted values.
- Case / whitespace: `APPLICATION/AI-Catalog+JSON`, `  application/ai-skill  `.
- Junk: `""`, `"garbage"`, `"application/"`, `"/json"`, `"application/+json"`.

### Integration tests (red-first)

- `internal/crawler`: a catalog whose entry is an **unsuffixed
  `application/ai-registry`** pointer must enqueue a registry harvest (currently
  fails). Same for an unsuffixed `application/ai-catalog` nested pointer.
- `internal/ard`: `verify` of a catalog using `application/agent-skills+md` and
  `application/vnd.oai.openapi` yields **no** `entry.media_type` warning.

### Regression guard

Re-run the four official conformance fixtures + our own
`website/.well-known/ai-catalog.json` through `ardvark verify`; verdicts must
not regress (all remain valid / valid_with_warnings).

## Decisions

1. **Unknown-base handling: strict allowlist.** Anything not explicitly in the
   Kind tables is `KindUnknown` (soft warning in `verify`). No heuristics —
   adding a base is a one-line table edit; heuristics risk false positives.
2. **`KindGeneric` suppresses the verify warning.** Only `KindUnknown` warns.
   Generic (openapi/linkset/markdown/json) is a legitimate artifact pointer. A
   future `--strict` may revisit; YAGNI now.

## Rollout / follow-ups (out of scope here)

- DB migration to persist Kind + Format on entries; surface via `export`/MCP.
- Upstream issue to `ards-project/ard-spec`: the conformance tool's hardcoded
  skill media-type list contradicts the spec's own `application/ai-skill[+md]`
  prose; and the wild `agent-skills` / `ai-skill-archive` bases suggest the spec
  should document the skill-type family explicitly.
