# Why InfiniteStream?

InfiniteStream is a purpose-built test server for HLS and DASH video players. It generates LL-HLS and LL-DASH plus 2s and 6s segment variants from looping VOD on a shared clock, so player bugs that are hard to reproduce — stalls, ABR misbehavior, recovery failures, loop-boundary glitches — can be triggered on demand, from the same source, against any player engine, in a deterministic way.

## Who this is for

- Video engineers and player/SDK developers
- QA teams who need reproducible streaming scenarios
- Anyone evaluating or comparing player behavior across HLS.js, Shaka, Video.js, AVPlayer, ExoPlayer, or Roku

It is **not** a production streaming origin and is not intended to serve real viewers.

## The problem it solves

Player bugs are usually environmental. You can't reproduce them because the failure was a one-off: a network blip, a truncated segment, a slow manifest, a discontinuity the player didn't handle. Reproducing these reliably is the hard part of player QA.

Existing options each fall short:

- **Public test streams** (Apple sample streams, DASH-IF vectors, Bitmovin demos) are not deterministic, don't loop cleanly on your schedule, and can't fail on command.
- **Production origins** (Wowza, AWS Elemental, Unified Streaming) are built to serve real viewers, not to misbehave on purpose.
- **DIY stacks** (ffmpeg + nginx-vod or nginx-rtmp) take days to wire up and don't give you LL-HLS and LL-DASH from the same clock, let alone a fault-injection UI.
- **Generic chaos tools** (toxiproxy, `tc` alone, Chaos Mesh) are protocol-agnostic — they don't understand segments, playlists, partials, or manifests, so the failures they create aren't meaningful at the streaming layer.
- **Man-in-the-middle proxies** (Charles, mitmproxy) work per-request and per-operator; they don't script repeatable schedules across runs or isolate concurrent testers.

## What makes InfiniteStream different

- **Deterministic looping live.** The sliding window moves on a stable clock and wraps on loop boundaries. The same test run produces the same timing.
- **All variants from one worker.** LL-HLS, LL-DASH, 2s, and 6s segment variants are generated from a single per-content worker on a shared clock, so cross-protocol comparisons are apples-to-apples.
- **Streaming-aware fault injection.** Inject HTTP errors, hangs, and payload corruption at the segment, playlist, or manifest layer — not just generic TCP faults.
- **Master-playlist manipulation.** The Content tab of the Fault Injection card rewrites the HLS master on the fly — strip CODEC / AVERAGE-BANDWIDTH, overstate BANDWIDTH, allowlist specific rungs, change the live hold-back offset. Explore how ladder shape and optional HLS attributes affect startup, ABR behavior, and live-edge latency **without re-encoding**.
- **Transport faults too.** Port-level DROP/REJECT via nftables and rate shaping via `tc`, composable with HTTP-layer faults.
- **Per-session isolation.** Each browser session binds to a dedicated proxy port via `player_id`, so concurrent testers don't collide and each session has its own fault schedule.
- **Side-by-side comparison UI.** Mosaic and quartet views let you watch multiple players or encodings against the same source simultaneously.
- **Player characterization.** Scripted ABR ramp sweeps with wire-overhead-adjusted bandwidth limits, for measuring how a player responds to bandwidth changes.
- **Everything is a REST API.** The dashboard is a thin client over HTTP + SSE — every fault, every shaping change, every session action the UI can do is available to test scripts and CI. No hidden controls, no UI-only paths. See [`API.md`](API.md).
- **Repeatable in CI.** The same fault schedule, same stream, same clock — portable across developer machines, shared QA rigs, and CI runners.

## Use cases

- Regression-test a player release against a known fault schedule before shipping.
- Reproduce a customer-reported stall by replaying the exact fault sequence.
- Compare HLS.js vs Shaka vs Video.js vs native playback on identical content under identical faults.
- Validate loop-boundary and discontinuity handling.
- Characterize a player's ABR algorithm under controlled bandwidth steps.
- **Pre-validate ladder and HLS parameter decisions.** Before committing to an encode, use the Content tab to serve a manipulated master playlist — hide rungs, strip CODECS, inflate BANDWIDTH, change the live offset — and watch the player's startup, adaptation, and live-edge behavior. Fast feedback loop for "what if we dropped the 4K rung" or "what if we removed AVERAGE-BANDWIDTH".
- Smoke-test a new encoding ladder before promoting to staging.
- Demonstrate what LL-HLS and the 2s and 6s segment variants actually look like to a player.

## Comparison to alternatives

| Tool or approach | Good at | Why InfiniteStream instead |
|---|---|---|
| Public test streams (Apple, DASH-IF, Bitmovin demos) | Quick playback sanity checks | Not deterministic; no failure injection; no looping on your schedule |
| Production origins (Wowza, AWS Elemental, Unified Streaming) | Serving real viewers | Heavy, costly; not built for faults, looping, or side-by-side QA |
| FFmpeg + nginx-rtmp / nginx-vod (DIY) | Full control of the stack | Days of setup; no shared-clock LL-HLS + LL-DASH; no fault UI |
| Shaka Streamer | Packaging pipelines | Not a live test server; no looping, no faults, no dashboard |
| toxiproxy, `tc`, Chaos Mesh | Generic network faults | Protocol-agnostic — no awareness of segments, partials, playlists, manifests |
| Charles, mitmproxy | Per-request rewriting | Manual and per-operator; not scripted, not repeatable across runs |
| mediamtx, SRS, OvenMediaEngine | Live ingest and serving | Not looping-VOD focused; not QA-focused; no fault injection surface |

## When not to use it

- You need a production streaming origin.
- You need full standards conformance on every LL-HLS or LL-DASH edge case. See `PRD.md` for the list of known limitations (blocking playlist reload, `EXT-X-RENDITION-REPORT`, chunked CMAF for LL-DASH partials, etc.).
- You need DRM.
- You need scale beyond a handful of concurrent test sessions.
