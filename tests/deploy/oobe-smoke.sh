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
#
# "Answered" means the FORWARDER replied, which it always does with a JSON body
# -- `{"items":...}` when healthy, `{"detail":...}` on a query error. Any HTTP
# response used to count (plain `curl -sk` exits 0 on a 404), so this passed
# while an unrelated host DNAT on :8080 answered in the forwarder's place with
# "404 page not found", and while nginx itself returned an error page. A
# forwarder query error (e.g. a 502 on a missing column) still counts as
# ready here, so the checks below can report the specific schema failure
# instead of a generic "never ready".
ready=""
for _ in $(seq 1 45); do
  body="$(curl -sk --max-time 5 "${BASE}/analytics/api/v2/plays?limit=1" 2>/dev/null || true)"
  case "$body" in
    \{*) ready=1; break ;;
  esac
  sleep 2
done
if [ -z "$ready" ]; then
  echo "OOBE SMOKE FAIL: analytics API never answered with JSON at ${BASE}"
  printf '  last response: %s\n' "$(printf '%s' "$body" | head -c 160)"
  exit 1
fi

# The Sessions page's plays query must succeed. It selects columns that an
# upgraded volume can lack (frames_dropped, on a v2.0.0 volume before the
# upgrade-parity ALTERs), which fails with a 502 and a JSON detail.
plays_code="$(curl -sk -o /tmp/oobe-smoke-plays.json -w '%{http_code}' --max-time 15 "${BASE}/analytics/api/v2/plays?limit=1" 2>/dev/null || true)"
case "$plays_code" in
  2??) ;;
  *)
    echo "OOBE SMOKE FAIL: /analytics/api/v2/plays returned ${plays_code} (the Sessions page is broken)"
    head -c 320 /tmp/oobe-smoke-plays.json 2>/dev/null; echo
    exit 1
    ;;
esac

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
echo "OOBE smoke OK: plays query + dashboard timeseries events+network path healthy (no schema drift)"
