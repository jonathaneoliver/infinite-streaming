#!/usr/bin/env bash
# farm.sh — one entry point to setup / reset / shutdown / inspect the Appium
# Device Farm (the iOS-sim pool on :4723) + recover its common failure modes.
#
# Wraps run.sh (DF server) + boot-pool.sh (boot sims, install app, warm WDA) and
# adds the recovery steps we learned the hard way:
#   - pkill'd runs leave sims stuck busy=true → `unblock` clears them (the
#     device-farm never runs reapDeviceFarm on an abrupt kill).
#   - a deleted/stray sim lingers in the DF's in-memory roster → only a DF
#     RESTART flushes it (`reset`).
#   - "App … unknown" from Appium while simctl launches the app fine = stale WDA
#     state → `reset` (fresh sims + fresh WDA).
#   - never kill the real-iPhone `ios tunnel` (RemoteXPC) or the `appium-mcp`
#     server — this script preserves both.
#
# Usage:  tools/appium-device-farm/farm.sh <status|setup|reset|shutdown|unblock>
#
# Env (passed through to boot-pool.sh):
#   DF_PORT        DF/Appium port                 (default 4723)
#   DF_POOL_MATCH  sim-name substring             (default "Fleet")
#   DF_POOL_COUNT  how many sims                   (default 4)
#   DF_BUILD_APP   reset: rebuild+install app (1) or reuse installed (0)
#                                                  (default 1 for reset, 0 for setup)
#   DF_WARM_WDA    warm WDA (1) or skip (0)        (default 1)
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
DF_PORT="${DF_PORT:-4723}"
DF_POOL_MATCH="${DF_POOL_MATCH:-Fleet}"
DF_POOL_COUNT="${DF_POOL_COUNT:-4}"
DF_WARM_WDA="${DF_WARM_WDA:-1}"
BASE="http://localhost:${DF_PORT}"
DF_LOG="${DF_LOG:-/tmp/appium-df-${DF_PORT}.log}"
# The go-proxy runs on the REMOTE test-dev deploy (not localhost) — proxy-session
# release below hits it. Reads HARNESS_BASE_URL, else the test-dev default.
PROXY_URL="${HARNESS_BASE_URL:-https://dev.jeoliver.com:21000}"
APP_BUNDLE="${APP_BUNDLE:-com.jeoliver.InfiniteStreamPlayer}"

