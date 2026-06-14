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

# --- health: sim booted + appium up + deploy reachable -----------------------
xcrun simctl bootstatus "$SIM_UDID" -b >/dev/null 2>&1 || {
  echo "booting sim $SIM_UDID…"; xcrun simctl boot "$SIM_UDID" 2>/dev/null; sleep 8;
}
if ! curl -s --max-time 4 http://localhost:4723/status >/dev/null 2>&1; then
  echo "appium down — starting it…"
  ( "${QE_APPIUM:-appium}" >"$HOME/Library/Logs/qe-appium.log" 2>&1 & )
  sleep 12
fi
curl -s --max-time 4 http://localhost:4723/status >/dev/null 2>&1 || { echo "FATAL: appium not reachable"; exit 1; }
curl -sk --max-time 6 "$HARNESS_BASE_URL/api/sessions" -o /dev/null || { echo "FATAL: deploy $HARNESS_BASE_URL unreachable"; exit 1; }
echo "health OK: sim booted · appium up · deploy reachable"

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

  # pattern recipe? read shape.pattern off the claimed experiment.
  shape=$(harness sweep ls running 2>/dev/null; true)
  recipe_json=$(curl -sk "$HARNESS_BASE_URL/analytics/api/v2/sweep/experiments?status=running" 2>/dev/null \
    | jq -r --arg id "$exp_id" '.items[]|select(.exp_id==$id)|.raw_json' 2>/dev/null)
  pattern=$(printf '%s' "$recipe_json" | jq -r '.shape.pattern // empty' 2>/dev/null)
  step=$(printf '%s' "$recipe_json" | jq -r '.shape.step_seconds // 12' 2>/dev/null)
  margin=$(printf '%s' "$recipe_json" | jq -r '.shape.margin_pct // 5' 2>/dev/null)

  # drive the probe (mechanical; no model call)
  log=$(mktemp)
  env CHAR_PLAYER_ID="$player_id" HARNESS_BASE_URL="$HARNESS_BASE_URL" LAUNCH_MODE=appium \
      CHAR_CONTENT="$CONTENT" CHAR_SWEEP_DURATION_S="$DURATION" CHARACTERIZATION_DEVICE_UDID="$SIM_UDID" \
      ${pattern:+CHAR_SWEEP_PATTERN="$pattern" CHAR_SWEEP_STEP_S="$step" CHAR_SWEEP_MARGIN="$margin"} \
      go test ./tests/characterization/modes -run TestSweepProbe -count=1 -v -timeout 8m >"$log" 2>&1
  play_id=$(grep -oE 'play_id: [0-9a-fA-F-]+' "$log" | head -1 | awk '{print $2}')
  rm -f "$log"
  if [ -z "$play_id" ]; then
    echo "no play_id (probe crash/inconclusive) — requeueing $exp_id"
    harness sweep reap --max-age-min 0 >/dev/null 2>&1
    continue
  fi

  # analyze (mechanical oracle verdict; records run history + retention)
  verdict=$(harness sweep analyze "$exp_id" --play "$play_id" --confirm-reps 1 --json 2>/dev/null | jq -r '.verdict // ""' 2>/dev/null)
  echo "$(date) $exp_id → verdict=$verdict (play $play_id)"

  # ONLY on a hit: spend tokens to investigate + isolate + promote
  case "$verdict" in
    aberration|notable)
      echo "$(date) hit — dispatching claude to investigate $exp_id"
      "$CLAUDE" -p "You are the QE Lab overnight investigator. A sweep run hit: experiment $exp_id, play $play_id, verdict $verdict, on the test-dev deploy ($HARNESS_BASE_URL; pass --insecure --base to harness). Do ONLY the investigate step of the sweep skill: (1) recall .claude/findings + memory for the signature; (2) 'harness --insecure --base $HARNESS_BASE_URL sweep annotate $exp_id --note \"<what happened / where / how>\"'; (3) reason about the cause and insert a one-axis isolation fan via 'harness ... sweep isolate $exp_id --flip <axis=value> …'; (4) promote a deduped Issue via 'harness ... sweep promote $exp_id'. Be concise. Do NOT drive any probe yourself — the bash runner does that." \
        --dangerously-skip-permissions || echo "claude investigate exited non-zero"
      ;;
    clean|"") : ;;  # clean → nothing to do
    *) echo "unexpected verdict: $verdict" ;;
  esac
done

echo "================ $(date) qe-offhours done ($iters iterations) ================"
