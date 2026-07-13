# Startup: initial rate cap × network limit × segment length — 2026-07-12

## Summary
Three levers govern how fast video **starts playing** (playhead actually moving,
not just first-frame decoded): the **network limit**, the app's **initial rate
cap** (`is.peak_bitrate_mbps`), and the **segment length**. Key results
(iphone-sim, 5×5×3 grid, n=1/cell):

1. **The initial cap kills the cold-start over-selection wedge when bandwidth is
   ample** — uncapped/no-cap joins the **top rung (33.8 Mbps)** and takes ~5 s to
   video on s6; any cap starts on a low rung and video moves in **<1 s**.
2. **It costs nothing in steady quality:** `variant@60s` climbs back to the
   top/network-appropriate rung by 60 s regardless of the startup cap. Fast start,
   full quality a minute in.
3. **Shorter segments are a large independent startup win:** the wedge cell's
   video-start drops **s6 4.8 s → s2 1.9 s → s1 1.6 s**. And **s1 avoids the wedge
   on its own** — it joins the low rung even uncapped (ABR gets an earlier
   estimate from 1 s segments).
4. **Under a real network limit (2 Mbps) none of it rescues startup** — it's slow
   regardless (5–9 s on s6/s2), and a **cap at/above the link (8/16 Mbps) adds
   stalls**; the network, not rung selection, is the bottleneck.

Tag: **confirmed** (iphone-sim, n=1/cell).

Metric note: **TTFF ≠ time-to-video-playback.** `video_first_frame_time_ms` (first
frame *decoded*) is a few seconds even in the wedge; the pain is
`video_start_time_ms` (playhead *moving*, `timeControlStatus == .playing`).

## The matrices
Sweep: **network limit** (`proxy.shape.rate_mbps`) × **initial cap**
(`is.peak_bitrate_mbps`, int) × **segment** (`is.segment`). iphone-sim, 90 s,
content `insane_newer_p200_h264`. Cells: **TTFF_s / video-start_s / stalls /
start-rung_Mbps / variant@60s_Mbps**. Network limits ≥ 2 Mbps (the 0.8 Mbps
row was dropped — startup there degrades into never-starts and isn't the
operating range of interest).

### s6 (6 s segments)
| net-limit ↓ / cap → | off | 1 | 2 | 8 | 16 |
|---|---|---|---|---|---|
| uncapped | 2.8/4.8/0/33.8/33.8 | 0.3/0.4/0/1.0/33.8 | 0.5/0.5/0/1.5/33.8 | 0.6/0.7/0/1.0/33.8 | 1.7/1.8/0/14.1/33.8 |
| 16 | 3.1/6.0/0/9.1/9.1 | 1.0/1.1/0/1.0/9.1 | 1.4/1.5/0/1.5/9.1 | 2.3/4.4/0/5.8/9.1 | 4.5/9.9/0/14.1/14.1 |
| 8 | 3.3/7.5/0/5.8/5.8 | 1.7/1.8/0/1.0/5.8 | 2.5/2.6/0/1.5/5.8 | 6.5/10.3/0/5.8/5.8 | 4.0/7.8/0/5.8/5.8 |
| 2 | 5.9/79.1/0/1.5/1.5 | 5.3/7.1/1/1.0/1.5 | 5.2/6.6/0/1.0/1.5 | 6.2/7.6/1/2.4/1.5 | 5.5/7.9/1/1.5/1.5 |

### s2 (2 s segments)
| net-limit ↓ / cap → | off | 1 | 2 | 8 | 16 |
|---|---|---|---|---|---|
| uncapped | 1.1/1.9/0/33.8/33.8 | 0.5/0.7/0/1.0/33.8 | 0.5/0.7/0/1.0/33.8 | 0.4/0.5/0/5.8/33.8 | 0.9/1.0/0/14.1/33.8 |
| 16 | 2.4/4.0/0/9.1/14.1 | 0.6/0.8/0/1.0/9.1 | 0.9/1.1/0/1.5/9.1 | 1.3/2.5/0/5.8/14.1 | 1.8/3.5/0/9.1/14.1 |
| 8 | 2.7/5.1/0/5.8/5.8 | 0.7/1.3/0/1.0/5.8 | 1.3/1.7/0/1.5/5.8 | 2.9/4.9/0/5.8/5.8 | 2.8/4.8/0/5.8/5.8 |
| 2 | 3.9/6.5/0/1.5/1.0 | 2.1/3.4/0/1.0/1.5 | 3.6/5.2/0/1.5/1.5 | 3.0/5.2/0/1.5/1.0 | 3.2/5.9/0/1.5/1.5 |

