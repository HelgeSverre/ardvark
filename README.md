<p align="center">
  <img src="mascots/ardvark-classic.svg" alt="ardvark mascot" width="220">
</p>

<h1 align="center">ardvark</h1>

<p align="center">
  Crawls the web for <a href="https://agenticresourcediscovery.org">ARD</a> <code>ai-catalog.json</code> documents,
  verifies them against the spec, and indexes every discovered agentic resource.
</p>

<p align="center"><a href="https://ardvark.no">ardvark.no</a></p>

---

Publishers advertise their AI agents, MCP servers, and skills in an `ai-catalog.json` at `/.well-known/`. ardvark finds those catalogs, checks them against the ARD specification, and records everything — hosts probed, catalogs found, entries, verification results — in SQLite/MySQL/Postgres plus a JSONL event log. The result is a clean, queryable dataset of the agentic resources the web is publishing — use it to track ARD adoption, feed a registry, or build whatever you want on top.

## Features

- **Domain harvesting** — crawl from seed URLs or URL lists, follow anchors to collect hosts, then probe each host once
- **Every ARD discovery mechanism** — `/.well-known/ai-catalog.json`, `Agentmap:` directives in robots.txt, and `<link rel="ai-catalog">` tags
- **Full catalog resolution** — recurses into nested catalogs, fetches referenced artifact documents (agent cards, MCP server cards), and harvests discovered registries via `POST /search`, including registry referrals
- **Spec verification** — official JSON Schema plus seven semantic checks (URN grammar, value-or-reference exclusivity, query counts, …), each recorded pass/fail with a message
- **Bootstrap seeding** — fill the frontier from Certificate Transparency logs, crt.sh, the Tranco top list, GitHub code search, the MCP registry, curated awesome-lists, or Common Crawl domain ranks (`ardvark seed ct|crtsh|tranco|github|mcp|curated|commoncrawl`)
- **Swappable storage** — SQLite by default; MySQL and Postgres via one config key; append-only JSONL event log alongside
- **Agent-ready** — `--json` on `crawl`, `probe`, `seed`, `verify`, and `stats` for machine-readable results, plus an embedded stdio MCP server (`ardvark mcp`) exposing the same operations as tools
- **Resumable runs** — the crawl queue lives in the database; kill a run, start it again, it picks up where it stopped
- **Polite by default** — per-host rate limiting, robots.txt compliance, body-size caps, redirect caps, backoff on transient failures

## Install

```sh
# Homebrew
brew install helgesverre/tap/ardvark

# Go
go install github.com/helgesverre/ardvark/cmd/ardvark@latest
```

