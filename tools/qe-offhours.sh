#!/bin/bash
# QE Lab off-hours runner (#772). Fired by launchd at 21:00; drives the sweep
# until the backlog drains or the 05:00 deadline — whichever comes first.
#
# Token-conscious by design: the mechanical loop (claim → bootstrap → probe →
# analyze) runs in pure bash with ZERO model calls. `claude -p` is invoked ONLY
# on a notable/aberration hit, to investigate → annotate → isolate → promote
# (the one part that needs reasoning). So overnight token spend scales with
# findings, not the (many) clean runs.
#
# Install: see tools/com.infinitestream.qe-offhours.plist. Test now:
#   bash tools/qe-offhours.sh            # runs until backlog empty or 05:00
#   QE_MAX_ITERS=1 bash tools/qe-offhours.sh   # a single iteration

set -u

REPO="${QE_REPO:-/Users/jonathanoliver/Projects/smashing}"
SIM_UDID="${QE_SIM_UDID:-4D62CB39-BAB7-4294-99D7-8E28FBCD0FF0}"   # Fleet iPhone 15 #1
CONTENT="${QE_CONTENT:-insane_new_p200_h264}"
DURATION="${QE_DURATION_S:-90}"
DEADLINE_HHMM="${QE_DEADLINE:-05:00}"
MAX_ITERS="${QE_MAX_ITERS:-1000}"
LOG="${QE_LOG:-$HOME/Library/Logs/qe-offhours.log}"
CLAUDE="${QE_CLAUDE:-claude}"

export HARNESS_BASE_URL="${HARNESS_BASE_URL:-https://dev.jeoliver.com:21000}"
export HARNESS_INSECURE="${HARNESS_INSECURE:-1}"
ANDROIDTV_UDID="${QE_ANDROIDTV_UDID:-}"          # adb serial of the physical Android TV (empty = auto-pick the one device)
ANDROIDTV_LAUNCH="${QE_ANDROIDTV_LAUNCH:-cli}"   # LAUNCH_MODE for android-tv (cli/adb)
OWNER="offhours-$$"

mkdir -p "$(dirname "$LOG")"
exec >>"$LOG" 2>&1
echo "================ $(date) qe-offhours start (owner $OWNER) ================"
cd "$REPO" || { echo "FATAL: repo $REPO not found"; exit 1; }

# --- deadline: seconds until the next DEADLINE_HHMM ---------------------------
now=$(date +%s)
deadline=$(date -j -f "%Y-%m-%d %H:%M" "$(date +%Y-%m-%d) $DEADLINE_HHMM" +%s 2>/dev/null)
[ -z "$deadline" ] && { echo "FATAL: bad deadline $DEADLINE_HHMM"; exit 1; }
[ "$deadline" -le "$now" ] && deadline=$((deadline + 86400))   # already past today → tomorrow
budget=$((deadline - now))
echo "budget: $((budget/60)) min until $DEADLINE_HHMM"
[ "$budget" -lt 600 ] && { echo "<10m to deadline; nothing to do"; exit 0; }

stop_at=$deadline
time_left() { echo $(( stop_at - $(date +%s) )); }

# --- health: deploy (hard) + per-platform device availability (soft) ----------
# Two runtimes now: appium+sim for iOS, adb for the physical Android TV. Probe a
# claim's platform against whichever is up; only abort if NOTHING is runnable.
curl -sk --max-time 6 "$HARNESS_BASE_URL/api/sessions" -o /dev/null || { echo "FATAL: deploy $HARNESS_BASE_URL unreachable"; exit 1; }

APPIUM_OK=0
xcrun simctl bootstatus "$SIM_UDID" -b >/dev/null 2>&1 || {
  echo "booting sim $SIM_UDID…"; xcrun simctl boot "$SIM_UDID" 2>/dev/null; sleep 8;
}
if ! curl -s --max-time 4 http://localhost:4723/status >/dev/null 2>&1; then
  echo "appium down — starting it…"; ( "${QE_APPIUM:-appium}" >"$HOME/Library/Logs/qe-appium.log" 2>&1 & ); sleep 12
fi
curl -s --max-time 4 http://localhost:4723/status >/dev/null 2>&1 && APPIUM_OK=1

ADB_OK=0
if command -v adb >/dev/null 2>&1; then
  if [ -z "$ANDROIDTV_UDID" ]; then
    ANDROIDTV_UDID=$(adb devices 2>/dev/null | awk '$2=="device"{print $1; exit}')
  fi
  [ -n "$ANDROIDTV_UDID" ] && adb -s "$ANDROIDTV_UDID" get-state >/dev/null 2>&1 && ADB_OK=1
