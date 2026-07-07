# User `live_offset` across iPhone (real) · iPhone (sim) · Android TV — 2026-07-07

**Date:** 2026-07-07 · **Platforms:** real iPhone 15 (AVPlayer), iPhone 15 sim (AVPlayer), Google TV Streamer (ExoPlayer/Media3) · HLS-LL, content `insane_newer_p200_h264`
**Levers:** app live-offset (`is.flag.live_offset_s` — Settings → Advanced → Live Offset). iOS applies it as a **seek** to `liveEdge − N`; Android applies it as ExoPlayer's native **`LiveConfiguration.targetOffsetMs`**.
**Metrics (per play):** `wall = seekable_end_s − position_s` (distance to the last-advertised segment) · `true_offset_s = now − PROGRAM-DATE-TIME` (glass-to-glass, the sports-fan number) · `buffer_depth_s` (loaded − position) · `recommended_offset_s` (what the player targets).

## TL;DR

**How close you sit to the live broadcast for a dialed offset N is a *platform × segment* function, not just N.**

- **iOS (AVPlayer):** `true_off ≈ N + 3×seg-holdback + ~4–6s pipeline`. The offset is a **seek behind the seekable edge, which already sits 3×seg behind live** → you're always ~holdback further back than you asked. Holdback = **21 / 9 / 6s** for **s6 / s2 / s1**.
- **Android TV (ExoPlayer):** `true_off ≈ N` on coarse segments. `targetLiveOffset` sets latency **directly and overrides the manifest holdback** (`recommended_offset_s = N`, not 3×seg). **~28s closer to live than iOS on s6.**
- **The gap is segment-dependent and collapses on s1:** on the 1s LL rung the holdback is only ~6s, so iOS is barely penalized and Android can't beat the ~6s LL floor → **all three converge to ~10–15s behind**.
- **Real iPhone ≈ iPhone sim** on every metric → AVPlayer behaves identically on hardware and sim; all prior sim conclusions hold.
- **Buffer trade:** iOS carries a **deep buffer that scales with N**; Android runs a **lean, ~flat LL buffer** (~2× less on s1/s2).
- **Precedence (both levers set):** app lever wins the playhead on both. On iOS the manifest holdback still shows in `recommended_offset_s`; on Android the app target fully overrides it.

## Data — steady-state, all `insane_newer` (median `wall` / median `true` / **mean** `buf` / median `recomm`)

### s6 (6s segment · holdback 21)
| N | iPhone (real) | iPhone (sim) | Android TV |
|---|---|---|---|
| 0 | −0.5 / 28.8 / 20.1 / 21 | 97.3 / 124.9 / 50.4 / 21 *(startup-join artifact)* | 17.9 / 24.8 / 16.8 / 23.3 |
| 6 | 3.1 / 31.9 / 23.4 / 21 | 8.5 / 36.6 / 28.4 / 21 | 7.6 / **10.0** / 6.0 / 6 |
| 12 | 11.2 / 40.1 / 31.3 / 21 | 13.5 / 40.6 / 33.5 / 21 | 5.8 / **13.9** / 9.1 / 12 |
| 18 | 17.7 / 43.4 / 37.6 / 21 | 17.2 / 40.5 / 36.2 / 21 | 11.0 / **18.1** / 9.5 / 18 |
| 24 | 22.1 / 53.0 / 42.4 / 21 | 28.5 / 55.9 / 48.1 / 21 | 16.2 / **24.0** / 14.4 / 24 |
| 30 | 29.0 / 58.0 / 49.1 / 21 | 29.7 / 57.5 / 49.1 / 21 | 27.7 / **30.1** / 26.5 / 30 |
| UX (24 + proxy 30) | 22.2 / 62.8 / 51.2 / **30** | 27.0 / 63.0 / 51.9 / **30** | 21.8 / **24.1** / 20.1 / **24** |

### s2 (2s segment · holdback 9)
| N | iPhone (real) | iPhone (sim) | Android TV |
|---|---|---|---|
| 2 | 1.0 / 13.3 / 9.1 / 9 | 1.7 / 13.6 / 10.5 / 9 | 5.0 / 7.6 / 3.6 / 4.1 |
| 4 | 4.5 / 16.3 / 12.5 / 9 | 3.0 / 15.1 / 12.1 / 9 | 4.3 / 7.2 / 3.5 / 4.0 |
| 6 | 5.9 / 17.8 / 13.9 / 9 | 6.7 / 18.2 / 14.8 / 9 | 6.0 / 8.5 / 4.5 / 6 |
| 12 | 11.1 / 23.3 / 19.2 / 9 | 11.5 / 23.6 / 20.4 / 9 | 9.7 / 12.1 / 8.8 / 12 |

