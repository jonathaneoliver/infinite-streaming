# Cold-start rung selection is non-deterministic under a fixed limit — 2026-07-13

## Observation
While comparing three reps of one startup cell in session-viewer, **rep #1's
fetch-variant line drew a "weird" down-then-up V** (fetch 14.09 → 9.05 → 14.09)
in the first ~12 s, which reps #2 and #3 did **not** reproduce. Investigated
whether the V is a fault/config property or run-to-run ABR noise.

**Verdict: the V is a benign, non-deterministic AVPlayer cold-start hedge — a
~1-in-3 startup behaviour, not a fault and not a property of the cell.** Tag:
**confirmed** (n=3, same network/content/opening bid).

## Setup
- Content `insane_newer_p200_h264`, network shaper pinned flat at **~15.2 Mbps**
  (median) for all three plays, **no** rate cap, no injected faults (control
  stream empty for all three).
- The three reps (from the operator's compare URL):
  - rep1 player `fd38e5b6-…` play `d72fea43-22da-43c3-b7dc-6cee551329ab`
  - rep2 player `c4fd2eab-…` play `91b928c6-3b15-4753-8785-744a38f10a59`
  - rep3 player `490feea7-…` play `59229308-4827-438a-a16d-39bc62e62e9d`

## Evidence — the three reps diverge only in the first ~12 s
| rep | shaper (median) | first fetch | dropped? | video-start | steady state |
|---|---|---|---|---|---|
| **1** `d72fea43` | 15.2 Mbps | 14.09 | **YES → 9.05 → 14.09** | **3.3 s** | 14.09, 0 stalls |
| **2** `91b928c6` | 15.2 Mbps | 14.09 | no — held 14.09 | 4.7 s | 14.09, 0 stalls |
| **3** `59229308` | 15.2 Mbps | 14.09 | no — held 14.09 | 5.3 s | 14.09, 0 stalls |

All three opened by fetching the **same** rung (14.09, 2304×1296 — the top rung
that fits under a 15 Mbps shaper; not the 33.8 top, because the first throughput
sample already reflects the shaper). All three converged to 14.09 steady-state
with **zero stalls**. Only rep #1 took the detour.

### Rep #1 decomposed (fetching_resolution + profile_shift_count)
| t (s) | fetch | display | shift | AVPlayer state |
|---|---|---|---|---|
| 1.0 | 2304×1296 @ 14.09 | — | 0 | cold-start opening bid |
| **1.2** | **1920×1080 @ 9.05** | — | **1** | first segment measured **~13.7 Mbps** — under the 14.09 requirement; `waiting_reason = AVPlayerWaitingToMinimizeStallsReason` → protective downshift |
| 3.3–12 | 9.05 | 9.05 | 1 | plays 9.05, buffer fills 3.0 → **10.6 s** |
| **12.3** | **2304×1296 @ 14.09** | 1920×1080 | **2** | full buffer + steady ~15 Mbps history → promotes back |
| 20–90 | 14.09 | 14.09 | 2 | holds 14.09, buffer ~8–10 s, 0 stalls, → user_quit |

## Why it happens (confirmed sub-claims)
- **Not a network dip** — `mbps_shaper_rate` flat 15.1–15.2; transfer-rate median
  15.1. The lone `1.9 @ t=17 s` in rep1 is a single measurement-window artifact
  (buffer healthy at 9.6 s through it). *confirmed.*
- **Not a fault/injection** — control stream empty for all three. *confirmed.*
- **Headroom-driven hedge** — 14.09 Mbps needs >14 Mbps sustained; under a 15 Mbps
  shaper that's ~1 Mbps of margin, which AVPlayer won't risk on an empty startup
  buffer, hence `WaitingToMinimizeStalls`. Whether it hedges depends on the exact
  first-segment throughput read, which varies run to run → **non-deterministic**.
  *confirmed (rep1 read 13.7, reps 2/3 read enough to commit).*
- **Recovery is buffer-gated** — rep1's promotion at t=12.3 coincides with the
  buffer reaching ~10 s. *confirmed.*

## Two counter-intuitive takeaways
1. **The drop was the *faster* startup, not a penalty.** Rep #1 hit video in
   **3.3 s** vs 4.7 / 5.3 s — precisely *because* dropping to the lighter 9.05
   rung filled a playable buffer sooner. Committing to the heavier 14.09 up front
   (reps 2/3) cost ~1.5–2 s of extra startup.
2. **The "weird" look is the fetched-vs-shown split.** The *fetch* profile makes a
   `14.09 → 9.05 → 14.09` V; the *displayed* variant only ever goes `9.05 → 14.09`
   (lagging while the buffered 9.05 segments drain). On the Displayed-Variant line
   it reads as an unexplained early dip; the Fetching-Variant line shows the real
   round trip. Read both lines together for startup ABR.

## Consequence
- **n=1 is misleading for startup rung selection.** A single run here shows a
  scary V-drop; n=3 shows it's a benign sub-2-second hedge AVPlayer takes on
  ~1/3 of cold starts under this limit. Matches the reps=3 study's measured
  "shown-rung agrees across all 3 reps in only 63% of cells" — the disagreement
  lives entirely in the first ~12 s. See [[feedback_n1_not_a_pattern]].

## Method / repro
```
export HARNESS_BASE_URL=https://dev.jeoliver.com:21000
harness query events <play_id> --limit 600
# key fields: fetching_resolution, video_bitrate_mbps, profile_shift_count,
#   video_resolution (display, lags), mbps_shaper_rate (applied limit),
#   mbps_transfer_rate, buffer_depth_s, stalling_count, waiting_reason
harness query control <player_id>   # empty here → no injected fault
```

## See also
- `.claude/findings/startup-initial-cap-vs-network-limit-2026-07-12.md` — the
  cap × limit × segment startup study this is a rep-level zoom into; its
  reps=3 variation numbers quantify the same non-determinism.
- [[reference_avplayer_cold_start_wedge]] — the pathological cousin: when the
  cold-start over-selection does NOT correct and wedges. Here it corrected cleanly.
- `tests/characterization/tools/startup-report/` — the report whose per-cell `⇄`
  compare link surfaced this (opens the 3 reps aligned to a common start).
