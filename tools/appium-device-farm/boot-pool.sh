#!/bin/sh
# Boot the Device Farm simulator pool — the operator-side "fire up N sims" step.
#
# Device Farm allocates ONLY among already-booted sims (bootedSimulators:true in
# appium.config.json), so the booted set IS the allowlist. This script boots N
# latest-OS, app-installed Fleet sims, verifies the app, and (if the DF server is
# up) warms each sim's WebDriverAgent so the FIRST real session doesn't cold-build
# WDA and blow a test's launch timeout. Run this BEFORE run.sh / a characterization
# run. Idempotent: already-booted sims and already-built WDA are no-ops.
#
# Env:
#   DF_POOL_COUNT     how many sims to boot           (default 4)
#   DF_POOL_MATCH     sim-name substring to pick from (default "Fleet")
#   DF_POOL_OS        iOS runtime major.minor         (default: latest installed)
#   DF_BUNDLE_ID      app to verify on each sim        (default com.jeoliver.InfiniteStreamPlayer)
#   DF_PORT           DF/Appium port for WDA warming   (default 4723)
#   DF_WARM_WDA       warm WDA via DF (1) or skip (0)  (default 1)
set -eu

DF_POOL_COUNT="${DF_POOL_COUNT:-4}"
DF_POOL_MATCH="${DF_POOL_MATCH:-Fleet}"
DF_BUNDLE_ID="${DF_BUNDLE_ID:-com.jeoliver.InfiniteStreamPlayer}"
DF_PORT="${DF_PORT:-4723}"
DF_WARM_WDA="${DF_WARM_WDA:-1}"

command -v xcrun >/dev/null 2>&1 || { echo "xcrun not on \$PATH" >&2; exit 1; }

# Latest installed iOS runtime major.minor (overridable). DF matches
# platformVersion against this, and the harness pins it (runner.dfPlatformVersion).
DF_POOL_OS="${DF_POOL_OS:-$(xcrun simctl list runtimes --json | python3 -c '
import sys, json
best = ""
for rt in json.load(sys.stdin)["runtimes"]:
    if rt.get("platform") != "iOS" or not rt.get("isAvailable"):
        continue
    v = rt.get("version", "")
    def key(s): return [int(x) for x in s.split(".") if x.isdigit()]
    if not best or key(v) > key(best):
        best = v
print(".".join(best.split(".")[:2]) if best else "")
')}"

[ -n "$DF_POOL_OS" ] || { echo "no available iOS simulator runtime found" >&2; exit 1; }
echo "pool target: $DF_POOL_COUNT sim(s) matching \"$DF_POOL_MATCH\" on iOS $DF_POOL_OS"

