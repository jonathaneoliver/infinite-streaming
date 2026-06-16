# Live-offset — how to configure it and how to test it

Operator + sweep-skill reference for the **distance-from-live-edge** controls. There are
**two independent levers** (manifest/proxy vs app override) that move **different fields**;
mixing them up is the usual source of confusion. This doc is the source of truth for both.
See also `docs/sweep-design.md §3.1` (the manipulation-check validity gate) and #793 / #797.

## TL;DR

| Lever | Set it with | Rewrites / sets | Moves which field | Floor? |
|---|---|---|---|---|
| **Manifest (proxy)** | `harness content <player> --live-offset N`, or the dashboard ContentManipulation radio | `EXT-X-START:TIME-OFFSET=-N` (master) **+** `EXT-X-SERVER-CONTROL:HOLD-BACK=N` (variant) | `configured_offset_s` / `recommended_offset_s` | **Yes** — must be ≥ 3× max-segment or it's rejected/degraded |
| **App override** | `-is.flag.live_offset_s N` (iOS) / `--es is.flag.live_offset_s N` (Android) | `configuredTimeOffsetFromLive` / `LiveConfiguration` **+ absolute seek to liveEdge−N** | `wall_offset` (= `seekable_end_s − position_s`) | **No** — it's a client seek; works below the floor |

The app override is **absolute, not additive**: when it's > 0 it overrides the manifest target; `configured` stays at the manifest value while `wall_offset` tracks the override.

## Lever 1 — manifest / proxy live_offset

The proxy rewrites the manifest per session so the player joins further back and targets a
larger hold-back.

- **CLI:** `harness content <player_id> --live-offset N` (patches `content_live_offset`).
  Note: this does **not** validate against the supported enum — it stores any int, so
  deliberately sub-spec values (to test rejection) pass through.
- **Dashboard:** ContentManipulation → Live offset radio. Supported set:
  `{0, 2, 4, 6, 12, 18, 24, 30, 36, 42}` (0 = no rewrite / native manifest).
- **What it rewrites** (both, since the #793 fix — the gate used to be master-only, so the
  variant `HOLD-BACK` was never rewritten and the lever had *no effect on any player*):
  - master playlist: `EXT-X-START:TIME-OFFSET=-N` (the join point)
  - each variant playlist: `EXT-X-SERVER-CONTROL:HOLD-BACK=N` (the target offset). HOLD-BACK
    is a **media-playlist-only** tag — it lives on the variant, never the master.
- **Fields it moves:** `configured_offset_s` and `recommended_offset_s` (the manifest target
  the player parsed).

### The 3×-max-segment floor (the headline gotcha)

HLS requires `HOLD-BACK ≥ 3 × target segment duration`. Segment durations round up, so:

| Master | Max segment | Floor (~3×) | A `live_offset=12` here is… |
|---|---|---|---|
| `master_6s` | ~7 s (6 s rounds up) | **~21 s** | sub-spec → rejected |
| `master_2s` | ~3 s | **~9 s** | legal → honoured |

**Below the floor:** iOS AVPlayer goes **degraded** — all offset/buffer/seekable telemetry
comes back null and `error_code = -12646` fires. **At/above the floor:** honoured exactly
(`configured`/`recommended` = N, linear). The same offset is therefore sub-spec on s6 but
legal on s2 — that's the load-bearing segment×offset contrast in the #793 matrix.

## Lever 2 — app override (`is.flag.live_offset_s`)

A client-side launch flag that seeks to `liveEdge − N` and pins the player's target offset.
Overrides the manifest when > 0.

- **iOS:** `-is.flag.live_offset_s N` (NSArgumentDomain launch arg) →
  `AVPlayerItem.configuredTimeOffsetFromLive` + an absolute seek (`PlayerViewModel.swift`).
