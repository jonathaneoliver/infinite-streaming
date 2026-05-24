# Worked example — driving the rig with the skills

> 📝 **Draft scaffold — needs a live run-through.** This is the
> rewritten skeleton against the post-#474 surface. Structure is
> final; concrete shell output is illustrative until you re-run it
> live against test-dev tomorrow. Replace each `[OUTPUT]` block with
> the real thing as you go.
>
> When you do the live run, capture into a side file with `script(1)`
> or copy from the terminal — that keeps each step's *exact* output
> in sync with what the reader will see if they paste the same prose
> at Claude.

A real transcript of one session, lightly trimmed. The user iterates
on a running iPad play through prose; Claude resolves targets, runs
the right skill, snapshots before mutating, then synthesises forensic
answers. The six project-level skills under `.claude/skills/`
(`triage`, `investigate`, `forensics`, `fault`, `shape`, `finding`)
load automatically. The `harness` CLI under `tools/harness-cli/` is on
$PATH after `make harness-cli` — Claude shells out to it for every
mutation and for archive reads.

The active rig is test-dev (`http://jonathanoliver-ubuntu.local:21000`);
the iPad is a single live session running the *Hydrofoil Windsurfing*
content.

---

## 1. Survey what's live — `harness players list`

> **User:** what sessions are running

```sh
harness --insecure players list
```

```text
[OUTPUT]
ID                                    DEVICE       CONTENT                                STATE     PORT   LAST SEEN
3bff77d6-56af-468b-…-38a9467c447b     iPad         INSANE_FPV_SHOTS_Hydrofoil_Windsurfing  playing   30181   2s ago
```

One session → "the iPad" disambiguates to `player_id=3bff77d6…`.
The resolver also accepts substrings, ≥6-char hex prefixes, `device=`
label values, or pieces of the User-Agent — same target.

---

## 2. Triage — "is anything broken right now?"

> **User:** how's the iPad doing

Invoke the `triage` skill:

```text
/triage 3bff77d6
```

The skill resolves the target, captures ~5 s of the live events SSE
into a tmpfile, buckets by severity / type, and prints a
menu-of-symptoms in <10 s. No mutation, no deep drill-down — just
"what's worth investigating?"

```text
[OUTPUT]
ipad (3bff77d6)  state=playing  buffer=12.3s  last_event=heartbeat  no errors

Last 5 min: 8 critical, 31 warning, 612 info.

Worst recent events:
  18:29:27  warning=*stall_long_midplay   2.4s
  18:52:51  warning=http_4xx              GET playlist_6s_audio.m3u8
  18:57:03  critical=*stall_severe_midplay 4.1s

Recent ABR churn: 39 upshifts, 12 downshifts — moderate.

Suggested next:
  /investigate 18:57:03   — drill the worst single event
  /forensics              — multi-event "why does this keep happening"
```

---

## 3. Snapshot before mutating — `harness fault` cycles

> **User:** can you start an HTTP 500, 1 every 15 s, on any kind of
> request

The `fault` skill always **snapshots before mutating**. Pre-state
saved to `~/.claude/state/harness/<slug>/<player_id>-<ISO8601>.json`
(slug derived from the git remote URL; `last-<player_id>.json` always
points at the most recent). One-line pre-snapshot summary lands
inline:

```text
[OUTPUT]
pre-mutation: shape=unset/unset/unset, all=none, pattern_enabled=false
```

Then the mutation, classified as "any kind of request → All tab"
(not three duplicate per-surface rules), `failures_per_seconds` mode,
`frequency=15`, `consecutive=1`:

```sh
harness --insecure fault add --player-id 3bff77d6 \
  --surface all \
  --type http_500 \
  --frequency 15 --consecutive 1 \
  --mode failures_per_seconds
```

Verification:

```sh
harness --insecure players show 3bff77d6 | yq '.failure_settings.all'
```

```text
[OUTPUT]
type: http_500
frequency: 15
consecutive: 1
mode: failures_per_seconds
```

The matching `control_events` row also lands:

```text
[OUTPUT]
ts                       source   event                     info
2026-05-19 09:32:18.214  harness  fault_rule_enabled        all:http_500
```

---

## 4. Watch the impact — `forensics` for the "why"

After ~90 s of the fault running, the user wants the causal picture
rather than a single-event drill.

> **User:** the iPad is downshifting — why? trace it back

Invoke `forensics`:

```text
/forensics 3bff77d6
```

The skill gathers events / network / control rows for the window into
tmpfiles, hits the findings library first (cheap lookup), then
dispatches to the `playback-forensics-expert` subagent with the
pre-collected evidence. The subagent reads the relevant
`.claude/standards/` files (`abr-decision-model.md`,
`hls-taxonomy.md`) and replies with a tagged hypothesis:

```text
[OUTPUT]
Hypothesis (confirmed): The downshift cascade traces directly to the
all/http_500 fault rule at port 30181. Every ~15s the proxy returned
500 on a segment fetch; AVPlayer's bufferingRateReason elevated, the
ABR module's bandwidth EWMA crashed, and the next variant probe
landed on the 1.8 Mbps rung. Three identical cycles in 90s match the
fault rule cadence 1:1.

Minimum evidence: 4 distinct `error=http_5xx` labels on
network_requests + 4 paired `info=shift_down` labels on session_events
within ±2s of each fault timestamp.

Citations: .claude/standards/abr-decision-model.md (§ "downshift after
single transient failure"), .claude/standards/avplayer-quirks.md (§
"bufferingRateReason on transient 5xx").
```

If the tag had been `needs-test`, the skill would have offered to run
a specific `harness` command to confirm — same skills-as-dispatch
pattern as triage → investigate.

---

## 5. Undo, then capture the finding

> **User:** clear it

```sh
harness --insecure fault undo --player-id 3bff77d6
```

`undo` replays the snapshot taken in step 3 — restoring the *exact*
prior state (not "clear shaping", which would erase any earlier
config). Verification mirrors step 3 and shows `all.type = none`.

> **User:** capture that — we'll want to recognise this pattern next
> time

Invoke `finding`:

```text
/finding "ipad-http500-downshift-cascade-2026-05-19"
```

The skill writes a structured note to `.claude/findings/<slug>.md`
with: keywords, the resolved hypothesis from step 4, the evidence
shape, the standards it cited, and the harness command sequence that
reproduces it. Future `forensics` invocations grep that library
*before* dispatching the subagent — turning this 90-second debug
into a tens-of-milliseconds lookup the next time something looks
similar.

---

## 6. Where else to look

- The dashboard's PlayLog at `https://<host>:21000/dashboard/v3/testing.html`
  shows the same events / network / control rows interleaved on one
  chronological scroll, with severity-tinted label chips matching the
  skill output.
- The Sessions page at
  `https://<host>:21000/dashboard/v3/sessions.html` aggregates by
  play_id; the tristate labels filter lets you query "shows me plays
  that had error=http_5xx AND has-not info=*pattern_enabled" with two
  clicks.
- For long-running soak analysis, `harness archive plays --player-id
  X --from ... --to ...` returns the play summaries the picker page
  surfaces, ready to pipe through `jq` for batch analysis.

---

## Cheat sheet — when to invoke which

| Question | Skill | Cost |
|---|---|---|
| "What's wrong with the iPad?" | `triage` | <10s, no subagent |
| "Why did the player stall at T?" | `investigate` | ~5s, no subagent |
| "Why does this keep happening?" | `forensics` | ~30s, subagent |
| "Drop in an HTTP 500 every 15s" | `fault` (snapshot+undo built in) | live |
| "Throttle to 5 Mbps" | `shape` (snapshot+undo built in) | live |
| "Capture what we just learned" | `finding` | <1s |

Direct `curl` against `/api/...` is always available as an escape
hatch, but the skills exist precisely to bake in the "snapshot before
mutating" and "look at the findings library first" disciplines that
nobody remembers consistently by hand.