Or grab a binary from [releases](https://github.com/helgesverre/ardvark/releases).

## Quickstart

Crawl a site and everything it links to, probing each discovered host for ARD catalogs:

```sh
ardvark crawl https://example.com
```

Or skip crawling and probe hosts directly:

```sh
ardvark probe example.com huggingface.co
```

Seed the queue with 1000 freshly-certified domains from Certificate Transparency logs, then work through them:

```sh
ardvark seed ct --count 1000
ardvark crawl
```

Or start from `seeds/adopters.txt`, the hosts already confirmed to publish ARD catalogs:

```sh
ardvark crawl --list seeds/adopters.txt
```

Results land in `ardvark.db` (SQLite) and `ardvark.jsonl`. See what you caught:

```sh
ardvark stats
ardvark export --format jsonl --out resources.jsonl
```

## Commands

| Command | What it does |
|---------|--------------|
| `ardvark crawl [url\|domain]... [--list file] [--force]` | Seed the frontier and run the crawler until the queue is empty. Resumes pending work from earlier runs. |
| `ardvark probe <host>...` | Probe specific hosts for ARD documents, no crawling. |
| `ardvark seed ct [--count N] [--log oak\|argon\|all\|URL]` | Harvest domains from the newest Certificate Transparency log entries. Logs resolve live from the CT log list (Oak by default), so shard URLs never go stale. |
| `ardvark seed crtsh [--count N] [--match keyword]` | Harvest domains from crt.sh, narrowed to certificate identities mentioning `--match` (e.g. `agent`, `mcp`); without `--match`, queries a curated agent/mcp/ai keyword set instead of an unfiltered wildcard, which crt.sh can't serve. |
| `ardvark seed tranco [--top N] [--url URL]` | Queue the top N domains from the Tranco list — the established web CT seeding misses. |
| `ardvark seed github [--count N] [--query q]` | Search GitHub code search for well-known ARD catalog files and queue the owning repositories' domains. Highest-precision source — a hit is a real deployed catalog. Requires `GITHUB_TOKEN`. |
| `ardvark seed mcp [--count N] [--registry URL]` | Harvest domains from the official MCP server registry: each server's remote endpoint host plus a domain decoded from its reverse-DNS-style name. |
| `ardvark seed curated [--count N] [--url U]...` | Extract candidate domains from curated awesome-lists (community MCP server lists by default), dropping hosting/badge/social infrastructure so only real product domains remain. Repeated `--url` replaces the default list set. |
| `ardvark seed commoncrawl [--top N] [--offset M] [--graph ID]` | Queue the top N domains from the latest Common Crawl web-graph domain ranks (~121M ranked domains vs Tranco's 1M). Streams the gzipped ranks file and stops reading as soon as N domains are collected; `--offset` skips the first M ranks to sample deeper slices. |
| `ardvark verify <path\|url>` | Verify one catalog — local file or remote URL — and print the check report. Exits 1 if invalid. `--stored` re-verifies everything in the database. |
| `ardvark export [--format jsonl\|csv] [--out file]` | Dump discovered resources with their verification status. |
| `ardvark stats` | Summarize the dataset: hosts probed, catalogs by verdict, entries by type. |
| `ardvark info` | Show installation metadata: version, resolved config path, database and log locations. Never opens the database. |
| `ardvark migrate` | Create/update the database schema. |
| `ardvark mcp` | Serve ardvark's commands as MCP tools over stdio: `ardvark_probe`, `ardvark_verify`, `ardvark_crawl`, `ardvark_seed`, `ardvark_stats`, `ardvark_info`, `ardvark_export`. |

`crawl`, `probe`, every `seed` source, `verify`, and `stats` also take `--json` to emit the result as pretty-printed JSON on stdout (diagnostics go to stderr; exit codes are unchanged) — the same typed structures the MCP tools return.

## Configuration

ardvark runs with sensible defaults and no config file. To change anything, drop an `ardvark.json` in the working directory, in your user config dir (`~/.config/ardvark/ardvark.json` on Linux/macOS — `$XDG_CONFIG_HOME` respected — or `%AppData%\ardvark\ardvark.json` on Windows), or pass `--config path`. Explicit `--config` wins, then the working directory, then the user config dir. The file is schema-validated — a typo'd key or bad value gets a precise error, not silent misbehavior.

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
    "userAgent": "ardvark/0.1 (+https://github.com/helgesverre/ardvark)",
    "respectRobotsTxt": true,
    "refreshAfterHours": 168
  },
  "ard":      { "maxCatalogDepth": 3, "fetchArtifacts": true },
  "registry": { "harvest": true, "maxReferralDepth": 2, "pageLimit": 20 },
  "seed": {
    "ct":     { "logListUrl": "https://www.gstatic.com/ct/log_list/v3/log_list.json", "logs": ["oak"], "entryCount": 1000 },
    "crtsh":  { "endpoint": "https://crt.sh", "count": 1000 },
    "tranco": { "listUrl": "https://tranco-list.eu/top-1m.csv.zip", "top": 1000 },
    "github": { "query": "filename:ai-catalog.json path:.well-known", "count": 100 },
    "mcp":    { "registryUrl": "https://registry.modelcontextprotocol.io", "count": 1000 },
    "curated": {
      "urls": ["https://raw.githubusercontent.com/punkpeye/awesome-mcp-servers/main/README.md"],
      "count": 500
    },
    "commoncrawl": {
      "graphInfoUrl": "https://index.commoncrawl.org/graphinfo.json",
      "graph": "",
      "top": 1000,
      "offset": 0
    }
  }
}
```

| Key | Default | Meaning |
|-----|---------|---------|
| `storage.driver` | `sqlite` | `sqlite`, `mysql`, or `postgres` |
| `storage.dsn` | `ardvark.db` | File path (sqlite) or DSN (mysql/postgres) |
| `log.file` | `ardvark.jsonl` | JSONL event log path |
| `crawler.concurrency` | `8` | Parallel workers |
| `crawler.maxDepth` | `2` | Anchor-following depth from seeds |
| `crawler.maxPagesPerDomain` | `50` | Page budget per domain |
| `crawler.perHostRequestsPerSecond` | `1` | Politeness rate limit |
| `crawler.refreshAfterHours` | `168` | Skip hosts probed within this window |
| `ard.maxCatalogDepth` | `3` | Nested-catalog recursion bound |
| `registry.maxReferralDepth` | `2` | Registry referral-following bound |
| `seed.crtsh.count` | `1000` | Default `seed crtsh` domain count (own key, not shared with `seed.ct.entryCount`) |
| `seed.tranco.top` | `1000` | Default `seed tranco` domain count |
| `seed.github.query` | `filename:ai-catalog.json path:.well-known` | GitHub code-search query for `seed github` |
| `seed.github.count` | `100` | Default `seed github` domain count |
| `seed.mcp.registryUrl` | `https://registry.modelcontextprotocol.io` | MCP registry API base URL for `seed mcp` |
| `seed.mcp.count` | `1000` | Default `seed mcp` domain count |
| `seed.curated.urls` | three awesome-mcp-servers READMEs | List documents scanned by `seed curated` |
| `seed.curated.count` | `500` | Default `seed curated` domain count |
| `seed.commoncrawl.graph` | `""` (latest) | Common Crawl web-graph release id for `seed commoncrawl` |
| `seed.commoncrawl.top` | `1000` | Default `seed commoncrawl` domain count |
| `seed.commoncrawl.offset` | `0` | Ranked domains skipped before collecting |

