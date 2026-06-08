# AVMetrics forensics (iOS 18+)

The iOS `AVMetricEvent` feed (`ios_avmetric_events`) is the highest-resolution failure-timing telemetry we have. The heartbeat/sample feed is **blind** to it — when a play "stalled, cause unknown," this is the evidence that decides it.

## How to pull it

- `harness query avmetrics <play_id> [--event-type T] [--from --to] [--limit N]` — bounded read, **closes on its own** (no `curl --max-time` SSE hack). Returns `event_type, ts, event_ts_ms, raw_json, classification`.
- `harness ts <player> --streams avmetrics` — live SSE tail; rows render `A <ts> <event_type> <raw preview>`.
- Session-less / cross-play sweep: `harness query avmetrics --event-type ErrorEvent --from <ISO> --to <ISO>` (no play_id needed).
- iOS-only. Android/ExoPlayer and Roku emit nothing here.

## What each event type tells you

- **ErrorEvent** — carries the CoreMedia error code in `raw_json`. The single most decisive field on a wedge (see the code key below).
- **`VariantSwitchStartEvent` WITHOUT a matching complete** — the player *tried* to downshift and **couldn't**. This is the fingerprint of the sudden-drop wedge: heartbeats show `error_count=0` and a frozen rendition, AVMetrics shows the attempted-but-stuck switch.
- **HLSPlaylistRequestEvent / HLSMediaSegmentRequestEvent** — per-request timing for playlists vs media segments, separately. A playlist request that never completes ≠ a segment request that stalls.
- **ContentKeyRequestEvent** — DRM/key fetch timing (rarely the cause in our test rig, but rules it in/out).

## CoreMedia error-code key (from `raw_json`)

The split here is literally "wedges permanently" vs "ugly-but-recovers" — read the code, don't guess from the stall:

- **`-12880`** "cannot proceed after removing variants" → **wedges permanently.** The player exhausted the ladder and gave up.
- **`-16839`** "unable to get playlist" → ugly **but usually recovers** once the playlist fetch succeeds.
- **`-12889`** "no response in 2s" / **`-16830`** "media not received in 5s" → transient delivery stalls; recover if delivery resumes.
- `-12174` is documented sim-only noise (#145), not a real error — don't attribute a wedge to it.

## Common mistakes

- **`metrics(forType:)` is exact-type, not is-a.** You must subscribe to each subclass individually (`HLSMediaSegmentRequestEvent`, `HLSPlaylistRequestEvent`, `ContentKeyRequestEvent`, …) — subscribing to the base type yields **zero** events. (`[[reference_avmetric_exact_type_filter]]`)
- **`byteRange.length` is 0 for non-ranged GETs.** Use `networkTransactionMetrics.countOfResponseBodyBytesReceived` + response start/end dates for actual bytes & duration. (`[[reference_avmetric_byterange_zero]]`)
- **Stream-level TTFB ≠ network RTT.** `responseStart - requestEnd` on an HTTP/2 keep-alive connection is sub-ms while real TCP RTT is ~100ms — label client series as TTFB, not RTT. (`[[reference_ttfb_vs_rtt_http2]]`)
- **`total_stalls` under-counts these wedges** — a "fetching-but-frozen" degradation never emits `stall_frozen`, so the heartbeat stall count reads 0. Trust AVMetrics over the stall counter on iOS wedges.

## See also

- `.claude/standards/avplayer-quirks.md` — the rest of the AVPlayer reporting gaps.
- `.claude/standards/abr-decision-model.md` — why a downshift attempt happens (and stalls).
- `forensics` skill — gathers `/tmp/forensics-avm-<t>.jsonl` as a first-class evidence file.
