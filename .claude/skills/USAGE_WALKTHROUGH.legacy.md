# Worked example — driving the rig with the skills (LEGACY — feat/claude-skills-437)

> ⚠️ **Reference / archive only.** This is the original walkthrough
> from `feat/claude-skills-437` when there were two mega-skills
> called `harness-control` and `session-forensics`. The dev branch
> has since split those into six focused skills and added the
> `harness` CLI. The current canonical walkthrough lives at
> [`USAGE_WALKTHROUGH.md`](./USAGE_WALKTHROUGH.md) — keep this file
> around for now as a reference while the new one is being polished
> against a live test-dev run; can be deleted once the new file is
> battle-tested.

A real transcript of one session, lightly trimmed. The user iterates on a running iPad play through prose; Claude resolves targets, snapshots, mutates, then synthesises forensic answers. Both skills (`harness-control`, `session-forensics`) are loaded automatically because they live under `.claude/skills/`.

The active rig is test-dev (`http://jonathanoliver-ubuntu.local:21000`); the iPad is a single live session running the *Hydrofoil Windsurfing* content.

---

## 1. Resolve the target — "the work iPad"

> **User:** what sessions are running

```bash
curl -s http://jonathanoliver-ubuntu.local:21000/api/sessions \
  | jq '[.[] | { session_id, player_id, play_id, device, content, state, port, control_revision }]'
```

| sid | Device | Content | State | Port |
|---|---|---|---|---|
| `1` | iPad (`73B0F16B…1999`) | `INSANE_FPV_SHOTS_Hydrofoil_Windsurfing…` | playing | `30181` |

One session → "the iPad" disambiguates to `sid=1`. The skill records the resolution in case you ask follow-up questions ("the windsurf session", "the player at 192.168.0.131" — same target).

---

## 2. Throttle — and snapshot first

> **User:** can you throttle the iPad network to 5 Mbps

The skill **always snapshots before mutating**. The pre-mutation state is saved at `~/.claude/state/harness/<slug>/1-<ISO8601>.json` (and `last-1.json` is updated). The slug is derived from the git remote URL — for this repo, `jonathaneoliver-infinite-streaming`. One-line summary surfaced inline:

```text
pre-mutation: shape=unset/unset/unset, all=none, pattern_enabled=false
```

Then the mutation:

```bash
curl -X POST http://jonathanoliver-ubuntu.local:21000/api/nftables/shape/30181 \
  -H 'Content-Type: application/json' \
  -d '{"rate_mbps":5.0,"delay_ms":0,"loss_pct":0}'
```

Verification (`/api/nftables/port/30181`):
- `bandwidth_limit: "5Mbit"`, `delay_ms: null`, `packet_loss: null`, `pattern_enabled: false`

**Why this matters:** without the snapshot, "undo" would just mean "clear shaping" — destroying any earlier configuration the user had. With it, undo replays the exact prior state, including any pre-existing fault rules.

---

## 3. Inject 1 × 500 every 15 s on any request

> **User:** and a HTTP error 500 1 for every 15s on any type of request

Three bits of judgement the skill makes implicitly:

1. **"Any type of request"** → the **All** tab (`all_*` fields), not three duplicated per-surface payloads. When `all_failure_type != "none"`, go-proxy applies it to every HTTP request and ignores the per-kind tabs.
2. **"1 every 15 s"** → mode `failures_per_seconds`, `frequency=15`, `consecutive=1`. Not `seconds` — that mode means "fail for 15-second *windows*", which is a different meaning.
3. **Snapshot first.** Saved with shape=5Mbps preserved.

```bash
curl -X POST http://jonathanoliver-ubuntu.local:21000/api/failure-settings/1 \
  -H 'Content-Type: application/json' \
  -d '{
    "all_failure_type":"500",
    "all_failure_frequency":15,
    "all_consecutive_failures":1,
    "all_failure_mode":"failures_per_seconds"
  }'
```

`control_revision` advances → the player will pick up the new rule on its next poll.

---

## 4. Cross-skill — what's actually failing on the wire

> **User:** can you look at the HAR networking events and show me the failing transfers

This is `session-forensics` territory; the skill keeps the same target context. It pulls `/analytics/api/network_requests` and filters `faulted=1 OR status >= 400`:

| ts | Kind | Status | Fault | Path |
|---|---|---|---|---|
| `15:11:44.921` | segment | 200 | `client_disconnect / transfer_abandoned` | `2160p/segment_00007.m4s` |
| `15:12:47.961` | manifest | 500 | `http_500` | `playlist_6s_audio.m3u8` |
| `15:13:08.971` | manifest | 500 | `http_500` | `playlist_6s_audio.m3u8` |
| `15:13:29.999` | manifest | 500 | `http_500` | `playlist_6s_audio.m3u8` |
| `15:13:51.025` | manifest | 500 | `http_500` | `playlist_6s_720p.m3u8` |

Two stories the skill calls out without being asked:

- **The `client_disconnect` at `15:11:44`** is a second-order effect of the throttle applied 8 s earlier — the iPad started a 15.7 MB 4K segment, the cap made it take 18 s+, the player gave up at 88% to chase the next segment. That's what triggered the big 4K → 720p downshift later.
- **The 500 cadence is ~21 s, not 15 s.** Floor is exactly 15 s (the configured frequency); the visible cadence is the iPad's audio-playlist poll rate.

---

## 5. Pick the interesting plays out of the archive

> **User:** what sessions do I have in the last hour

→ four plays, all on `sid=1`.

> **User:** sort by most interesting

The skill ranks by classification (`favourite > interesting > other`) then by signal volume (stalls + failures + downshifts + errors + user marks).

