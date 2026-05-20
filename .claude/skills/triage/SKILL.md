---
name: triage
description: Quick health check at the *menu* level — either scan recent plays for ones needing attention (no target given, or "what's broken right now"), or drill the named player's current session for state + recent failures. Surfaces 3-6 worst signals with timestamps and a suggested next skill. Invoke when the user says "triage X", "what's wrong with X", "how's X doing", "any plays needing attention", "scan plays", "what's broken right now", or asks for a summary of recent failures. Does NOT drill into individual events (use `investigate` for that).
last_reviewed: 2026-05-19
---

# Triage — scan plays OR diagnose one player

Read first: top of the file at `~/.local/bin/harness` is the `harness` CLI. If `which harness` fails, run `make harness-cli` in the repo root.

**Conventions:** this skill follows the cross-skill rules in `.claude/skills/CONVENTIONS.md`. The load-bearing ones for triage: lead every shell command with the tool name (no `VAR=…` / `cd …` / `echo "header"; …` prefixes); tag causal claims `confirmed`/`refuted`/`needs-test`; display times in local, store in UTC. If the rollup is clean, say so in one line and stop — don't manufacture findings.

Goal: in <10 seconds tell the user *what* is broken — at the *menu* level, not the *cause* level.

## Two modes

**Mode A — Sweep.** No target, or the user asks "what's broken now", "scan plays", "any plays needing attention", "show me problem sessions". Use the v2 plays endpoint to surface the most-attention-worthy plays across the recent window, then suggest a single drill-in.

**Mode B — Single-player.** The user names a player (UUID prefix, label, device, IP, UA substring) — drill into that one player's live state + recent events.

