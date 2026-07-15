# HEVC single-pass average undershoot under tight VBV — fixed by `--two-pass`

- **Observed:** 2026-07-13
- **Content:** `insane_fpv_shots_hydrofoil_windsurfing.mkv`, apple-uniq ladder, software libx265, 6s segments
- **Status:** **confirmed** — scoped 1080p-cap A/B + full 4K 12-rung re-run
  (`insane_fpv_apple_uniq_2pass_hevc_20260713_081239`, 197m). Full 4K: avg_accuracy
  fails 10→0, undershoot −18%(top)→flat −3..−7%, measured overlap 7→1 pair, meas avg
  rose +13%(360p)..+27%(4K) toward target, VMAF held.

## Symptom

On the single-pass HEVC encode `insane_fpv_apple_uniq_hevc_20260707_085406`, the measured
average bitrate ran **12–18% below the advertised AVERAGE-BANDWIDTH**, and the shortfall
*grew* up the ladder (−4% at 360p → **−17%** at 1080p+). `ladder_audit.py` logged **10
`avg_accuracy` fails**. Because avg-keyed ABR players select on AVERAGE-BANDWIDTH, this
produced **7 overlapping rung pairs** (measured peak(N) > measured avg(N+1)) from 540p up —
the manifest hid the overlap behind an overstated average.

The peaks were NOT the problem: adv peak ≈ meas peak (Δ~0%). The tight `BUFSIZE_MULT=0.25`
(#868, a 0.25s VBV buffer) flattened peaks correctly — but left single-pass x265 no buffer
to bank bits for complex scenes, so it played conservative and **undershot the `-b:v`
target**. The *floor* dropped, not the ceiling. x264 was unaffected (±1% avg accuracy) —
this is an x265 1-pass ABR conservatism specific to small VBV.

## Fix

Added `--two-pass` CLI option to `generate_abr/create_abr_ladder.sh` (helper
`encode_two_pass_sw`, wired into the software libx264 + libx265 branches; hardware
VideoToolbox / AV1 stay single-pass with a warning). Pass 1 profiles complexity to a stats
file (`-f null`, output discarded); pass 2 distributes bits to hit `-b:v` accurately while
the SAME `maxrate`/`bufsize` VBV still holds peaks flat.

## Evidence (scoped 1080p-cap A/B, same source/ladder)

| Metric | Single-pass | Two-pass |
|---|---|---|
| `avg_accuracy` fails | **10** | **0** |
| Avg undershoot (worst) | −17% (top, worsening up ladder) | **−5 to −7%, flat** |
| Measured overlap pairs | 7 (from 540p up) | **2** (only 720↔1044↔1080) |
| VMAF | baseline | held / +~0.1 |

The 2 residual overlaps both involve **1044p, independently flagged `vmaf_inversion`
(52.8 < 720p 55.1) + `vmaf_redundant_rung`** — a junk rung to drop, not a VBV issue.
Residual ~5% undershoot is the 0.25× VBV still constraining slightly; within the ±10% gate.

## Takeaways

- `bufsize` controls peak-above-maxrate; it does NOT make the encoder *spend to target*.
  For accurate average under a tight VBV, use **two-pass** (decouples avg-accuracy from
  peak-flatness) — not a looser bufsize (which re-inflates peaks and re-widens overlap).
- Follow-up: drop the VMAF-inverted/redundant 1044p (and re-check 1440p/2124p on the full
  4K ladder, which were also inverted single-pass).

Related: [[reference_avplayer_cold_start_wedge]] (ABR selection), `.claude/standards/abr-ladder.md`.

## Update 2026-07-14 — the vmaf_inversion flags were a scoring artifact

`ladder_audit.py` scored each rung at its OWN native resolution (both distorted +
reference downscaled to the variant's res), so every rung faced a different-difficulty
target — a higher-res rung is compared to a more-detailed reference and scores lower even
when its on-screen quality is equal/better. Fixed: `measure_vmaf` now scales to a COMMON
comparison resolution (`--vmaf-compare-res source`, default = probe the mezzanine → 4K here;
`native` keeps legacy; `--vmaf-model` added for vmaf_4k). Re-scoring the two-pass 4K ladder
at common 4K: the ladder is **monotonic (3.4→53.2)** and all **3 inversions vanish**
(1044<720, 1440<1080, 2124<1440 → NONE). Native scoring had inflated the low rungs (+27 VMAF
at 360p) which compressed the top and manufactured the inversions.

**Correction:** do NOT drop 1044p/1440p/2124p on VMAF grounds — they are not wasteful.
Remaining real issues are `tight_spacing` (bitrate math) only. Caveat: absolute common-4K
scores are low (top ~53) — possibly the 1080p-trained model at 4K; re-run with
`--vmaf-model vmaf_4k_v0.6.1` to confirm the absolute scale.
