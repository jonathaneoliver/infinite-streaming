---
name: investigate
description: Deep-dive ONE specific event on a player — pull network log, samples, and player state from a focused time window around the event, then state an explicit hypothesis about cause. Invoke when the user says "investigate the X at Y", "why did Z happen at T", "what was happening around <timestamp>", or after `triage` has surfaced a specific event worth chasing. Output is timeline + evidence + hypothesis tagged confirmed/refuted/needs-test.
---

# Investigate one event

`triage` finds *what* broke. `investigate` finds *why* — for one specific event at one specific time.

## Inputs you need

- **target** — same resolver shape as `triage` (UUID, prefix, label, IP, UA substring)
- **event timestamp** — either an explicit ISO time (`2026-05-17 18:29:27`), a clock time (`18:29:27` — assume today UTC), or a relative anchor (`the 262s stall`, in which case re-run triage and pick the matching event)

If the user said "the worst stall" without a time, re-derive: run a quick triage events query, pick the P1 event with the largest duration in info, use its ts.

## The critical idiom

**The SSE never stops if the play is still live.** A query with `from=` / `to=` will return your historical backfill rows AND then keep emitting current rows. If you just `jq` the full output, you'll see today's 23:xx data mixed in with the 18:xx data you asked for. Always:

1. Cap with `timeout`
2. Extract `data:` lines with `grep + sed`
3. **Client-side-filter to the actual window with `jq 'select(.ts >= … and .ts <= …)'`**

Skipping step 3 is the single most-common bug investigating a live player.

## Flow

### 1. Pick the window

Default: **60s before, 90s after** the event timestamp. Override:
- For stalls: pull from 30s before to (event_ts + stall_duration + 30s)
- For all_failure / restart events: 120s before, 30s after (causes precede effects)
- For HAR-derived events (http_5xx, request_*): 30s before, 30s after — narrower because there's more data per second

### 2. Pull network log for the window

```sh
PID=<player-uuid-from-resolver>
FROM="2026-05-17T18:28:30Z"   # 60s before the event
TO="2026-05-17T18:30:30Z"     # 90s after, or longer if event has duration

timeout 8 harness --insecure raw GET \
  "/analytics/api/v2/timeseries?player_id=$PID&streams=network&bundles=network&from=$FROM&to=$TO" 2>/dev/null \
  | grep '^data: ' | sed 's/^data: //' \
  | jq -c 'select(.ts >= "2026-05-17 18:28:30" and .ts <= "2026-05-17 18:30:30")' \
  > /tmp/net.jsonl
```

