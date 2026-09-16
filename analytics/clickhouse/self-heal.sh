#!/bin/sh
# Self-healing analytics schema for ClickHouse — used as the container entrypoint.
#
# Why this exists: the stock clickhouse-server entrypoint runs
# /docker-entrypoint-initdb.d/*.sql ONLY when the data directory is empty (the
# first-ever boot). A container brought up on an EXISTING volume from an earlier
# version therefore never picks up schema changes (new columns), which silently
# breaks the dashboard's timeseries query (e.g. session_events missing
# control_revision -> "backfill failed" -> empty Network Log / PlayLog). init.d
# gives no warning that it skipped.
#
# So on EVERY boot — not just first init — once the server this entrypoint is
# about to start accepts queries, we (re-)apply every init.d schema file in the
# background. Each file is fully idempotent (every statement is
# CREATE/ALTER ... IF [NOT] EXISTS), so it CREATES on an empty database and
# UPGRADES an existing one in place: no data loss, no external database to copy
# from. The container repairs itself.
INITDB=/docker-entrypoint-initdb.d

(
  # Wait for the very server we exec below to come up.
  until clickhouse-client -q "SELECT 1" >/dev/null 2>&1; do sleep 1; done
  echo "clickhouse self-heal: applying analytics schema (create-or-upgrade)"
  # Apply EVERY init.d file, in the same lexical order the stock entrypoint
  # uses on first boot -- not just 01-schema.sql. Files added later
  # (04-derived.sql creates derived_tokens, which the dashboard timeseries
  # query reads) otherwise never reach an upgraded volume, and the backfill
  # fails with "Unknown table ... derived_tokens". Every file is idempotent
  # (CREATE/ALTER ... IF [NOT] EXISTS, re-runnable GRANTs).
  #
  # Each file runs separately: clickhouse-client --multiquery aborts at the
  # first failing statement, so one bad statement used to silently skip the
  # rest of the schema. Now it costs at most the rest of its own file, and
  # the failing file is named in the log.
  failed=""
  for f in "$INITDB"/*.sql; do
    [ -f "$f" ] || continue
    if clickhouse-client --multiquery < "$f"; then
      echo "clickhouse self-heal: $(basename "$f") ok"
    else
      failed="$failed $(basename "$f")"
      echo "clickhouse self-heal: WARNING — $(basename "$f") reported errors" >&2
    fi
  done
  if [ -z "$failed" ]; then
    echo "clickhouse self-heal: schema up to date"
  else
    echo "clickhouse self-heal: WARNING — schema apply reported errors in:$failed" >&2
  fi
) &

# Hand off to the stock entrypoint (runs first-boot init.d if the volume is
# empty, then execs clickhouse-server as PID-ish foreground).
exec /entrypoint.sh "$@"
