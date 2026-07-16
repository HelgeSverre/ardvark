# Changelog

All notable changes to ardvark are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
aims to follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.4.1] - 2026-07-16

### Fixed

- **Frontier dedup key is hashed to a fixed 64-char width** — long URLs no
  longer overflow the `frontier_items.dedup_key` column on MySQL/Postgres,
  where a raw `kind:natural` key past the varchar limit would truncate and
  silently collide distinct pending URLs onto the unique index (SQLite was
  unaffected). The key is now a SHA-256 digest, so it is fixed-width and
  driver-independent regardless of URL length.
- **`crawler.worker.count` is capped at the host-shard space (8192)** — a count
  above 8192 with an index at or beyond it matched no shard and silently
  dequeued nothing forever; it is now rejected at config load and by the
  `--worker i/n` flag, matching the existing fail-fast validation for an
  out-of-range index.

### Upgrading from 0.4.0 on MySQL/Postgres

- The `dedup_key` column narrows from `varchar(512)` to `varchar(64)`. A
  populated 0.4.0 frontier almost always holds keys longer than 64 chars, which
  an in-place `ALTER` cannot narrow (strict `sql_mode` aborts; non-strict would
  truncate-and-collide), so **the first 0.4.1 start drops and recreates the
  `frontier_items` table automatically.** Pending frontier work is discarded and
  re-discovered from the seed/domain tables on the next crawl — no other tables
  are touched. Finish or drain any in-progress 0.4.0 crawl before upgrading if
  you need its pending queue preserved. SQLite deployments are unaffected.

## [0.4.0] - 2026-07-16

### Added

- **Distributed crawling** — several `ardvark work` processes can share one
  MySQL/Postgres database and drain a single frontier cooperatively. Hosts are
  partitioned across workers by hash (`crawler.worker.index`/`crawler.worker.count`
  or the `--worker i/n` flag), so exactly one worker ever talks to a given host —
  keeping per-host politeness correct with no cross-process coordination. The
  frontier now leases in-flight items (`crawler.leaseSeconds`, default 600s); a
  worker that dies has its items reclaimed by peers after the lease expires.
  Termination is global — a worker exits only when the whole shared frontier is
  empty — and per-domain page budgets are enforced in the database. SQLite
  remains single-process only.
- **`ardvark work [--worker i/n] [--force] [--json]`** — drains the shared
  frontier without seeding it, exiting cleanly with a friendly note when the
  frontier is empty. Pair it with a seeded `ardvark crawl` (or other `work`
  peers) when a worker fleet is configured.
- **Config keys** — `crawler.leaseSeconds` (600), `crawler.worker.index` (0),
  and `crawler.worker.count` (1); `index` must be less than `count`, validated
  at config load.
- **`tools/smoketest/`** — a Docker-based multi-worker test harness for
  exercising distributed crawling end-to-end.

### Changed

- **Work-item provenance is persisted** — nested-catalog, artifact, and
  registry attribution is now stored in the frontier, so crawls resume fully
  across restarts instead of losing attribution that only lived in process
  memory.
- **Recognized entry media types** — the crawler now matches catalog and
  registry entry media types by semantic kind rather than exact string suffix,
  via a dedicated internal media-type parsing package.
- **Registry search results are followed** — catalog and registry pointers
  returned in a registry's `POST /search` results are now followed (pointed-to
  catalogs fetched and verified, pointed-to registries harvested), matching how
  pointers in catalog entries are already resolved. Ordinary (non-pointer)
  results are not artifact-fetched.

## [0.3.0] - 2026-07-16

### Added

- **`ardvark info`** — installation metadata: version, resolved config path,
  storage backend with absolute sqlite location and size, and event log —
  without opening the database. Also exposed as the `ardvark_info` MCP tool.
- **Config resolution** — `--config` flag, then `./ardvark.json`, then the
  user config dir (`~/.config/ardvark/ardvark.json`, `%AppData%\ardvark`);
  relative `storage.dsn` and `log.file` resolve against the config file's
  directory.
- **`ardvark mcp`** — a stdio MCP (Model Context Protocol) server embedded in
  the binary, exposing `ardvark_probe`, `ardvark_verify`, `ardvark_crawl`,
  `ardvark_seed`, `ardvark_stats`, and `ardvark_export` as tools. Each tool
  returns the same typed JSON structure as the corresponding command's
  `--json` output; diagnostics go to stderr (stdout carries the protocol).
- **`--json`** flag on `crawl`, `probe`, every `seed` source, `verify`
  (including `--stored`), and `stats` — one pretty-printed JSON document on
  stdout instead of human-readable rows; exit codes unchanged.
