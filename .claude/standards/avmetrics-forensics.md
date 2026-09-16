# AVMetrics feed — detailed failure timings

The iOS app (iOS 18+) subscribes to Apple's `AVMetrics` streams on the
AVPlayerItem and POSTs them to `/api/session/{id}/avmetrics`; the forwarder
writes them to ClickHouse `ios_avmetric_events`. This is a **second,
higher-resolution telemetry channel** alongside the polled heartbeat/sample
feed — and for failure forensics it routinely shows what the heartbeat feed
**cannot**.

## Why reach for it

The heartbeat/sample feed is polled (~1s) and summarised. It misses
sub-second events and, critically, **CoreMedia errors**: in the rampup
sudden-drop wedge, `query play` reported `error_count=0` while the AVMetrics
feed carried **6 distinct CoreMedia errors** plus the variant-switch that
failed. If a play "stalled / unexpected_end" with no obvious cause, the
AVMetrics feed is where the cause lives.

## How to pull it

First-class harness surface (#693) — bounded, closes on its own:

```sh
# Returns event_type, ts, event_ts_ms, raw_json, labels, classification.
harness --insecure --json query avmetrics <play_id> [--event-type T] [--from ISO] [--to ISO] [--limit N]
# Sweep error-bearing events across a window with no play_id:
harness --insecure --json query avmetrics --event-type AVMetricErrorEvent --from <ISO> --to <ISO>
# Live tail (renders 'A <ts> <event_type> <raw preview>'):
harness --insecure ts <player> --streams avmetrics
```

Raw/SSE fallback (same rows via the timeseries multiplexer — needs a
`--max-time`/`limit` because the SSE doesn't close):

```sh
curl -sk "https://$TEST_HOST:21000/analytics/api/v2/timeseries?streams=avmetrics&player_id=<PLR>&play_id=<PLY>&limit=4000"
```

`player_id`/`play_id` must be **lowercase** (forwarder canonicalises; iOS
emits uppercase — see case_sensitivity_ids).

## Event types that carry timing/cause

- `AVMetricErrorEvent` — `raw_json.error` is the full `NSError`. **The error
  string names the failing resource and the timeout.** `NSURL=` is the
  *parent playlist* (e.g. `playlist_2s_audio.m3u8`), but the message says
  what actually failed: `map` (EXT-X-MAP init segment) / `media file`
  (media segment) — i.e. **segments, not the playlist**. Classify
  audio-vs-video from the NSURL (`playlist_2s_audio` vs `playlist_2s_2160p`).
- `AVMetricHLSMediaSegmentRequestEvent` / `…PlaylistRequestEvent` — per-request
  client-side timings: `derived_ttfb_ms`, `derived_mbps`, `derived_bytes`.
  This is the **client's** view of delivery (vs the proxy network-request
  view, which is the proxy→client send time).
- `AVMetricPlayerItemVariantSwitchStartEvent` / `…VariantSwitchEvent` — a
  switch has a **Start** and a **complete** (`…VariantSwitchEvent`). A
  `Start` with **no matching complete** = a **failed ABR switch** (the
  player tried to downshift and couldn't). `fromVariant`/`toVariant` give
  the rungs (e.g. `2160p → 360p`).
- `AVMetricPlayerItemStallEvent`, `…RateChangeEvent`,
  `…LikelyToKeepUpEvent` — playback-engine state transitions.

## CoreMedia error glossary (observed; Apple does not document these)

| code | message | meaning |
|------|---------|---------|
| -12889 | "No response for map/media file in 2s" | segment fetch timed out — **live-edge timeout, scales with segment duration** (2s segments → "in 2s") |
| -16830 | "Media file not received in 5s" | segment hard timeout (~2.5× segment duration) |
| -16839 | "Unable to get playlist before long downshift" | couldn't reach a usable playlist state before a big switch |
| -12880 | "Can not proceed after removing variants" | enough renditions marked unplayable that the switch can't complete → wedge. With a **single audio track**, the audio rendition's removal is fatal. |
| -15628 | (generic) | seen alongside the above |
| -12174 | content not updated | sim-only noise (#145), not a real error |

Live-edge timeouts are **far shorter** than the ~30s VOD segment-fetch
timeout in fault-injection-wire-contract.md — they track segment duration,
so 2s content is much less tolerant of a slow/dead link than 6s.

## Forensic recipe (sudden-drop wedge)

1. Find the dead window in the **network-request view** (proxy→client
   delivery): a gap where nothing is delivered = the link went away
   (`transport_disconnect`).
2. Overlay the **AVMetrics** errors on that window: segment timeouts
   (`-12889`/`-16830`) → failed `VariantSwitchStart` (no complete) →
   `-12880` → `StallEvent`.
3. Read NSURL to confirm which segments (audio vs video) starved.

## Caveat — batched over the same connection

AVMetrics are **batched** before POST and travel the **same connection** as
the heartbeats. A wedge that kills the connection can drop the **final
batch** — so the very last events (e.g. the app-side `frozen` detection that
would yield `stall_frozen`) may never land. Absence near the end is not
proof of absence; corroborate with the proxy-side network/control feeds.

## See also

- `avplayer-quirks.md`, `abr-decision-model.md` — player behaviour these
  events expose.
- `data-fields.md` — heartbeat/sample schema (the lower-resolution feed).
- memory: `reference_avmetric_exact_type_filter`, `reference_avmetric_byterange_zero`,
  `reference_avplayer_stale_connection_localproxy`.
