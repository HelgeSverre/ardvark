# Changelog

All notable changes to ardvark are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
aims to follow [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
