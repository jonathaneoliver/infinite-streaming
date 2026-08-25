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
# about to start accepts queries, we (re-)apply the canonical schema in the
# background. 01-schema.sql is fully idempotent (every statement is
# CREATE/ALTER ... IF [NOT] EXISTS), so it CREATES on an empty database and
# UPGRADES an existing one in place: no data loss, no external database to copy
# from. The container repairs itself.
SCHEMA=/docker-entrypoint-initdb.d/01-schema.sql

(
  # Wait for the very server we exec below to come up.
  until clickhouse-client -q "SELECT 1" >/dev/null 2>&1; do sleep 1; done
  if [ -f "$SCHEMA" ]; then
    echo "clickhouse self-heal: applying analytics schema (create-or-upgrade)"
    if clickhouse-client --multiquery < "$SCHEMA"; then
      echo "clickhouse self-heal: schema up to date"
    else
      echo "clickhouse self-heal: WARNING — schema apply reported errors" >&2
    fi
  fi
) &

# Hand off to the stock entrypoint (runs first-boot init.d if the volume is
# empty, then execs clickhouse-server as PID-ish foreground).
exec /entrypoint.sh "$@"
