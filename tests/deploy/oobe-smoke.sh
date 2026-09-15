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

# Per-run response file. curl leaves an existing -o file untouched when it
# can't connect, so a fixed path could hand this run a previous run's body.
PLAYS_JSON="$(mktemp)"
trap 'rm -f "$PLAYS_JSON"' EXIT

# Wait until the analytics read path is ready: nginx -> forwarder -> ClickHouse.
#
# Ready means the Sessions page's plays query either SUCCEEDS (2xx), or reaches
# ClickHouse and is REJECTED by it (a JSON body carrying a DB::Exception, e.g. a
# missing column on an upgraded volume). The latter is a real failure, reported
# below without further waiting.
#
# Everything else just means "not ready yet", so keep waiting:
#   - no response at all
#   - an nginx / HTML error page
#   - a port answered by something other than the forwarder (plain `curl -sk`
#     used to count a stray "404 page not found" as ready)
#   - the forwarder answering that it can't REACH ClickHouse, which is what a
#     fresh install returns while ClickHouse is still on its first boot. Counting
#     that as ready failed every clean `make test-deploy-oobe`.
#
# 90 x 2s = 3 minutes, enough for a first-boot ClickHouse plus init.d.
ready=""
plays_code=""
for _ in $(seq 1 90); do
  : > "$PLAYS_JSON"
  plays_code="$(curl -sk -o "$PLAYS_JSON" -w '%{http_code}' --max-time 10 "${BASE}/analytics/api/v2/plays?limit=1" 2>/dev/null || true)"
  body="$(head -c 4000 "$PLAYS_JSON" 2>/dev/null || true)"
  case "$plays_code" in
    2??) ready=1; break ;;
  esac
  case "$body" in
    \{*DB::Exception*) ready=1; break ;;
  esac
  sleep 2
done
if [ -z "$ready" ]; then
  echo "OOBE SMOKE FAIL: analytics read path (nginx -> forwarder -> ClickHouse) never became ready at ${BASE}"
  printf '  last response: HTTP %s %s\n' "${plays_code:-none}" "$(head -c 200 "$PLAYS_JSON" 2>/dev/null)"
  exit 1
fi

# The Sessions page's plays query must succeed. It selects columns an upgraded
# volume can lack (frames_dropped, on a v2.0.0 volume before the upgrade-parity
# ALTERs), which ClickHouse rejects with an exception and the forwarder turns
# into a 502.
case "$plays_code" in
  2??) ;;
  *)
    echo "OOBE SMOKE FAIL: /analytics/api/v2/plays returned ${plays_code} (the Sessions page is broken)"
    head -c 320 "$PLAYS_JSON" 2>/dev/null; echo
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
