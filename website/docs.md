---
title: "Documentation — ardvark"
description: "ardvark documentation: install, quickstart, discovery mechanism, verification checks, seeding strategies, publishing a catalog, programmatic use (JSON output, MCP server), querying the dataset, command reference, configuration, storage schema, and operational notes."
canonical_url: "https://ardvark.no/docs.html"
---

<!-- Generated from docs.html by npm run docs:generate. Do not edit directly. -->

# ardvark documentation

A crawler and indexer for the agentic web, in one Go binary.

ardvark crawls the web for [ARD](https://agenticresourcediscovery.org) (Agentic Resource Discovery) `ai-catalog.json` documents, verifies each one against the spec, and indexes every discovered agent, MCP server, and skill into a database. It's a single Go binary: no runtime, no external services beyond the sources it seeds from and the storage backend you point it at.

A run works in two phases. The frontier gets populated with hosts — from a URL, a domain list, or one of the seed sources below — then the crawler drains it: probing each host for a catalog, resolving what it finds (nested catalogs, referenced artifact documents, registries), and running every catalog through the verification pipeline. Everything lands in SQLite (or MySQL/Postgres) plus a JSONL event log, whether the catalog was valid, invalid, or never found at all.

## Install

```bash
# Homebrew
brew install helgesverre/tap/ardvark
```

```bash
# Go
go install github.com/helgesverre/ardvark/cmd/ardvark@latest
```

Or grab a binary from the [releases page](https://github.com/helgesverre/ardvark/releases) — macOS, Linux, and Windows are all published.

**Availability** — this page documents v0.7.0.

## Quickstart

Crawl a site and everything it links to, probing each discovered host for ARD catalogs:

```bash
ardvark crawl https://example.com
```

You'll see one row per probe as it happens, then a run summary — for `example.com`, which publishes no catalog, that's a wall of honest misses:

```console
$ ardvark crawl https://example.com
seeded 1 of 1 requested seed(s)
  miss  example.com            well_known       404
  miss  example.com            robots_agentmap  no robots.txt
  miss  iana.org               well_known       404
  ...
run complete: 25 pages fetched · 40 hosts probed · 0 catalogs found · 0 valid · 0 errors
```

Or skip crawling entirely and probe hosts directly:

```bash
ardvark probe example.com huggingface.co
```

Seed the frontier from Certificate Transparency logs — new deployments are where adopters show up first — then work through it:

```bash
ardvark seed ct --count 1000
ardvark crawl
```

Or start from `seeds/adopters.txt` in the repository, the hosts already confirmed to publish ARD catalogs:

```bash
ardvark crawl --list seeds/adopters.txt
```

Results land in `ardvark.db` (SQLite by default) and `ardvark.jsonl`. See what you caught:

```bash
ardvark stats
ardvark export --format jsonl --out resources.jsonl
```

## How discovery works

ardvark checks a host for an ARD catalog through four mechanisms. Each is attempted and recorded independently — a host publishing through more than one gets a result row for each, and every discovered catalog is fetched:

1.  The well-known path: `/.well-known/ard.json` first, falling back to `/.well-known/ai-catalog.json`. The two are aliases for the same resource, so the first one that answers wins and the second is not requested.
2.  `Agentmap:` directive in `robots.txt` — a pointer to a catalog served from somewhere else.
3.  `<link rel="ard">` (or the legacy `rel="ai-catalog"`) tag on a crawled page — found while spidering during `ardvark crawl` (not tried by `ardvark probe`, which does no HTML spidering).
4.  An ARD catalog embedded inline in a page as `<script type="application/ld+json">` — also spidering-only. Unrelated JSON-LD (Schema.org `Organization` markup and the like) is ignored.

DNS Service Binding discovery is not implemented.

Every probe is recorded regardless of outcome, one row per attempt:

-   **hit** — a document was found at that location. It still goes through verification; a hit can be a valid catalog, one with warnings, or an invalid one — invalid catalogs are stored too, since "found but broken" is useful data.
-   **miss** — the location returned 404 (or equivalent); nothing published there.
-   **err** — the request itself failed (timeout, connection error, malformed response) before a hit/miss could be determined.

A hit that resolves to a valid catalog is then recursed into: nested catalogs are followed up to `ard.maxCatalogDepth`, entries pointing at artifact documents (agent cards, MCP server cards) are fetched, and entries pointing at registries are harvested via `POST /search`, including registry referrals, up to `registry.maxReferralDepth`.

## Verification & verdicts

Every fetched catalog is run through the same pipeline `ardvark verify` exposes standalone: official JSON Schema validation (Draft 2020-12) against the vendored ARD spec schema, followed by seven semantic checks the schema can't express. Each check is recorded pass/fail with a message — a report card per catalog, stored in `verification_checks`.

By default verification is deliberately lenient toward catalogs seen in the wild: the legacy `urn:ai:` NID, uppercase URN prefixes, Unicode-form IDN publishers, and an explicit `data: null` are accepted, while malformed `uri`/`date-time` values and an out-of-range `representativeQueries` count downgrade to warnings. Every deviation is documented in the vendored schema's `PROVENANCE.md`. Pass `--strict` to validate against the exact published spec schema instead, with format assertions enforced as errors.

The rolled-up verdict is one of:

-   **valid** — every check passed.
-   **valid\_with\_warnings** — all error-severity checks passed; one or more warning-severity checks failed.
-   **invalid** — at least one error-severity check failed (including schema validation itself).

Identifiers use the URN grammar `urn:<nid>:<publisher>:<namespace...>:<name>`. `air` is the namespace identifier (NID) in the ARD spec draft; `ai` also appears on catalogs already published in the wild, so both `urn:air:` and `urn:ai:` are accepted.

| Check ID | Severity | What it means |
| --- | --- | --- |
| `schema.validation` | error | The document doesn't conform to the official ai-catalog.json JSON Schema. Failures are reported per leaf, with the instance location and detail. |
| `catalog.spec_version` | error | `specVersion` must be exactly `"1.0"`. Skipped if schema validation already failed on this field. |
| `identifier.unique` | error | No two entries share the same identifier, compared on the normalized parsed URN so case and encoding differences don't hide a duplicate. |
| `entry.value_or_reference` | error | Each entry has exactly one of `url` or `data` — not both, not neither. |
| `urn.format` | error | The identifier parses as a well-formed ARD URN: `urn:air:` or `urn:ai:`, a publisher segment, zero or more namespace segments, and a mandatory name segment. |
| `urn.publisher_matches` | warning | The URN's publisher segment shares a registrable domain (eTLD+1) with the domain the catalog was served from. May legitimately fail for aggregators publishing on behalf of others. |
| `queries.count` | warning | Callable entries should list 2–5 `representativeQueries`. Not checked on container/pointer entries (nested catalogs, registries). |
| `entry.media_type` | warning | The entry's `type` is a recognized ARD media type. The spec doesn't strictly enforce this, so an unrecognized type only warns. |

Run it standalone against anything, local or remote — add `--strict` for exact spec conformance:

```bash
ardvark verify ./my-catalog.json
ardvark verify https://example.com/.well-known/ai-catalog.json
ardvark verify --strict ./my-catalog.json
```

#### What failure looks like

A failing report renders `✗` for failed error-severity checks and `!` for failed warnings. Here's a catalog whose entry identifier isn't a URN and whose media type is made up — note that semantic checks on a field the schema already rejected are skipped rather than double-reported:

```console
$ ardvark verify ./broken.json
./broken.json
  ✗ schema.validation          support-agent — at /entries/0/identifier: 'support-agent' does not match pattern '^urn:air:[a-zA-Z0-9.-]+(:[a-zA-Z0-9._-]+)+$'
  ✓ identifier.unique          all entry identifiers are unique
  ! queries.count              support-agent — 0 representativeQueries present (recommended: 2-5)
  ! entry.media_type           support-agent — type "application/x-agent" is not a recognized ARD media type (spec does not enforce this strictly)
verdict: invalid
Error: verify: ./broken.json is invalid
```

An `invalid` verdict makes `verify` exit 1 — see [exit codes](https://ardvark.no/docs.html#exit-codes) — so it slots straight into CI. Publishers: the [Publishing a catalog](https://ardvark.no/docs.html#publishing) guide walks through the failures seen most often in the wild.

## Seeding strategies

Seven sources fill the frontier, each trading off precision (how likely a domain is to actually publish a catalog) against breadth (how many domains it surfaces). Every source accepts `--json` and reads its defaults from [config](https://ardvark.no/docs.html#configuration); seeding only queues domains — run `ardvark crawl` afterwards to drain the frontier.

```console
$ ardvark seed ct --count 40
seed ct_log complete: 40 entries from oak,argon,nimbus log(s) · 40 added · 0 skipped (already queued)
```

### seed ct — Certificate Transparency logs

Harvests domains from the newest CT log entries — freshly issued certificates, so new deployments show up here first. Logs resolve live from the CT log list (the `oak`, `argon`, and `nimbus` operators by default), so shard URLs never go stale. High breadth, low precision: most new certs aren't ARD adopters. No token needed.

| Flag | Meaning |
| --- | --- |
| `--count int` | entries to harvest (default: config `seed.ct.entryCount`, 1000) |
| `--log string` | operator token (`oak`, `argon`, `nimbus`, `all`) or explicit log URL (default: config `seed.ct.logs`) |

### seed crtsh — crt.sh certificate search

Certificate identity search narrowed by keyword. With `--match` (e.g. `agent`, `mcp`) it queries identities mentioning that keyword; without it, a curated agent/mcp/ai keyword set is queried instead of an unfiltered wildcard, which crt.sh can't serve. Better precision than raw CT when keyword-matched — still keyword coincidence, not confirmation. No token needed.

| Flag | Meaning |
| --- | --- |
| `--count int` | domains to enqueue (default: config `seed.crtsh.count`, 1000) |
| `--match string` | narrow to certificate identities mentioning this keyword (default: a curated agent/mcp/ai keyword set) |

### seed tranco — Tranco top domains

Queues the top N domains from the Tranco list — the established web that CT-log seeding, which only sees freshly issued certificates, misses. High breadth, low precision: mostly domains with no reason to publish an ARD catalog. No token needed.

| Flag | Meaning |
| --- | --- |
| `--top int` | top domains to enqueue (default: config `seed.tranco.top`, 1000) |
| `--url string` | Tranco list URL (default: config `seed.tranco.listUrl`) |

### seed github — GitHub code search

Searches GitHub's code search API for well-known ARD catalog files directly and queues the owning repositories' domains. The highest-precision source of all — a hit is a real deployed catalog, not a keyword coincidence. Low breadth: bounded by GitHub's index and rate limits. Requires a `GITHUB_TOKEN` environment variable (the only environment variable ardvark reads — see [Configuration](https://ardvark.no/docs.html#configuration)).

| Flag | Meaning |
| --- | --- |
| `--count int` | domains to enqueue (default: config `seed.github.count`, 100) |
| `--query string` | GitHub code-search query (default: config `seed.github.query`, `filename:ai-catalog.json path:.well-known`) |

### seed mcp — MCP server registry

Harvests domains from the official MCP server registry: each listed server's remote endpoint host, plus a domain decoded from its reverse-DNS-style name. Highest-propensity adopters — MCP server operators are exactly the audience ARD targets. Bounded by registry size. No token needed.

| Flag | Meaning |
| --- | --- |
| `--count int` | domains to enqueue (default: config `seed.mcp.count`, 1000) |
| `--registry string` | MCP registry base URL (default: config `seed.mcp.registryUrl`) |

### seed curated — curated awesome-lists

Fetches curated list documents (community MCP server lists by default), extracts every absolute http(s) URL, and queues the unique domains — dropping hosting, badge, and social infrastructure (github.com, shields.io, glama.ai, …) so only real product domains remain. Moderate precision, small breadth: as large as the lists scanned. No token needed.

| Flag | Meaning |
| --- | --- |
| `--count int` | domains to enqueue (default: config `seed.curated.count`, 500) |
| `--url stringArray` | list document URL, repeatable; **replaces** the default set (default: config `seed.curated.urls`) |

### seed commoncrawl — Common Crawl web graph

Streams the domain-level ranks file of the latest Common Crawl web-graph release and queues the top N domains by harmonic centrality — the established web at scale (~121M ranked domains vs Tranco's 1M). Reading stops as soon as N domains are collected, so the full ~1 GB file is never downloaded. Very high breadth, low precision; use `--offset` to sample deeper slices instead of always taking the same top domains. No token needed.

| Flag | Meaning |
| --- | --- |
| `--top int` | top domains to enqueue (default: config `seed.commoncrawl.top`, 1000) |
| `--offset int` | ranked domains to skip before collecting (default: config `seed.commoncrawl.offset`, 0) |
| `--graph string` | web-graph release id (default: config `seed.commoncrawl.graph`, else the latest release) |

## Publishing a catalog

The other side of the crawler: this guide is for sites that want to _be found_. Publishing an ARD catalog takes one JSON file and one of three exposure mechanisms — and `ardvark verify` tells you before you deploy whether crawlers will accept it.

#### A minimal valid catalog

This is the smallest catalog that passes every check, warnings included:

```json
{
  "specVersion": "1.0",
  "host": { "displayName": "Example Inc" },
  "entries": [
    {
      "identifier": "urn:air:example.com:agent:support",
      "displayName": "Support Agent",
      "description": "Answers questions about Example Inc products.",
      "type": "application/a2a-agent-card+json",
      "url": "https://example.com/.well-known/agent-card.json",
      "representativeQueries": [
        "how do I reset my password",
        "cancel my Example subscription"
      ]
    }
  ]
}
```

The moving parts: `specVersion` must be exactly `"1.0"`; each entry needs an [ARD URN](https://ardvark.no/docs.html#verification) identifier, a `displayName`, a media `type`, exactly one of `url` or `data`, and ideally 2–5 `representativeQueries` (fewer only warns, but indexers use them).

#### Exposing it

Any one of the [discovery mechanisms](https://ardvark.no/docs.html#discovery) is enough; publishing through several gets you found by more crawler configurations:

1.  Serve it at `/.well-known/ard.json`, and keep `/.well-known/ai-catalog.json` alongside it as an alias — both are checked by every probe, and older crawlers only know the second name.
2.  Add an `Agentmap:` directive to `robots.txt`, pointing wherever the catalog actually lives:

    ```bash
    Agentmap: https://example.com/.well-known/ai-catalog.json
    ```

3.  Add a link tag to your pages — only found by spidering crawls, so don't make it your only mechanism:

    ```html
    <link rel="ard" href="/.well-known/ard.json">
    ```

#### Verify before you publish

Run the file through `ardvark verify` locally, and again against the live URL after deploying. It exits 1 on an `invalid` verdict, so it works as a CI gate. As a publisher, use `--strict`: ardvark's default is lenient about a few real-world quirks, but other ARD validators won't be — strict mode holds your catalog to the exact published spec schema:

```bash
ardvark verify --strict ./ai-catalog.json
ardvark verify --strict https://example.com/.well-known/ai-catalog.json
```

The failures seen most often on real published catalogs:

-   **`schema.validation` on unexpected fields** — the schema is strict where you'd least expect it: unknown keys at the catalog root or inside `host` are rejected (`additional properties 'generator' not allowed`), and `metadata` values must be scalars — a nested object fails with `at /entries/0/metadata/nested: validation failed`. Put custom extensions in `metadata` as flat string/number/boolean values.
-   **Unrecognized media types only warn** — an unknown entry `type` yields a `!` from `entry.media_type`, not an `✗`; the catalog can still roll up `valid_with_warnings`. Prefer the recognized types (e.g. `application/a2a-agent-card+json`, `application/mcp-server-card+json`, `application/ai-skill+md`).
-   **`urn.publisher_matches` mismatch** — a warning when the URN's publisher segment doesn't share a registrable domain with the serving host. Legitimate for aggregators publishing on behalf of others; for your own resources, make the publisher segment your own domain.

#### A worked example: ardvark.no

This site eats its own dog food — it publishes a catalog at [`/.well-known/ai-catalog.json`](https://ardvark.no/.well-known/ai-catalog.json) and an `Agentmap:` directive in [`robots.txt`](https://ardvark.no/robots.txt), and the catalog verifies clean:

```console
$ ardvark verify https://ardvark.no/.well-known/ai-catalog.json
https://ardvark.no/.well-known/ai-catalog.json
  ✓ identifier.unique          all entry identifiers are unique
  ✓ catalog.spec_version       specVersion is "1.0"
  ✓ entry.value_or_reference   urn:air:ardvark.no:skill:ardvark-cli — exactly one of url or data is present
  ✓ urn.format                 urn:air:ardvark.no:skill:ardvark-cli — identifier is a well-formed ARD URN
  ✓ urn.publisher_matches      urn:air:ardvark.no:skill:ardvark-cli — URN publisher "ardvark.no" matches serving domain "ardvark.no"
  ✓ queries.count              urn:air:ardvark.no:skill:ardvark-cli — 4 representativeQueries present (recommended: 2-5)
  ✓ entry.media_type           urn:air:ardvark.no:skill:ardvark-cli — type "application/ai-skill+md" is a recognized ARD media type
  ✓ entry.value_or_reference   urn:air:ardvark.no:tool:ardvark — exactly one of url or data is present
  ✓ urn.format                 urn:air:ardvark.no:tool:ardvark — identifier is a well-formed ARD URN
  ✓ urn.publisher_matches      urn:air:ardvark.no:tool:ardvark — URN publisher "ardvark.no" matches serving domain "ardvark.no"
  ✓ queries.count              urn:air:ardvark.no:tool:ardvark — 3 representativeQueries present (recommended: 2-5)
  ✓ entry.media_type           urn:air:ardvark.no:tool:ardvark — type "application/ai-skill" is a recognized ARD media type
  ✓ entry.value_or_reference   urn:air:ardvark.no:mcp:ardvark — exactly one of url or data is present
  ✓ urn.format                 urn:air:ardvark.no:mcp:ardvark — identifier is a well-formed ARD URN
  ✓ urn.publisher_matches      urn:air:ardvark.no:mcp:ardvark — URN publisher "ardvark.no" matches serving domain "ardvark.no"
  ✓ queries.count              urn:air:ardvark.no:mcp:ardvark — 3 representativeQueries present (recommended: 2-5)
  ✓ entry.media_type           urn:air:ardvark.no:mcp:ardvark — type "application/mcp-server-card+json" is a recognized ARD media type
verdict: valid
```

Three entries — a SKILLS.md skill, a docs page, and an MCP server card — each with a matching publisher URN and 2–5 representative queries. Steal the pattern.

## Programmatic use

Three machine-readable surfaces, one data model: `--json` for scripts, the MCP server for agents, and the JSONL event log for anything that tails files. All of them expose the same structures.

### JSON output (--json)

`crawl`, `work`, `probe`, every `seed` source, `verify` (including `--stored`), `search`, both `get` subcommands, `stats`, and `info` accept a `--json` flag: the command runs exactly as before but emits one pretty-printed JSON document on stdout instead of human-readable rows. `export` has no `--json` because its output is already machine-readable — pick JSONL or CSV with `--format`.

-   **stdout is the document** — live per-host rows are suppressed and diagnostics go to stderr, so piping stdout into `jq` is always safe.
-   **Exit codes are unchanged** — `verify --json` still exits 1 when the verdict is `invalid`. See [exit codes](https://ardvark.no/docs.html#exit-codes).
-   **Same shapes as the MCP tools** — each structure is exactly what the corresponding [MCP tool](https://ardvark.no/docs.html#mcp) returns.

`probe --json` returns per-host, per-mechanism results plus a hit/miss/error summary:

```console
$ ardvark probe example.com --json
{
  "hosts": [
    {
      "host": "example.com",
      "results": [
        {
          "method": "well_known",
          "url": "https://example.com/.well-known/ai-catalog.json",
          "http_status": 404,
          "outcome": "miss"
        },
        {
          "method": "robots_agentmap",
          "url": "https://example.com/robots.txt",
          "outcome": "miss",
          "error_detail": "no robots.txt"
        }
      ]
    }
  ],
  "summary": {
    "hits": 0,
    "misses": 2,
    "errors": 0
  }
}
```

The other shapes: `crawl --json` returns the final run summary once the frontier is drained (`seeds_requested`, `seeded`, `pages_fetched`, `hosts_probed`, `catalogs_found`, `catalogs_valid`, `errors`). `verify --json` returns the full report card — `source`, `verdict`, and every check's `id`, `severity`, `subject`, `passed`, and `message`. `search --json` returns an `entries` page and an optional `next_cursor`; `get entry --json` returns one complete stored entry, and `get catalog --json` returns catalog details with an entry page. `stats --json` returns domain, catalog, and entry totals with their per-status breakdowns; `seed <source> --json` returns the source label and seeded/duplicate counts.

### Search and get indexed data

`ardvark search` provides a portable query layer over SQLite, MySQL, and Postgres. Free text matches URNs, display names, descriptions, tags, representative queries, and reference URLs without case sensitivity. Exact filters can narrow the result by serving host, URN publisher, media type, tag, or catalog verdict.

```bash
ardvark search "database agent" \
  --type application/mcp-server-card+json \
  --verdict valid \
  --json
```

Results are ordered by entry ID. `--limit` accepts 1–200 entries. When another page exists, JSON output includes `next_cursor`; pass that value to `--cursor` to continue without repeating earlier rows.

`ardvark get catalog <id|url>` returns one stored catalog with its verbatim document, catalog checks, and an entry page. Entry pages contain up to 50 rows by default and include `next_entry_cursor` when another page exists; continue with `--entry-cursor`. `ardvark get entry <id|urn>` returns one stored entry with its catalog context and checks. URL and URN selectors return the newest matching stored version; numeric IDs select an exact row. Raw documents are JSON strings because ardvark also stores malformed catalogs for diagnostics.

```bash
ardvark get entry urn:air:example.com:tools:search --json
```

### MCP server

`ardvark mcp` runs a stdio MCP (Model Context Protocol) server exposing ardvark's commands as tools, so an agent can drive the crawler directly instead of shelling out. Each tool is a thin wrapper over the same code path as the CLI and returns the same typed JSON structure as the corresponding command's [`--json`](https://ardvark.no/docs.html#json-output) output. Config resolution is identical too: `--config`, then `ardvark.json` in the server process's working directory, then the user config dir. Protocol messages flow over stdout; diagnostics go to stderr.

```text
ardvark mcp [flags]
```

| Tool | Arguments | What it does |
| --- | --- | --- |
| `ardvark_probe` | `hosts` | Probe hosts directly for ARD documents via the well-known path and robots.txt `Agentmap:`, recording results to the database. A few polite requests per host. |
| `ardvark_verify` | `target` | Verify one catalog — a URL or a local file path — and return the full check report. Does not write to the database. |
| `ardvark_crawl` | `seeds`, `force` (both optional) | Seed the frontier, run the crawler until it's empty, and return the run summary. Pending work from prior runs is resumed automatically. |
| `ardvark_seed` | `source`, `count`, `offset` | Bootstrap the frontier from `ct`, `crtsh`, `tranco`, `github` (needs `GITHUB_TOKEN`), `mcp`, `curated`, or `commoncrawl`. |
| `ardvark_stats` | — | Summarize the indexed dataset. Read-only and fast — a good first call. |
| `ardvark_info` | — | Report installation metadata: version, resolved config path, storage backend (with absolute sqlite path, existence, size), and event log location. Read-only and instant; never opens the database or the network, so it works even when storage is misconfigured. |
| `ardvark_search` | `query`, `host`, `publisher`, `media_type`, `tag`, `verdict`, `limit`, `cursor` | Search indexed entries with stable cursor pagination. Read-only. |
| `ardvark_get_catalog` | `selector`, `entry_limit`, `entry_cursor` | Read a stored catalog by numeric ID or exact URL, including its raw document, checks, and a cursor-paginated entry page. Read-only. |
| `ardvark_get_entry` | `selector` | Read a stored entry by numeric ID or exact URN, including its catalog context and checks. Read-only. |
| `ardvark_export` | `format`, `out`, `overwrite` | Dump every indexed entry to a file as JSONL or CSV and return the row count. Existing files are rejected unless `overwrite` is true. |

Register it in any client that takes an `mcpServers` map (Claude, Cursor, and friends):

```json
{
  "mcpServers": {
    "ardvark": {
      "command": "ardvark",
      "args": ["mcp", "--config", "/path/to/ardvark.json"]
    }
  }
}
```

**Resources** — clients that support MCP resources can read `ardvark://catalog/{id}` and `ardvark://entry/{id}`. Search first to obtain the numeric IDs. A catalog resource returns its first 50-entry page; use `ardvark_get_catalog` with `entry_cursor` for later pages. Resource contents use `application/json`.

**Safety metadata** — every tool declares MCP read-only, destructive, idempotent, and open-world hints. These hints help clients present suitable confirmation controls. They do not replace client policy. In particular, `ardvark_export` writes to the local filesystem and requires `overwrite: true` before it can replace a file.

Clients rarely launch servers from your project directory, so either pass an absolute `--config` path or keep your config in the user config dir (`~/.config/ardvark/ardvark.json`, `%AppData%\ardvark\ardvark.json`). Relative paths in a config file resolve against the config file's directory, so a user-dir config keeps its database and event log there — no absolute paths needed. Only with no config file at all do the defaults land in whatever directory the client starts the server in.

**Long-running crawls** — `ardvark_crawl` drains the entire frontier at polite per-host rates and can take minutes to hours depending on what's queued, and many MCP clients time out tool calls well before that. Prefer `ardvark_probe` for quick checks of specific hosts, keep `ardvark_crawl` for small seed lists, and run big crawls from the CLI.

**Testing by hand** — piping a one-shot request into stdin (`echo '...' | ardvark mcp`) closes stdin immediately, and the server exits when its input does — usually before you see the responses. Hold stdin open for the whole session, the way real clients do (an interactive terminal, a coprocess, or an MCP inspector tool), and start with the protocol's initialize handshake.

ardvark.no describes this server in its own catalog: a server card at [`/.well-known/ardvark-mcp.json`](https://ardvark.no/.well-known/ardvark-mcp.json), referenced from [`/.well-known/ai-catalog.json`](https://ardvark.no/.well-known/ai-catalog.json).

### JSONL event log

Alongside the database, every significant crawl event — probe result, catalog verified, item failed, registry harvested, … — is appended as a JSON line to `log.file` (`ardvark.jsonl` by default, verbosity set by `log.level`). It's the machine-readable record of what happened when; the CLI's printed rows are the human-readable one. Two real lines from a crawl of ardvark.no:

```console
{"time":"2026-07-15T22:55:16.360059+02:00","level":"INFO","msg":"crawler: host probed","host":"ardvark.no","domain_id":1}
{"time":"2026-07-15T22:55:17.360293+02:00","level":"INFO","msg":"crawler: catalog verified","url":"https://ardvark.no/.well-known/ai-catalog.json","host":"ardvark.no","verdict":"valid","entries":3}
```

Tail it with `jq` for live filtering — `tail -f ardvark.jsonl | jq 'select(.verdict)'` shows only verification results.

### Exit codes

| Command | Exit codes |
| --- | --- |
| `verify <path|url>` | 0 when the verdict is `valid` or `valid_with_warnings`; 1 when it's `invalid` (or the document couldn't be fetched or parsed). |
| `verify --stored` | 0 when every stored catalog verifies; 1 if any comes back `invalid`. |
| everything else | 0 on success, 1 on failure — e.g. `seed github` without `GITHUB_TOKEN`, an unreachable database, or an invalid config file. |

So `if ardvark verify "$URL"; then ...` means "at least `valid_with_warnings`", and a crawl that finds nothing still exits 0 — no catalogs is a result, not an error.

## Querying the dataset

Use [`ardvark search`](https://ardvark.no/docs.html#search-index) for portable entry lookup and `ardvark get` for stored catalog and entry details. Use direct SQL for aggregation and joins that the CLI does not provide.

The crawl output is a plain relational schema ([nine tables](https://ardvark.no/docs.html#storage)), identical across SQLite, MySQL, and Postgres — the queries below run unchanged on any of them; the `sqlite3` invocations just match the default backend. Column names are snake\_case versions of the model fields.

Catalogs by verdict, with the host that served them:

```bash
sqlite3 ardvark.db "
  SELECT d.host, c.verification_status, c.source_url
  FROM catalogs c
  JOIN domains d ON d.id = c.domain_id
  ORDER BY c.verification_status, d.host;
"
```

Entries by media type — what kinds of resources the ecosystem is actually publishing:

```bash
sqlite3 ardvark.db "
  SELECT media_type, COUNT(*) AS n
  FROM catalog_entries
  GROUP BY media_type
  ORDER BY n DESC;
"
```

Everything one publisher has put out, wherever it was discovered (URN segments are split into their own columns):

```bash
sqlite3 ardvark.db "
  SELECT urn, display_name, media_type, ref_url
  FROM catalog_entries
  WHERE urn_publisher = 'ardvark.no';
"
```

Artifacts by content hash — the same agent card served from multiple entries or hosts dedupes to one hash:

```bash
sqlite3 ardvark.db "
  SELECT content_hash, COUNT(*) AS copies, MIN(url) AS example_url
  FROM artifacts
  GROUP BY content_hash
  HAVING COUNT(*) > 1;
"
```

Two details worth knowing:

-   **`artifacts.raw_body` is a byte column** (BLOB in SQLite) because artifacts can be binary — skill archives, gzip tarballs. SQLite's `length()` on a BLOB returns bytes directly, so `length(raw_body)` is fine; dumps made back when it was a TEXT column needed a `CAST(raw_body AS BLOB)` to count bytes instead of characters.
-   **JSON-in-text columns** — `catalog_entries.tags`, `.capabilities`, `.representative_queries`, and `.trust_manifest` hold JSON text; unpack them with `json_each()` in SQLite (equivalents exist in MySQL and Postgres).

## Commands

Every command accepts the global flags `--config <path>` (default: `./ardvark.json`, then `~/.config/ardvark/ardvark.json` on Unix/macOS or `%AppData%\ardvark\ardvark.json` on Windows) and `--color auto|always|never`. `crawl`, `work`, `probe`, every `seed` source, `verify`, `search`, both `get` subcommands, `stats`, and `info` also accept `--json` — see [JSON output](https://ardvark.no/docs.html#json-output).

### crawl

Seeds the persistent frontier from the given URLs and/or bare domains (and/or a `--list` file), then runs the crawler until the frontier is empty. Pending work from prior runs is resumed automatically. Spidering depth and page budget come from `crawler.maxDepth` and `crawler.maxPagesPerDomain`; pace and politeness from `crawler.concurrency`, `crawler.perHostRequestsPerSecond`, and `crawler.respectRobotsTxt` (see [Configuration](https://ardvark.no/docs.html#configuration)).

With a worker fleet configured (`crawler.worker.count` > 1), this process seeds every shard but only dequeues its own (`crawler.worker.index`), so run [`ardvark work`](https://ardvark.no/docs.html#cmd-work) for the other worker indices to drain them — see [Distributed crawling](https://ardvark.no/docs.html#distributed).

```text
ardvark crawl [url|domain]... [flags]
```

| Flag | Meaning |
| --- | --- |
| `--list string` | path to a file of newline-separated URLs/domains to seed |
| `--force` | bypass the host\_probe freshness window (`crawler.refreshAfterHours`) and re-probe hosts probed recently |
| `--json` | emit the run summary as JSON instead of live rows |

```console
$ ardvark crawl https://acme.example
seeded 1 of 1 requested seed(s)
  hit   acme.example           well_known       catalog valid          14 entries
  hit   docs.acme.example      link_tag         valid_with_warnings    queries.count
  miss  static.acme.example    well_known       404
  miss  static.acme.example    robots_agentmap  no Agentmap directive
run complete: 12 pages fetched · 3 hosts probed · 2 catalogs found · 2 valid · 0 errors
```

### work

Drains the shared frontier with the crawl engine _without seeding it_ — the worker half of a [distributed crawl](https://ardvark.no/docs.html#distributed). Several `work` processes can point at the same MySQL/Postgres database, each taking a disjoint `--worker` share of hosts so they crawl cooperatively instead of duplicating each other's requests. Like `crawl`, it waits for the whole shared frontier to drain before exiting; an empty frontier is a friendly no-op, not an error (seed it first with `ardvark seed` or `ardvark crawl`). SQLite is single-process, so running more than one `work` process against a SQLite database is not supported.

```text
ardvark work [flags]
```

| Flag | Meaning |
| --- | --- |
| `--worker string` | this process's share of the frontier as `i/n` — 0-based index / total worker count (e.g. `2/4`), overriding config `crawler.worker` |
| `--force` | bypass the host\_probe freshness window (`crawler.refreshAfterHours`) and re-probe hosts probed recently |
| `--json` | emit the run summary as JSON instead of live rows |

### probe

Probe host(s) directly for ARD documents, without HTML spidering — checks `/.well-known/ai-catalog.json` and the robots.txt `Agentmap:` directive only. Request timeout and per-host pacing follow `crawler.requestTimeoutSeconds` and `crawler.perHostRequestsPerSecond`.

```text
ardvark probe <host>... [flags]
```

```console
$ ardvark probe huggingface.co github.com
  hit   huggingface.co         well_known       found                  200 application/json; charset=utf-8
  miss  huggingface.co         robots_agentmap  no Agentmap directive
  miss  github.com             well_known       404
  miss  github.com             robots_agentmap  no Agentmap directive
probe complete: 1 hit · 3 misses · 0 errors
```

### seed

Bootstrap the frontier from an external domain source — `seed` itself has no flags; each of the seven sources is a subcommand, covered flag-by-flag in the [Seeding strategies](https://ardvark.no/docs.html#seeding) guide.

```text
ardvark seed [ct|crtsh|tranco|github|mcp|curated|commoncrawl] [flags]
```

### verify

Runs the ARD verification pipeline (JSON Schema + semantic checks) against a local file or remote URL and prints the check report, exiting 1 if the verdict is invalid. With `--stored`, every catalog already in the configured database (`storage.driver`/`storage.dsn`) is re-verified in place instead — useful after a spec or schema update.

```text
ardvark verify <path|url> [flags]
```

| Flag | Meaning |
| --- | --- |
| `--stored` | re-verify every catalog stored in the database instead of a single document |
| `--strict` | validate against the exact published schema, promoting format assertions to errors |
| `--json` | emit the check report as JSON instead of the printed report card |

```console
$ ardvark verify ./my-catalog.json
./my-catalog.json
  ✓ identifier.unique          all entry identifiers are unique
  ✓ catalog.spec_version       specVersion is "1.0"
  ✓ entry.value_or_reference   urn:air:example.com:agent:support — exactly one of url or data is present
  ✓ urn.format                 urn:air:example.com:agent:support — identifier is a well-formed ARD URN
  ! queries.count              urn:air:example.com:agent:support — 0 representativeQueries present (recommended: 2-5)
  ✓ entry.media_type           urn:air:example.com:agent:support — type "application/a2a-agent-card+json" is a recognized ARD media type
verdict: valid_with_warnings
```

See [what failure looks like](https://ardvark.no/docs.html#verification-failure) for a failing report, and [Publishing a catalog](https://ardvark.no/docs.html#publishing) for using this as a pre-publish check.

### search

Search indexed catalog entries. The optional text argument is matched without case sensitivity across URNs, display names, descriptions, tags, representative queries, and reference URLs. Filters are combined with AND. Results are ordered by entry ID.

```text
ardvark search [text] [flags]
```

| Flag | Meaning |
| --- | --- |
| `--host string` | exact serving host |
| `--publisher string` | exact URN publisher |
| `--type string` | exact entry media type |
| `--tag string` | exact entry tag |
| `--verdict string` | `valid`, `valid_with_warnings`, or `invalid` |
| `--limit int` | maximum results, 1–200 (default: 50) |
| `--cursor uint` | last entry ID returned by the previous page |
| `--json` | emit entries and `next_cursor` as JSON |

### get

Read one stored catalog or entry. Numeric IDs select an exact stored row. Exact catalog URLs and entry URNs select the newest matching stored version. JSON output includes the verbatim stored document and verification checks. Catalog entries use cursor pagination.

```text
ardvark get catalog <id|url> [--entry-limit 50] [--entry-cursor id] [--json]
ardvark get entry <id|urn> [--json]
```

### export

Dump catalog entries, joined with domain and verification status, as JSONL or CSV. Reads the database configured by `storage.driver`/`storage.dsn`.

```text
ardvark export [flags]
```

| Flag | Meaning |
| --- | --- |
| `--format string` | output format: jsonl or csv (default "jsonl") |
| `--out string` | output file path (default: stdout) |
| `--overwrite` | replace an existing output file; without this flag an existing path is rejected |

```bash
ardvark export --format jsonl --out resources.jsonl
```

### stats

Summarize the dataset: hosts probed, catalogs by verdict, entries by media type. Use [`search`](https://ardvark.no/docs.html#cmd-search) to find entries and direct SQL for custom aggregation.

```text
ardvark stats [flags]
```

```console
$ ardvark stats
domains
  total                        847
    found_valid                3
    not_found                  798
    unprobed                   46
catalogs
  total                        3
    valid                      2
    valid_with_warnings        1
entries
  total                        41
    application/mcp-server-card+json 18
    application/a2a-agent-card+json 15
    application/ai-skill+md    8
```

### info

Reports the running installation's metadata: binary version, the resolved config file path (and whether it exists — see [configuration](https://ardvark.no/docs.html#configuration) for the resolution order), the storage backend with the absolute sqlite database location and size, and the event log file. It never opens the database, so it works even when storage is misconfigured — a good first call when something looks off.

```text
ardvark info [flags]
```

```console
$ ardvark info
ardvark
  version                      0.7.0
config
  path                         /Users/you/ardvark.json
  exists                       false
storage
  driver                       sqlite
  dsn                          ardvark.db
  path                         /Users/you/ardvark.db
  exists                       true
  size                         139264
log
  file                         /Users/you/ardvark.jsonl
  level                        info
```

### migrate

Opens the configured database (`storage.driver`/`storage.dsn`) and runs GORM AutoMigrate for all ardvark tables, creating the schema if it does not already exist.

```text
ardvark migrate [flags]
```

```bash
ardvark migrate
```

### mcp

Serves ardvark's commands as MCP tools over stdio, for use from an MCP client rather than a shell. No flags of its own; see the [MCP server](https://ardvark.no/docs.html#mcp) section for the tool list, client configuration, and caveats.

```text
ardvark mcp [flags]
```

## Configuration

ardvark runs with sensible defaults and no config file. To change anything, drop an `ardvark.json` in the working directory or in your user config dir (`~/.config/ardvark/ardvark.json` on Unix/macOS, `%AppData%\ardvark\ardvark.json` on Windows), or pass `--config path` — explicit flag wins, then working directory, then user config dir. Relative file paths in a config file (`storage.dsn` for sqlite, `log.file`) resolve against the config file's directory, not the process's working directory — a user-dir config keeps its database and event log in `~/.config/ardvark/` unless it says otherwise. With no config file at all, the defaults land in the working directory. The file is schema-validated — a typo'd key or bad value gets a precise `config.<path>: ...` error, not silent misbehavior. Values present in the file override defaults; missing keys keep their default.

**Environment** — `GITHUB_TOKEN` is the only environment variable ardvark reads for its own behavior; it's required by [`seed github`](https://ardvark.no/docs.html#seed-github) and nothing else. (The standard `NO_COLOR`/`TERM` conventions are honored for color, alongside `--color`.)

| Key | Default | Meaning |
| --- | --- | --- |
| `storage.driver` | `sqlite` | `sqlite`, `mysql`, or `postgres` |
| `storage.dsn` | `ardvark.db` | File path (sqlite; relative resolves against the config file's directory) or DSN (mysql/postgres) |
| `log.file` | `ardvark.jsonl` | JSONL event log path (relative resolves against the config file's directory) |
| `log.level` | `info` | `debug`, `info`, `warn`, or `error` |
| `crawler.concurrency` | `8` | Parallel workers |
| `crawler.maxDepth` | `2` | Anchor-following depth from seeds |
| `crawler.maxPagesPerDomain` | `50` | Page budget per domain |
| `crawler.perHostRequestsPerSecond` | `1` | Politeness rate limit |
| `crawler.requestTimeoutSeconds` | `15` | Per-request timeout |
| `crawler.maxBodyBytes` | `5242880` | Response body size cap (5 MiB) |
| `crawler.userAgent` | `ardvark (+https://github.com/helgesverre/ardvark)` | User-Agent header sent on every request |
| `crawler.respectRobotsTxt` | `true` | Honor robots.txt disallow rules |
| `crawler.refreshAfterHours` | `168` | Skip hosts probed within this window (see `--force`) |
| `crawler.leaseSeconds` | `600` | How long a dequeued frontier item stays leased before a peer may reclaim it; only matters for [distributed crawling](https://ardvark.no/docs.html#distributed). Must outlast the slowest handler. |
| `crawler.worker.index` | `0` | This process's 0-based position among `crawler.worker.count` cooperating workers; must be less than `count`. See [Distributed crawling](https://ardvark.no/docs.html#distributed). |
| `crawler.worker.count` | `1` | Total cooperating workers sharing the frontier; `1` means no sharding (single-process) |
| `ard.maxCatalogDepth` | `3` | Nested-catalog recursion bound |
| `ard.fetchArtifacts` | `true` | Fetch the agent/MCP-server cards entries point at |
| `registry.harvest` | `true` | Harvest registries discovered via catalog entries |
| `registry.maxReferralDepth` | `2` | Registry referral-following bound |
| `registry.pageLimit` | `20` | Max `POST /search` pages per registry |
| `seed.ct.logListUrl` | gstatic CT log list v3 | Source of live CT log shard URLs |
| `seed.ct.logs` | `["oak", "argon", "nimbus"]` | CT log operator tokens tried for `seed ct` |
| `seed.ct.entryCount` | `1000` | Default `seed ct` entry count |
| `seed.crtsh.endpoint` | `https://crt.sh` | crt.sh endpoint |
| `seed.crtsh.count` | `1000` | Default `seed crtsh` domain count (own key, not shared with `seed.ct.entryCount`) |
| `seed.tranco.listUrl` | `tranco-list.eu/top-1m.csv.zip` | Tranco list URL |
| `seed.tranco.top` | `1000` | Default `seed tranco` domain count |
| `seed.github.query` | `filename:ai-catalog.json path:.well-known` | GitHub code-search query for `seed github` |
| `seed.github.count` | `100` | Default `seed github` domain count |
| `seed.mcp.registryUrl` | `https://registry.modelcontextprotocol.io` | MCP registry API base URL for `seed mcp` |
| `seed.mcp.count` | `1000` | Default `seed mcp` domain count |
| `seed.curated.urls` | three awesome-mcp-servers READMEs | List documents scanned by `seed curated` |
| `seed.curated.count` | `500` | Default `seed curated` domain count |
| `seed.commoncrawl.graphInfoUrl` | `index.commoncrawl.org/graphinfo.json` | Lists available web-graph releases |
| `seed.commoncrawl.graph` | `""` (latest) | Pinned Common Crawl web-graph release id |
| `seed.commoncrawl.top` | `1000` | Default `seed commoncrawl` domain count |
| `seed.commoncrawl.offset` | `0` | Ranked domains skipped before collecting |

## Storage

Everything flows from seeders into one persistent queue, out through five handler types, and into the tables — plus a [JSONL event log](https://ardvark.no/docs.html#jsonl-log) alongside:

```text
seed ct / crtsh / tranco / github / mcp / curated / commoncrawl    crawl url · domain · --list
                         │                                                    │
                         └───────────────────┬────────────────────────────────┘
                                             ▼
                         frontier_items — persistent queue in the DB
                         (pending → in_flight → done / failed)
                                             │
                         dispatcher + workers (crawler.concurrency)
                                             │
       ┌──────────────┬──────────────────────┼─────────────────────┬─────────────────────┐
       ▼              ▼                      ▼                     ▼                     ▼
  page_fetch     host_probe           catalog_fetch       artifact_fetch       registry_harvest
  spider HTML,   well-known +         fetch, verify,      agent cards,         POST /search,
  link tags      robots Agentmap      split entries       MCP server cards     follow referrals
       │              │                      │                     │                     │
       │              ▼                      ▼                     ▼                     ▼
       │       domains · probes    catalogs · catalog_entries   artifacts           registries
       │                           · verification_checks
       └── new hosts, catalogs, artifacts, and registries feed back into the frontier ───┘

  every step also appends an event line to the JSONL log (log.file)
```

Every raw document is kept verbatim alongside the extracted data, so you can re-process without re-crawling. Nine tables:

| Table | What's in it |
| --- | --- |
| `crawl_runs` | One row per `ardvark crawl` invocation: start/finish time, config snapshot, run totals. |
| `frontier_items` | The persistent work queue — page fetches, host probes, catalog fetches, artifact fetches, registry harvests — with status, attempts, and dedup key. |
| `domains` | Every discovered host, its discovery source, and its ARD status (unprobed / not\_found / found\_invalid / found\_valid). |
| `probes` | Every probe attempt against a domain: method, outcome (hit/miss/error), HTTP status, full history. |
| `catalogs` | Fetched catalogs with raw JSON kept verbatim, content hash, and verification status. Invalid catalogs are stored too — flagged `invalid`, not dropped. |
| `catalog_entries` | Individual resource entries with URN segments split out for filtering; registry-harvested entries live in the same table with provenance back to their source registry. |
| `artifacts` | The fetched agent cards and MCP server cards entries point at, raw body included. |
| `registries` | ARD registries discovered via catalog entries, with harvest status and referral provenance. |
| `verification_checks` | One row per check per catalog (or entry) — the machine-readable report card `ardvark verify` prints. |

Swapping storage backends is one config key, `storage.driver`, plus a matching `storage.dsn`:

```jsonc
// sqlite (default) — dsn is a file path
{
  "storage": {
    "driver": "sqlite",
    "dsn": "ardvark.db"
  }
}
```

```jsonc
// mysql
{
  "storage": {
    "driver": "mysql",
    "dsn": "user:pass@tcp(127.0.0.1:3306)/ardvark?parseTime=true"
  }
}
```

```jsonc
// postgres
{
  "storage": {
    "driver": "postgres",
    "dsn": "postgres://user:pass@127.0.0.1:5432/ardvark?sslmode=disable"
  }
}
```

`ardvark migrate` creates or updates the schema for whichever driver is configured; `crawl`, `verify`, and every other command that touches the database run the same migration on open. The schema is identical across all three drivers — see [Querying the dataset](https://ardvark.no/docs.html#querying) for working with it directly.

## Distributed crawling

One frontier, many workers. When the frontier lives in MySQL or Postgres, several `ardvark` processes — on one machine or many — can drain it cooperatively. Each worker owns a fixed share of hosts, so exactly one worker ever talks to a given host: per-host politeness (`crawler.perHostRequestsPerSecond`, robots.txt) stays correct with no cross-process coordination. SQLite is single-process only — distributed crawling needs a shared MySQL/Postgres database.

Hosts are partitioned by hash across `crawler.worker.count` shards; each worker claims the shard matching its `crawler.worker.index` (0-based, less than `count`). Configure the count once in the shared `ardvark.json` and give each process its index with the `--worker i/n` flag. Seed the frontier, then run one worker per shard:

```jsonc
// shared ardvark.json — every worker points at the same database
{
  "storage": {
    "driver": "postgres",
    "dsn": "postgres://user:pass@db:5432/ardvark?sslmode=disable"
  },
  "crawler": { "worker": { "count": 4 } }
}
```

```bash
# seed the shared frontier once...
ardvark seed ct --count 5000
# ...then start one worker per shard (each on its own machine or core)
ardvark work --worker 0/4
ardvark work --worker 1/4
ardvark work --worker 2/4
ardvark work --worker 3/4
```

The `--worker i/n` flag overrides `crawler.worker` from config, so a single shared config plus a per-process flag is enough; you don't need a different config file per worker. A seeded [`ardvark crawl`](https://ardvark.no/docs.html#cmd-crawl) can stand in for one of the workers — it seeds every shard but only dequeues its own — so a common pattern is one `crawl` that seeds and works shard 0, paired with `work` peers for the rest.

#### Leases and reclaim

When a worker dequeues an item it takes a lease on it for `crawler.leaseSeconds` (default 600s). A worker that dies mid-item can't requeue the item itself, so once the lease expires a peer reclaims it back to pending and finishes it. The default outlasts the slowest legitimate handler (registry harvest pagination plus retry backoff), so a live worker's item is never reclaimed out from under it — raise `crawler.leaseSeconds` if you tune handlers to run longer, don't lower it below the slowest one.

Termination is global: each worker keeps polling until the _whole_ shared frontier is empty, not just its own shard, then exits. Per-domain page budgets (`crawler.maxPagesPerDomain`) are enforced in the database, so they hold across the fleet rather than per process.

#### Dead workers and clocks

-   **Sharding is static** — a host belongs to a shard, and a shard belongs to a fixed `(index, count)` partition. If a worker dies permanently, its shard's not-yet-started items are never leased, so lease-expiry reclaim never touches them, and its peers keep polling forever because the global pending count stays non-zero. Recovery is operational, not automatic: restart the missing worker with the _same_ `--worker index/count`, or restart the whole fleet with a new `count`.
-   **Changing `count` is fleet-wide** — every worker sharing a frontier must use the same `count`; fully stop and drain the existing fleet before starting workers with a new one. Running old (`count=N`) and new (`count=M`) workers at the same time (e.g. a rolling deploy) violates one-worker-per-host for roughly 1/lcm(N,M) of shards — a politeness overshoot, not data corruption.
-   **Keep clocks in sync** — lease expiry compares timestamps written by different machines, so run the workers' hosts under NTP. A badly skewed clock can reclaim a live worker's items early or leave a dead worker's items stranded too long.

## Operational notes

-   **Politeness by default** — `crawler.perHostRequestsPerSecond` rate-limits requests per host (default 1/s), `crawler.respectRobotsTxt` honors disallow rules (default on), and `crawler.maxBodyBytes`/redirect caps bound what a single response can cost. One host having a bad day doesn't sink a run.
-   **Resumable runs** — the crawl frontier lives in the database as `frontier_items`, not in process memory. Kill `ardvark crawl` at any point and run it again: pending and in-flight items are picked back up automatically, no separate resume flag needed.
-   **`--force`** — hosts probed within `crawler.refreshAfterHours` (default 168h / 7 days) are skipped on a normal run. `--force` bypasses that freshness window and re-probes them anyway.
-   **`refreshAfterHours`** — tune this to control how often previously-probed hosts get re-checked; lower it for fast-moving domain sets, raise it to avoid re-hitting a stable dataset.
-   **Exit codes** — 0 on success, non-zero on error; `verify` also exits 1 on an `invalid` verdict. Full table under [Programmatic use](https://ardvark.no/docs.html#exit-codes).

#### Troubleshooting

-   **`seed crtsh` returns 503 or times out** — crt.sh is a shared public service and rate-limits aggressively under load. Retry later, lower `--count`, or use `--match` to narrow the query; `seed ct` reads the CT logs directly and doesn't share this bottleneck.
-   **`GITHUB_TOKEN is not set`** — `seed github` fails with `GitHub's code search API requires authentication; set it to a personal access token`. Export a token (no special scopes needed for public code search) and re-run.
-   **A host shows `not_found` but you know it publishes** — the catalog probably appeared after the last probe, and hosts probed within `crawler.refreshAfterHours` (default 7 days) are skipped on subsequent runs. Re-probe it now with `ardvark crawl <host> --force`.
-   **Ctrl-C mid-crawl** — safe. On interrupt, in-flight frontier items are requeued as pending; if a run dies without cleanup (crash, `kill -9`), items stranded `in_flight` are reclaimed at the next startup. Either way, re-running `ardvark crawl` resumes where it stopped.