fi
echo "health: deploy OK · appium=$([ $APPIUM_OK = 1 ] && echo up || echo DOWN) · androidtv=$([ $ADB_OK = 1 ] && echo \"$ANDROIDTV_UDID\" || echo none)"
[ "$APPIUM_OK" = 0 ] && [ "$ADB_OK" = 0 ] && { echo "FATAL: no runnable device (no appium, no adb device)"; exit 1; }

# --- selftest: a clean 2 Mbps-capped play, no fault, so a human can WATCH the
#     sim actually stream video and confirm the whole pipeline works ----------
if [ "${QE_SELFTEST:-0}" = 1 ]; then
  echo "$(date) SELFTEST: capped 2 Mbps play of $CONTENT — you should see video on the sim"
  raw=$(jq -nc --arg now "$(date -u +%Y-%m-%dT%H:%M:%SZ)" --arg c "$CONTENT" \
    '{id:"qe-selftest",created_at:$now,class:"config",platform:"ipad-sim",protocol:"hls",content:$c,mode:"steps",shape:{rate_mbps:2},kind:"seed",reps:1,why:"selftest",why_text:"shakeout: 2 Mbps cap, confirms the player streams"}')
  body=$(jq -nc --arg raw "$raw" \
    '{experiments:[{exp_id:"qe-selftest",class:"config",status:"running",kind:"seed",platform:"ipad-sim",protocol:"hls",mode:"steps",recipe:"rate_2mbps",why:"selftest",why_text:"shakeout: 2 Mbps cap",score:1,raw_json:$raw}]}')
  curl -sk -X POST "$HARNESS_BASE_URL/analytics/api/v2/sweep/experiments" -H 'Content-Type: application/json' -d "$body" >/dev/null
  sleep 1
  boot=$(harness sweep bootstrap qe-selftest 2>&1)
  pid=$(printf '%s' "$boot" | grep -oE 'player_id=[0-9a-fA-F-]+' | head -1 | cut -d= -f2)
  echo "  player_id=$pid (2 Mbps cap live on connect)"
  if [ -n "$pid" ]; then
    log=$(mktemp)
    env CHAR_PLAYER_ID="$pid" HARNESS_BASE_URL="$HARNESS_BASE_URL" LAUNCH_MODE=appium \
        CHAR_CONTENT="$CONTENT" CHAR_SWEEP_DURATION_S="$DURATION" CHARACTERIZATION_DEVICE_UDID="$SIM_UDID" \
        go test ./tests/characterization/modes -run TestSweepProbe -count=1 -v -timeout 6m >"$log" 2>&1
    grep -E "playing for|SWEEP PROBE|play_id:|session-viewer:|PASS|FAIL" "$log"
    play_id=$(grep -oE 'play_id: +[0-9a-fA-F-]+' "$log" | head -1 | awk '{print $2}')
    rm -f "$log"
  fi
  curl -sk -X POST "$HARNESS_BASE_URL/analytics/api/v2/sweep/delete" -H 'Content-Type: application/json' -d '{"exp_id":"qe-selftest"}' >/dev/null
  if [ -n "${play_id:-}" ]; then
    echo "  ✓ SELFTEST OK — streamed $CONTENT under 2 Mbps; play_id=$play_id"
    echo "  viewer: $HARNESS_BASE_URL/dashboard/session-viewer.html?player_id=$pid&play_id=$play_id"
  else
    echo "  ✗ SELFTEST: no play — the sim app didn't stream (check the content selection / server in the app)"
    exit 1
  fi
  exit 0
fi