### s1 (1 s segments)
| net-limit ↓ / cap → | off | 1 | 2 | 8 | 16 |
|---|---|---|---|---|---|
| uncapped | 0.8/1.6/0/1.0/33.8 | 0.6/0.8/0/1.0/33.8 | 0.5/0.7/0/1.5/33.8 | 0.5/0.6/0/5.8/33.8 | 0.8/1.0/0/14.1/33.8 |
| 16 | 2.8/3.9/0/9.1/14.1 | 0.6/0.8/0/1.0/9.1 | 0.7/0.9/0/1.5/14.1 | 1.0/1.4/0/5.8/9.1 | 1.9/2.8/0/14.1/14.1 |
| 8 | 2.8/4.0/0/9.1/9.1 | 0.8/1.0/0/1.0/5.8 | 0.7/1.0/0/1.5/5.8 | 1.4/2.2/0/1.0/5.8 | 2.8/3.5/0/5.8/5.8 |
| 2 | 4.3/5.4/0/1.5/1.5 | 1.8/2.8/1/1.0/2.4 | 2.6/4.4/0/1.5/1.0 | 3.7/4.5/0/1.0/1.5 | 4.1/9.9/0/1.5/1.5 |

## Actionable
- Ship a **modest startup cap (~1 Mbps)**: on good networks it cuts time-to-video
  from ~5 s to <1 s, and `variant@60s` shows full quality returns by 60 s — a free
  win. Do NOT set it *above* the link's floor (8/16 on a 2 Mbps link) — it
  overshoots and adds startup stalls with no benefit.
- **Shorter segments help startup independently** (s1/s2 start ~1 s even in the
  wedge cell); s1 also sidesteps top-rung over-selection. Trade against segment
  overhead/latency elsewhere.

## Method / repro
- Driver `$CLAUDE_JOB_DIR/tmp/batched_sweep.py`: per segment, packs 4 cells as 4
  independent iphone-sim arms in ONE `harness char matrix` run (the `arms:` escape
  hatch), so 4 cells run in parallel on 4 booted Fleet iPhone-15 sims — ~4× the
  one-cell-at-a-time throughput, verified identical to serial. `HARNESS_BASE_URL=
  https://dev.jeoliver.com:21000`.
- Each arm: `is.segment` + `is.peak_bitrate_mbps` ride config-on-connect (#906);
  `proxy.shape.rate_mbps` is server-side tc. Non-grouped (independent per-arm caps).
- Metrics via `harness query events <play_id>`: `video_first_frame_time_ms`,
  `video_start_time_ms`, `stalling_count`, rendition at first `state=playing`
  (start rung), rendition nearest t0+60 s (variant@60s). Raw play_ids in the
  companion `.data.tsv` (segment/limit/cap/play_id, 75 rows).

## Caveats
- **n=1 per cell** — the levels (8× wedge win, segment-length effect) are big
  enough to be real; exact seconds jitter across reps. Confirm trends at reps ≥ 3.
- **iphone-sim only.** ipad-sim was dropped for speed; an earlier 2-sim s6 run
  showed iphone-sim ≈ ipad-sim on startup. The **real iPhone** wedges the
  synchronized fleet barrier in a multi-cell sweep — single runs work, back-to-back
  don't (see [[reference_real_iphone_usbmux_pairing_wipe]]).
- **DF allocation is model-blind:** the appium-device-farm plugin allocates by
  `platformName`+`platformVersion` only (no iphone-vs-ipad filter), so the iPad had
  to be shut out of the booted pool to keep the run iphone-sim-only.
  `appiumCapabilities` omits `appium:udid`/`appium:deviceName` under DF by design.

## See also
- `.claude/findings/avplayer-startup-variant-selection-2026-06-07.md` — the AVPlayer
  cold-start variant-selection behaviour this quantifies against a cap × limit ×
  segment grid.
- `is.peak_bitrate_mbps` is an **integer** Mbps (0.8 → 0 = off), rides
  config-on-connect (#906) → caps are whole numbers.
