#!/usr/bin/env bash
# Distributed-crawling smoke test driver. Brings up the frontier DB (MySQL by
# default, or Postgres via `DB=postgres ./run.sh`) + the multi-host TLS fixture,
# builds the ardvark worker image from the worktree source, and runs four
# scenarios against a fleet of 10 workers sharing one frontier:
#
#   a  full cooperative drain (10 workers, --worker 0/10 .. 9/10)
#   b  shard partition (per-worker log disjointness + foreign-artifact ownership)
#   c  kill-and-reclaim (SIGKILL one worker mid-crawl, restart, drain, exactly-once)
#   d  re-crawl page budget re-activation
#
# All assertions live in ./assert (Go). This script only orchestrates container
# lifecycle and prints observed exit codes; the assert tool prints the numbers.
#
# Requirements: docker + docker compose v2/v5, and Go on the host (for gencerts
# and the assert tool). Everything it creates is torn down on exit.
set -uo pipefail

cd "$(dirname "$0")"
HERE="$(pwd)"
REPO_ROOT="$(cd ../.. && pwd)"
COMPOSE="docker compose -f $HERE/docker-compose.yml"
WORKERS=10
SEED_CMD=(crawl --list /seeds/seeds.txt)

# Frontier backend: mysql (default) or postgres. Selected with `DB=postgres
# ./run.sh`. Everything downstream keys off this — the compose service brought
# up, the in-container storage driver+DSN injected into the config by the
# worker entrypoint, the host-side DSN the assert tool connects with, and the
# assert -driver. Default is mysql, so the original run is unchanged.
#
# Only the selected DB service is ever `up`'d, and the ardvark containers run
# with --no-deps, so the other backend's container is never created.
DB="${DB:-mysql}"
case "$DB" in
  mysql)
    DB_SERVICE=mysql
    DB_DRIVER=mysql
    # In-container DSN (compose service host); host DSN (published port) for assert.
    DB_DSN="ardvark:ardvark@tcp(mysql:3306)/ardvark?charset=utf8mb4&parseTime=True&loc=UTC"
    DSN_HOST="ardvark:ardvark@tcp(127.0.0.1:13399)/ardvark?charset=utf8mb4&parseTime=True&loc=UTC"
    ;;
  postgres|postgresql)
    DB=postgres
    DB_SERVICE=postgres
    DB_DRIVER=postgres
    DB_DSN="host=postgres user=ardvark password=ardvark dbname=ardvark port=5432 sslmode=disable TimeZone=UTC"
    DSN_HOST="host=127.0.0.1 user=ardvark password=ardvark dbname=ardvark port=15499 sslmode=disable TimeZone=UTC"
    ;;
  *)
    echo "unknown DB=$DB (want mysql or postgres)"; exit 2 ;;
esac
# Exported so `-e DB_DRIVER -e DB_DSN` passthrough on `docker compose run` picks
# them up for the worker entrypoint's config templating.
export DB_DRIVER DB_DSN

# Track overall result so a failing scenario is reported but does not abort the
# rest of the run (a failing scenario is a valid, reportable outcome). Plain
# vars (not an associative array) to stay robust under `set -u`.
RESULT_A=1; RESULT_B=1; RESULT_C=1; RESULT_D=1

log() { printf '\n\033[1;36m== %s ==\033[0m\n' "$*"; }
sub() { printf '\033[0;90m-- %s\033[0m\n' "$*"; }