# UDIDs of available sims on that runtime whose name contains DF_POOL_MATCH.
UDIDS=$(xcrun simctl list devices available --json | DF_POOL_OS="$DF_POOL_OS" DF_POOL_MATCH="$DF_POOL_MATCH" DF_POOL_COUNT="$DF_POOL_COUNT" python3 -c '
import sys, json, os
want_os = os.environ["DF_POOL_OS"].replace(".", "-")
match = os.environ["DF_POOL_MATCH"].lower()
count = int(os.environ["DF_POOL_COUNT"])
out = []
for rt, devs in json.load(sys.stdin)["devices"].items():
    if ("iOS-" + want_os) not in rt:
        continue
    for d in devs:
        if match in d.get("name", "").lower():
            out.append((d["name"], d["udid"]))
out.sort()
for name, udid in out[:count]:
    print(udid)
')

if [ -z "$UDIDS" ]; then
	echo "no available sims matching \"$DF_POOL_MATCH\" on iOS $DF_POOL_OS" >&2
	echo "  (list candidates: xcrun simctl list devices available | grep -i \"$DF_POOL_MATCH\")" >&2
	exit 1
fi

n=$(printf '%s\n' "$UDIDS" | grep -c .)
echo "selected $n sim(s):"

missing_app=0
for u in $UDIDS; do
	name=$(xcrun simctl list devices --json | python3 -c "import sys,json
for rt,devs in json.load(sys.stdin)['devices'].items():
    for d in devs:
        if d['udid']=='$u': print(d['name'])" 2>/dev/null || echo "$u")
	echo "  - $name ($u)"

	# Boot (idempotent — swallow 'current state: Booted').
	if out=$(xcrun simctl boot "$u" 2>&1); then
		xcrun simctl bootstatus "$u" >/dev/null 2>&1 || true
		echo "      booted"
	elif printf '%s' "$out" | grep -q "current state: Booted"; then
		echo "      already booted"
	else
		echo "      BOOT FAILED: $out" >&2
		continue
	fi

	# Verify the app is installed (the app-on-pool invariant — HANDOFF §7).
	if xcrun simctl listapps "$u" 2>/dev/null | grep -q "$DF_BUNDLE_ID"; then
		echo "      app $DF_BUNDLE_ID present"
	else
		echo "      ⚠️  app $DF_BUNDLE_ID NOT installed — DF would allocate this sim then fail at launch" >&2
		missing_app=1
	fi
done

# Warm WebDriverAgent on each sim via DF so the first real session is fast.
if [ "$DF_WARM_WDA" = "1" ]; then
	if curl -s --max-time 3 "http://localhost:$DF_PORT/status" 2>/dev/null | grep -q '"ready":true'; then
		echo "warming WebDriverAgent via DF on :$DF_PORT (first build per sim is slow; cached after)…"
		for u in $UDIDS; do
			# || true: a slow/failed warm on one sim must not abort the loop (set -e).
			resp=$(curl -s --max-time 180 -X POST "http://localhost:$DF_PORT/session" -H 'Content-Type: application/json' -d '{
				"capabilities":{"alwaysMatch":{"platformName":"iOS","appium:automationName":"XCUITest",
				"appium:udid":"'"$u"'","appium:bundleId":"'"$DF_BUNDLE_ID"'","appium:noReset":true,
				"appium:forceAppLaunch":false,"appium:useNewWDA":false,"appium:newCommandTimeout":60},
				"firstMatch":[{}]}}' 2>/dev/null || true)
			sid=$(printf '%s' "$resp" | python3 -c 'import sys,json
try: print(json.load(sys.stdin)["value"].get("sessionId",""))
except Exception: print("")' 2>/dev/null)
			if [ -n "$sid" ]; then
				echo "  $u: WDA ready"
				curl -s --max-time 20 -X DELETE "http://localhost:$DF_PORT/session/$sid" >/dev/null 2>&1 || true
			else
				echo "  $u: WDA warm failed (will cold-build on first real session): $(printf '%s' "$resp" | head -c 120)" >&2
			fi
		done
	else
		echo "DF not up on :$DF_PORT — skipping WDA warm (start it with run.sh, then re-run, or set DF_WARM_WDA=0)"
	fi
fi

# Leave the pool quiet: terminate the player app on each sim. Warming (and any
# prior run) leaves the app foregrounded — streaming + heartbeating a player to
# the server — which is just noise when no test is using it. WDA is a SEPARATE
# process and stays resident, so the next session is still fast; and every test
# does appium:forceAppLaunch anyway, so an off app costs the next run nothing.
# Opt out with DF_KEEP_APP_RUNNING=1.
if [ "${DF_KEEP_APP_RUNNING:-0}" != "1" ]; then
	echo "terminating $DF_BUNDLE_ID on the pool (WDA stays resident; quiet until a test launches it)…"
	for u in $UDIDS; do
		xcrun simctl terminate "$u" "$DF_BUNDLE_ID" >/dev/null 2>&1 || true
	done
fi

echo "pool ready. start the server with tools/appium-device-farm/run.sh (if not already running)."
[ "$missing_app" = "0" ] || { echo "WARNING: one or more sims are missing $DF_BUNDLE_ID — install it before running." >&2; exit 2; }