- **Android:** `--es is.flag.live_offset_s N` (intent extra, added in #796 / PR for #266) →
  `LiveConfiguration.setTargetOffsetMs` + a seek to `liveEdge − N` (the 0.97–1.03 speed
  window converges too slowly on its own, so it seeks). `MainActivity.kt` reads the extra.
- **Works below the floor** (it's a seek, not a manifest constraint), so it's the only way to
  drive a *tight* offset on s6.
- **Field it moves:** `wall_offset` (derived = `seekable_end_s − position_s`). `configured`
  stays at the manifest's value.

## The fields (what to read, what to trust)

| Field | Meaning | Trust |
|---|---|---|
| `configured_offset_s` / `recommended_offset_s` | manifest target the player parsed | **the proxy-lever ground truth** |
| `wall_offset` (= `seekable_end_s − position_s`) | physical playhead-behind-edge | **the app-override ground truth** |
| `buffer_depth_s` | forward loaded range; ≈ `wall_offset + hold-back` | use for buffer questions |
| `live_offset_s` / `true_offset_s` | player-reported achieved offset | **over-reads ~1.3–2×** — don't use as the headline |

## How to test it

### Per-run probe (sweep / characterization)

```
# manifest lever: app pinned to 0 (avoid confounding), proxy sets the offset
harness content <player_id> --live-offset 24
CHAR_PLAYER_ID=<player_id> LAUNCH_MODE=appium \
  CHAR_SWEEP_PLATFORM=ipad-sim CHAR_SWEEP_SEGMENT=s6 CHAR_SWEEP_LIVE_OFFSET=0 \
  CHAR_CONTENT=insane_new_p200_h264 CHAR_SWEEP_DURATION_S=65 \
  CHARACTERIZATION_DEVICE_UDID=<udid> \
  go test ./tests/characterization/modes -run TestSweepProbe -count=1 -v -timeout 8m

# app-override lever: manifest neutral (0), app flag sets the offset
harness content <player_id> --live-offset 0
CHAR_SWEEP_LIVE_OFFSET=24   # (same probe; → -is.flag.live_offset_s 24)
```

- `CHAR_SWEEP_LIVE_OFFSET` maps to the app flag; the probe **always pins it** (default 0) so
  no run silently inherits the persisted app value — a real confound we hit (a manifest-only
  sweep showed the app's base offset leaking in).
- `CHAR_SWEEP_SEGMENT=s6|s2` picks the master (the floor scales with it).
- Measure from CH after a short flush delay (the forwarder batches): query
  `/analytics/api/v2/events?play_id=<play>` and take the median of the fields above. The play
  archive persists, so capture the `play_id` and re-measure if a read comes back empty.

### Server-behavior test (the proxy rewrite itself)

```
env THROUGHPUT_HOST=dev.jeoliver.com THROUGHPUT_API_PORT=21000 THROUGHPUT_INSECURE=1 \
  go test ./tests/server_behavior -run 'TestServerContent/master_live_offset' -v
```

Asserts the proxy rewrites `EXT-X-START:TIME-OFFSET=-N` on the master **and** `HOLD-BACK=N`
on the variant — the regression test for the master-only-gate bug.

### The manipulation-check gate (validity, #793)

`harness sweep analyze … --confirm-reps 3` marks a live_offset run **`inconclusive`** (not a
finding) if the *achieved* offset doesn't reach the *intended* value (segment-slack-aware). A
run that never moved the IV says nothing about any live_offset→QoE hypothesis. See
`docs/sweep-design.md §3.1`.

## Platform coverage (what's testable where)

| | iOS (sim / device) | Android TV |
|---|---|---|
| Manifest live_offset | ✅ | ✅ |
| App override (`is.flag.live_offset_s`) | ✅ | ✅ (#796) |
| Segment select (`s2` / LL, to move the floor) | ✅ (`-is.segment`) | ✅ (`--es is.segment`, #798) |

Android `is.segment` landed in **#798** (closing the #797 parity gap), so the
**`master_2s` / LL live-offset quadrants are now drivable on Android** — provided the device runs
a #798-or-later build (rebuild + `adb install`). On a pre-#798 build Android is locked to
`master_6s` (floor ~21 s), where anything below the floor degrades and can't be distinguished
from the lever simply not landing.

> **Android behaviour (measured, clean #793 matrix on a #798 build):** **both levers work and
> track the set value.** Manifest/proxy: configured tracks with a slight ~1-segment over-read
> (s6 12→13, 24→27, 30→36; s2 4→5 … 12→12.5). App override (`is.flag.live_offset_s`, #796):
> `configured` tracks the value *exactly* (s6 12→12 … 30→30; s2 4→4 … 12→12). Two platform
> differences vs iOS: (1) Android does **not** enforce the 3×-segment floor — sub-spec offsets
> play at the requested value instead of refusing to start; (2) on Android `configured_offset_s`
> reflects the **effective player target** (manifest *or* override), whereas on iOS it reflects the
> **manifest target** and the override shows only in the achieved offset (`wall`).
>
> **Gotcha that confounds Android runs:** the in-app **`advanced_live_offset_s`** setting persists
> in `shared_prefs/servers.xml` and pins the offset across launches; the launch arg does **not**
> reliably override it (unlike iOS, where NSArgumentDomain outranks UserDefaults — see #800). Reset
> it to 0 (or via the Settings UI) before a clean run, or every cell reads the saved value.
