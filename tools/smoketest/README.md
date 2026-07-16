# Distributed-crawling smoke test

A live, containerized smoke test for ardvark's distributed crawling: **10 worker
containers sharing one frontier database** (MySQL by default, or PostgreSQL via
`DB=postgres`), crawling a synthetic multi-host TLS fixture. It exercises the frontier lease/reclaim machinery, host-affinity
sharding (including the foreign-host artifact fix), the DB-backed page budget,
and global termination.

Everything here is disposable test-harness scaffolding. It is self-contained
under `tools/smoketest/` and touches no product code.

## Running

```sh
cd tools/smoketest
./run.sh              # MySQL (default)
DB=postgres ./run.sh  # PostgreSQL
```

Requirements: Docker + `docker compose` v2/v5, and Go on the host (used for the
cert generator and the assertion tool). `run.sh` builds everything, runs all
four scenarios, prints per-scenario PASS/FAIL with observed numbers, and tears
down every container it created on exit (`docker compose down -v` plus removal
of the `sm-*` one-off containers). It only ever touches containers it created.

## Database backend (`DB=mysql|postgres`)

The frontier backend is selected by the `DB` env var (default `mysql`, so the
original flow is unchanged). `DB=postgres` swaps the compose service (a
`postgres:16` with a `pg_isready` healthcheck, published on host port `15499`),
the storage `driver`/`dsn` injected into the config (gorm's postgres keyword
DSN, `host=... user=... dbname=... sslmode=disable`), the `reset_db`
schema-reset SQL (`DROP SCHEMA public CASCADE` instead of `DROP DATABASE`), and
the assert tool's `-driver`. Only the selected DB service is ever brought up;
the ardvark containers run with `--no-deps` so the unused backend is never
created. Product code is already portable — the frontier's dequeue uses
`SELECT ... FOR UPDATE SKIP LOCKED`, which both MySQL 8 and Postgres support
natively, and every SQL statement in `assert/` and `run.sh` is
backend-agnostic. Both backends pass all four scenarios with identical counts.

Note: the host port `15499` is chosen to avoid colliding with any pre-existing
local `postgres` (e.g. an `ardvark-postgres` dev container the harness did not
create). It only ever touches its own `ardvark-smoketest` project containers.

## Topology

- **mysql** (`mysql:8`) / **postgres** (`postgres:16`, `DB=postgres`) — the
  shared frontier database. Exposed on host port `13399` (mysql) / `15499`
  (postgres) so the host-side assert tool can connect.
- **fixture** — one Go TLS server (`fixture/main.go`) attached to the compose
  network with all of `site1.test` … `site20.test` as network aliases, so the
  workers resolve every synthetic host to this one server. It serves, per host:
  - `/.well-known/ai-catalog.json` — a valid ARD catalog for hosts 1–15;
    **404** for hosts 16–20 (misses).
  - `/robots.txt` — allow-all.
  - `/artifacts/*.json` — artifact documents referenced by catalog entries.
  - `/` + `/pageN` — HTML link pages, on `site1.test` only, to drive the
    page-fetch fan-out and the `maxPagesPerDomain` budget.

  Two catalogs carry a **foreign-host** artifact entry (`site1.test` →
  `site8.test`, `site3.test` → `site11.test`) whose `url` points at a host owned
  by a *different* worker shard — this is what exercises the host-affinity fix.
- **ardvark** — the worker image, built from the worktree source
  (`Dockerfile.ardvark`). It is not a long-lived service; the harness invokes it
  once per worker via `docker compose run` (each with its own `--worker i/10`),
  because each worker needs a distinct index and its own lifecycle
  (start-after-seed, SIGKILL, restart).

## TLS trust

Probing is https-only by policy (`internal/probe` builds `https://` URLs
directly), so the fixture must serve TLS the workers trust. `gencerts/main.go`
generates a throwaway CA + a SAN leaf covering `*.test` and `site1..20.test` at
harness-run time into `certs/` (gitignored, disposable). `Dockerfile.ardvark`'s
final stage copies `certs/ca.crt` into the system trust store
(`update-ca-certificates`), so Go's default transport trusts the fixture.