## What gets stored

Every raw document is kept verbatim alongside the extracted data, so you can re-process without re-crawling:

- **domains / probes** — every host probed, by which mechanism, with full probe history
- **catalogs / catalog_entries** — parsed catalogs with URN segments split out for filtering; registry-harvested entries live in the same table with provenance
- **artifacts** — the fetched agent cards and MCP server cards entries point at
- **verification_checks** — one row per check per catalog: a machine-readable spec report card

Invalid catalogs are stored too, flagged `invalid` — "found but broken" is useful data.

## Verification

A catalog's verdict is `valid`, `valid_with_warnings`, or `invalid`, rolled up from:

1. JSON Schema validation against the official ARD schema (Draft 2020-12)
2. Semantic checks the schema can't express — errors: `specVersion == "1.0"`, exactly one of `url`/`data` per entry, URN grammar (`urn:air:` or `urn:ai:` `<publisher>:<namespace>:<name>`), unique identifiers; warnings: URN publisher matching the serving domain, 2–5 representative queries, recognized media types

Run it standalone against anything:

```sh
ardvark verify ./my-catalog.json
ardvark verify https://example.com/.well-known/ai-catalog.json
```

ardvark.no publishes its own `ai-catalog.json` and dogfoods the CLI as an agent skill — see [SKILLS.md](SKILLS.md). The catalog also carries a server card for `ardvark mcp` at [`/.well-known/ardvark-mcp.json`](https://ardvark.no/.well-known/ardvark-mcp.json).

## Development

```sh
just            # list recipes
just build      # build to ./bin/ardvark
just check      # vet + fmt check + tests
just snapshot   # local goreleaser dry-run
```

## License

MIT
