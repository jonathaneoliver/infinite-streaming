# Startup characterization test

What the test measures, what each field means, and how to interpret a row of results. **Read this before reasoning about a startup_test result OR before editing `tests/characterization/modes/startup_test.go`.**

## What the test is doing

Characterizes the player's behavior when playback begins, across two lifecycle boundaries:

| Boundary | What it models | Setup per cycle |
|---|---|---|
| `app_cold` | First play after the user kills/relaunches the app, or first play after device boot. No in-process AVPlayer state — fresh URLSession, fresh bandwidth estimate, fresh DNS/TLS caches at the OS layer. | `appium.Kill` → `LaunchToHome` → `ReadPlayerID` from AX node → `ApplyRate(cap)` → 2 s settle → `ResumePlayback` (tap Continue Watching). |
| `channel_change` | User switches from one content item to a different one without leaving the app. AVPlayer instance survives, learned bandwidth estimate carries, TCP keepalive / TLS session ticket likely reused. | (player is currently playing something) → tap playback-back-button → tap `home-tile-<other-clip>` to start a fresh play on a different content item. |

Each cycle applies a single network cap (`CHAR_STARTUP_CAP_MBPS`, default 30 Mbps — wide enough that most platforms pick the top variant). Then a 30-second observation window collects:

- Per-request timings from the forwarder's `network_requests` table.
- Per-second samples from the player's metrics SSE.

A `StartupCycleResult` is built from those two streams. See the next section.

Default matrix: 2 boundaries × 3 reps = 6 cycles. The channel_change cycles alternate direction each rep (rep 0 goes A→B, rep 1 goes B→A, rep 2 goes A→B) so we test both ends of the round-trip on the same content roster.

## Per-cycle fields — what each one means

| Field | Source | What it tells you |
|---|---|---|
| `boundary_type` | test config | `app_cold` or `channel_change` (the independent variable) |
| `content_clip_id` | test config | which content the cycle SWITCHED TO (informational) |
| `cap_mbps` | test config | the network cap applied before resume |
| `started_at` | wall clock | t=0 reference; everything else is relative |
| `player_id` | AX node read at home screen (`home-player-id`) | persistent UUID — same value across app_cold reps (UserDefaults-backed) |
| `first_master_at_s` | first `master_manifest` row in `network_requests` | how long from cycle start until the player asked for the master playlist. High = setup overhead; low = warm app. |
| `first_variant_at_s` | first `manifest` row after master | when the player committed to a starting variant. Difference (first_variant - first_master) ≈ master parse + ABR decision time. |
| `first_segment_at_s` | first `segment` row | when bytes started flowing. Difference (first_segment - first_variant) ≈ variant playlist parse + segment selection. |
| `first_variant_picked` | parsed from the first variant playlist URL (`.../playlist_6s_<variant>.m3u8` → `<variant>`) | the variant the player STARTED on. Critical: with the same cap, this can vary based on AVPlayer's learned estimate (or absence of one). |
| `time_to_first_frame_s` | `player_metrics_video_first_frame_time_s` from samples | THE UX number. Below 2s = "instant"; 2–5s = "fine"; >5s = "user-noticed slow start." |
| `first_req_dns_ms` / `first_req_connect_ms` / `first_req_tls_ms` | medians of the first 5 request rows | TCP/TLS warmth indicators. Zero `dns_ms` = DNS cache hit. Zero `connect_ms` = TCP keepalive reused. Low `tls_ms` (<20ms typical) = TLS session resumption. All non-zero on `app_cold` after device boot. **Currently 0-stubbed — NetworkRow schema needs dns/connect/tls timing columns; see TODO below.** |
| `reached_5s_buffer_at_s` | from samples, when `buffer_depth_s` first crosses 5 seconds | "ready to ride out network blips." If 0, never reached. |
| `reached_15s_buffer_at_s` | same, 15 s threshold | "comfortably buffered" |
| `variant_at_5s` / `at_15s` / `at_30s` | sampled `video_resolution` at those marks | trajectory — does the player START at the top variant or climb to it? |
| `upshifts_in_30s` / `downshifts_in_30s` | `profile_shift_count` delta over the window | Today the test lumps both into `upshifts_in_30s` — sample-level data doesn't separate direction. See limitation below. |
| `stalls_in_30s` | `stall_count` delta | did the cold-start path stall? Any >0 is notable. |
| `dropped_frames_in_30s` | sample's `dropped_frames` | decode pressure during startup |
| `settled_variant` | the resolution holding the majority of samples in the LAST 10 s of the window | the player's stable choice after settling. Empty = never stabilised. |
| `network_bitrate_at_start_mbps` | first sample's `network_bitrate_mbps` post-cycle | **THE TELL** for channel_change: non-zero on the very first sample = bandwidth estimate carried from the previous play. Zero on `app_cold` (fresh AVPlayer). |
| `network_bitrate_at_30s_mbps` | last sample's network bitrate | what the player believes the link will sustain at end-of-window |

## How to interpret a row

Walk through these checks in order when you see a `StartupCycleResult` row:

1. **Did the cycle actually start cleanly?** `first_master_at_s` should be < 5s on app_cold, < 1s on channel_change. If not, the boundary setup itself was sluggish (sim still cold, network problem) — the per-cycle data is suspect.

2. **What variant did the player pick?** `first_variant_picked`. Compare against `cap_mbps`. On app_cold, AVPlayer typically starts at a middle rung (no learned data); on channel_change at top (carried estimate). If the picked variant's required bandwidth exceeds `cap_mbps × ~1.2`, the player will likely downshift soon — watch `downshifts_in_30s`.