## Wiring: seed + drain

`ardvark crawl` takes its shard from `crawler.worker` in the config (it has no
`--worker` flag — only `ardvark work` does). So:

- **Worker 0** runs `ardvark crawl --list /seeds/seeds.txt`. Seeding writes
  host-probe/page-fetch rows for **every** shard (enqueue is not sharded), then
  worker 0 drains only its own shard 0. Its shard is set via the config, which
  the entrypoint injects from `WORKER_INDEX=0` / `WORKER_COUNT=10`.
- **Workers 1–9** run `ardvark work --worker i/10`. They are started only
  *after* the seeder has populated the frontier (the harness polls
  `frontier_items` for ≥21 rows), so they never hit `work`'s empty-frontier
  early-exit.

All 10 processes share the frontier database and each dequeues only its shard
(`host_shard % 10 = index`), but every worker waits for the *global* frontier to
drain (`Frontier.Counts()`) before exiting.

The shared config template (`config/ardvark.json`) is mounted into every
container; `worker-entrypoint.sh` personalizes it per container: the event log
(`__LOGFILE__` → `/logs/worker-<index>.jsonl`, so each worker gets a clean JSONL
stream for the log-based assertions) and `crawler.worker` (`__WORKER_INDEX__` /
`__WORKER_COUNT__`), plus the storage `driver`/`dsn` (`__DB_DRIVER__` /
`__DB_DSN__`, defaulting to the mysql wiring when unset). Key config values:
`leaseSeconds=15` (low, for the kill
test), `maxPagesPerDomain=4` (so the page budget binds on `site1.test`),
`perHostRequestsPerSecond=50`. The fixture adds `FIXTURE_DELAY_MS=150` of
response latency so the crawl runs long enough to SIGKILL a worker mid-flight.

## Scenarios

All assertions live in `assert/main.go` (Go; connects to the frontier DB via
`-driver`/`-dsn` and reads the per-worker JSONL logs). Expected counts are derived from the fixture: 20 domains,
15 catalogs, 17 entries, 17 artifacts, 40 probes.

- **a — full cooperative drain.** Seed 20 hosts, run 10 workers. Asserts every
  worker exits 0; frontier fully drained; every seeded host probed; exact
  catalog/entry/artifact counts (no duplication); no duplicate catalog per
  `(domain_id, source_url)`; all leases cleared.
- **b — shard partition.** From the per-worker logs (`crawler: item complete`
  events), asserts each worker only completed items whose fetch-target host maps
  to that worker's shard (pairwise-disjoint host sets), and that each foreign
  artifact was fetched by the worker owning the *artifact URL's* host, not the
  catalog's host.
- **c — kill-and-reclaim.** Fresh DB, SIGKILL one worker mid-crawl, wait past
  `leaseSeconds` + the 30s reclaim interval, then **restart the same worker
  index**. Asserts the fleet still drains, every (surviving/restarted) worker
  exits 0, and the reclaimed work is completed exactly once (no duplicate
  catalogs/entries). See the note below on why a restart is required.
- **d — re-crawl budget.** Re-run the fleet (no `--force`) against scenario a's
  DB. Asserts the budget host's `page_fetch` rows re-activate (flip back through
  pending and complete) rather than being starved by the budget, and that
  catalogs/entries are not duplicated by the re-crawl.

### Note: static sharding and the kill test

Sharding is **static** (`host_shard % count = index`), as documented on
`config.WorkerConfig` and `store.FrontierItem.HostShard`. A worker that dies
*permanently* strands its shard's pending items — peers only dequeue their own
shard, so they cannot pick them up, and `ReclaimExpired` returns expired leases
to *pending* but does not change ownership. Recovery is therefore operational:
**restart the same worker index**. Scenario c does exactly that; the restarted
worker's startup `ReclaimExpired` returns its own killed lease to pending and it
re-processes exactly once (idempotent handlers + lease-ownership guards).