cleanup() {
  log "CLEANUP"
  # Remove any one-off run containers we created (names all start with sm-).
  docker ps -aq --filter "name=^sm-" | while read -r id; do docker rm -f "$id" >/dev/null 2>&1 || true; done
  $COMPOSE down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

db_q() {
  # Run a single read-only SQL statement against the active backend, returning
  # bare values (no headers). All queries here are portable across both drivers.
  if [ "$DB" = postgres ]; then
    $COMPOSE exec -T postgres psql -U ardvark -d ardvark -tA -c "$1" 2>/dev/null
  else
    $COMPOSE exec -T mysql mysql -uardvark -pardvark ardvark -N -B -e "$1" 2>/dev/null
  fi
}

reset_db() {
  sub "resetting database schema ($DB)"
  if [ "$DB" = postgres ]; then
    # ardvark is the database owner, so it can drop/recreate schema public.
    $COMPOSE exec -T postgres psql -U ardvark -d ardvark \
      -c "DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public; GRANT ALL ON SCHEMA public TO ardvark;" >/dev/null 2>&1
  else
    $COMPOSE exec -T mysql mysql -uroot -proot -e \
      "DROP DATABASE IF EXISTS ardvark; CREATE DATABASE ardvark; GRANT ALL ON ardvark.* TO 'ardvark'@'%'; FLUSH PRIVILEGES;" 2>/dev/null
  fi
  sub "running ardvark migrate"
  $COMPOSE run --rm --no-deps -e "WORKER_INDEX=migrate" -e DB_DRIVER -e DB_DSN ardvark migrate >/dev/null || {
    echo "migrate failed"; return 1; }
}

start_detached() { # NAME INDEX CMD...
  local name="$1" index="$2"; shift 2
  $COMPOSE run -d --no-deps --name "$name" -e "WORKER_INDEX=$index" -e "WORKER_COUNT=$WORKERS" -e DB_DRIVER -e DB_DSN ardvark "$@" >/dev/null
}

wait_containers() { # TIMEOUT_SECONDS NAME...
  local deadline=$(( $(date +%s) + $1 )); shift
  local names=("$@")
  while :; do
    local running=0
    for n in "${names[@]}"; do
      if [ "$(docker inspect -f '{{.State.Running}}' "$n" 2>/dev/null)" = "true" ]; then
        running=$((running+1))
      fi
    done
    [ "$running" -eq 0 ] && return 0
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "TIMEOUT waiting for: ${names[*]} ($running still running)"
      return 1
    fi
    sleep 2
  done
}

exit_code_of() { docker inspect -f '{{.State.ExitCode}}' "$1" 2>/dev/null; }

report_exit_codes() { # NAME...
  local ok=1
  for n in "$@"; do
    local c; c="$(exit_code_of "$n")"
    printf '  %-10s exit=%s\n' "$n" "$c"
    [ "$c" = "0" ] || ok=0
  done
  return $((1-ok))
}

diagnostics() {
  sub "DIAGNOSTICS: docker ps"
  docker ps --filter "name=^sm-" --format '  {{.Names}}\t{{.Status}}'
  sub "DIAGNOSTICS: frontier state"
  db_q "SELECT status, COUNT(*) FROM frontier_items GROUP BY status" || true
}

clear_logs() { rm -f "$HERE"/logs/*.jsonl 2>/dev/null; mkdir -p "$HERE/logs"; }

# clear_fleet removes any worker containers left over from a previous scenario.
# `docker compose run` containers persist after exit, so without this the next
# scenario's `run --name sm-wN` would fail with a name conflict and silently
# never start that worker. Only sm-* names are touched (never the mysql/fixture
# services, nor any container this harness did not create).
clear_fleet() {
  docker ps -aq --filter "name=^sm-" | while read -r id; do
    docker rm -f "$id" >/dev/null 2>&1 || true
  done
}

# start_fleet: seeder = worker 0 (crawl, seeds all shards + drains shard 0),
# workers 1..9 = plain `work`. Workers 1..9 are started only AFTER the seeder
# has populated the frontier, so they never hit the empty-frontier early exit.
start_fleet() {
  clear_fleet
  # The seeder is worker 0: it runs `crawl` (which seeds every shard, then
  # drains its own shard 0). `crawl` has no --worker flag, so its shard comes
  # from crawler.worker in the config, injected by the entrypoint from
  # WORKER_INDEX=0 / WORKER_COUNT=$WORKERS (set by start_detached).
  sub "starting seeder/worker 0 (crawl)"
  start_detached "sm-w0" 0 "${SEED_CMD[@]}"

  # Wait for PENDING work to appear before starting peers. Counting pending
  # (not total rows) is robust for the re-crawl scenario, where the DB already
  # holds ~21 DONE rows from a prior run: peers must not start until the seeder
  # has re-activated seeds to pending, or a peer could observe a globally-empty
  # (0 pending, 0 in_flight) frontier and terminate immediately.
  sub "waiting for the seeder to populate pending work"
  local deadline=$(( $(date +%s) + 60 ))
  while :; do
    local n; n="$(db_q "SELECT COUNT(*) FROM frontier_items WHERE status IN ('pending','in_flight')")"
    [ -n "$n" ] && [ "$n" -ge 15 ] 2>/dev/null && break
    if [ "$(date +%s)" -ge "$deadline" ]; then echo "seeding did not populate pending work"; return 1; fi
    sleep 1
  done
  sub "frontier has pending work; starting workers 1..$((WORKERS-1))"
  # Start the peers in parallel. Sequential `docker compose run -d` takes
  # ~0.5s each (~5s for nine), long enough that a fast worker's whole shard can
  # drain before they are all up — which is exactly what defeated the
  # kill-and-reclaim timing. Launching them concurrently gets the fleet running
  # within ~1s so scenario C can still catch worker 6 mid-flight.
  for i in $(seq 1 $((WORKERS-1))); do
    start_detached "sm-w$i" "$i" work --worker "$i/$WORKERS" &
  done
  wait
}

fleet_names() { local a=(); for i in $(seq 0 $((WORKERS-1))); do a+=("sm-w$i"); done; echo "${a[@]}"; }

# ---------------------------------------------------------------------------
log "SETUP: generate certs"
( cd "$REPO_ROOT" && go run ./tools/smoketest/gencerts "$HERE/certs" )

log "SETUP: build images"
$COMPOSE build

log "SETUP: start $DB_SERVICE + fixture"
$COMPOSE up -d "$DB_SERVICE" fixture
sub "waiting for $DB_SERVICE + fixture health"
for i in $(seq 1 60); do
  mh="$(docker inspect -f '{{.State.Health.Status}}' "$($COMPOSE ps -q "$DB_SERVICE")" 2>/dev/null)"
  fh="$(docker inspect -f '{{.State.Health.Status}}' "$($COMPOSE ps -q fixture)" 2>/dev/null)"
  [ "$mh" = "healthy" ] && [ "$fh" = "healthy" ] && break
  sleep 2
done
echo "  $DB_SERVICE=$mh fixture=$fh"

build_assert() { ( cd "$REPO_ROOT" && go build -o "$HERE/bin/assert" ./tools/smoketest/assert ); }
log "SETUP: build assert tool"; mkdir -p "$HERE/bin"; build_assert
ASSERT="$HERE/bin/assert"

# ===========================================================================
log "SCENARIO A: full cooperative drain"
reset_db || true
clear_logs
start_fleet || true
sub "waiting for fleet to drain (timeout 600s)"
# shellcheck disable=SC2046
if wait_containers 600 $(fleet_names); then
  sub "fleet exit codes:"
  report_exit_codes $(fleet_names); A_EXIT=$?
else
  diagnostics; A_EXIT=1
fi
"$ASSERT" a -driver "$DB_DRIVER" -dsn "$DSN_HOST" -logs "$HERE/logs" -workers "$WORKERS"; A_ASSERT=$?
RESULT_A=$(( A_EXIT + A_ASSERT ))

# --- Scenario B reuses Scenario A's logs -----------------------------------
log "SCENARIO B: shard partition (assert on scenario A logs)"
"$ASSERT" b -driver "$DB_DRIVER" -dsn "$DSN_HOST" -logs "$HERE/logs" -workers "$WORKERS"; RESULT_B=$?

# --- Scenario D: re-crawl budget (same DB as A, fresh logs) -----------------
log "SCENARIO D: re-crawl page-budget re-activation"
clear_logs
sub "re-running fleet (no --force) against scenario A's populated DB"
start_fleet || true
# shellcheck disable=SC2046
if wait_containers 600 $(fleet_names); then
  sub "fleet exit codes:"; report_exit_codes $(fleet_names); D_EXIT=$?
else
  diagnostics; D_EXIT=1
fi
"$ASSERT" d -driver "$DB_DRIVER" -dsn "$DSN_HOST" -logs "$HERE/logs" -workers "$WORKERS"; D_ASSERT=$?
RESULT_D=$(( D_EXIT + D_ASSERT ))

# --- Scenario C: kill-and-reclaim (fresh DB) -------------------------------
log "SCENARIO C: kill-and-reclaim"
reset_db || true
clear_logs
start_fleet || true
KILL_TARGET="sm-w6"   # worker 6 owns shard %10==6 (sites 2,4,6,20) — a busy shard
# Deterministic mid-flight kill: poll until worker 6's shard actually holds an
# in_flight item, then SIGKILL immediately. A fixed sleep is unreliable — the
# whole crawl can finish in a few seconds, so a timed kill may land after the
# worker already completed and exited (nothing to reclaim). The fixture's
# response latency keeps the item in_flight long enough that it is still leased
# when the kill lands.
sub "waiting for worker 6 to hold an in_flight item, then SIGKILL"
kill_deadline=$(( $(date +%s) + 30 ))
killed=0
while [ "$(date +%s)" -lt "$kill_deadline" ]; do
  inflight6="$(db_q "SELECT COUNT(*) FROM frontier_items WHERE status='in_flight' AND host_shard % 10 = 6")"
  if [ -n "$inflight6" ] && [ "$inflight6" -ge 1 ] 2>/dev/null; then
    if docker kill "$KILL_TARGET" >/dev/null 2>&1; then
      echo "  killed $KILL_TARGET (SIGKILL) while shard-6 in_flight=$inflight6"
      killed=1
    fi
    break
  fi
  sleep 0.3
done
[ "$killed" = "1" ] || echo "  WARNING: never observed an in_flight item on shard 6 to kill"
sub "shard-6 frontier state immediately after kill (stranded in_flight/pending):"
db_q "SELECT status, COUNT(*) FROM frontier_items WHERE host_shard % 10 = 6 GROUP BY status" | sed 's/^/    /'
sub "waiting past leaseSeconds(15) + reclaim interval(30) before restart"
sleep 50
sub "restarting worker 6 (same index) as sm-w6b"
start_detached "sm-w6b" 6 work --worker "6/$WORKERS"
# The fleet now consists of w0..w5, w7..w9, plus the restarted w6b.
C_NAMES=(sm-w0 sm-w1 sm-w2 sm-w3 sm-w4 sm-w5 sm-w7 sm-w8 sm-w9 sm-w6b)
sub "waiting for remaining workers + restarted worker to drain (timeout 600s)"
if wait_containers 600 "${C_NAMES[@]}"; then
  sub "exit codes (sm-w6 = the killed instance, expected 137):"
  report_exit_codes "${C_NAMES[@]}"; C_EXIT=$?
  echo "  killed instance sm-w6 exit=$(exit_code_of sm-w6)"
else
  diagnostics; C_EXIT=1
fi
"$ASSERT" c -driver "$DB_DRIVER" -dsn "$DSN_HOST" -logs "$HERE/logs" -workers "$WORKERS"; C_ASSERT=$?
RESULT_C=$(( C_EXIT + C_ASSERT ))

# ===========================================================================
log "SUMMARY"
overall=0
for pair in "a:$RESULT_A" "b:$RESULT_B" "c:$RESULT_C" "d:$RESULT_D"; do
  s="${pair%%:*}"; v="${pair#*:}"
  if [ "$v" = "0" ]; then
    printf '  scenario %s: \033[0;32mPASS\033[0m\n' "$s"
  else
    printf '  scenario %s: \033[0;31mFAIL\033[0m\n' "$s"; overall=1
  fi
done
exit $overall
