# ardvark — ARD Catalog Crawler & Indexer: Design

**Date:** 2026-07-15
**Status:** Approved design, pending implementation plan

## Purpose

`ardvark` is a storage-agnostic web crawler written in Go that discovers
[Agentic Resource Discovery (ARD)](https://agenticresourcediscovery.org/spec/)
`ai-catalog.json` documents, verifies them against the ARD specification, and
writes a machine-readable record of every discovered agentic resource to a
JSONL log and a database (SQLite by default; MySQL and Postgres supported).
The resulting dataset feeds a future directory site in the vein of skills.sh
or mcpmarket.

The name: ARD + aardvark — it forages the web digging up catalogs.

## Scope

**In scope:**
- Domain harvesting: crawl HTML pages from seeds (single URL, URL-list file,
  or bare domains), follow anchors up to a configurable depth/page budget to
  collect unique hosts. Page content is not persisted; pages exist only to
  discover hosts and `<link rel="ai-catalog">` hints.
- Probing each host for ARD documents via all three web discovery mechanisms:
  `/.well-known/ai-catalog.json`, `Agentmap:` directives in `robots.txt`, and
  `<link rel="ai-catalog">` tags on harvested pages. (DNS Service Binding
  discovery is out of scope for v1.)
- Full catalog resolution: recurse into nested catalogs (`data`-embedded and
  `url`-referenced `application/ai-catalog+json` entries), fetch every
  entry's referenced artifact document (agent cards, MCP server cards, …).
- Registry harvesting: for entries of type `application/ai-registry+json`,
  query the registry's `POST /search` endpoint, paginate, ingest returned
  entries with provenance, and follow registry referrals (bounded, deduped).
- Verification: JSON Schema validation (Draft 2020-12, official spec schema)
  plus the spec's semantic rules, recorded per-check with messages.
- Resumable CLI runs backed by a persistent frontier in the database.
- Seeding from Certificate Transparency logs: fetch the latest N entries
  from an RFC 6962 log (default: Let's Encrypt Oak), extract SAN domains
  from the leaf certificates, dedupe/sanitize, and enqueue host probes.

**Out of scope (for now):**
- Cryptographic trust verification (JWS signatures, attestation digests,
  DID/SPIFFE resolution). `trustManifest` data is stored verbatim.
- DNS-based discovery.
- The directory website itself.
- A long-running daemon / HTTP API (the frontier design allows evolving into
  one later).

## Runtime model

A CLI (`ardvark`) with subcommands:

| Command   | Purpose |
|-----------|---------|
| `crawl`   | Seed the frontier (URL, `--list file`, bare domains) and drain it with a worker pool. Resumes pending work from prior runs. |
| `probe`   | Probe specific host(s) for ARD documents without HTML spidering. |
| `seed ct` | Pull the most recent N entries from Certificate Transparency logs (default: Let's Encrypt Oak), extract SAN domains, and enqueue them as `host_probe` frontier items. |
| `verify`  | Re-run verification against stored catalogs (e.g. after a spec/schema update), or verify a local/remote catalog ad hoc. |
| `export`  | Dump resources from the DB as JSONL/CSV for downstream use. |
| `stats`   | Summarize the dataset (hosts probed, catalogs found/valid, entries by type…). |
| `migrate` | Apply database schema migrations. |

Scheduling is external (cron). Re-runs skip hosts probed within a freshness
window (`refreshAfterHours`) unless `--force`.

## Architecture

Everything revolves around a **DB-backed frontier** (work queue). Each unit
of work is a typed frontier item; a bounded worker pool drains it. Because
the frontier is persistent, a run can be killed and resumed at any point.

### Work item types

1. `page_fetch` — fetch an HTML page (depth-bounded), extract anchor hosts
   and `<link rel="ai-catalog">` hints → enqueue `host_probe` for new hosts,
   `page_fetch` for in-budget links.
2. `host_probe` — GET `/.well-known/ai-catalog.json`; scan `robots.txt` for
   `Agentmap:` directives; record the probe outcome → on hit, enqueue
   `catalog_fetch`.
3. `catalog_fetch` — fetch + verify an `ai-catalog.json` (well-known or
   nested-by-url) → store catalog + entries + verification checks → enqueue
   `artifact_fetch` per entry `url`, `catalog_fetch` per nested catalog,
   `registry_harvest` per registry entry.
4. `artifact_fetch` — fetch a referenced artifact document, store raw + hash.
5. `registry_harvest` — `POST /search` against a discovered registry,
   paginate, store returned entries with registry provenance; referrals
   enqueue further `registry_harvest` items.

**Loop-safety:** nested catalogs and registry referrals are the two recursion
sources; both are bounded by max-depth config (`ard.maxCatalogDepth`,
`registry.maxReferralDepth`) and a dedup key (canonical URL/URN) enforced
with a unique constraint in the DB.

### Package layout

```
cmd/ardvark/          cobra CLI entry point and subcommands
internal/config/      JSON config load + jsonschema validation, defaults
internal/frontier/    persistent queue: enqueue/dequeue/complete, dedup, retries
internal/fetch/       HTTP client: per-host rate limiting, robots.txt gate,
                      timeouts, max body size, redirect cap, custom UA
internal/harvest/     HTML parsing: anchors → hosts, link-rel hints
internal/probe/       well-known + robots Agentmap probing
internal/ard/         spec types, JSON Schema + semantic verification
internal/registry/    ARD registry /search client, pagination, referrals
internal/ctseed/      Certificate Transparency log client: get-sth,
                      get-entries, SAN extraction, domain sanitization
internal/store/       storage interface + GORM implementation
internal/eventlog/    slog JSONL crawl-event log
```

Each package communicates through interfaces so units are independently
testable (e.g. `frontier` and `store` accept interfaces; `ard` verification
is pure functions over parsed documents).

### Data flow

seeds → frontier → worker pool (configurable concurrency, per-host rate
limiting) → results written to store + JSONL event log. Dedup at enqueue
time: hosts probed within the freshness window and URLs already fetched
(matched by content hash) are skipped unless `--force`.

## Package / dependency choices

| Concern | Choice | Rationale |
|---------|--------|-----------|
| CLI | `spf13/cobra` | Standard, subcommand-friendly. |
| HTTP fetching | stdlib `net/http` | Full control over politeness, limits, redirects. |
| HTML parsing | `golang.org/x/net/html` | Sufficient for anchor/link extraction; goquery unnecessary. |
| robots.txt | `temoto/robotstxt` + custom line scan for `Agentmap:` | The directive is non-standard, so we scan raw robots.txt ourselves for it. |
| Rate limiting | `golang.org/x/time/rate` | Per-host token buckets. |
| ORM / storage | `gorm.io/gorm` with `glebarez/sqlite` (pure-Go, no CGO), `gorm.io/driver/mysql`, `gorm.io/driver/postgres` | One model layer, dialect swapped via config; the "easily swappable" requirement. |
| JSON Schema | `santhosh-tekuri/jsonschema/v6` | Best Draft 2020-12 support in Go; structured errors with instance/keyword locations. Used for both ARD validation and config validation. |
| Logging | stdlib `log/slog` | JSON handler → JSONL file; text handler → stderr. No dependency. |
| CT log client | `google/certificate-transparency-go` | RFC 6962 client + MerkleTreeLeaf/X.509 parsing for `seed ct`; hand-rolling TLS-encoded leaf parsing is not worth it. |

The official `ai-catalog.schema.json` is vendored into the repo, pinned to a
spec version, with provenance noted (source URL + commit).

## Data model

Nine tables. Raw documents are always stored verbatim (JSON/text column)
alongside extracted columns so the directory can re-process without
re-crawling.

### Crawl bookkeeping

- **crawl_runs** — id, started_at, finished_at, config_snapshot (JSON),
  counters (pages fetched, hosts probed, catalogs found/valid, errors).
- **frontier_items** — id, run_id, kind, url/host, depth, priority, status
  (`pending | in_flight | done | failed`), attempts, last_error, dedup_key
  (unique), created_at, updated_at.

### Discovery

- **domains** — id, host (unique), first_seen_at, last_probed_at,
  discovery_source (`seed | anchor | url_list | catalog_ref |
  registry_referral | ct_log`), ard_status (`unprobed | not_found | found_invalid |
  found_valid`).
- **probes** — id, domain_id, method (`well_known | robots_agentmap |
  link_tag`), url, http_status, content_type, outcome (`hit | miss | error`),
  error_detail, probed_at. One row per attempt → probe history over time.

### ARD documents

- **catalogs** — id, domain_id, source_url, parent_catalog_id (nested),
  spec_version, host_display_name, host_identifier, raw_json, content_hash
  (sha256), fetched_at, verification_status (`valid | valid_with_warnings |
  invalid`).
- **catalog_entries** — id, catalog_id, urn, urn_publisher, urn_namespace,
  urn_name (parsed segments for filtering), display_name, media_type,
  ref_url, has_embedded_data, description, version, entry_updated_at,
  tags (JSON), capabilities (JSON), representative_queries (JSON),
  trust_manifest (JSON, verbatim), raw_json, source (`catalog | registry`),
  source_registry_id (nullable).
- **artifacts** — id, entry_id, url, http_status, content_type, raw_body,
  content_hash, fetched_at, fetch_status.
- **registries** — id, entry_id (declaring catalog entry), base_url,
  last_harvested_at, harvest_status, referral_source_id (nullable).

### Verification

- **verification_checks** — id, subject_type (`catalog | entry`), subject_id,
  check_id, severity (`error | warning`), passed, message, spec_ref,
  checked_at. One row per check per pass → per-catalog "report card".

### Design notes

- `content_hash` gives cheap change detection: re-crawls only write new rows
  when the hash changes.
- URN segments are split into columns because the directory will filter by
  publisher. Tags/queries remain JSON columns; join tables are a
  directory-phase optimization.
- Registry-harvested entries reuse `catalog_entries` with
  `source = registry`, so the directory reads one table for all resources.
- GORM AutoMigrate for SQLite/dev; numbered migrations before MySQL/Postgres
  are used in production.

## Verification pipeline

Per catalog, in order, all results recorded to `verification_checks`:

1. **Transport checks** (warnings): JSON content type, size within cap,
   valid UTF-8.
2. **JSON Schema** — official `ai-catalog.schema.json`; each schema failure
   becomes a check row with instance location + message.
3. **Semantic checks** — independent Go functions for rules the schema
   cannot express:

| check_id | Severity | Rule |
|----------|----------|------|
| `catalog.spec_version` | error | `specVersion` must be `"1.0"` |
| `entry.value_or_reference` | error | exactly one of `url` / `data` |
| `urn.format` | error | `urn:air:<publisher>:<namespace…>:<name>` grammar, RFC 8141 |
| `identifier.unique` | error | no duplicate URNs within a catalog |
| `urn.publisher_matches` | warning | URN publisher domain ≠ serving domain (legit for aggregators, flagged) |
| `queries.count` | warning | 2–5 `representativeQueries` recommended |
| `entry.media_type` | warning | unrecognized ARD media type (spec says do not enforce strictly) |

4. **Verdict roll-up:** any failed error-severity check → `invalid`; only
   warnings → `valid_with_warnings`; else `valid`. Invalid catalogs are
   stored in full — the directory may show "found but broken".

## Configuration

Single JSON file (`ardvark.json`), validated against a schema shipped with
the binary. Validation failures produce precise, human-friendly messages
(e.g. `config.storage.driver: must be one of "sqlite", "mysql",
"postgres"`). All keys optional with sane defaults; CLI flags override.

```json
{
  "storage": { "driver": "sqlite", "dsn": "ardvark.db" },
  "log":     { "file": "ardvark.jsonl", "level": "info" },
  "crawler": {
    "concurrency": 8,
    "maxDepth": 2,
    "maxPagesPerDomain": 50,
    "perHostRequestsPerSecond": 1,
    "requestTimeoutSeconds": 15,
    "maxBodyBytes": 5242880,
    "userAgent": "ardvark/0.1 (+repo URL)",
    "respectRobotsTxt": true,
    "refreshAfterHours": 168
  },
  "ard":     { "maxCatalogDepth": 3, "fetchArtifacts": true },
  "registry":{ "harvest": true, "maxReferralDepth": 2, "pageLimit": 20 },
  "ctSeed":  {
    "logUrl": "https://oak.ct.letsencrypt.org/2026h2/",
    "entryCount": 1000
  }
}
```

## CT log seeding (`ardvark seed ct`)

Bootstraps the frontier when you have no seed list:

1. `GET <logUrl>ct/v1/get-sth` → current tree size.
2. `GET <logUrl>ct/v1/get-entries?start=<size-N>&end=<size-1>` in chunks
   (logs cap entries per response; paginate until N collected).
3. Parse each leaf (X.509 or precert) via `certificate-transparency-go`,
   collect SAN DNS names.
4. Sanitize: strip a leading `*.` (probe the apex instead), lowercase,
   drop IP addresses and non-public suffixes, dedupe against `domains`.
5. Insert as `domains` rows with `discovery_source = ct_log` and enqueue
   `host_probe` frontier items.

The log URL and entry count are configurable (`ctSeed`), with
`--count` / `--log` flag overrides. Multiple invocations are naturally
idempotent thanks to frontier dedup keys.

## Error handling & politeness

- Transient failures (timeouts, 5xx, 429) retry with exponential backoff up
  to an attempts cap, then the frontier item is marked `failed` with
  `last_error`. A failing item never crashes the run.
- Permanent failures (4xx other than 429, DNS errors) fail immediately.
- robots.txt is respected for page crawling. Well-known probes are always
  attempted (that is the purpose of well-known paths) but still rate-limited.
- Body size cap via `io.LimitReader`; redirect cap of 5; `http(s)` schemes
  only; per-host token-bucket rate limiting.

## Logging

Every significant event (probe result, catalog verified, item failed,
registry harvested) is emitted once through `slog`:
- JSON handler appends to the JSONL log file — the machine-readable record
  of discovered resources.
- Text handler prints human-readable progress to stderr.

`ardvark export` additionally dumps DB contents as JSONL/CSV.

## Testing strategy

- **Unit tests** (table-driven): URN parsing, each semantic check, config
  validation, frontier dedup/retry logic.
- **Integration tests:** `httptest.Server` fixtures simulating complete fake
  websites — valid catalog, invalid catalog, nested catalogs, redirect
  loops, robots-blocked pages, oversized bodies, and a fake registry with
  pagination and referrals — driving end-to-end crawls against in-memory
  SQLite.
- **Golden corpus:** the spec's example documents (enterprise + solo
  developer catalogs) as fixtures; grown with real-world catalogs as
  adoption appears.
- **Dialect tests:** MySQL/Postgres integration tests behind a build tag so
  plain `go test ./...` stays dependency-free.

## Decisions log

| Decision | Choice |
|----------|--------|
| Crawl depth into catalogs | Everything: nested catalogs, artifacts, registry harvesting incl. referrals |
| HTML crawling role | Domain harvester only; page content not persisted |
| Storage | GORM; SQLite default, MySQL/Postgres via config; JSONL event log |
| Runtime | Resumable CLI runs, persistent frontier, cron for scheduling |
| Verification depth | JSON Schema + semantic rules; trust crypto out of scope |
| Config | JSON file, schema-validated with good error messages |
| Name | ardvark |
| Bootstrap seeding | `seed ct` command pulling latest N entries from Let's Encrypt Oak CT log (added 2026-07-15) |