> **User:** only show the interesting ones from the last 4 hours

| play_id | When | Signal | Stalls | Down/Up | Notes |
|---|---|---|---|---|---|
| `0169006b…b8e4` | 11:27 → 11:32 | **45** | 0 | 14/12 | hls levelLoadError; 14 downshifts in 5 min — chaotic ABR |
| `08f5b5b2…122d` | 11:22 → 11:27 | 29 | 0 | 6/1 | hls levelLoadError |
| `918C4DFE…BDEA` | 14:50 → 14:59 | 9 | 1 | 2/0 | every fault surface fired (404 storm) |
| … | … | … | … | … | … |

Three signal clusters jump out, with the highest-signal one being a 5-minute 14-downshift fight.

---

## 6. Forensic write-up of the throttled play

> **User:** can you look at the session I throttled and tell me about how it went

The skill walks `/analytics/api/session_heatmap` → `/session_events` (player-effect rows: downshift / upshift / stall / restart / error) → `/snapshots` for rendition timeline.

**Stats it surfaces:**

- 0 stalls, 0 player errors, 0 restarts — so far
- 1 startup buffering event (0.45 s)
- 10 distinct downshifts, 6 distinct upshifts
- ~1,200 fault rows across the throttled window (peaks of 50 / 24-s bucket)

**Story it tells:**

- 12 minutes clean → throttle lands → first downshift `29.86 → 1.84 Mbps` in 27 s (correct: cap forbids 4K-tier)
- 500 storm enabled at `15:12:41` → no stalls, but ABR oscillates between 1.0 / 1.84 / 3.46 Mbps as the player retries 500s
- The repeated probe-up cycles to 3.46 Mbps are interesting — either an over-eager bandwidth estimator or the throttle's burst behaviour briefly looking generous.

**Math it does:** 5 Mbps cap vs ~1.84 Mbps decoded → ~2.7× headroom in steady state → no stalls expected, none observed.

---

## 7. Disprove negatives

> **User:** for the active session, can you find any errors not caused deliberately

The skill filters `faulted=0` errors and `error / restart / timejump / user_marked` effect events. Result: zero `faulted=0` errors, zero player-side error events. The only borderline event (a `client_disconnect` on a 4K segment) is itself attributable to the throttle. Verdict: no genuinely unexplained errors.

> **User:** can you tell if there are any HTTP requests using byte ranges?

Scan over 1,113 requests: zero `Range:` headers, zero `Content-Range:` responses, zero HTTP 206 — the iPad is fetching whole objects only, as expected for HLS/CMAF.

---

## 8. Why playback stopped

> **User:** playback has stopped why?

Live state: `state=paused, position=1732s, buffer_depth=0, player_restarts=2`.

Last 90 s of timeline:

| Time | Event |
|---|---|
| `15:24 – 15:26:20` | 500s on every bitrate playlist in turn |
| `15:26:00` | upshift 1.0 → 1.84 Mbps |
| `15:26:20` | aggressive upshift 1.84 → **7.06 Mbps** |
| `15:26:38` | upshift 7.06 → **15.36 Mbps** ← above the 5 Mbps cap |
| `15:26:38` | `client_disconnect` on 1440p segment 40 |
| `15:26:44` | `client_disconnect` on 2160p segment 40 |
| `15:26:51` | **CoreMediaErrorDomain -15628** — playback errored out |

Diagnosis:

1. The 500s arrive instantly (< 2 ms TTFB). The player's bandwidth estimator interpreted "manifests responding super fast" as headroom and probed past the throttle ceiling to 15.36 Mbps.
2. When it tried to actually pull a multi-MB 1440p / 2160p segment at that estimate, the throttle bit. Two consecutive 4K-class segments were abandoned.
3. The buffer was already at 0 (the cap had been keeping it shallow for 15 min). With no cushion, the decode pipeline ran out of samples and threw -15628.
4. Two auto-reloads couldn't recover because the underlying conditions hadn't changed.

**Crucially: neither fault alone causes this.** Throttle alone → ABR settles at 1.84 Mbps and plays. 500s alone → player retries the next manifest, no harm. *Together* → the 500 latency confuses the bandwidth estimator, the player overshoots the cap, the cap kills high-tier segments, the buffer drains, playback errors.

---

## 9. Undo

> **User:** can we undo those commands to the test harness

The skill replays the most recent snapshot — three POSTs in order:

1. `POST /api/failure-settings/1` with the snapshot's failure-settings fields
2. `POST /api/nftables/shape/30181` with the snapshot's `rate_mbps / delay_ms / loss_pct` (zero everywhere = clear shaping)
3. `POST /api/nftables/pattern/30181` with `{steps:[]}` (no pattern was active in the snapshot)

Verification confirms all five fault surfaces back to `none / 0 / 0 / failures_per_seconds`, shaping cleared. `control_revision` advances; the player picks up the clean state on its next poll.

---

## What this transcript demonstrates

| Skill | Capability shown |
|---|---|
| harness-control | Resolve target by device label · pick correct **All** vs per-surface field set · pick correct mode for "1 per N seconds" · snapshot before every mutation · undo by replaying snapshot · clear with explicit zeroing of every schedule field |
| session-forensics | Filter network requests by faulted/status · cross-reference fault-injection events with on-the-wire 500s · sort archive plays by classification + signal · synthesise a forensic write-up of one play · disprove negatives ("no real errors") · diagnose a death-spiral stall by walking causes-just-before-effects |

The bigger pattern: each skill knows enough field semantics, endpoint shapes, and convention (snapshot, target resolution) that a non-trivial test scenario can be driven and analysed in plain English, without anyone touching the dashboard or remembering URL structures.
