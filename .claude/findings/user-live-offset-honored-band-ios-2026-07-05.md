# iOS user `live_offset` — when AVPlayer honors it vs. overrides it

**Date:** 2026-07-05 · **Platform:** iPhone 15 simulator (ipad-sim), AVPlayer, HLS-LL
**Signal:** `wall_offset = seekable_end_s − position_s` (the app seek's achieved offset behind the available live edge). `live_offset_s` / `true_offset_s` over-read (they include the HOLD-BACK + encode/deliver latency).

> **Followed up 2026-07-07** with a three-platform sweep (real iPhone + sim + Android TV) on `insane_newer` — real iPhone ≈ this sim data, and Android/ExoPlayer applies the offset as a native `targetLiveOffset` (much closer to live). See `.claude/findings/user-live-offset-crossplatform-2026-07-07.md`.

## TL;DR

The user (app) live-offset lever — `is.flag.live_offset_s`, a.k.a. **Settings → Advanced → Live Offset**, a seek to `liveEdge − N` — **is honored above a shallow, segment-scaled threshold and ignored/clamped below it.** The threshold is **~1–2 segment durations**, NOT the 3×-segment HOLD-BACK it was hypothesised to obey.

- **Refuted (H-floor):** "the user setting is ignored until N ≥ 3×max-seg (the HOLD-BACK: 21 on s6, 9 on s2, 6 on s1)." It is honored well below the floor.
- **Confirmed (H-seek):** honored down to ~1–2 segments (the seekable end-gap); the seek just can't place you within ~1 segment of the live edge.
- **Confirmed:** the shallow threshold **scales with segment size** — the minimum offset you can hold is **~1.7s (s1), ~3.5s (s2), ~6–8s (s6)**. Smaller segments → sit closer to live.
- **Precedence:** with both levers set, the **app seek wins the actual playhead**; the manifest (`proxy.live_offset`) sets only the advisory `recommended_offset_s`.

## Method

15 arms swept over the **sweep queue** (only possible after adding the 4 client launch knobs — codec/live_offset/peak_bitrate/starts_first_variant — to `sweep.Experiment`; see branch `feat/sweep-lab`). Plain play, no shaping; `proxy.live_offset=0` except the precedence arm. Each arm: `bootstrap --from backlog` → cold-launch `TestSweepProbe` with `CHAR_SWEEP_LIVE_OFFSET=N` on a Fleet iPhone 15 sim, 60s, then read `wall_offset` (steady-state median). YAMLs: `tests/characterization/matrix/user-live-offset.yaml` (+ `-fine`).

## Data (steady-state median)

| seg | N | wall | live_off | true_off | buf | recomm | honored? |
|---|---|---|---|---|---|---|---|
| s1 | 2 | 1.7 | 7.7 | 9.5 | 8.1 | 6 | ✅ |
| s1 | 4 | 5.0 | 11 | 11.8 | 11 | 6 | ✅ |
| s1 | 6 | 7.0 | 13 | 13.7 | 13 | 6 | ✅ |
| s2 | 2 | 3.5 | 12.5 | 14.9 | 12.5 | 9 | ❌ clamp ~3.5 |
| s2 | 4 | 3.6 | 12.6 | 14.7 | 13.1 | 9 | ❌ clamp ~3.5 |
| s2 | 6 | 6 | 15 | 18 | 15.5 | 9 | ✅ |
| s2 | 12 | 12 | 21 | 25 | 21.5 | 9 | ✅ |
| s6 | 0 | ~0 | 21 | 23 | 14 | 21 | — baseline |
| s6 | 6 | ~0 | 21 | 25 | 20 | 21 | ❌ ignored (edge) |
| s6 | 12 | 11 | 32 | 38 | 32 | 21 | ✅ |
| s6 | 18 | 16.5 | 37.5 | 43 | 38 | 21 | ✅ |
| s6 | 24 | 23 | 44 | 53 | 44 | 21 | ✅ |
| s6 | 30 | 28.5 | 49.5 | 56 | 50 | 21 | ✅ |
| s6 | 24 + proxy30 | 21 | 51 | 60.8 | 51 | 30 | ✅ **app wins** (≈24, not 30) |
| **s1** | **0 †** | 0.2 | 6.2 | **8.4** | 6.2 | 6 | — floor, **on insane_newer** |

† All other rows played `bucks_bunny` (content-pin, see caveats); the s1 N=0 floor was re-run on the **correct** `insane_newer` stream. Config numbers match bucks_bunny exactly (`recomm=6`, `wall~0`) → behaviour is content-independent, so the honored-band/threshold results stand. Only `true_off` shifts: **+~1.6 s on insane_newer** (bucks_bunny s1 N=0 was 6.8), because it includes each content's encode/deliver pipeline latency. So the behind-broadcast *absolute* numbers below under-report the real stream by ~1.6 s.

**Precedence (both levers set, `is.live_offset=24` vs `proxy.live_offset=30`):** the **app seek wins the playhead** — achieved `wall=21` (tracks the app's 24 within a segment; the pure app-24 arm gave 23), **not** the manifest's 30. The manifest offset surfaces only as `recommended_offset_s=30` (advisory) and in the deeper `live_offset=wall+recommended=51`. So the manifest sets the *recommended* position, the app seek sets the *actual* one (the seek runs after manifest bring-up). [n=1, ±1 segment]

## Metric relationships (held across every arm)

- **`live_offset_s = wall + recommended`** exactly (`recommended` = 3×seg HOLD-BACK = 21/9/6). So `live_offset_s` is "behind live *including* the advisory hold-back" — that's the fixed ~21/9/6 it always adds over `wall`.
- **`true_offset_s ≈ live_offset_s + ~5`** — extra encode/deliver latency (the documented over-read).
- **`buffer_depth ≈ live_offset_s`** — AVPlayer buffers *forward to the live edge*, so **deeper offset ⇒ proportionally deeper buffer** (~14–20s at the edge → ~50s at N=30). Second-order effect of the setting: more stall resilience + more latency + more memory, roughly linear.
- **Unforced (N=0) the app rides the live edge (wall~0), not the 21s HOLD-BACK** — the manifest offset is advisory; the user seek (and default behaviour) override it.

## Threshold, quantified

| segment (dur) | min achievable offset (wall) | **closest to broadcast (true_off)** | honored from |
|---|---|---|---|
| s1 (1s) | ~1.7 s | **~9.5 s** | N ≥ 2 (tightest requestable) |
| s2 (2s) | ~3.5 s | **~15 s** | N ≥ 6 (2 & 4 clamp to ~3.5) |
| s6 (6s) | ~6–8 s | **~23 s** | N ≥ 12 (N=6 rode the edge) |

The shallow *wall* threshold ≈ **1–2 segment durations**, well below the 3×seg HOLD-BACK.

## Behind-broadcast latency (the sports-fan view)

`wall_offset` is offset behind the *packaged edge*; the real "how far behind the live event" number is **`true_offset_s`** = wall-clock now − the playhead's `PROGRAM-DATE-TIME` (glass-to-glass latency). It decomposes as:

> **`true_off ≈ (achieved wall) + 3×seg HOLD-BACK + ~2–4 s deliver latency`**
> s1: 1.7 + 6 + 2 ≈ **9.5** · s2: 3.5 + 9 + 2 ≈ **15** · s6: 0 + 21 + 2 ≈ **23** · both-levers (app24+proxy30): 21 + 30 + ~10 ≈ **60.8**

Implications:
- **Segment size dominates liveness** — the 3×seg HOLD-BACK is the biggest term, so s1 gets ~2.5× closer to broadcast than s6 (~9.5 s vs ~23 s), no matter the offset setting.
- **The user Live-Offset setting only makes you *further* behind** — every +N adds ~N s to `true_off` (s6 N=30 → 56 s).
- **A deep manifest offset compounds it** — the precedence arm is the deepest (60.8 s) even though the app seek "won" the wall position, because `proxy=30` shifted the reference.
- Absolute numbers are this rig's encode/deliver latency (synthetic live + sim); a production LL path could shave the ~2–4 s term, but the segment-size dominance holds.
- **Validated liveness floor (correct stream):** `s1 N=0` on `insane_newer` → **`true_off ≈ 8.4 s`** (the true "closest to broadcast" on the intended content; the ~9.5 s in the threshold table is bucks_bunny's s1 N=2 and runs ~1–1.6 s low).

## Caveats / provenance

- **n=1 per arm.** Trends are monotonic within-run but not rep-confirmed.
- **`live_offset` enum:** the harness validates it against `{0,2,4,6,12,18,24,30,36,42}` (the *proxy* manifest enum, applied to the *app* lever too), so odd values / <2 can't be requested — finest shallow resolution is 2.
- **Content-pin quirk (#883) — mostly bucks_bunny:** the driver never set `CHAR_CONTENT`, so nearly all arms played `bucks_bunny` (continue-watching hero), not the pinned `insane_newer`. The requested `master_{1,2,6}s` variant + config loaded correctly regardless (verified via `manifest.master_url`; `recomm`=3×seg identical), so behaviour/thresholds are content-independent. **s1 N=0 was re-run on the correct `insane_newer` stream** (`CHAR_CONTENT` set): identical config (`recomm=6`, `wall~0`), `true_off` +~1.6 s. Net: the *config*-driven results hold on the real stream; the *absolute* behind-broadcast numbers here are bucks_bunny's — add ~1.6 s for insane_newer.
- **#793 auto-verdict does NOT score the app lever** (it reads `recommended_offset_s`, the manifest lever) — all these runs mark *inconclusive*; `wall_offset` was read manually.
- **Sim farm wedges after ~4 consecutive probes** — runs were batched with `farm.sh reset` between batches.

## Related

- [[reference_avplayer_cold_start_wedge]] · [[project_ios_startup_speed_levers]] · `docs/live-offset-testing.md` · `.claude/findings/live-offset-androidtv-untestable-2026-06-15.md`
- Two-lever model: `proxy.live_offset` (manifest HOLD-BACK, obeys the 3×seg floor) vs `is.live_offset` (this app seek, obeys the ~1-2seg end-gap).