# --- the loop ----------------------------------------------------------------
iters=0
while [ "$iters" -lt "$MAX_ITERS" ] && [ "$(time_left)" -gt 360 ]; do  # need >6min headroom for a probe
  iters=$((iters + 1))

  claim=$(harness sweep next --claim --owner "$OWNER" 2>&1)
  exp_id=$(printf '%s\n' "$claim" | head -1 | awk '{print $1}')
  if printf '%s' "$claim" | grep -qi "nothing to claim"; then
    echo "$(date) backlog drained — done after $((iters-1)) runs"; break
  fi
  [ -z "$exp_id" ] && { echo "claim parse failed: $claim"; break; }
  echo "--- $(date) iter $iters: claimed $exp_id"

  boot=$(harness sweep bootstrap "$exp_id" 2>&1)
  player_id=$(printf '%s' "$boot" | grep -oE 'player_id=[0-9a-fA-F-]+' | head -1 | cut -d= -f2)
  [ -z "$player_id" ] && { echo "bootstrap failed: $boot"; harness sweep reap --max-age-min 0 >/dev/null 2>&1; continue; }

  # read the recipe: platform (→ launch mode + device) + pattern shape.
  recipe_json=$(curl -sk "$HARNESS_BASE_URL/analytics/api/v2/sweep/experiments?status=running" 2>/dev/null \
    | jq -r --arg id "$exp_id" '.items[]|select(.exp_id==$id)|.raw_json' 2>/dev/null)
  platform=$(printf '%s' "$recipe_json" | jq -r '.platform // "ipad-sim"' 2>/dev/null)
  pattern=$(printf '%s' "$recipe_json" | jq -r '.shape.pattern // empty' 2>/dev/null)
  step=$(printf '%s' "$recipe_json" | jq -r '.shape.step_seconds // 12' 2>/dev/null)
  margin=$(printf '%s' "$recipe_json" | jq -r '.shape.margin_pct // 5' 2>/dev/null)

  # route to the platform's runtime: android-tv → adb; everything else → appium.
  if [ "$platform" = "androidtv" ]; then
    [ "$ADB_OK" = 1 ] || { echo "  androidtv not available (no adb device) — requeueing $exp_id"; harness sweep reap --max-age-min 0 >/dev/null 2>&1; continue; }
    launch="$ANDROIDTV_LAUNCH"; device="$ANDROIDTV_UDID"
  else
    [ "$APPIUM_OK" = 1 ] || { echo "  $platform needs appium (down) — requeueing $exp_id"; harness sweep reap --max-age-min 0 >/dev/null 2>&1; continue; }
    launch="appium"; device="$SIM_UDID"
  fi
  echo "  platform=$platform launch=$launch device=$device"

  # drive the probe (mechanical; no model call)
  log=$(mktemp)
  env CHAR_PLAYER_ID="$player_id" HARNESS_BASE_URL="$HARNESS_BASE_URL" LAUNCH_MODE="$launch" \
      CHAR_CONTENT="$CONTENT" CHAR_SWEEP_DURATION_S="$DURATION" CHARACTERIZATION_DEVICE_UDID="$device" \
      ${pattern:+CHAR_SWEEP_PATTERN="$pattern" CHAR_SWEEP_STEP_S="$step" CHAR_SWEEP_MARGIN="$margin"} \
      go test ./tests/characterization/modes -run TestSweepProbe -count=1 -v -timeout 8m >"$log" 2>&1
  play_id=$(grep -oE 'play_id: +[0-9a-fA-F-]+' "$log" | head -1 | awk '{print $2}')
  rm -f "$log"
  if [ -z "$play_id" ]; then
    echo "no play_id (probe crash/inconclusive) — requeueing $exp_id"
    harness sweep reap --max-age-min 0 >/dev/null 2>&1
    continue
  fi

  # analyze (mechanical oracle verdict; records run history + retention).
  # Wait for the forwarder to ingest the play's labels first — analyzing
  # immediately reads 0 labels and mis-verdicts a real hit as clean.
  sleep 15
  verdict=$(harness sweep analyze "$exp_id" --play "$play_id" --confirm-reps 1 --json 2>/dev/null | jq -r '.verdict // ""' 2>/dev/null)
  echo "$(date) $exp_id → verdict=$verdict (play $play_id)"

  # ONLY on a hit: spend tokens to investigate + isolate + promote
  case "$verdict" in
    aberration|notable)
      if [ "${QE_NO_CLAUDE:-0}" = 1 ]; then
        echo "$(date) hit on $exp_id — QE_NO_CLAUDE set, skipping the claude dispatch (shakeout mode)"
      else
        echo "$(date) hit — dispatching claude to investigate $exp_id"
        "$CLAUDE" -p "You are the QE Lab overnight investigator. A sweep run hit: experiment $exp_id, play $play_id, verdict $verdict, on the test-dev deploy ($HARNESS_BASE_URL; pass --insecure --base to harness). Do ONLY the investigate step of the sweep skill: (1) recall .claude/findings + memory for the signature; (2) 'harness --insecure --base $HARNESS_BASE_URL sweep annotate $exp_id --note \"<what happened / where / how>\"'; (3) reason about the cause and insert a one-axis isolation fan via 'harness ... sweep isolate $exp_id --flip <axis=value> …'; (4) promote a deduped Issue via 'harness ... sweep promote $exp_id'. Be concise. Do NOT drive any probe yourself — the bash runner does that." \
          --dangerously-skip-permissions || echo "claude investigate exited non-zero"
      fi
      ;;
    clean|"") : ;;  # clean → nothing to do
    *) echo "unexpected verdict: $verdict" ;;
  esac
done

echo "================ $(date) qe-offhours done ($iters iterations) ================"
