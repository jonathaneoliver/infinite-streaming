---
name: forensics
description: Multi-event causal analysis — "why does X keep happening", "compare A vs B", "find the pattern across these failures", "is this related to a change". Gathers events/network/samples + relevant past findings + applicable standards, then dispatches to the `playback-forensics-expert` subagent for hypothesis generation. Invoke when one event isn't enough (use `investigate` for single events) and the question is "why" not "what". Returns a tagged hypothesis plus a suggested test to confirm or refute.
last_reviewed: 2026-05-19
---

# Forensics — dispatch a "why" question to the playback expert

`investigate` answers "what happened at time T". Forensics answers "why does this pattern keep recurring" or "why is A different from B". Different question, different cost: forensics calls out to a Sonnet subagent (`playback-forensics-expert`) primed with HLS/AVPlayer/ABR knowledge. The main session stays cheap; the subagent does the reasoning.

**Conventions:** this skill follows `.claude/skills/CONVENTIONS.md`. Most load-bearing for forensics: pre-fetch all evidence before dispatching (the subagent can't run shell); tag the returned hypothesis verbatim with `confirmed` / `refuted` / `needs-test`; grep `.claude/memory/` and `.claude/findings/` *before* dispatching so we don't burn subagent tokens on already-answered questions.

## When to use this over `investigate`

| Question | Skill |
|---|---|
| "Why did the player stall at 18:29:27?" | `investigate` |
| "Why does the iPad keep stalling on 2160p segments?" | `forensics` |
| "What was happening when the buffer collapsed?" | `investigate` |
| "Compare the iPad's stall pattern to the Roku's" | `forensics` |
| "Is this related to the change we made yesterday?" | `forensics` |

Rule of thumb: if the answer requires comparing two or more event clusters, or correlating with a non-obvious cause (a code change, a platform behaviour, a recurring fault pattern), it's forensics. If the answer is "look at what happened in this 90-second window", it's investigate.

## Flow

### 1. Resolve scope

- Which player(s)? Single = "why does X keep stalling". Multi = comparison.
- Which time window? Default 30 min ending at "now"; expand if the user named a specific range.
- Which play_id? Optional. Restrict if specified, else span all plays in window.

If the user named a player but not specific plays, **enumerate candidate plays first** using the v2 plays endpoint — it makes the comparison set concrete and surfaces which plays carry which trouble signals before you decide what to gather:

```sh
harness --insecure --json query plays \
  --player-id "$PID" \
  --from "$FROM" --to "$TO" --limit 50 2>/dev/null \
  | jq '.items[] | {play_id, started_at, classification, last_player_error,
                     stalls, restart_count, frozen_count, error_event_count,
                     bitrate_shifts, dropped_frames}'
```

Pick the 2-4 plays with the strongest signal contrast (e.g. one "clean" + one "stalled" for an A/B; or all 3 plays where `last_player_error` is non-empty for a same-error comparison). State which plays you picked and why before gathering — it tightens the subagent's prompt and prevents over-gathering on plays that don't help the question.

### 2. Recall first (mandatory)

Before pulling fresh data or dispatching, check the findings library:

```sh
# pull keywords from the user's question
grep -ril <keyword-1> .claude/findings/
grep -ril <keyword-2> .claude/findings/
```

If the library already answers the question, return that answer instead of dispatching. Cite the finding file. Don't burn subagent tokens on a question we've already answered.

### 3. Gather the data the subagent needs

The subagent is **read-only** — it can't run harness or curl. You have to gather everything upfront.

For each player in scope:

```sh
# Events for the window
timeout 6 harness --insecure --json ts <t> --streams events --bundles events 2>/dev/null \
  | jq -c 'select(.type and .priority and .ts >= "<FROM>" and .ts <= "<TO>")' \
  > /tmp/forensics-events-<t>.jsonl

# Network for the window
timeout 8 harness --insecure raw GET \
  "/analytics/api/v2/timeseries?player_id=$PID&streams=network&bundles=network&from=<FROM_ISO>&to=<TO_ISO>" 2>/dev/null \
  | grep '^data: ' | sed 's/^data: //' \
  | jq -c 'select(.ts >= "<FROM>" and .ts <= "<TO>")' \
  > /tmp/forensics-net-<t>.jsonl

# Samples at 1Hz for the window
timeout 8 harness --insecure raw GET \
  "/analytics/api/v2/timeseries?player_id=$PID&streams=samples&bundles=charts_minimal,lanes_v1&from=<FROM_ISO>&to=<TO_ISO>&stride_ms=1000" 2>/dev/null \
  | grep '^data: ' | sed 's/^data: //' \
  | jq -c 'select(.ts >= "<FROM>" and .ts <= "<TO>")' \
  > /tmp/forensics-sam-<t>.jsonl

# AVMetrics for the window — the highest-resolution failure-timing feed
# (CoreMedia error codes, VariantSwitchStart-without-complete). The
# heartbeat/sample feed is BLIND to these, so on a "stalled, cause
# unknown" wedge this is the evidence that decides it (e.g. -12880 =
# wedges permanently vs -16839 = ugly-but-recovers). Bounded query —
# closes on its own, no SSE --max-time hack. iOS-only. #693.
harness --insecure --json query avmetrics <PLAY> --from <FROM_ISO> --to <TO_ISO> --limit 2000 2>/dev/null \
  | jq -c 'select(.ts >= "<FROM>" and .ts <= "<TO>")' \
  > /tmp/forensics-avm-<t>.jsonl
# (No play scoped? Pull error-bearing events across the window instead:
#  harness --insecure --json query avmetrics --event-type ErrorEvent --from <FROM_ISO> --to <TO_ISO>)
```

(Same SSE-bleed-through guard as `investigate` — always filter ts in jq.
The AVMetrics query is bounded NDJSON, so it needs no `timeout` wrapper.)
See `.claude/standards/avmetrics-forensics.md` for which event types carry
which signal and how to read the CoreMedia codes.

### 4. Identify applicable standards

Based on the user's question, name the standards docs the subagent should read:

- Anything about iOS / iPad / AVPlayer → `.claude/standards/avplayer-quirks.md`
- ABR / variant choice / downshift / upshift → `.claude/standards/abr-decision-model.md`
- m3u8 / manifest behaviour → `.claude/standards/hls-taxonomy.md`
- codec rejection / playback init failure → `.claude/standards/codec-strings.md`
- iOS wedge / "stalled, cause unknown" / CoreMedia error reading → `.claude/standards/avmetrics-forensics.md` (pairs with the `/tmp/forensics-avm-<t>.jsonl` evidence)

### 5. Dispatch to the subagent

Invoke the `playback-forensics-expert` agent (via the Agent tool, `subagent_type: playback-forensics-expert`). The prompt should be self-contained — the subagent doesn't see your conversation history.

Prompt template:

```
The user is asking: "<verbatim user question>"

Scope:
- Player(s): <list with shortids + UA>
- Window: <FROM> → <TO>
- play_id(s): <if scoped>

Pre-gathered evidence (already filtered to the window — DON'T re-query):
- /tmp/forensics-events-<t>.jsonl ({N} events)
- /tmp/forensics-net-<t>.jsonl ({M} network rows)
- /tmp/forensics-sam-<t>.jsonl ({K} samples at 1Hz)
- /tmp/forensics-avm-<t>.jsonl ({A} AVMetrics events — iOS only; CoreMedia errors + variant-switch events)

Findings library hits (already grepped):
- <file1.md>: <one-line summary>
- <file2.md>: <one-line summary>
(if none: "no prior findings for these keywords")

Standards to read (cite when used):
- .claude/standards/<applicable>.md

Output format (mandatory):
- Hypothesis (one paragraph, tagged confirmed | refuted | needs-test)
- Minimum evidence that would confirm or refute
- Suggested next harness command if needs-test
- Citations to standards / findings you used
```

### 6. Surface the subagent's reply

Don't paraphrase the hypothesis — quote it verbatim. The user is going to take action based on the exact tag (confirmed → ship a fix; refuted → drop the hypothesis; needs-test → run the suggested command).

If the subagent's reply tag is `needs-test`, offer to run the suggested test:

```
The expert's hypothesis is needs-test. To confirm, run:

  harness procedure abr-sweep ipad --rates 30,29.86 --hold 30s

Run it now? [y/n]
```

If the user says yes, run it, then re-`investigate` the resulting event(s) to confirm.

## When forensics returns "I need more data"

If the subagent says "the evidence is insufficient, I need X", don't dispatch again immediately. Tell the user what's missing, ask whether to:
- Pull a wider window of the same streams
- Pull additional streams (e.g. you only gave events but it needs network)
- Capture a fresh test (operator runs a reproducible scenario)

Then re-gather and re-dispatch.

## Cost discipline

Forensics is the most-expensive skill — Sonnet reasoning + larger context. Use only when:
- The question genuinely needs multi-event causal analysis
- The findings library doesn't already answer it
- The data is already gathered (don't dispatch on incomplete evidence and hope)

For "what" questions, `triage` + `investigate` are cheaper and usually enough.

## What NOT to do here

- **Don't skip the findings recall.** The library exists to make forensics cheaper over time; bypassing it makes every "why" question pay full price.
- **Don't let the subagent run shell commands.** It's read-only by design. If it asks for more data, gather it yourself and re-dispatch.
- **Don't summarise the hypothesis in your own words.** Quote it. The tag matters; paraphrasing weakens the signal.
- **Don't dispatch without standards pointers.** The subagent reasons better with citable ground truth than without.

## See also

- `triage` / `investigate` — gather data BEFORE dispatching to forensics
- `finding` — capture the forensics result if it reached a confirmed hypothesis
- `.claude/agents/playback-forensics-expert.md` — the subagent's system prompt + tool allowlist
