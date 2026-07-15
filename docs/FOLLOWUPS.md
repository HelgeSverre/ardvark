# Follow-ups

Known issues and enhancements deferred past v0.1.0, from the pre-release
adversarial review (2026-07-15). The high-impact correctness bugs from that
review are already fixed; what remains is listed here.

## Bugs (minor)

- **`--force` + catalog reference cycle** (`internal/crawler/handlers.go`) —
  `--force` re-fetches unchanged catalogs, so a catalog A→B→A reference cycle
  can re-loop. The frontier depth-adopt fix mitigates most cases; verify with
  a dedicated cycle test and, if needed, a per-run visited set.
- **`identifier.unique` compares raw URN strings** (`internal/ard/verify.go`)
  — case-insensitive-equivalent URNs aren't caught as duplicates. Compare on
  the normalized parsed URN.
- **`urn.publisher_matches` uses exact host equality** — flags common
  apex-vs-`www` and subdomain serving as a warning. Compare on registrable
  domain (eTLD+1).
- **IDN publishers in Unicode form** — the schema regex is ASCII-only while
  the parser accepts U-label domains. Punycode-normalize before schema check,
  or widen the pattern.
- **`seed crtsh` with no `--match`** (`internal/seed/crtsh.go`) — a bare
  `q=%` wildcard is not something crt.sh serves; require `--match` or supply a
  sensible default filter.
- **crtsh/tranco borrow `seed.ct.entryCount`** for their default count — give
  each source its own count config key.
- **`seed ct --log oak`** errors while Oak is retired from the CT log list —
  the default (`oak, argon, nimbus`) works, but an explicit retired operator
  could give a friendlier "retired, try argon" message.

## Seed sources (enhancements, highest-value first)

- **GitHub code search for `filename:ai-catalog.json`** — the only direct,
  high-precision ARD source; extract the owning domains. Needs a GitHub token.
- **Official MCP registry + public A2A registries** — highest-propensity ARD
  adopters; harvest their listed domains directly.
- **Curated lists** (`awesome-mcp-servers` and similar) — parse for candidate
  domains.
- **Tune CT/crt.sh matching toward `agent`/`mcp`/`ai` identities** — the
  crt.sh `--match` plumbing is already there; add curated keyword sets.
- **Common Crawl columnar URL index** — find `/.well-known/ai-catalog.json`
  across all crawled hosts. Heavy but exhaustive.
- **CT `coversNow` shard selection** — CT temporal shards partition by
  certificate `NotAfter`, not issuance time, so "covers now" can pick a shard
  that is filling with soon-to-expire certs rather than freshly issued ones.
  For freshest domains, also consider the shard whose interval starts nearest
  the future. Lower priority: seeding tolerates some staleness.
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

- Provenance maps and per-domain page counters in the engine grow for the life
  of a crawl; bound or periodically prune for very long runs.
- Semantic checks still run after JSON Schema validation fails, which can
  double-report the same defect. Consider short-circuiting or de-duping rows.
