#!/bin/sh
# Entrypoint for the ardvark worker/seeder containers in the smoke test.
#
# The smoke test mounts ONE shared config template (/config/ardvark.json) into
# every container. This wrapper personalizes it per container from environment
# variables set by the harness, then exec's ardvark:
#
#   __LOGFILE__       -> /logs/worker-${WORKER_INDEX}.jsonl  (per-worker event log)
#   __WORKER_INDEX__  -> numeric worker index  (crawler.worker.index)
#   __WORKER_COUNT__  -> total worker count    (crawler.worker.count)
#
# Injecting crawler.worker into the config matters for the SEEDER, which runs
# `ardvark crawl` — unlike `ardvark work`, crawl has no --worker flag, so its
# shard can only come from config. The `work` workers additionally pass
# --worker i/n on the command line (which overrides config identically), per
# the harness design.
#
# WORKER_INDEX may be non-numeric (e.g. "migrate" for the one-off migrate
# container) purely to name its log file; in that case the injected numeric
# shard falls back to 0/1 so the config still validates.
set -eu

RAW_INDEX="${WORKER_INDEX:-x}"
COUNT="${WORKER_COUNT:-1}"

# Numeric shard index: use WORKER_INDEX when it is a plain number, else 0.
case "$RAW_INDEX" in
  ''|*[!0-9]*) IDX=0 ;;
  *)           IDX="$RAW_INDEX" ;;
esac
case "$COUNT" in
  ''|*[!0-9]*) COUNT=1 ;;
esac
# Keep index < count so config validation (crawler.worker) passes.
[ "$IDX" -lt "$COUNT" ] 2>/dev/null || IDX=0

CFG_TEMPLATE="${ARDVARK_CONFIG_TEMPLATE:-/config/ardvark.json}"
CFG_OUT="/tmp/ardvark.json"

# Storage backend is injected too, so the same template drives both the mysql
# and postgres runs (run.sh's DB switch). Defaults reproduce the original
# mysql wiring exactly, so an invocation that sets neither var is unchanged.
DB_DRIVER="${DB_DRIVER:-mysql}"
DB_DSN="${DB_DSN:-ardvark:ardvark@tcp(mysql:3306)/ardvark?charset=utf8mb4&parseTime=True&loc=UTC}"

# Escape characters that are special in a sed replacement (& = whole match,
# # = our delimiter, \ = escape). The mysql DSN contains '&', the postgres
# keyword DSN contains spaces and '=' (both sed-safe), so only &/#/\ matter.
sed_escape() { printf '%s' "$1" | sed -e 's/[&#\\]/\\&/g'; }
DB_DRIVER_E="$(sed_escape "$DB_DRIVER")"
DB_DSN_E="$(sed_escape "$DB_DSN")"

sed \
  -e "s#__LOGFILE__#/logs/worker-${RAW_INDEX}.jsonl#" \
  -e "s#__WORKER_INDEX__#${IDX}#" \
  -e "s#__WORKER_COUNT__#${COUNT}#" \
  -e "s#__DB_DRIVER__#${DB_DRIVER_E}#" \
  -e "s#__DB_DSN__#${DB_DSN_E}#" \
  "$CFG_TEMPLATE" > "$CFG_OUT"

exec ardvark --config "$CFG_OUT" "$@"
