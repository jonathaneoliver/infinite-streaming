# AVPlayer (iOS / iPadOS / tvOS) quirks

What the Apple HLS stack does differently from the spec — facts we've actually hit while debugging.

## Reporting gaps

- **`buffer_depth_s` is often 0 even during normal playback.** AVPlayer doesn't reliably expose a "seconds of buffer ahead of playhead" metric. Use `buffer_end_s` (most-distant loaded segment position) as the truer signal: if it doesn't advance, the player isn't ingesting new data, regardless of what `buffer_depth_s` says.
- **`frames_displayed` is always 0 for HLS.** `AVAssetTrack.nominalFrameRate` returns 0 for HLS variants — there's no frame-count surface to report. Tracked in #147 (we read FRAME-RATE from the master playlist to fill this in). Don't use frames_displayed as a liveness signal on iOS.
- **`stall_count` only counts `stall_start` events the player emitted explicitly.** It does NOT include the implicit stalls from `(frozen)` or `(segment)` taxonomy events — those come from server-side inference, not player notifications.

## Transfer abandonment

- **AVPlayer abandons in-flight segment transfers when it decides the segment is no longer wanted** (e.g. after an ABR rate-shift). The server logs this as `fault_type=client_disconnect`, `fault_action=transfer_abandoned`.
- The decision threshold is roughly: if a segment is taking longer than the available buffer + 2 seconds, abandon. So a 14MB 2160p segment + a 12s buffer + slow transfer = abandon at ~14s.
- After abandonment, the player will aggressively downshift (often skipping multiple rungs of the ladder).

## ABR aggression by network type

- On wifi: conservative ramp-up after a stall (1 → 1.84 → 3.46 → 7 → 15 → 30 over ~12s).
- On 5G cellular: more aggressive ramp-up and more willing to attempt 2160p without sustained probing.
- On wired (Apple TV ethernet): least aggressive — tends to stick with the chosen variant for ~30s after a transition.

## Init segment caching

- AVPlayer caches init segments in the AVAsset cache. Once fetched for a variant, the init is not re-fetched on subsequent rotations to that variant within the play.
- Operator implication: when an investigation shows segments being fetched but `buffer_end_s` not advancing, it's NOT usually an init-segment issue (cache has it). More likely: variant-boundary issue or decoder-level rejection.
- Exception: if the master playlist changes mid-play (e.g. variant URLs rotate), AVPlayer treats it as a new asset and clears the init cache.

## Loop & timejump

- AVPlayer emits `timejump` when the playhead moves non-linearly (seek, live-edge-snap, recovery from stall).
- A `buffering_start` immediately followed by a `timejump` and then a `buffering_end` is the player skipping forward to the live edge after a stall. This is normal recovery, not a fault.
- If the player emits `timejump` without a preceding `buffering_start`, it's an unsolicited seek — investigate.

## State-machine peculiarities

- `player_state=playing` is reported even during multi-second underruns; AVPlayer's "stalled" state is only entered after a longer threshold. Use `buffer_end_s` + `last_event` together for ground truth, not `player_state` alone.
- `player_state=stalled` does NOT immediately stop network requests — AVPlayer keeps fetching segments during stalls (usually at a lower variant). See the iPad 262s-stall finding.

## Common mistakes when reading iPad data

- Trusting `buffer_depth_s` instead of `buffer_end_s`.
- Assuming `player_state=playing` means playback is healthy.
- Assuming a `stall` event with `info=(frozen)` came from the player (it's server-inferred).
- Not accounting for init-cache when seeing "no init segment fetched after variant rotation".

## See also

- `.claude/standards/abr-decision-model.md` — Apple's rate-shift heuristic
- `.claude/findings/ipad-262s-stall-2026-05-17.md` — canonical abandon-cascade-stall case
- `.claude/standards/hls-taxonomy.md` — what last_event values mean
