---
name: triage
description: Quick health check of a player session — pull the current state, recent events bucketed by priority, and surface the 3-6 most-interesting failures with timestamps. Invoke when the user says "triage X", "what's wrong with X", "diagnose X", "how's X doing", or asks for a summary of recent failures on a player. Output is a short report + suggested next skill to invoke; this skill does NOT drill into individual events (use `investigate` for that).
---

# Triage a player session

Read first: top of the file at `~/.local/bin/harness` is the `harness` CLI. If `which harness` fails, run `make harness-cli` in the repo root.

Goal: in <10 seconds tell the user what's broken on a player session — at the *menu* level, not the *cause* level.

## Inputs

The user asks "triage ipad", "what's wrong with the apple tv", "how's session ee091d13 doing", or similar. The free-form target is anything the resolver accepts: full UUID, ≥6-char hex prefix, label value (`device=ipad`), player IP, or a substring of the User-Agent. **Don't ask the user for a UUID** unless the resolver returns ambiguous.

## Flow

Always do these four steps in order. Stop at step 4 — drilling deeper is `investigate`'s job.

### 1. Resolve + sanity check

```sh
harness --insecure players show <target> 2>&1 | head -40
```

Look at: `id`, `last_seen_at`, `current_play.id`, `user_agent`, `player_metrics.player_state`, `player_metrics.last_event`, `player_metrics.player_error`, `fault_counters.*`.

If `last_seen_at` is more than 60 seconds old, the player has disconnected — say so, suggest checking `harness players list` for a live one, and stop.

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

## See also

- `investigate` — drill into a single event with windowed context
- `forensics` — multi-event causal analysis via subagent
- `finding` — capture what triage + investigate taught you
