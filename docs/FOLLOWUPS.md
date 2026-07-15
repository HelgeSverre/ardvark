# Follow-ups

Known issues and enhancements deferred past v0.1.0, from the pre-release
adversarial review (2026-07-15). The high-impact correctness bugs from that
review are already fixed; what remains is listed here.

## Bugs (minor)

- ~~**`--force` + catalog reference cycle** (`internal/crawler/handlers.go`) —
  `--force` re-fetches unchanged catalogs, so a catalog A→B→A reference cycle
  can re-loop. The frontier depth-adopt fix mitigates most cases; verify with
  a dedicated cycle test and, if needed, a per-run visited set.~~ Done:
  bounded by `ard.maxCatalogDepth` plus frontier dedup; regression test
  `TestRun_ForceCatalogCycleTerminates` in `internal/crawler/crawler_test.go`.
- ~~**`identifier.unique` compares raw URN strings** (`internal/ard/verify.go`)
  — case-insensitive-equivalent URNs aren't caught as duplicates.~~ Done:
  compares normalized parsed URNs (lowercased NID/publisher, ASCII publisher).
- ~~**`urn.publisher_matches` uses exact host equality**.~~ Done: compares on
  registrable domain (eTLD+1) via `golang.org/x/net/publicsuffix`.
- ~~**IDN publishers in Unicode form**.~~ Done: identifiers are
  punycode-normalized before schema validation
  (`normalizeIdentifiersForSchema`).
- ~~**`seed crtsh` with no `--match`**.~~ Done: falls back to the curated
  `DefaultCrtshMatches` keyword set instead of the unservable bare wildcard.
- ~~**crtsh/tranco borrow `seed.ct.entryCount`**.~~ Done: `seed.crtsh.count`
  and `seed.tranco.top` are their own config keys.
- ~~**`seed ct --log oak`** errors while Oak is retired.~~ Done: an explicit
  retired operator gets a friendly "retired, usable operators are …" error.

## Seed sources (enhancements, highest-value first)

- ~~**GitHub code search for `filename:ai-catalog.json`**.~~ Done:
  `ardvark seed github` (needs `GITHUB_TOKEN`).
- ~~**Official MCP registry**.~~ Done: `ardvark seed mcp`. Public **A2A
  registries** remain open — no stable public registry endpoint identified
  yet; revisit when one exists.
- **Curated lists** (`awesome-mcp-servers` and similar) — parse for candidate
  domains. Still open.
- ~~**Tune CT/crt.sh matching toward `agent`/`mcp`/`ai` identities**.~~ Done:
  `DefaultCrtshMatches = agent/mcp/ai-catalog` curated set.
- **Common Crawl columnar URL index** — find `/.well-known/ai-catalog.json`
  across all crawled hosts. Heavy but exhaustive. Still open.
- ~~**CT `coversNow` shard selection**.~~ Done: shard resolution also
  considers the near-future shard window (`ctFreshCertWindow` in
  `internal/seed/ctloglist.go`) so freshly issued certs near shard boundaries
  aren't missed.
- Not worth it for bulk seeding: crt.sh alternatives (Merklemap / Censys /
  SSLMate) and CrUX (Tranco already incorporates CrUX).

## Release / ops

- ~~**Homebrew publishing pinned to goreleaser 2.15** to keep a cross-platform
  formula (`brews`); 2.16+ hard-deprecated it for macOS-only casks. Unpin by
  moving to a manual formula-generation workflow step (see how sibling repos
  do it) so the release can track the latest goreleaser.~~ Done: goreleaser
  now publishes raw per-platform binaries (`ardvark-{os}-{arch}`, `archives:
  format: binary` in `.goreleaser.yaml`) and is unpinned to `~> v2`; a new
  `homebrew` job in `.github/workflows/release.yml` downloads the release
  binaries, computes sha256s, and renders `Formula/ardvark.rb` in
  `helgesverre/homebrew-tap` itself (macOS arm64/x86_64 + Linux arm64/x86_64).
- ~~`goreleaser release` needs `HOMEBREW_TAP_TOKEN` and the `homebrew-tap`
  repo; both are configured. Consider gating the brew step so a fork without
  the secret doesn't fail the whole release.~~ Done: the `homebrew` job
  checks for `secrets.HOMEBREW_TAP_TOKEN` first and skips cleanly (with a
  `::notice::`) if it's absent, so forks without the secret don't fail the
  release. It's also skipped entirely on prerelease tags (containing `-`).

## Housekeeping

- ~~Provenance maps and per-domain page counters in the engine grow for the
  life of a crawl; bound or periodically prune for very long runs.~~ Done:
  provenance entries are released as items complete/permanently fail, and page
  counters are LRU-evicted.
- ~~Semantic checks still run after JSON Schema validation fails, which can
  double-report the same defect.~~ Done: entry-level semantic checks
  short-circuit when schema validation failed (`schemaFailed` gating in
  `internal/ard/verify.go`).
- ~~Batch-synchronized worker pool: one slow item idles the other workers
  until the batch drains.~~ Done (2026-07-15 architecture-review refactor):
  continuous worker pool with dispatcher-fed workers.