### s1 (1s LL segment · holdback 6)
| N | iPhone (real) | iPhone (sim) | Android TV |
|---|---|---|---|
| 2 | 3.0 / 10.5 / 8.2 / 6 | 2.7 / 10.9 / 8.6 / 6 | 5.0 / 12.7 / 7.3 / 5.1 |
| 4 | 5.0 / 12.8 / 10.4 / 6 | 4.2 / 12.1 / 10.0 / 6 | 6.1 / 13.6 / 4.6 / 6.2 |
| 6 | 5.7 / 13.3 / 10.7 / 6 | 6.2 / 14.6 / 12.1 / 6 | 6.2 / 15.0 / 5.2 / 6 |

## Reading the numbers

- **`recommended_offset_s` is the tell for the mechanism.** iOS = **3×seg holdback** (21/9/6), flat across N — the manifest governs. Android = **≈ N** on s6/s2 — the app's `targetLiveOffset` governs, overriding the manifest. (On s1 Android's `recomm` clamps to ~6, the LL floor, so it stops tracking N.)
- **Behind-broadcast (`true_off`), s6:** iOS ≈ N + 27; Android ≈ N. At N=12 that's **40s vs 14s**; at N=24, **53–56s vs 24s**. Android is roughly the holdback (~28s) closer.
- **Segment collapses the gap:** s6 gap ≈ 26–30s → s2 gap ≈ 5–11s → **s1 gap ≈ 0** (all ~10–15s). The *segment choice* is what actually buys live-ness; on s1 the platform barely matters.
- **Buffer:** iOS `buf` grows with N (s6: 20→49; s1: 8→11). Android `buf` is lean and ~flat off the coarse rung (s1: ~5; s2: ~4). On s1/s2 iOS holds ~2× Android's buffer — more rebuffer cushion, at the cost of more memory + no closer to live.
- **Real ≈ sim:** every s6/s2/s1 cell matches within ~2–5s. The lone exception is **sim s6 N=0 = 124.9** (a startup-join artifact — the real phone's N=0 is a sane 28.8, confirming it).

## Methodology fix that made this possible

Batched single-device sweeps were wedging: the sweep probe (`TestSweepProbe`) never called `appium.Close()`, so each arm **leaked its appium session** (held 2h by `newCommandTimeout`) **and the device-farm device lock**. The next back-to-back arm then failed `create session: context deadline exceeded`. Masked on iOS by 4-sim round-robin (wedge ~every 4); exposed on the 1-device Android TV / real iPhone (wedge every 1).

**Fix (committed `dac1941c`, `feat/sweep-lab`):** a `t.Cleanup` calling `appium.Close()` — frees the lock + deletes the session per arm. Validated **14/14 back-to-back** on all three single-device rigs (Android TV, real iPhone) with **zero** farm-reset/unblock workaround.

## Real-iPhone rig (for repeating)

Real iPhone is driven **off the device farm**: go-ios RemoteXPC tunnel + a **plain appium on :4799** (not the farm's :4723). Gotchas hit, in order:
1. **Tunnel needs a wired USB-C connect** (unlocked) to negotiate — wireless RSD alone left `ios tunnel ls` empty. Clear any stale `:60105` squatter first.
2. **WDA code-signing:** export `CHAR_IOS_XCODE_ORG_ID=63328J83Q8`, `CHAR_IOS_XCODE_SIGNING_ID="Apple Development"`, `CHAR_IOS_WDA_BUNDLE_ID=com.jeoliver.WebDriverAgentRunner` (the probe runs from `tests/characterization/`, so the repo-root `.env` isn't auto-found — export them).
3. **On-device: Settings → Developer → Enable UI Automation** (+ Developer Mode) — else WDA fails "Timed out while enabling automation mode."
4. Probe env: `CHAR_SWEEP_PLATFORM=iphone`, `CHARACTERIZATION_DEVICE_UDID=00008120-000242DE1152201E` (hardware UDID), `CHAR_IOS_DIRECT_APPIUM_URL=http://localhost:4799`.

## Caveats

- **n=1 per cell.** Levels (platform gaps, iOS-2×-buffer) are large enough to be real; exact values jitter across reps. Confirm trends at `reps ≥ 3`.
- `live_offset` enum is `{0,2,4,6,12,18,24,30,36,42}` (odd values rejected at import).
- Android s1 `targetLiveOffset` not tracking N (clamps ~6) is worth a dedicated look — the LL rung may handle the target differently in ExoPlayer.

## See also
- `.claude/findings/user-live-offset-honored-band-ios-2026-07-05.md` — the deeper iOS-sim-only honored-band analysis (the "seek beats holdback threshold" write-up).
- `.claude/findings/live-offset-androidtv-untestable-2026-06-15.md` — **superseded**: the Android client lever (#793 step 4) is now plumbed and works; Android is no longer untestable.
- Reproduce: sweep YAMLs `tests/characterization/matrix/user-live-offset*.yaml`; Android/iPhone arm sets seeded as `import-A-*` / `import-I-*`.
