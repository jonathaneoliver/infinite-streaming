#!/usr/bin/env bash
# schema-upgrade-check.sh — prove the analytics schema upgrades an existing
# volume to exactly what a fresh install gets.
#
# Why: init.d runs only on a fresh ClickHouse volume. Existing volumes are
# upgraded by self-heal.sh re-applying the init.d files on every boot, and that
# only works if every change is expressible as CREATE/ALTER ... IF [NOT]
# EXISTS. Three ways it broke going from v2.0.0 to v2.1.0, all invisible to a
# fresh-install test:
#   - a column added twice inside one ALTER aborted the whole apply;
#   - columns added only inside CREATE TABLE never reached an existing table;
#   - new init.d files (04-derived.sql, ...) were never applied at all.
#
# This builds two throwaway ClickHouse servers:
#   fresh    — every current init.d file applied to an empty volume, in order
#              (what first boot does)
#   upgraded — the BASELINE ref's init.d files first, then the current files
#              applied the way self-heal.sh does (per file, --multiquery)
# then applies the current files to the upgraded server a second time
# (idempotency), and fails if any table or column in fresh is missing from
# upgraded. Columns that exist only in upgraded are legacy leftovers from
# renames and are listed but allowed.
#
# Usage: tests/deploy/schema-upgrade-check.sh [baseline-ref]   (default v2.0.0)
# Needs: docker, git. Run from anywhere inside the repo.
set -euo pipefail

BASELINE="${1:-v2.0.0}"
REPO="$(git rev-parse --show-toplevel)"
CURRENT="$REPO/analytics/clickhouse/init.d"
IMAGE="$(grep -oE 'clickhouse/clickhouse-server:[^ "]+' "$REPO/docker-compose.yml" | head -1)"
WORK="$(mktemp -d)"
FRESH="schema-check-fresh-$$"
UPGRADED="schema-check-upgraded-$$"

cleanup() {
  docker rm -f "$FRESH" "$UPGRADED" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

mkdir -p "$WORK/baseline"
for f in $(git -C "$REPO" ls-tree --name-only "$BASELINE" analytics/clickhouse/init.d/ | grep '\.sql$'); do
  git -C "$REPO" show "$BASELINE:$f" > "$WORK/baseline/$(basename "$f")"
done

# No CLICKHOUSE_DB: with it set, the stock entrypoint first runs a temporary
# server to create the database, then restarts into the real one, and a
# readiness probe can catch the temporary server and race the restart. The
# schema creates the database itself. ACCESS_MANAGEMENT matches compose;
# 02-llm-reader.sql creates a user.
start() {
  docker run -d --name "$1" \
    -e CLICKHOUSE_DEFAULT_ACCESS_MANAGEMENT=1 \
    "$IMAGE" >/dev/null
  local ok=0
  for _ in $(seq 1 90); do
    if docker exec "$1" clickhouse-client -q "SELECT 1" >/dev/null 2>&1; then
      ok=$((ok + 1))
      [ "$ok" -ge 3 ] && return 0   # stable, not a server mid-restart
    else
      ok=0
    fi
    sleep 1
  done
  echo "FAIL: $1 never became stably ready" >&2
  return 1
}

# apply <container> <dir> <label>: each .sql in lexical order, one
# --multiquery per file (mirrors self-heal.sh). Returns non-zero if any file
# failed, naming each one.
apply() {
  local c="$1" dir="$2" label="$3" rc=0
  for f in "$dir"/*.sql; do
    if ! docker exec -i "$c" clickhouse-client --multiquery < "$f" > "$WORK/$label-$(basename "$f").log" 2>&1; then
      echo "  $label: $(basename "$f") FAILED: $(grep -m1 -oE 'Code: [0-9]+\. DB::Exception: [^(]*' "$WORK/$label-$(basename "$f").log" || head -1 "$WORK/$label-$(basename "$f").log")"
      rc=1
    fi
  done
  return $rc
}

snapshot() {
  docker exec "$1" clickhouse-client -q \
    "SELECT concat(table, '.', name) FROM system.columns WHERE database = 'infinite_streaming' ORDER BY 1 FORMAT TSV" \
    | LC_ALL=C sort
}

echo "schema-upgrade-check: $BASELINE -> working tree ($IMAGE)"
start "$FRESH" & p1=$!
start "$UPGRADED" & p2=$!
wait "$p1" && wait "$p2" || { echo "schema-upgrade-check FAILED: ClickHouse did not start"; exit 1; }

status=0
echo "fresh install:"
apply "$FRESH" "$CURRENT" fresh && echo "  all files applied"
echo "upgrade: baseline $BASELINE"
apply "$UPGRADED" "$WORK/baseline" baseline && echo "  all files applied"
echo "upgrade: current files over $BASELINE (as self-heal.sh)"
apply "$UPGRADED" "$CURRENT" upgrade && echo "  all files applied" || status=1
echo "upgrade: current files again (idempotency)"
apply "$UPGRADED" "$CURRENT" reapply && echo "  all files applied" || status=1

snapshot "$FRESH" > "$WORK/fresh.tsv"
snapshot "$UPGRADED" > "$WORK/upgraded.tsv"
missing="$(LC_ALL=C comm -23 "$WORK/fresh.tsv" "$WORK/upgraded.tsv")"
legacy="$(LC_ALL=C comm -13 "$WORK/fresh.tsv" "$WORK/upgraded.tsv")"

echo "columns: fresh=$(wc -l < "$WORK/fresh.tsv" | tr -d ' ') upgraded=$(wc -l < "$WORK/upgraded.tsv" | tr -d ' ')"
if [ -n "$legacy" ]; then
  echo "legacy columns kept on upgrade (allowed):"
  printf '%s\n' "$legacy" | sed 's/^/  /'
fi
if [ -n "$missing" ]; then
  echo "FAIL: in a fresh install but missing after upgrade:"
  printf '%s\n' "$missing" | sed 's/^/  /'
  status=1
fi

if [ "$status" -eq 0 ]; then
  echo "schema-upgrade-check OK: $BASELINE upgrades cleanly to the working-tree schema"
else
  echo "schema-upgrade-check FAILED"
fi
exit "$status"
