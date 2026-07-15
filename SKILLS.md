# ardvark CLI skill

Teach an agent to install, run, and query `ardvark` — a crawler that finds
ARD (Agentic Resource Discovery) `ai-catalog.json` documents on the web,
verifies them against the spec, and indexes them into SQLite/MySQL/Postgres
plus a JSONL event log.

## Install

```sh
# Homebrew
brew install helgesverre/tap/ardvark

# Go
go install github.com/helgesverre/ardvark/cmd/ardvark@latest
```

Verify it's on PATH: `ardvark --version`.

## Command surface

Every command accepts `--config path` (default `./ardvark.json`) and
`--color auto|always|never`. Add `--json` where noted to get
machine-readable output instead of the human table.

```sh
# Verify one catalog — local file or remote URL. Exits 1 if the verdict is invalid.
ardvark verify ./my-catalog.json
ardvark verify https://example.com/.well-known/ai-catalog.json

# Re-verify every catalog already in the database (after a spec/schema update)
ardvark verify --stored

# Probe specific hosts for ARD documents, no HTML spidering
ardvark probe example.com huggingface.co

# Crawl a site and everything it links to, probing each discovered host
ardvark crawl https://example.com
ardvark crawl --list seeds/adopters.txt
ardvark crawl --force          # ignore refreshAfterHours, re-probe everything

# Bootstrap the frontier from an external domain source, then drain it
ardvark seed ct --count 1000
ardvark seed crtsh --match mcp
ardvark seed tranco --top 5000
ardvark seed github --query 'filename:ai-catalog.json path:.well-known'
ardvark seed mcp
ardvark seed curated
ardvark seed commoncrawl --top 1000
ardvark crawl

# Summarize the dataset
ardvark stats

# Dump discovered resources with verification status
ardvark export --format jsonl --out resources.jsonl
ardvark export --format csv --out resources.csv

# Apply schema migrations without crawling
ardvark migrate
```

## Interpreting `verify` output

`verify` prints one row per check, prefixed `✓` (pass), `✗` (failed
error-severity check), or `!` (failed warning-severity check), then a
rolled-up verdict:

- `valid` — schema and all semantic checks pass, no warnings
- `valid_with_warnings` — schema passes, but a warning-severity check
  failed (e.g. URN publisher doesn't match the serving domain, fewer than
  2 `representativeQueries`, an unrecognized media type)
- `invalid` — an error-severity check failed (bad JSON, schema violation,
  malformed URN, duplicate identifiers, both/neither of `url`/`data`)

Exit code: `0` for `valid` and `valid_with_warnings`, `1` for `invalid` (and
for `--stored` if any stored catalog comes back invalid). Script against it:

```sh
if ardvark verify https://example.com/.well-known/ai-catalog.json; then
  echo "catalog is at least valid_with_warnings"
fi
```

`verify` does not repeat transport-level checks (Content-Type, body size,
UTF-8) when run standalone — those only apply during a live crawl fetch,
where the response headers are available.

## Querying the SQLite output

Default DSN is `ardvark.db` in the working directory. Tables:
`domains`, `probes`, `catalogs`, `catalog_entries`, `artifacts`,
`registries`, `verification_checks`, `crawl_runs`, `frontier_items`.

```sh
# Every host that failed verification, with its checks
sqlite3 ardvark.db "
  SELECT d.host, c.verification_status, vc.check_id, vc.message
  FROM catalogs c
  JOIN domains d ON d.id = c.domain_id
  JOIN verification_checks vc
    ON vc.subject_type = 'catalog' AND vc.subject_id = c.id AND vc.passed = 0
  WHERE c.verification_status = 'invalid';
"

# Count entries by media type across all indexed catalogs
sqlite3 ardvark.db "
  SELECT media_type, COUNT(*) AS n
  FROM catalog_entries
  GROUP BY media_type
  ORDER BY n DESC;
"

# Every MCP server card entry, with its publisher and source URL
sqlite3 ardvark.db "
  SELECT urn, urn_publisher, display_name, ref_url
  FROM catalog_entries
  WHERE media_type IN ('application/mcp-server-card+json', 'application/mcp-server+json');
"
```

`verification_checks.subject_id` is a catalog ID when `subject_type =
'catalog'`, or a `catalog_entries.id` when `subject_type = 'entry'` — join
accordingly. `catalog_entries.tags`, `.capabilities`,
`.representative_queries`, and `.trust_manifest` are stored as JSON text;
use `json_each()` to unpack them in SQLite, e.g.:

```sh
sqlite3 ardvark.db "
  SELECT ce.urn, je.value AS tag
  FROM catalog_entries ce, json_each(ce.tags) je;
"
```

## Politeness and config knobs

No config file is required; defaults are polite. To change anything, drop
an `ardvark.json` in the working directory (or pass `--config path`) — it's
schema-validated, so a bad key or value errors instead of silently
misbehaving. Relevant knobs under `crawler`:

- `concurrency` (default `8`) — parallel workers
- `perHostRequestsPerSecond` (default `1`) — rate limit per host
- `maxDepth` (default `2`) — anchor-following depth from seeds
- `maxPagesPerDomain` (default `50`) — page budget per domain
- `requestTimeoutSeconds` (default `15`)
- `maxBodyBytes` (default `5242880`)
- `respectRobotsTxt` (default `true`)
- `refreshAfterHours` (default `168`) — skip hosts probed within this window;
  `crawl --force` bypasses it
- `userAgent` (default `ardvark/0.1 (+https://github.com/helgesverre/ardvark)`)

Crawls are resumable: the frontier lives in the database, so killing a run
and re-running `ardvark crawl` picks up where it stopped.
