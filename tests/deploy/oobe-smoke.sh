#!/usr/bin/env bash
# oobe-smoke.sh — post-deploy smoke test for a fresh (OOBE) install.
#
# Why: bringing the stack up is NOT enough to prove a clean install works. A
# ClickHouse analytics schema drift — e.g. session_events missing a column the
# dashboard SELECTs (control_revision, labels, app_*) — is invisible to a
# boot/health check: every container comes up green and video plays fine. But
# the dashboard's timeseries query for the events/network streams then errors
# ("backfill failed: ... Unknown expression identifier 'control_revision'"),
# and because the backfill loop bails on the first error, BOTH the Network Log
# and PlayLog panels render empty. This exercises that exact query so a broken
# clean install FAILS the deploy loudly instead of shipping a dead dashboard.
#
# Works with zero data: the events/network backfill SELECTs the schema's
# columns regardless of matching rows, so a missing column errors even for a
# nonexistent player_id.
#
# Usage: oobe-smoke.sh <base_url>   (e.g. https://localhost:26000)
set -euo pipefail
BASE="${1:?base url required, e.g. https://localhost:26000}"

# Wait for the analytics read API (nginx -> forwarder) to answer.
ready=""
for _ in $(seq 1 45); do
  if curl -sk --max-time 5 "${BASE}/analytics/api/v2/plays?limit=1" >/dev/null 2>&1; then
    ready=1
    break
  fi
  sleep 2
done
if [ -z "$ready" ]; then
  echo "OOBE SMOKE FAIL: analytics API never became ready at ${BASE}"
  exit 1
fi

# The dashboard's exact multi-stream timeseries query, with a dummy player so it
# needs no data. A healthy schema returns 0 rows and completes; a drifted schema
# emits a "backfill failed" error event (that's the whole bug this guards).
url="${BASE}/analytics/api/v2/timeseries"
url="${url}?player_id=00000000-0000-0000-0000-0000000005b8"
url="${url}&streams=events,network,control,avmetrics"
url="${url}&bundles=charts_minimal,lanes_v1,panel_v1,session_details,network"
url="${url}&from=2020-01-01T00:00:00.000Z"
resp="$(curl -sk --max-time 25 "$url" 2>/dev/null || true)"

if printf '%s' "$resp" | grep -qi "backfill failed"; then
  echo "OOBE SMOKE FAIL: dashboard timeseries backfill errored on a clean install"
  echo "  (a ClickHouse schema drift breaks the Network Log / PlayLog panels):"
  printf '%s\n' "$resp" | grep -i "backfill failed" | head -1 | cut -c1-320
  exit 1
fi
if ! printf '%s' "$resp" | grep -q '"columns"'; then
  echo "OOBE SMOKE FAIL: timeseries returned no schema/meta frame (endpoint unhealthy)"
  printf '%s\n' "$resp" | head -3
  exit 1
fi
echo "OOBE smoke OK: dashboard timeseries events+network path healthy (no schema drift)"
