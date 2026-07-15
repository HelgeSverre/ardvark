# Changelog

All notable changes to ardvark are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
aims to follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

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

- **`verify` on local files** — the `urn.publisher_matches` check is now
  skipped when the catalog wasn't fetched from a host (a local file has no
  serving domain to compare against). Catalogs that previously rolled up as
  `valid_with_warnings` purely from that check now report `valid`; exit
  codes are unchanged.
- **Recognized entry media types** — `application/ai-skill+md` (the spec's
  Markdown skill form) is now recognized by the `entry.media_type` check;
  `application/ai-skill+json` remains recognized as a form seen on
  published catalogs.

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

[Unreleased]: https://github.com/helgesverre/ardvark/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/helgesverre/ardvark/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/helgesverre/ardvark/releases/tag/v0.1.0
