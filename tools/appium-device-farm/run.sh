#!/bin/sh
# Bring up an Appium server with the Device Farm plugin enabled.
#
# Device Farm turns ONE Appium server into a device-pool manager: it
# auto-discovers booted simulators + connected real devices, and assigns a
# free one per `POST /session` BY CAPABILITY (platformName / platformVersion),
# queuing when none are free and auto-allocating per-session WDA/MJPEG ports.
# That removes the manual device picking + port-offset dance the
# characterization harness does today (CHAR_FLEET_UDIDS / CHAR_FLEET_PORT_OFFSET)
# and lets independent test runs coexist on the same server without colliding on
# wdaLocalPort 8100.
#
# Dashboard (live device grid + session queue): http://<host>:<port>/device-farm/
#
# Env:
#   DF_PORT      Appium port (default 4723 — the port the harness expects).
#   DF_PLATFORM  ios | android | both (default both — iOS sims + Android TV).
#   DF_LOG       log file (default /tmp/appium-device-farm.log).
#
# The plugin is installed globally into the Appium install (not this repo):
#   appium plugin install --source=npm appium-device-farm
# This script installs it on first run if it's missing.
set -eu

DF_PORT="${DF_PORT:-4723}"
DF_PLATFORM="${DF_PLATFORM:-both}"
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

echo "starting Appium + Device Farm on :$DF_PORT (platform=$DF_PLATFORM), log -> $DF_LOG"
echo "dashboard: http://localhost:$DF_PORT/device-farm/"
exec appium server \
	--port "$DF_PORT" \
	--keep-alive-timeout 800 \
	--use-plugins=device-farm \
	--plugin-device-farm-platform="$DF_PLATFORM"