3. **Did the bandwidth estimate carry?** `network_bitrate_at_start_mbps`. On app_cold this should be 0 (no estimate yet) for the FIRST sample, climbing over the next few seconds as fetches complete. On channel_change this should be the previous play's estimate (typically the cap, or close to it). If app_cold shows non-zero on the first sample, AVPlayer is restoring an estimate from somewhere — worth investigating.

4. **Did playback start fast?** `time_to_first_frame_s`. On app_cold, expect 2–6s (app re-init + master+variant+first segment fetch). On channel_change, expect 0.5–2s (warm app, mostly just the variant+segment fetches). If app_cold is < 2s, AVPlayer may have pre-warmed something we don't expect; if > 8s, something blocked the start path.

5. **Did the buffer fill?** `reached_5s_buffer_at_s` and `reached_15s_buffer_at_s`. With `cap_mbps=30` and the typical 6 MB top-variant segment, expect 5s buffer at ~5-7s and 15s buffer at ~15-20s. If those don't progress, the player chose a variant too heavy for the cap or stalled.

6. **What did the player settle on?** `settled_variant`. With `cap_mbps=30` this should be the top variant (e.g. `3840x2160` on the test-dev catalogue). If it's lower, the cap is biting OR the player's estimate underestimates the link.

7. **Cross-cycle: does channel_change beat app_cold?** Compare medians of `time_to_first_frame_s` between the two boundaries. Channel_change SHOULD be faster (warm app + carried estimate). If not, AVPlayer's channel-change path isn't taking advantage of warm state.

## Anomalous outcomes — what's interesting

- `first_variant_picked = "360p"` on **channel_change** with `cap=30 Mbps` — bandwidth estimate didn't carry (or was unrealistically low). Indicates AVPlayer reset its estimate on URL change.
- `time_to_first_frame_s > 8s` on **either** boundary — something blocked the start path (master playlist slow, DNS, codec init). Drill via the per-request timings.
- `settled_variant = ""` (empty) — the player never picked a single resolution to commit to. Either the cap is right at a variant boundary or the player is oscillating. Look at `upshifts_in_30s` for the count of resolution changes.
- `network_bitrate_at_start_mbps > 0` on **app_cold** — AVPlayer restored an estimate from somewhere. Sometimes the OS keeps a per-host estimate; this would be useful behavior to characterize but also surprising.
- `stalls_in_30s > 0` — any stall in the first 30s is operationally interesting; cold-start should be smooth on a typical broadband link.

## Real-world correspondence

Each cycle models a class of real user action:

- **app_cold** ≡ user opens the app from scratch (device just booted, first play of a session).
- **channel_change** ≡ user finishes one show and immediately starts another (no exit/relaunch). The single most common in-session UX flow on a "next episode" / "next channel" interface.

The two together cover the spectrum from "no warmth" to "max warmth." A future `playback_stop_start` boundary (stop the current play, tap the SAME tile) would cover the middle case.

## Limitations / known gaps in this version

- **`first_req_dns_ms` / `first_req_connect_ms` / `first_req_tls_ms` are stubbed at 0.** The `runner.NetworkRow` struct doesn't yet expose those columns (the forwarder has them on the CH row; the Go projection needs to lift them). Once exposed, the test should populate from the medians of the first 5 requests' connection-stage timings.
- **`upshifts_in_30s` / `downshifts_in_30s` are not separated.** Sample-level data carries `profile_shift_count` as a single counter; to break direction we'd need per-step sample analysis (compare consecutive `video_resolution` values across samples). Currently the field stores the combined delta in `upshifts_in_30s`; `downshifts_in_30s` is 0 — fix planned.
- **`playback_stop_start` boundary not implemented.** Only `app_cold` and `channel_change` cycles run. The third boundary (stop current → tap same tile) would close the lifecycle picture but needs an additional Appium gesture flow.
- **iOS rebuild required** for the `home-tile-<clip_id>` accessibility identifiers to exist on LiveRow tiles. Without that, `channel_change` cycles fail with "find element not found." `app_cold` cycles work even without the rebuild.

## When you're about to change this code

1. **Don't change cycle structure silently** — a different ordering of LaunchToHome / ApplyRate / Tap changes which fields measure what. If you reorder, update this doc.
2. **New fields require a new entry above** + a paragraph in "How to interpret a row."
3. **New boundary types** need a new row in the "What the test is doing" table, plus an entry in `startupBoundaries` and a `case` in the runStartupCycle switch.
4. **Re-run** the test after any change and compare result shape against a previously-archived run — characterization data should be stable across no-op refactors.

## Related code

- `tests/characterization/modes/startup_test.go` — the test driver.
- `tests/characterization/runner/report.go § StartupCycleResult` — the per-cycle struct.
- `tests/characterization/runner/appium.go § TapTileByClipID` — Appium helper for channel-change taps.
- `tests/characterization/runner/appium.go § ReadPlayerID` — Appium helper for the pre-launch player_id read.
- `apple/InfiniteStreamPlayer/InfiniteStreamPlayer/HomeScreen.swift § LiveRow` — where `home-tile-<clip_id>` AX ids are emitted.

## See also

- `.claude/standards/fault-injection-wire-contract.md` — what each fault type does on the wire (relevant if combining startup with mid-play faults).
- `.claude/standards/abort-characterization-test.md` — companion test for mid-play recovery.
- `.claude/standards/avplayer-quirks.md` — known AVPlayer behaviors that affect startup.
- `~/.claude/plans/abort-characterization-test.md` — the plan that originally proposed the startup test as a follow-on.