(Note: `harness ts`/`tail` don't accept `from`/`to` flags today — use `raw`. The CLI may add typed flags later; check `harness ts --help` first.)

### 3. Pull samples (1Hz) for the same window

```sh
timeout 8 harness --insecure raw GET \
  "/analytics/api/v2/timeseries?player_id=$PID&streams=samples&bundles=charts_minimal,lanes_v1&from=$FROM&to=$TO&stride_ms=1000" 2>/dev/null \
  | grep '^data: ' | sed 's/^data: //' \
  | jq -c 'select(.ts >= "2026-05-17 18:28:30" and .ts <= "2026-05-17 18:30:30")' \
  > /tmp/sam.jsonl
```

`stride_ms=1000` gives one sample per second — enough to see player_state / buffer / bitrate changes without drowning in data.

### 4. Surface the evidence

Four lenses, in this order. Each is one shell pipeline; show output inline as you go.

**(a) Faulted or slow requests during the window**

```sh
jq -c 'select(.faulted==1 or (.transfer_ms // 0) > 2000) | {ts, request_kind, status, transfer_ms, faulted, fault_type, path: .path[-50:]}' /tmp/net.jsonl
```

If `fault_type=client_disconnect`+`fault_action=transfer_abandoned` shows up, the player gave up on an in-flight transfer — that's almost always either bandwidth pain or a too-aggressive variant choice.

**(b) Request volume + variants over the window**

```sh
echo "by request_kind:" ; jq -r .request_kind /tmp/net.jsonl | sort | uniq -c
echo "by variant:" ; jq -r 'select(.request_kind=="segment") | .path | split("/")[-2]' /tmp/net.jsonl | sort | uniq -c
```

A burst of one variant followed by silence on it = the player abandoned that ladder. Rapid rotation through 4+ variants = ABR churn.

**(c) Player state + bitrate timeline at 1Hz**

```sh
jq -r '[.ts, .player_state, .video_resolution, .video_bitrate_mbps, .buffer_depth_s, .last_event] | @tsv' /tmp/sam.jsonl \
  | awk -F'\t' '{printf "%s  %-10s  %-9s  bw=%-5s  buf=%-5s  %s\n", $1, $2, $3, $4, $5, $6}'
```

Look for: `player_state` transitions (playing → stalled → buffering), buffer collapse rate (1s/s = normal drain; 10s/s = something other than playback is consuming buffer), unexplained bitrate jumps.

**(d) Was anything injected?**

```sh
jq -r '[.ts, .nftables_bandwidth_mbps, .nftables_pattern_enabled, .nftables_pattern_rate_runtime_mbps] | @tsv' /tmp/sam.jsonl | uniq
```

Rules out "operator hosed it" before chasing "the player did something weird".

### 5. State a hypothesis

End the investigation with one explicit paragraph:

```
Hypothesis: The 262s stall at 18:29:27 was triggered by the player
abandoning a 14MB 2160p segment that took 23.5s to transfer (xfer
finished at 18:28:56, fault_type=client_disconnect). After abandon,
the player downshifted aggressively (29.86 → 1 Mbps in two steps),
tried to climb back, then crashed again at 18:29:27. During the
stall the player WAS successfully fetching small segments (audio/540p
at <10ms) but buffer_end_s did not advance — suggests the player
couldn't decode them into the play buffer, possibly because of an
init-segment / variant boundary issue.

Tag: needs-test
Next step to confirm: re-run the 30 Mbps upshift with `harness
procedure abr-sweep ipad --rates 30,29.86 --hold 30s` while watching
`harness events ipad`. If the same client_disconnect → stall pattern
reproduces, the cause is variant 2160p being too heavy for the
shaper's current rate cap.
```

The hypothesis MUST be tagged one of:
- **confirmed** — evidence directly demonstrates the cause
- **refuted** — evidence rules out the cause we initially suspected
- **needs-test** — plausible but not provable from this data alone

`needs-test` is the most-common honest answer. If you can't test it
right now, suggest the `fault` / `shape` / `procedure` command that
would reproduce.

## What NOT to do here

- **Don't pull data outside the window** unless you have a specific
  reason. Bigger windows = more rows = jq pain + harder to spot
  patterns.
- **Don't reason without checking the shaper.** "The shaper was on"
  vs "the shaper was off" is a different investigation every time.
  Always include step 4(d).
- **Don't skip step 5.** A timeline without a hypothesis is a wall
  of text. If you can't form a hypothesis, say so explicitly and
  list what additional data you'd need.

## Common gotchas

- **Live SSE bleed-through.** Already covered. The single biggest
  source of confusion when investigating a live player.
- **`buffer_depth_s = 0` doesn't always mean empty buffer.** iOS
  AVPlayer often doesn't report this metric — use `buffer_end_s`
  (which is the playhead position of the most-distant loaded
  segment) as the truer signal. If `buffer_end_s` isn't advancing,
  the player isn't ingesting new segments even if it's "fetching"
  them.
- **`fault_counters.*` on the player record is server-side counts.**
  It tells you proxy-applied faults, not player-experienced ones.
  For player-perceived faults, use the HAR `faulted` field per row.
- **CH JSON encoding inconsistency.** `bytes_in` arrives as a JSON
  string, `faulted` arrives as a JSON number. If a jq filter
  silently returns nothing, check whether you're treating a number
  as a string (or vice versa). `tonumber` and `tostring` are your
  friends.

## See also

- `triage` — the menu that points here
- `forensics` — when "why does this keep happening" needs multi-event
  causal analysis, dispatch there instead
- `finding` — once you've nailed the hypothesis, capture it
- `.claude/standards/avplayer-quirks.md` — iOS-specific reporting
  gaps and behaviours
- `.claude/standards/abr-decision-model.md` — why downshift cascades
  happen
