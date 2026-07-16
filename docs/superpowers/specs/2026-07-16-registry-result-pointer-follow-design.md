# Follow catalog/registry pointers in registry search results

**Date:** 2026-07-16
**Status:** Approved (brainstorm), pending implementation plan
**Branch:** feat/distributed-crawling
**Builds on:** `2026-07-16-media-type-parsing-design.md`

## Problem

`handleRegistryHarvest` (`internal/crawler/handlers.go`) queries a discovered
ARD registry's `/search`, persists the returned results via `SaveEntries`, and
enqueues further `registry_harvest` for the registry's `referrals`. But the
**results themselves are never followed**: a result that is an `ai-catalog` or
`ai-registry` *pointer* is saved as a row and then dead-ends. This is the same
class of coverage gap the media-type feature closed on the catalog path
(`enqueueEntryFollowups`), just on the registry path — a pointer discovered
*through* a registry is not crawled.

## Goal

Follow catalog and registry **pointers** found in registry search results,
reusing the same Kind-based classification, so discovery recurses through
registries as it does through catalogs.

## Non-goals (decision A)

- **Pointers only.** Do not artifact-fetch ordinary results (skills, generic,
  cards). The registry already returns the resource metadata; artifact-fetching
  every paged hit is a large, low-value fan-out.
- No DB migration; no change to what `SaveEntries` persists.

## Design

### Extract two shared pointer primitives

Pull the pointer case-bodies out of `enqueueEntryFollowups` into helpers used by
both the catalog path and the new registry-results path. This is a
behavior-preserving extraction — `enqueueEntryFollowups` keeps its exact current
semantics.

```go
// followCatalogPointer follows an ai-catalog pointer entry: a url-referenced
// nested catalog becomes a catalog_fetch at depth; an embedded-data catalog is
// processed recursively at depth.
func (e *Engine) followCatalogPointer(ctx context.Context, en ard.Entry, host, sourceURL string, depth int, parentCatID *uint)

// followRegistryPointer persists a registries row and enqueues a
// registry_harvest for it at depth. The caller supplies the row (with its
// provenance columns) and the frontier-item provenance; this sets
// prov.RegistryRowID to the new row's ID.
func (e *Engine) followRegistryPointer(url, host string, depth int, regRow *store.Registry, prov provenance)
```

`enqueueEntryFollowups` rewritten to call them (unchanged behavior):
- `KindCatalog` → `followCatalogPointer(ctx, en, host, sourceURL, depth+1, &cat.ID)`
- `KindRegistry` (gated by `cfg.Registry.Harvest`) → `followRegistryPointer(en.URL, host, 0, regRow, prov)` where `regRow = {EntryID: entryID, BaseURL: en.URL, HarvestStatus: Pending}` and `prov = {RegistryEntryID: &entryID, RegistryCatalogID: &cat.ID}`
- artifact-fetch case unchanged.

### New follow step in `handleRegistryHarvest`

After `SaveEntries(rows)` (gorm populates `rows[i].ID`), iterate results and
follow **pointers only**:

```go
for i, res := range result.Results {
    switch mediatype.Parse(res.Type).Kind() {
    case mediatype.KindCatalog:
        e.followCatalogPointer(ctx, res.Entry, item.Host, item.URL, 0, &catalogID)
    case mediatype.KindRegistry:
        if res.URL == "" || !e.cfg.Registry.Harvest {
            continue
        }
        resultEntryID := rows[i].ID // the result row that IS the registry pointer
        e.followRegistryPointer(res.URL, item.Host, item.Depth+1,
            &store.Registry{EntryID: resultEntryID, BaseURL: res.URL, HarvestStatus: store.HarvestStatusPending, ReferralSourceID: uintPtr(regRowID)},
            provenance{RegistryEntryID: &resultEntryID, RegistryCatalogID: &catalogID})
    }
}
```

(`res.Entry` is the embedded `ard.Entry`; `res.Type`/`res.URL` are its fields.)

### Depth semantics

- **Catalog pointer → depth 0** on the catalog-depth axis (`maxCatalogDepth`). A
  registry-discovered catalog is a fresh recursion root, like a seed catalog.
- **Registry pointer → `item.Depth+1`** on the referral-depth axis
  (`maxReferralDepth`), identical to how existing `result.Referrals` are
  enqueued. `handleRegistryHarvest` already guards `item.Depth > maxReferralDepth`
  on entry, so an over-budget item is dropped on dequeue.

The two axes stay independent — referral depth never bleeds into the catalog
budget or vice versa.

### Loop / duplication safety (existing guards, unchanged)

- Frontier `enqueue` dedups by URL, so the same pointer URL is not enqueued twice.
- Catalog re-processing is skipped by content-hash in `processCatalog`.
- Both `handleCatalogFetch` and `handleRegistryHarvest` gate their own depth on
  dequeue.
- Registry-row creation for a repeated URL mirrors existing `referrals`
  behavior (acceptable).

## Testing (TDD)

1. **Primitives (unit, in `internal/crawler`):**
   - `followCatalogPointer` with a url entry enqueues exactly one
     `catalog_fetch` for that url at the given depth with `ParentCatalogID` set.
   - `followCatalogPointer` with embedded data (no url) recurses (persists a
     nested catalog) rather than enqueuing.
   - `followRegistryPointer` saves one registries row and enqueues one
     `registry_harvest` at the given depth.
2. **`enqueueEntryFollowups` regression:** the existing crawler tests
   (nested-catalog + registry-harvest + artifact-fetch counts, and the
   unsuffixed catalog/registry pointer tests) stay green — extraction changed no
   behavior.
3. **Integration — registry results are followed:** a mock registry whose
   `/search` returns three results:
   - `application/ai-catalog` (unsuffixed) with a url → exactly 1 `catalog_fetch`
     enqueued for that url.
   - `application/ai-registry+json` with a url → exactly 1 `registry_harvest`
     enqueued + 1 new registries row (with `ReferralSourceID` set).
   - `application/mcp-server-card+json` with a url → neither a `catalog_fetch`
     nor a `registry_harvest` nor an `artifact_fetch` (pointers-only).

## Follow-ups (unchanged, out of scope)

- DB migration to persist Kind/Format; surface via export/stats/MCP.