- **Docs page** — single-page documentation at
  [ardvark.no/docs.html](https://ardvark.no/docs.html): command reference,
  JSON output, MCP server, seeding guide, verification checks, configuration,
  storage schema, and operational notes.
- **Dogfooding** — ardvark.no publishes its own
  `/.well-known/ai-catalog.json`, an `Agentmap:` directive in robots.txt, a
  SKILLS.md agent skill, and an MCP server card at
  `/.well-known/ardvark-mcp.json`.

### Changed

- **`export` streams** — rows flow from the database cursor straight to the
  output through a hand-rolled JSON serializer (byte-identical to
  `encoding/json`, equivalence pinned by test). On a 7.75M-entry dataset:
  134s / 9.6 GB peak memory before, 55s / 36 MB after. Output is unchanged.
- **Summary counts pluralize correctly** — `probe`, `crawl`, and `stats`
  summaries now say "1 hit", "1 page fetched", "1 entry" instead of "1 hits",
  "1 pages fetched", "1 entries".
- **`verify` on local files** — the `urn.publisher_matches` check is now
  skipped when the catalog wasn't fetched from a host (a local file has no
  serving domain to compare against). Catalogs that previously rolled up as
  `valid_with_warnings` purely from that check now report `valid`; exit
  codes are unchanged.
- **Recognized entry media types** — `application/ai-skill+md` (the spec's
  Markdown skill form) is now recognized by the `entry.media_type` check;
  `application/ai-skill+json` remains recognized as a form seen on
  published catalogs.

### Fixed

- **MySQL/MariaDB/Postgres portability**, found by live testing against all
  three: binary artifacts (skill tarballs) are stored in a byte column
  instead of text (Postgres and MySQL rejected non-UTF-8; MySQL capped at
  64 KB), large JSON columns use `LONGTEXT` on MySQL, `ardvark stats` no
  longer aliases a column as the reserved word `key`, and the export scan
  tolerates NULL text columns via portable `COALESCE`.

## [0.2.0] - 2026-07-15

### Added

- **`seed github`** — GitHub code search for well-known catalog files; the
  highest-precision seed source (requires `GITHUB_TOKEN`).
- **`seed mcp`** — the official MCP server registry: remote endpoint hosts
  plus domains decoded from reverse-DNS server names.
- **`seed curated`** — scans curated awesome-mcp-servers lists (or `--url`
  overrides) for candidate domains, dropping hosting/badge/social
  infrastructure.
- **`seed commoncrawl`** — streams the newest Common Crawl web-graph
  domain-ranks file (~121M ranked domains) with early stop; `--top`,
  `--offset`, and `--graph` select the slice.
- **`--color=auto|always|never`** global flag (NO_COLOR still respected).
- `seeds/adopters.txt` — starter list of hosts confirmed to publish ARD
  catalogs, for `ardvark crawl --list`.

### Changed

- **Continuous worker pool** — workers pull frontier items individually
  instead of in synchronized batches, so one slow host no longer stalls the
  rest. 1,700+ hosts probe in ~3 minutes at concurrency 24 in ~4 MB RSS.
- Verification hardening: identifier uniqueness compares normalized URNs,
  publisher matching compares registrable domains (eTLD+1), IDN publishers
  are punycode-normalized before schema validation, and semantic checks
  short-circuit after schema failure.
- crt.sh seeding without `--match` queries a curated agent/mcp/ai keyword
  set instead of an unservable bare wildcard; every seed source has its own
  count config key.
- Homebrew publishing moved off goreleaser's deprecated `brews` to a
  formula-rendering release job (macOS + Linux, arm64 + x86_64).

### Fixed

- Registries answering HTTP 501 to `/search` are now correctly detected as
  "search not supported" (the check previously failed on wrapped errors) and
  are skipped instead of retried.
- Registry base URLs ending in `/search` no longer produce
  `/search/search` request URLs.
- Catalog reference cycles under `--force` terminate (bounded by
  `ard.maxCatalogDepth`), with a regression test.
- Interrupted runs requeue in-flight work; crashed runs reclaim stranded
  items at startup.

## [0.1.0] - 2026-07-15

First release.

### Added

- **Crawler** — a resumable, DB-backed frontier drained by a polite worker
  pool. Five work-item types (page fetch, host probe, catalog fetch, artifact
  fetch, registry harvest); per-host rate limiting, robots.txt compliance,
  body-size and redirect caps, and exponential backoff on transient failures.
- **Discovery** — finds catalogs via `/.well-known/ai-catalog.json`,
  `Agentmap:` directives in robots.txt, and `<link rel="ai-catalog">` tags.
  Recurses into nested catalogs, fetches referenced artifact documents, and
  harvests discovered ARD registries including referrals.
- **Verification** — official JSON Schema plus seven semantic checks (URN
  grammar for both `urn:air:` and `urn:ai:`, value-or-reference exclusivity,
  identifier uniqueness, representative-query count, media types, …), each
  recorded pass/fail per catalog.
- **Seeding** — `seed ct` (Certificate Transparency logs, resolved live from
  the CT log list so shard URLs never go stale), `seed crtsh`, and
  `seed tranco`.
- **Storage** — SQLite by default; MySQL and Postgres via one config key.
  Raw documents kept verbatim alongside extracted columns; append-only JSONL
  event log.
- **CLI** — `crawl`, `probe`, `seed`, `verify`, `export`, `stats`, `migrate`,
  with live per-host result rows and a JSON config validated with friendly
  error messages.

[Unreleased]: https://github.com/helgesverre/ardvark/compare/v0.4.1...HEAD
[0.4.1]: https://github.com/helgesverre/ardvark/compare/v0.4.0...v0.4.1
[0.4.0]: https://github.com/helgesverre/ardvark/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/helgesverre/ardvark/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/helgesverre/ardvark/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/helgesverre/ardvark/releases/tag/v0.1.0
