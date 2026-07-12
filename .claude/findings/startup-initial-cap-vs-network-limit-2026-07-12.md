# Startup: initial rate cap vs network limit — sim matrix — 2026-07-12

## Summary
An **initial rate cap** (`is.peak_bitrate_mbps`, the app-side startup bitrate
clamp) is a large startup win **only when there is spare bandwidth** — it stops
AVPlayer over-selecting the **top rung** at join and cuts **time-to-video** from
~5 s to **~0.6 s** (≈8×). Under a real **network limit** (≤2 Mbps) the cap is
**neutral-to-harmful**: startup is slow regardless because the network, not rung
selection, is the bottleneck, and an over-large clamp under a tight pipe can make
video **never** reach sustained playback. iphone-sim ≈ ipad-sim. Tag:
**confirmed** (sims, n=1 per cell).

Distinction that matters: **TTFF ≠ time-to-video-playback.** `video_first_frame_time_ms`
(first frame *decoded*) is a few seconds even in the wedge case; the pain is in
`video_start_time_ms` (playhead *actually moving* — `timeControlStatus == .playing`),
which is what the operator perceives as "an age to get to video."

## The matrix
Sweep: **network limit** (`proxy.shape.rate_mbps`) × **initial cap**
(`is.peak_bitrate_mbps`, int) × platform. One grouped 3-arm run per cell, 90 s,
content `insane_newer_p200_h264`. Cells: **TTFF_s / video-start_s / stalls / start-rung_Mbps**.

### iphone-sim
| net-limit ↓ / init-cap → | off | 1 Mbps | 2 Mbps |
|---|---|---|---|
| uncapped | 3.3 / 4.8 / 0 / 33.8 | 0.6 / 0.6 / 0 / 1.0 | 0.5 / 0.5 / 0 / 1.5 |
| 8 Mbps | 3.1 / 6.4 / 0 / 33.8 | 1.7 / 1.8 / 0 / 1.0 | 2.3 / 2.6 / 0 / 1.5 |
| 2 Mbps | 4.1 / 6.9 / 1 / 1.0 | 5.0 / 7.4 / 1 / 1.0 | 6.9 / 9.3 / 1 / 1.5 |
| 0.8 Mbps | 11.0 / 66.5 / 0 / 1.0 | 14.5 / 30.1 / 0 / 1.0 | 49.3 / never / 0 / – |

### ipad-sim
| net-limit ↓ / init-cap → | off | 1 Mbps | 2 Mbps |
|---|---|---|---|
| uncapped | 2.7 / 4.9 / 1 / 33.8 | 0.6 / 0.6 / 0 / 1.0 | 0.4 / 0.4 / 0 / 1.5 |
| 8 Mbps | 2.9 / 6.2 / 0 / 5.8 | 1.7 / 1.8 / 0 / 1.0 | 2.3 / 2.6 / 0 / 1.5 |
| 2 Mbps | 4.6 / 7.5 / 0 / 1.0 | 5.3 / 8.7 / 0 / 1.0 | 6.9 / 9.3 / 0 / 1.5 |
| 0.8 Mbps | 11.2 / 18.9 / 1 / 1.0 | 16.6 / 22.8 / 1 / 1.0 | 48.3 / never / 0 / – |

## Reading it
1. **Cap kills the over-selection wedge when bandwidth is ample.** Uncapped + no
   cap → joins the **top rung (33.8 Mbps)**, ~5 s to video-start. Any cap → joins
   the **1–1.5 Mbps rung**, video moves in **~0.6 s**. ~8× faster to video.
2. **Under a network limit (≤2 Mbps) the cap does not help** — startup is 7–9 s
   regardless, slightly worse with the higher cap (rung selection isn't the
   bottleneck). A stall shows up at 2 Mbps.
3. **Severe (0.8 Mbps) is punishing:** no-cap 19–66 s to video-start; **cap=2
   never reaches sustained playback in 90 s** — the 2 Mbps clamp overshoots a
   0.8 Mbps pipe.
4. **iphone-sim ≈ ipad-sim**; worst outlier iphone-sim @ 0.8/no-cap (66 s vs
   iPad 19 s).

Actionable: ship a **modest** startup cap (~1 Mbps) to dodge the top-rung
over-selection on good networks; do NOT set it *above* the expected floor on
constrained links, or startup stalls / never-starts.

## Method / repro
- Driver: `$CLAUDE_JOB_DIR/tmp/startup_sweep_sims.sh` (generates one `ss_L*_C*.yaml`
  matrix per cell, runs `harness char matrix`, records play_ids; inter-cell
  unblock + settle). `HARNESS_BASE_URL=https://dev.jeoliver.com:21000`.
- Per-cell matrix = a `groups:` block, control iphone-sim + variant ipad-sim,
  `defaults: {is.peak_bitrate_mbps: C, proxy.shape: {rate_mbps: L}}` — the startup
  cap rides config-on-connect (#906), the network limit is server-side tc.
- Metrics pulled per play via `harness query events <play_id>`:
  `video_first_frame_time_ms` (TTFF), `video_start_time_ms` (time-to-video),
  `stalling_count`, and rendition at first `state=playing` (start rung).

## Caveats
- **n=1 per cell** — levels (8× wedge win, never-start at 0.8/cap=2) are large
  enough to be real; exact seconds jitter across reps. Confirm trends at reps ≥ 3.
- **Sims only.** The real iPhone (iphone) was excluded: it wedges the synchronized
  fleet HOME barrier when run back-to-back across 12 cells (single runs work; a
  12-cell relaunch sweep does not — see [[reference_real_iphone_usbmux_pairing_wipe]]).
  A real-device spot-check on the two telling cells (uncapped/off vs uncapped/cap=1)
  is the outstanding follow-up.

## See also
- `.claude/findings/avplayer-startup-variant-selection-2026-06-07.md` — the AVPlayer
  cold-start variant-selection behaviour this quantifies against a cap × limit grid.
- `is.peak_bitrate_mbps` round-trips through config-on-connect (#906); it is an
  **integer** Mbps (the app truncates — 0.8 → 0 = off), so caps are whole numbers.
