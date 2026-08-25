# `content_manipulation.live_offset` on s2 wedges AVPlayer into a no-video stall

**Date:** 2026-07-12 · **Platform:** Fleet iPhone 15 sim (ipad-sim runner), AVPlayer, HLS · content `insane_newer_p200_h264`, s2 (2s segments) · via the sweep streaming pool + `char matrix` config-on-connect
**Tag:** **confirmed** (trigger isolated; n=4 failures across 3 distinct sims + 1 clean control + healthy baseline manifest)

## TL;DR

Applying the **proxy manifest live-offset** (`content_manipulation.live_offset`, the config-class `live_offset:` recipe knob) on the s2 stream makes AVPlayer **fetch the playlists but request ZERO media segments** — it never renders a frame, POSTs no `player_metrics`, and archives **0 snapshots**. The identical recipe **without** the manipulation plays cleanly. The trigger is the manipulation itself, at **every** offset value tested (2 and 6), **not** a sim wedge, a redeploy, or the sub-spec-vs-floor distinction.

Surfaced when an operator noticed a sim "not showing video" during a sweep pool run whose oracle verdict was **CLEAN** — a false negative (see §Oracle bug).

## Signature (identical across every failure)

Exactly **4 requests, all HTTP 200, all manifests, 0 media segments**:
- 2× master manifest (the config-on-connect bootstrap master carrying the play labels)
- `playlist_2s_audio.m3u8`
- `playlist_2s_360p.m3u8` (the lowest rung — AVPlayer's startup pick)
- **0×** `*.m4s` — AVPlayer never issues a single segment GET → 0 frames → 0 snapshots.

## Isolation (one variable at a time, config-on-connect on a Fleet iPhone 15 sim)

| variant | live_offset | sim | result |
|---|---|---|---|
| **control — no manipulation** | (none) | #3 | ✅ **played** — ffs 1.36s, 1435 frames, 73 segments |
| N=2 (sub-spec, below the 3×seg≈6s floor) | 2 | #4 (×2) | ❌ 0 segments, no video |
| N=6 (at the s2 floor) | 6 | #2 | ❌ 0 segments, no video |
| (original pool hit) | 2 | #1 & #4 | ❌ 0 segments, no video |

**Baseline manifest is healthy** — a direct GET of `playlist_2s_360p.m3u8` (no session/manipulation) returns 122 `#EXTINF` segments, `HOLD-BACK=9.000`, `TIME-OFFSET=-9.000`, valid PDT + `EXT-X-MAP`. So the stream is fine; only the *manipulated* session breaks.

## Hypotheses tested

- **Sim wedge / one sim gone bad** — **refuted.** Reproduced on sims #1, #2, #4 (3 distinct devices); other sims played *other* recipes fine in the same pool run.
- **Post-redeploy stale socket** (the `make deploy` earlier) — **refuted by timeline.** The deploy was ~18 min before the run; the same sim played a full clean play *after* it, in an earlier pool wave.
- **Sub-spec-specific** (offset below the holdback floor) — **refuted.** N=6 (at the floor) wedges identically to N=2 (sub-spec). It is not about being below the floor.
- **The s2 / startup / content path itself** — **refuted.** The no-manipulation control on the identical s2/startup/content path played cleanly (73 segments).
- **Confirmed trigger:** presence of `content_manipulation.live_offset` on the manifest, independent of value.

## Open sub-question (for the fix) — proxy vs. player

Not yet isolated: does the live-offset rewrite produce a **segmentless / malformed media playlist** (proxy `go-proxy` content-manipulation bug) or a **valid manifest AVPlayer mishandles** (client bug)? The next step (blocked today only by sweep Gap A/B — can't bootstrap a done/backlog experiment to grab its session port): fetch the *manipulated* `playlist_2s_360p.m3u8` through the session's shaper port and diff vs the healthy baseline — check whether `#EXTINF` segments survive and whether the rewritten `HOLD-BACK` / `EXT-X-START:TIME-OFFSET` / `MEDIA-SEQUENCE` point AVPlayer outside the available segment window. The healthy baseline has HOLD-BACK=9 / TIME-OFFSET=−9; a rewrite to a smaller offset that isn't matched by the segment list would explain a player that parses the playlist but finds nothing to start on.

Relation to prior work: the Android-TV finding documented a sub-spec manifest holdback being *clamped to the floor*; on iOS the manipulation instead **wedges** (no video) — and here even an **at-floor** value wedges, so this is a distinct, more severe manifest-manipulation defect than the earlier clamp.

## Oracle bug (secondary, independently actionable)

`harness sweep analyze` verdicted these **zero-snapshot / zero-frame** plays as **`clean`** (the pool summary showed `CLEAN`, and the probe returned `PASS`). A play that archived no snapshots / never reached first-frame is an **infra failure** and should be **`inconclusive`**, not clean — the same disposition the loop already gives a *missing* play_id, extended to a *present* play_id with empty metrics. This false-negative is why the no-video only surfaced via an operator's eyeball, not the oracle. Vindicates the standing rule: **confirm playback from `player_metrics` (TTFF / frames / position), never from PASS or a green verdict.**

## Reproduce

```
# wedges (no video, 0 segments):
matrix arm: class=config content=insane_newer_p200_h264 is.segment=s2 mode=startup \
            proxy.content_manipulation.live_offset={2|6}
# plays (control): same arm with NO content_manipulation
CHARACTERIZATION_DEVICE_UDID=<fleet-iphone-15-sim> LAUNCH_MODE=appium \
  harness char matrix <arm>.yaml   # then: harness query play <play_id>  → "no archived snapshots" = wedge
```
(Note the platform-token split: the sweep queue uses `iphone-sim`, but `char matrix` only accepts `ipad-sim` — set the arm's platform to `ipad-sim` for the standalone repro.)

## Related
- `.claude/findings/user-live-offset-honored-band-ios-2026-07-05.md` · `.claude/findings/user-live-offset-crossplatform-2026-07-07.md` — the app vs proxy live-offset levers and the 3×seg holdback floor.
- `.claude/findings/live-offset-androidtv-untestable-2026-06-15.md` — the sub-spec-holdback clamp (iOS wedges instead).
- `.claude/findings/sweep-queue-fleet-integration-gaps-2026-07-10.md` — the Gap A/B friction that blocked the manipulated-manifest fetch; the oracle-verdict gap is a sibling of Gap-family confounds.
