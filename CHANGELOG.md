# Changelog

All notable changes to ardvark are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
aims to follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/helgesverre/ardvark/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/helgesverre/ardvark/releases/tag/v0.1.0
