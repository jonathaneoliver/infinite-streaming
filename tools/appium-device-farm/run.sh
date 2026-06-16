#!/bin/sh
# Bring up an Appium server with the Device Farm plugin enabled.
#
# Device Farm turns ONE Appium server into a device-pool manager: it allocates a
# free device per `POST /session` BY CAPABILITY (platformName / platformVersion),
# queues when none are free and auto-allocates per-session WDA/MJPEG ports — so
# clients stop hand-picking UDIDs and offsetting ports.
#
# Config lives in appium.config.json (next to this script). The key setting is
# bootedSimulators:true — DF only allocates among ALREADY-BOOTED sims and never
# cold-boots one (cold-booting an arbitrary sim is unreliable on this box). So
# the workflow is: boot the known-good, app-installed, latest-OS Fleet sims you
# want in the pool, then start this server — the booted set IS the allowlist.
# Attached real devices are always "booted", so they're picked up automatically.
#
# Dashboard (live device grid + session queue): http://<host>:<port>/device-farm/
#
# Env:
#   DF_PORT   Appium port (default 4723 — the port the harness expects).
#   DF_CONFIG config file (default: appium.config.json beside this script).
#   DF_LOG    log file (default /tmp/appium-device-farm.log).
#
# The plugin + drivers are installed globally into the Appium install (not this
# repo); this script installs the plugin on first run if it's missing:
#   appium plugin install --source=npm appium-device-farm
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
DF_PORT="${DF_PORT:-4723}"
DF_CONFIG="${DF_CONFIG:-$SCRIPT_DIR/appium.config.json}"
DF_LOG="${DF_LOG:-/tmp/appium-device-farm.log}"

if ! appium plugin list --installed 2>&1 | grep -q "device-farm"; then
	echo "device-farm plugin not installed — installing from npm…"
	appium plugin install --source=npm appium-device-farm
fi

# Free the port if a plain Appium (or a stale Device Farm) is already holding it.
if lsof -nP -iTCP:"$DF_PORT" -sTCP:LISTEN >/dev/null 2>&1; then
	echo "port $DF_PORT in use — stopping the listener so Device Farm can take it"
	lsof -nP -tiTCP:"$DF_PORT" -sTCP:LISTEN | xargs -r kill
	sleep 2
fi

echo "booted simulators currently in the Device Farm pool:"
xcrun simctl list devices booted 2>/dev/null | grep -iE "iphone|ipad|apple tv" || echo "  (none — boot the Fleet sims you want in the pool)"

echo "starting Appium + Device Farm on :$DF_PORT (config $DF_CONFIG), log -> $DF_LOG"
echo "dashboard: http://localhost:$DF_PORT/device-farm/"
# --config carries the plugin block (bootedSimulators, platforms); --port here
# overrides the config's port so DF_PORT still wins.
exec appium --config "$DF_CONFIG" --port "$DF_PORT"
