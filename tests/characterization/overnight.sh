#!/usr/bin/env bash
# Run rampup → rampdown → pyramid back-to-back for one platform and
# capture per-test logs + a top-line PASS/FAIL summary. Designed for
# overnight runs where artifacts must survive a partial failure.
#
# Usage:
#   scripts/overnight.sh <platform>
#
# Platforms: ipad-sim, iphone, apple-tv, android-tv, web
#
# Artifacts:
#   tests/characterization/artifacts/runs/<datetime>-<platform>/
#     rampup.log, rampdown.log, pyramid.log   — full go test output per test
#     summary.txt                             — one-line PASS/FAIL per test
#   tests/characterization/artifacts/
#     rampup-…json/.html, rampdown-…json/.html, pyramid-…json/.html
#
# Env overrides:
#   LAUNCH_MODE       default: appium
#   PER_TEST_TIMEOUT  default: 120m

set -u

if [[ $# -lt 1 ]]; then
    echo "usage: $0 <platform>"
    echo "platforms: ipad-sim iphone apple-tv android-tv web"
    exit 2
fi

case $1 in
    ipad-sim)   TEST_SUFFIX=IPadSim ;;
    iphone)     TEST_SUFFIX=IPhone ;;
    apple-tv)   TEST_SUFFIX=AppleTV ;;
    android-tv) TEST_SUFFIX=AndroidTV ;;
    web)        TEST_SUFFIX=Web ;;
    *) echo "unknown platform: $1"; exit 2 ;;
esac
PLATFORM=$1

REPO_ROOT=$(cd "$(dirname "$0")/../.." && pwd)
CHAR_ROOT=$REPO_ROOT/tests/characterization
RUN_ID=$(date -u +%Y%m%dT%H%M%SZ)
RUN_DIR=$CHAR_ROOT/artifacts/runs/${RUN_ID}-${PLATFORM}
mkdir -p "$RUN_DIR"

LAUNCH_MODE=${LAUNCH_MODE:-appium}
PER_TEST_TIMEOUT=${PER_TEST_TIMEOUT:-120m}
# Resolve Device Farm on/off the SAME way the Go harness does (runner.DeviceFarmEnabled):
# DEFAULT ON — off only when explicitly 0/false/off. The shell env wins, else the
# nearest .env. Without this, a DIRECT `./overnight.sh` run would allocate via DF
# (the harness defaults on / reads .env) yet skip booting the pool here. `make
# characterize-*` already `-include`s + exports .env.
DEVICE_FARM="${CHAR_DEVICE_FARM:-}"
if [[ -z "$DEVICE_FARM" && -f "$REPO_ROOT/.env" ]]; then
    df_line="$(grep -E '^[[:space:]]*CHAR_DEVICE_FARM[[:space:]]*=' "$REPO_ROOT/.env" | tail -1)"
    DEVICE_FARM="${df_line#*=}"
    DEVICE_FARM="${DEVICE_FARM//[[:space:]]/}"
    DEVICE_FARM="${DEVICE_FARM//\"/}"
    DEVICE_FARM="${DEVICE_FARM//\'/}"
fi
DF_ON=1
case "$(printf '%s' "$DEVICE_FARM" | tr '[:upper:]' '[:lower:]')" in
    0|false|off) DF_ON=0 ;;
esac

# Under Device Farm the sim pool must be booted (+ WDA warm) before the run so DF
# can allocate. boot-pool.sh is iOS-sim specific; real hardware / web don't need
# it. Best-effort — a warning there shouldn't abort the run.
if [[ "$DF_ON" == "1" && "$PLATFORM" == "ipad-sim" ]]; then
    echo "Device Farm on — booting the sim pool (boot-pool.sh)…"
    "$REPO_ROOT/tools/appium-device-farm/boot-pool.sh" || echo "  boot-pool reported an issue (continuing)"
fi

SUMMARY=$RUN_DIR/summary.txt
{
    echo "platform:   $PLATFORM"
    echo "run_id:     $RUN_ID"
    echo "launch:     $LAUNCH_MODE"
    echo "device_farm: $([[ "$DF_ON" == "1" ]] && echo on || echo off)"
    echo "timeout:    $PER_TEST_TIMEOUT (per test)"
    echo "run_dir:    $RUN_DIR"
    echo "started:    $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo
    echo "tests:"
} > "$SUMMARY"

# macOS ships Bash 3.2 — no associative arrays. Tiny case for the
# three test names we know about.
go_test_name() {
    case $1 in
        rampup)   echo Rampup ;;
        rampdown) echo Rampdown ;;
        pyramid)  echo Pyramid ;;
    esac
}

OVERALL_RC=0
for t in rampup rampdown pyramid; do
    LOGFILE=$RUN_DIR/${t}.log
    printf "▶ %-9s … " "$t"
    STARTED=$(date +%s)
    go test -C "$CHAR_ROOT" ./modes/... -v \
        -run "Test$(go_test_name "$t")${TEST_SUFFIX}" \
        -timeout "$PER_TEST_TIMEOUT" -count=1 \
        -launch-mode="$LAUNCH_MODE" \
        > "$LOGFILE" 2>&1
    RC=$?
    ELAPSED=$(( $(date +%s) - STARTED ))
    if [[ $RC -eq 0 ]]; then STATUS=PASS; else STATUS=FAIL; OVERALL_RC=1; fi
    # Pull the play_id the test labeled — it's emitted as
    # `play_id: <uuid>  (find later: harness query play <uuid>)` near the
    # top of every test log.
    PLAY_ID=$(grep -m1 -oE 'play_id: [0-9a-f-]{36}' "$LOGFILE" | awk '{print $2}')
    [[ -z $PLAY_ID ]] && PLAY_ID='(not-captured)'
    printf "%s  (%dm%ds)  play_id=%s\n" "$STATUS" $((ELAPSED/60)) $((ELAPSED%60)) "$PLAY_ID"
    printf "  %-9s %s  (%dm%ds)  rc=%d  play_id=%s  log=%s\n" \
        "$t" "$STATUS" $((ELAPSED/60)) $((ELAPSED%60)) "$RC" "$PLAY_ID" "$LOGFILE" \
        >> "$SUMMARY"
done

{
    echo
    echo "finished:   $(date -u +%Y-%m-%dT%H:%M:%SZ)"
    echo
    echo "Reports + charts are in tests/characterization/artifacts/ — files"
    echo "named <test>-${PLATFORM}-<player8>-<run_id>.{json,html,md}."
    echo "Each test labels its play with test=<name> platform=$PLATFORM run_id=<ts>;"
    echo "query later via:"
    echo "  harness --json query plays --label test=rampup --label platform=$PLATFORM"
} >> "$SUMMARY"

echo
echo "── summary ──"
cat "$SUMMARY"
exit $OVERALL_RC