# Verbose: `farm.sh <cmd> -v` (or FARM_VERBOSE=1) → timestamped steps + live
# stream of the slow xcodebuild app-compile log so you can see WHY it's slow.
V="${FARM_VERBOSE:-0}"
case "${2:-}" in -v|--verbose) V=1 ;; esac
log()  { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*"; }
APP_BUILD_LOG="${DF_APP_DERIVED:-/tmp/iphone-sim-build}"; APP_BUILD_LOG="/tmp/df-app-build.log"

df_up()       { curl -s -m3 "${BASE}/status" >/dev/null 2>&1; }
fleet_udids() { xcrun simctl list devices 2>/dev/null | grep -F "$DF_POOL_MATCH" | grep -oE '[0-9A-F]{8}-[0-9A-F-]{27}'; }

# Kill the DF stack but PRESERVE the real-iPhone tunnel + the MCP server.
kill_farm() {
	pkill -f 'appium --config'              2>/dev/null && echo "  killed DF appium"        || true
	pkill -f 'appium --port 4799'           2>/dev/null && echo "  killed off-farm appium"  || true
	pkill -f 'xcodebuild.*WebDriverAgent'   2>/dev/null && echo "  killed WDA builds"       || true
	pkill -f 'harness char matrix'          2>/dev/null || true
	pkill -f 'TestCharMatrixFleet'          2>/dev/null || true
	# NOT killed: 'ios tunnel start' (real-iPhone RemoteXPC) and 'appium-mcp'.
	sleep 2
}

start_df() {
	if df_up; then echo "  DF already up on :${DF_PORT}"; return; fi
	echo "  starting DF (run.sh) on :${DF_PORT}…"
	DF_LOG="$DF_LOG" nohup "${SCRIPT_DIR}/run.sh" >/tmp/farm-df.out 2>&1 &
	for _ in $(seq 1 40); do df_up && { echo "  DF ready"; return; }; sleep 2; done
	echo "  !! DF did not become ready — see $DF_LOG" >&2; return 1
}

shutdown_sims() {
	for u in $(fleet_udids); do
		xcrun simctl shutdown "$u" 2>/dev/null && echo "  shutdown ${u:0:8}" || true
	done
}

# Booted iOS sims that are NOT in the pool poison the DF roster: `ipad-sim`
# allocation can land on one, but the app is only installed on the pool sims →
# "App … unknown". Shut every non-pool iPhone/iPad sim down so the DF roster is
# exactly the pool. (Fleet sims are re-booted by boot-pool right after.)
shutdown_foreign_sims() {
	keep=$(fleet_udids | tr '\n' '|' | sed 's/|$//')
	[ -z "$keep" ] && keep='__none__'
	xcrun simctl list devices booted 2>/dev/null \
		| grep -iE 'iphone|ipad' \
		| grep -oE '[0-9A-F]{8}-[0-9A-F-]{27}' \
		| while read -r u; do
			printf '%s' "$u" | grep -qE "$keep" && continue
			xcrun simctl shutdown "$u" 2>/dev/null && log "  shut down FOREIGN sim ${u:0:8} (no app — would poison the DF pool)" || true
		done
}

# Clear sims left busy=true by a pkill'd run (device-farm skips reapDeviceFarm on kill).
unblock_stuck() {
	df_up || { echo "  DF not up — nothing to unblock"; return; }
	DF_BASE="$BASE" python3 - <<'PY'
import os, json, urllib.request
b = os.environ["DF_BASE"]
try:
    ds = json.load(urllib.request.urlopen(b + "/device-farm/api/device", timeout=8))
except Exception as e:
    print("  roster fetch failed:", e); raise SystemExit(0)
ds = ds if isinstance(ds, list) else ds.get("devices", [])
n = 0
for d in ds:
    if d.get("busy"):
        body = json.dumps({"udid": d.get("udid"), "host": d.get("host", "")}).encode()
        try:
            urllib.request.urlopen(urllib.request.Request(
                b + "/device-farm/api/unblock", data=body,
                headers={"Content-Type": "application/json"}, method="POST"), timeout=8)
            print("  unblocked", d.get("name")); n += 1
        except Exception as e:
            print("  unblock failed", d.get("name"), e)
print(f"  {n} device(s) unblocked" if n else "  no stuck devices")
PY
}

# Terminate the player app on every pool sim — stop in-flight playback before
# teardown. Harmless if the app isn't running.
terminate_apps() {
	for u in $(fleet_udids); do
		xcrun simctl terminate "$u" "$APP_BUNDLE" 2>/dev/null && echo "  stopped app on ${u:0:8}" || true
	done
}

# Release orphaned config-on-connect sessions on the go-proxy. These are the pool
# slots that, left dangling by pkill'd runs, 503 the next large fleet bootstrap.
# Read the live session map (/api/sessions) and DELETE /api/session/<player_id>.
free_proxy_sessions() {
	PROXY_URL="$PROXY_URL" python3 - <<'PY'
import os, json, ssl, urllib.request
base = os.environ["PROXY_URL"].rstrip("/")
ctx = ssl.create_default_context(); ctx.check_hostname = False; ctx.verify_mode = ssl.CERT_NONE
try:
    d = json.load(urllib.request.urlopen(base + "/api/sessions", timeout=8, context=ctx))
except Exception as e:
    print("  /api/sessions fetch failed:", e); raise SystemExit(0)
ss = d if isinstance(d, list) else d.get("sessions", d.get("players", []))
n = 0
for s in ss:
    pid = s.get("player_id") or s.get("playerId")
    if not pid:
        continue
    try:
        urllib.request.urlopen(urllib.request.Request(
            base + "/api/session/" + str(pid), method="DELETE"), timeout=8, context=ctx)
        n += 1
    except Exception as e:
        print("  release failed", str(pid)[:8], e)
print(f"  freed {n} proxy session(s)" if n else "  no active proxy sessions")
PY
}

# DELETE every non-pool iPhone/iPad SIMULATOR — the default Xcode sims that
# Xcode auto-creates per installed runtime and that the DF will boot+allocate for
# `ipad-sim` (→ "app unknown", since the app is only on the pool sims). Real
# devices and the Fleet sims are NEVER touched. Destructive, but the deleted sims
# are recreatable defaults. Restarts the DF afterward to flush its cached roster.
delete_foreign_sims() {
	keep=$(fleet_udids | tr '\n' '|' | sed 's/|$//'); [ -z "$keep" ] && keep='__none__'
	n=0
	for u in $(xcrun simctl list devices available 2>/dev/null | grep -iE 'iphone|ipad' | grep -oE '[0-9A-F]{8}-[0-9A-F-]{27}'); do
		printf '%s' "$u" | grep -qE "$keep" && continue
		xcrun simctl shutdown "$u" 2>/dev/null || true
		if xcrun simctl delete "$u" 2>/dev/null; then log "  deleted foreign sim ${u:0:8}"; n=$((n+1)); fi
	done
	log "  deleted ${n} foreign iOS sim(s)"
}

# Standalone: delete the default sims + restart the DF to flush its cached roster.
purge_foreign() { delete_foreign_sims; log "  restarting DF to flush its roster…"; kill_farm; start_df; }

# Clear the Simulator "External Displays" setting on the pool sims. When set,
# the app's video routes to a phantom external-display window and the main sim
# window shows a placeholder — playback is fine (metrics confirm), it's just
# invisible while you watch. Deletes the per-UDID SimulatorExternalDisplay key +
# bounces cfprefsd. Idempotent; takes effect on the sim's next boot.
clear_external_display() {
	plist="$HOME/Library/Preferences/com.apple.iphonesimulator.plist"
	# Simulator.app re-writes SimulatorExternalDisplay per-device ON BOOT from its
	# in-memory copy, so the delete only sticks if Simulator.app is quit first.
	osascript -e 'tell application "Simulator" to quit' 2>/dev/null || true
	sleep 1
	n=0
	for u in $(fleet_udids); do
		if /usr/libexec/PlistBuddy -c "Delete :DevicePreferences:${u}:SimulatorExternalDisplay" "$plist" 2>/dev/null; then n=$((n+1)); fi
	done
	killall cfprefsd 2>/dev/null || true
	log "  cleared external-display on ${n} pool sim(s) (Simulator quit so it sticks; relaunch to watch)"
}

boot_pool() {
	tailpid=""
	if [ "$V" = 1 ]; then
		log "boot-pool: booting sims, ${1:-0} = build_app, warm WDA — streaming ${APP_BUILD_LOG} (xcodebuild is the long pole)"
		( tail -n0 -F "$APP_BUILD_LOG" 2>/dev/null | sed 's/^/  [app-build] /' ) & tailpid=$!
	fi
	DF_BUILD_APP="${1:-0}" DF_WARM_WDA="$DF_WARM_WDA" DF_POOL_MATCH="$DF_POOL_MATCH" \
		DF_POOL_COUNT="$DF_POOL_COUNT" DF_PORT="$DF_PORT" "${SCRIPT_DIR}/boot-pool.sh"
	[ -n "$tailpid" ] && kill "$tailpid" 2>/dev/null || true
}

status() {
	echo "=== Appium Device Farm status ==="
	if df_up; then echo "DF: UP on :${DF_PORT}"; else echo "DF: DOWN on :${DF_PORT}"; fi
	echo "--- roster ---"
	if df_up; then
		DF_BASE="$BASE" python3 - <<'PY'
import os, json, urllib.request
b = os.environ["DF_BASE"]
ds = json.load(urllib.request.urlopen(b + "/device-farm/api/device", timeout=8))
ds = ds if isinstance(ds, list) else ds.get("devices", [])
for x in ds:
    if x.get("platform") in ("ios", "tvos"):
        print(f"  busy={str(x.get('busy')):5} real={str(x.get('realDevice')):5} sdk={x.get('sdk','?'):5} {x.get('name')}")
PY
	fi
	echo "--- booted ${DF_POOL_MATCH} sims + app installed? ---"
	for u in $(fleet_udids); do
		st=$(xcrun simctl list devices 2>/dev/null | grep "$u" | grep -oE 'Booted|Shutdown' | head -1)
		app=$(xcrun simctl listapps "$u" 2>/dev/null | grep -c 'com.jeoliver.InfiniteStreamPlayer' || true)
		echo "  ${u:0:8}  ${st:-?}  app=$([ "${app:-0}" -gt 0 ] && echo yes || echo NO)"
	done
	echo "--- foreign booted sims (poison the pool → 'app unknown') ---"
	keep=$(fleet_udids | tr '\n' '|' | sed 's/|$//'); [ -z "$keep" ] && keep='__none__'
	fc=$(xcrun simctl list devices booted 2>/dev/null | grep -iE 'iphone|ipad' | grep -oE '[0-9A-F]{8}-[0-9A-F-]{27}' | grep -cvE "$keep" || true)
	if [ "${fc:-0}" -gt 0 ]; then echo "  !! ${fc} non-Fleet iOS sim(s) booted → 'farm reset' clears them"; else echo "  none (pool is clean)"; fi
	echo "--- preserved processes ---"
	pgrep -fl 'ios tunnel start' >/dev/null 2>&1 && echo "  ios tunnel: UP (real iPhone)" || echo "  ios tunnel: down"
	echo "--- go-proxy active sessions (${PROXY_URL}) ---"
	PROXY_URL="$PROXY_URL" python3 - <<'PY' 2>/dev/null || echo "  (proxy unreachable)"
import os, json, ssl, urllib.request
base = os.environ["PROXY_URL"].rstrip("/")
ctx = ssl.create_default_context(); ctx.check_hostname = False; ctx.verify_mode = ssl.CERT_NONE
d = json.load(urllib.request.urlopen(base + "/api/sessions", timeout=6, context=ctx))
ss = d if isinstance(d, list) else d.get("sessions", d.get("players", []))
print(f"  {len(ss)} active session(s) — 'farm free' releases these if orphaned")
PY
}

case "${1:-status}" in
	status)   status ;;
	setup)    echo "=== farm setup ==="; clear_external_display; start_df; boot_pool "${DF_BUILD_APP:-0}"; unblock_stuck; echo "farm ready." ;;
	reset)    log "farm reset (known-good: stop apps → free sessions → purge FOREIGN sims → nuke → repave)"
	          log "stopping apps…";          terminate_apps
	          log "freeing proxy sessions…"; free_proxy_sessions
	          log "killing DF stack…";       kill_farm
	          if [ "${FARM_PURGE:-0}" = 1 ]; then
	            log "FARM_PURGE=1 → DELETING foreign default sims so the DF pool is Fleet-only…"; delete_foreign_sims
	          else
	            log "shutting foreign sims (they re-boot on allocation — set FARM_PURGE=1 to DELETE them for good)…"; shutdown_foreign_sims
	          fi
	          log "shutting Fleet sims (boot-pool re-boots them)…"; shutdown_sims
	          log "clearing external-display pref (so video renders on the main window)…"; clear_external_display
	          log "restarting DF…";           start_df
	          log "boot-pool (build_app=${DF_BUILD_APP:-1}) — the long pole; use -v to stream…"; boot_pool "${DF_BUILD_APP:-1}"
	          log "unblocking…";              unblock_stuck
	          log "farm reset complete." ;;
	shutdown) echo "=== farm shutdown ==="; terminate_apps; free_proxy_sessions; kill_farm; shutdown_sims
	          echo "farm down (ios tunnel + appium-mcp preserved)." ;;
	free)     echo "=== free proxy sessions + stop apps + unblock devices (no teardown) ==="
	          terminate_apps; free_proxy_sessions; unblock_stuck ;;
	purge-foreign) log "purge-foreign: DELETE default (non-Fleet) iPhone/iPad sims so the DF pool = Fleet + real only"
	          purge_foreign; log "purge complete — DF roster is now real devices + ${DF_POOL_MATCH} sims." ;;
	unblock)  echo "=== unblock stuck devices ==="; unblock_stuck ;;
	*) echo "usage: $0 <status|setup|reset|shutdown|free|purge-foreign|unblock>" >&2; exit 2 ;;
esac
