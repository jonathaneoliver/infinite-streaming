# ABR (Adaptive Bitrate) decision model

How players choose variants, why downshifts cascade, what triggers timejump. Generic across HLS players unless noted.

## The two signals

A player makes ABR decisions from two primary inputs:

1. **Estimated bandwidth** (`network_bitrate_mbps` in samples). Derived from segment transfer times: bytes / transfer_ms. Smoothed via a moving average over recent segments.
2. **Buffer health** (`buffer_end_s` distance from playhead). The fuller the buffer, the more risk the player is willing to take on a higher variant.

Decision per segment: pick the *highest variant whose declared bandwidth × safety factor* ≤ *estimated bandwidth*, subject to buffer not being below the panic threshold.

Safety factor: typically 0.7-0.9 (player wants headroom). When the buffer is small, it skews lower.

## Why downshifts cascade

A single slow transfer drops the bandwidth estimate. The player picks the next-lower variant. That variant's segments are smaller → transfer faster → bandwidth estimate stabilises or recovers. The player ramps back up *one variant at a time*, with a short hold between each step (typically 1-2s).

But if the cause of the slow transfer was *the segment*, not *the network* (e.g. a 14MB segment from one specific high-bitrate variant), the next variant down is also slow if the bitrate estimate didn't reflect "this one was an outlier". Result: cascade past where the true network capacity is, then long climb back.

## What triggers `timejump`

- **Live-edge snap.** After a stall, if the playhead is now too far behind the live edge, the player jumps forward (skipping content).
- **Seek operation.** User-initiated or programmatic. Rare in our test harness (no user controls).
- **Variant reset.** When the master playlist changes (variant URLs added/removed), some players re-establish from scratch and that can manifest as a timejump.

A `timejump` between `buffering_start` and `buffering_end` is normal stall recovery. A standalone `timejump` (no surrounding buffer events) is unusual — investigate.

## What triggers `restart`

- Player explicitly tore down the AVPlayer instance and built a new one. Usually app-level (user navigated away and back) or recovery-from-fatal-error.
- After `restart`, `first_frame` fires when the new instance has its first decoded frame. (Rows ingested before #622 also carry a redundant `playback_start` label at the same moment — dropped because it duplicated `first_frame` and read like a play-open boundary, which is `play_start`'s job.)
- A restart cluster (multiple restarts in <30s) suggests the player is in an unrecoverable state and the app/operator is repeatedly retrying.

## Variant ladder vs effective ladder

The master playlist declares N variants (`#EXT-X-STREAM-INF`). The player's *effective* ladder is the subset it'll actually consider:

- AVPlayer respects declared BANDWIDTH; if it's lying (e.g. proxy stripped AVERAGE-BANDWIDTH and BANDWIDTH is over-declared), the player will probe high variants and burn segments.
- Some players cap at the highest variant their display can render. Apple TV 4K considers 2160p; iPad considers up to its panel resolution (1366×1024 for non-Pros — they won't pick 2160p on wifi typically).
- Codec strings matter: a variant declared with `hev1.*` won't be chosen by a player lacking HEVC. See `.claude/standards/codec-strings.md`.

## ABR aggression knobs we control

The harness has two switches that change ABR behaviour:

- **`shape --rate X`** caps the kernel rate. Player's bandwidth estimate falls; player downshifts. Use for forcing a specific variant.
- **`content --overstate-bandwidth`** inflates BANDWIDTH attribute by 10% in the master. Player picks higher variants for a given network estimate; useful to provoke a stall.
- **`content --strip-average-bandwidth`** removes AVERAGE-BANDWIDTH; some players fall back to BANDWIDTH which is the per-variant max not average → over-estimation.

## Common mistakes when interpreting ABR data

- Reading `video_bitrate_mbps` as "the player's network choice" — it's the variant's *declared* bandwidth, not what the player measured.
- Comparing `video_bitrate_mbps` to `nftables_pattern_rate_runtime_mbps` and concluding "shaper isn't throttling". The shaper sets the kernel cap; the variant choice reflects the player's estimate from previous segment transfers, which may lag the shaper change by 5-30 seconds.
- Treating a downshift as a bug. Downshifts are correct ABR behaviour; cascades into stalls are the bug.

## See also

- `.claude/standards/avplayer-quirks.md` — Apple-specific ABR aggression
- `.claude/standards/codec-strings.md` — variant-eligibility by codec
- `.claude/findings/ipad-262s-stall-2026-05-17.md` — abandon → cascade → stall