Pick mode by whether you have a resolvable target. If the user said "triage" alone with no target → Mode A. If they named a target → Mode B (but you may use Mode A's plays scan as supporting context if the player has multiple recent plays).

## Mode A — Sweep recent plays

### A1. Pull the plays list

```sh
# Default: last 6 hours. Widen with `from=…` if the user asked for a longer window.
harness --insecure --json query plays \
  --from $(date -u -v-6H '+%Y-%m-%dT%H:%M:%SZ') \
  --limit 200 2>/dev/null \
  | jq '.items' > /tmp/plays.json

echo "rows: $(jq length /tmp/plays.json)"
```

Each item is a `PlaySummary` (see `api/openapi/v2/forwarder.yaml`). Trouble signals live in these fields:

| field | meaning | how to read |
|---|---|---|
| `classification` | tiered retention class | `favourite` = operator-starred; `interesting` = system-flagged; `other` = normal |
| `stalls` | buffer-empty events | >0 ⇒ user-visible freeze; ≥3 ⇒ painful |
| `frozen_count` | renderer hung (≠ stall) | always notable |
| `restart_count` | mid-play player recovery | >0 ⇒ something was bad enough to restart |
| `error_event_count` | explicit player_error | always notable |
| `user_marked_count` | operator pressed 911 | always notable |
| `segment_stall_count` | stall waiting on segment fetch | >0 = network-correlated stall |
| `all_failures` / `transport_failures` | failure-tier counters | non-zero = real failures, not retries |
| `master_manifest_failures` / `manifest_failures` / `segment_failures` | per-resource fail counts | indicates which HLS layer broke |
| `dropped_frames` | renderer drops | >100 = visible; >1000 = serious |
| `net_errors` / `net_faults` | network plane errors | from `network_requests` table join |
| `bitrate_shifts` / `resolution_changes` | ABR churn | very high = unstable variant boundary |
| `avg_quality_pct` / `min_quality_pct` | quality envelope | low avg = sustained downshifting |
| `last_player_error` | terminal error string | non-empty = play ended in error |

### A2. Score each play

Sum the "user-visible badness" signals with sensible weights. The dashboard's "interesting" filter is `(user_marked OR frozen OR error_event OR segment_stall OR restart) > 0`; mirror that but rank-order by magnitude.

```sh
jq -r '.[] | {
  play_id, player_id, started_at, last_seen_at,
  classification, last_state, last_player_error,
  stalls: (.stalls|tonumber? // 0),
  frozen: (.frozen_count|tonumber? // 0),
  user_marked: (.user_marked_count|tonumber? // 0),
  errors: (.error_event_count|tonumber? // 0),
  restarts: (.restart_count|tonumber? // 0),
  seg_stalls: (.segment_stall_count|tonumber? // 0),
  all_failures: (.all_failures|tonumber? // 0),
  transport_failures: (.transport_failures|tonumber? // 0),
  manifest_failures: (.manifest_failures|tonumber? // 0),
  segment_failures: (.segment_failures|tonumber? // 0),
  drops: (.dropped_frames|tonumber? // 0),
  net_errors: (.net_errors|tonumber? // 0),
  net_faults: (.net_faults|tonumber? // 0),
  shifts: (.bitrate_shifts|tonumber? // 0),
  avg_q: .avg_quality_pct,
  score: (
    (.user_marked_count|tonumber? // 0) * 100   # operator 911 — biggest weight
    + (.frozen_count|tonumber? // 0)     * 50
    + (.error_event_count|tonumber? // 0) * 40
    + (.restart_count|tonumber? // 0)   * 20
    + (.segment_stall_count|tonumber? // 0) * 15
    + (.stalls|tonumber? // 0)          * 10
    + (.all_failures|tonumber? // 0)    *  5
    + (.transport_failures|tonumber? // 0) * 3
    + ((.dropped_frames|tonumber? // 0) / 100)
    + ((.net_errors|tonumber? // 0)     *  2)
    + ((.net_faults|tonumber? // 0)     *  3)
  )
}' /tmp/plays.json \
  | jq -s 'sort_by(-.score) | .[0:8]' > /tmp/plays-ranked.json
```

(The weights are heuristic — operator-marked events trump everything, frozen frames > errors > restarts > stalls. Tune if your scenario emphasises different signals; the rank order matters more than the absolute score.)

### A3. Report — top of the heap

```
14 plays in the last 6h. 5 worth attention:

★ cfcde730  ee091d13 (iPad)        20:55→21:36 (41m)  score 1831
    5 stalls, 30 min stall-time, 2 restarts, classification=interesting
    last_state=playing, no terminal error
    → /triage ee091d13   (drill the player)

  2476fd74  ee091d13 (iPad)        17:25→20:23 (3h)   score 765
    11 stalls, 10 min stall-time, 1 restart, 583 bitrate shifts
    → ABR thrash candidate — /forensics ee091d13

  f9565db5  97282446 (AVPlayer/iOS) 17:34→18:13 (39m)  score 612
    197 stalls(!), 9082 dropped frames, 18 error events
    last_player_error="hls networkError levelLoadError"
    → very bad — /investigate 17:34 or /forensics

  1d2f07ea  ee091d13 (iPad)        23:27→ongoing      score 215
    5 stalls (last 36s), 1 frozen frame, audio-rendition 5xx storm
    → /triage ee091d13  (in-flight; check again in 5 min)

  21637fda  ee091d13 (iPad)        20:41→20:49 (8m)   score 142
    11 stalls in 8 minutes, restart at end
```

8–15 lines. Lead with count + favourites-or-stars (★ for `classification=favourite`). For each: player short-id, device/UA, time window, score, the 2-3 numbers that drive the score, and a one-line *next-step* skill suggestion. Don't list more than the top 5-8.

If everything is clean ("no stalls, no errors, all classification=other"), say so in one line and stop.

### A4. Suggest exactly one next-step

The user wants a single click forward, not a menu. Pick the worst play and recommend either `/triage <player_id>` (Mode B) or `/forensics …` if the pattern is repeating across plays for the same player.

## Mode B — Single-player triage

Always do these four steps in order. Stop at step 4 — drilling deeper is `investigate`'s job.

### 1. Resolve + sanity check

```sh
harness --insecure players show <target> 2>&1 | head -40
```

Look at: `id`, `last_seen_at`, `current_play.id`, `user_agent`, `player_metrics.player_state`, `player_metrics.last_event`, `player_metrics.player_error`, `fault_counters.*`.

If `last_seen_at` is more than 60 seconds old, the player has disconnected — say so, suggest checking `harness players list` for a live one, and stop.

### 1b. Pull recent plays for this player (multi-play context)

The live `players show` only sees the current play. To know whether the player has been stable across the last few hours or thrashing through many plays, ask the v2 plays endpoint:

```sh
harness --insecure --json query plays \
  --player-id "$PID" \
  --from $(date -u -v-6H '+%Y-%m-%dT%H:%M:%SZ') \
  --limit 50 2>/dev/null \
  | jq '.items[0:12] |
        map({play_id, started_at, stalls, restart_count, frozen_count,
             error_event_count, classification, last_player_error, bitrate_shifts})' \
  > /tmp/player-plays.json
```

Skim it: how many plays in the window, how many flagged `interesting`, any with `last_player_error`. If the current play is the 10th in two hours, that's the headline — not whatever just stalled. If they all share a recurring failure shape (every play has a stall, every play has the same terminal error), call that out and recommend `/forensics` instead of continuing this triage.

### 2. Pull recent events

Live SSE — capture ~5 seconds of backfill then bail. The `timeout` here is what makes this fast; don't use a longer one. The grep+sed extracts the SSE payload; the jq filters to operator-relevant events (drops heartbeats and the meta frame).

```sh
timeout 5 harness --insecure --json ts <target> --streams events --bundles events 2>/dev/null \
  | jq -c 'select(.type and .priority)' > /tmp/events.jsonl
```

If the file is empty, the events stream may have failed — check `harness info` and tell the user (don't silently continue with no data).

### 3. Bucket + surface

Count by priority and type, then surface the most-interesting events:

```sh
echo "=== count by priority (1=critical … 4=low) ==="
jq -r .priority /tmp/events.jsonl | sort | uniq -c

echo "=== count by type ==="
jq -r .type /tmp/events.jsonl | sort | uniq -c | sort -rn | head -10

echo "=== P1 events, chronological ==="
jq -c 'select(.priority==1) | {ts, type, info}' /tmp/events.jsonl | sort

echo "=== P2 events, most-recent first, top 10 ==="
jq -c 'select(.priority==2) | {ts, type, info}' /tmp/events.jsonl | sort -r | head -10
```

### 4. Report

Give the user a tight summary in roughly this shape:

```
ipad (ee091d13)  player_state=playing  buffer=12.3s  last_event=heartbeat  no errors

Last 5 min: 22 critical, 47 high, 2554 medium/low.

Worst recent events:
  18:29:27  stall 262s     ← user-visible 4-minute freeze
  18:52:51  stall 91s
  18:57:03  stall 84s
  18:59:26  stall 104s

Recent ABR churn: 1109 upshifts, 297 downshifts in window — heavy
re-evaluation, suggests the player is hovering near a variant
boundary or repeatedly probing.

Suggested next:
  /investigate 18:29:27   — deep-dive the worst single event
  /forensics              — multi-event "why does this keep happening"
```

8–15 lines total. State the headline fact first ("player is fine" or
"frequent stalls"). List 3-6 specific events with timestamps. Suggest
one next skill — don't list every option.

## What NOT to do here

- **Don't pull network or sample data.** That's investigate's job and
  it's slow. Triage is the menu, not the meal.
- **Don't speculate about cause.** Surface the symptom, suggest the
  next skill. The user (or `investigate`) decides what to chase.
- **Don't filter out P1 events as noise.** Even if "this player
  always stalls" is true, the operator wants to see it.
- **Don't suggest `harness fault add` from triage.** Mutation is a
  separate skill and a separate decision.

## Common pitfalls

- The events SSE keeps streaming live rows after backfill, so without
  `timeout` the loop never returns. Always cap with `timeout`.
- `harness ts --insecure` is required on test-dev (self-signed cert).
  If the user has `HARNESS_INSECURE=1` exported it works without; if
  not, the `--insecure` flag is needed every time.
- The "5 minutes" framing assumes the player's been active that long.
  If `last_seen_at` is younger, just say "last N minutes since
  player came online" — don't pretend you have 5 min of data.

## The v2 plays endpoint, in one line

`/api/v2/plays` returns one row per archived playback with the trouble-signal fields the dashboard uses for its "interesting" filter. CLI: `harness --insecure --json query plays [--from ISO] [--player-id UUID] [--play-id UUID] [--classification interesting|other|favourite] [--limit N]`. Single-play detail: `harness --insecure --json query play <play_id>`. Spec: `api/openapi/v2/forwarder.yaml § /api/v2/plays`.

## See also

- `investigate` — drill into a single event with windowed context
- `forensics` — multi-event causal analysis via subagent
- `finding` — capture what triage + investigate taught you
