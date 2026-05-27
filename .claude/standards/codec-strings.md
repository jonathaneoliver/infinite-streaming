# Codec strings — `CODECS=` formatting + platform requirements

The `CODECS` attribute on a `#EXT-X-STREAM-INF` line tells the player what decoder it'll need. Get this wrong and a variant is silently skipped on the platform that should support it.

## The format

```
CODECS="<video>,<audio>"
```

Most common values:

| String | Meaning | Notes |
|---|---|---|
| `avc1.640028` | H.264 High profile, level 4.0 | Universally supported. |
| `avc1.4d401f` | H.264 Main profile, level 3.1 | Lower-tier compat. |
| `hev1.1.6.L120.90` | HEVC (H.265) Main profile, level 4.0 | iOS 11+ / tvOS 11+ / Android 5+ |
| `hvc1.1.6.L120.90` | Same as `hev1.*` but with a different sample-entry-format | Apple platforms prefer `hvc1`; some non-Apple players only accept `hev1`. |
| `mp4a.40.2` | AAC-LC | Universal. |
| `mp4a.40.5` | HE-AAC v1 | Lower bitrate. |
| `mp4a.40.29` | HE-AAC v2 | Lower still. |
| `ac-3` / `ec-3` | AC-3 / E-AC-3 (Dolby Digital / Digital Plus) | Platform-specific. |

## Decoding the suffix

For `avc1`: `<profile><constraint><level>` in hex. `64=High`, `4D=Main`, `42=Baseline`. Level `28=4.0`, `1F=3.1`.

For `hev1` / `hvc1`: `<profile_space>.<profile_idc>.<compatibility>.L<level>.<constraint>`. Don't try to decode these by eye — copy from a working manifest.

## Platform requirements (the parts we've actually hit)

| Platform | Notes |
|---|---|
| AVPlayer (iOS/iPadOS/tvOS) | Prefers `hvc1.*` for HEVC. Will accept `hev1.*` but with some compatibility quirks on older iOS. Requires CODECS to be present — bare BANDWIDTH is silently ignored as "not playable". |
| ExoPlayer (Android TV / Google TV) | Accepts both `hev1.*` and `hvc1.*`. Tolerant of missing CODECS (probes the segment). |
| Roku Stream Player | Strict on CODECS strings — case-sensitive, exact-match. Roku-specific compat layer rejects some valid strings. |
| hls.js (browser) | Probes via MediaSource feature detection; CODECS is informational only. |
| Shaka Player | Same as hls.js. |

## Common stripping bugs (proxy)

The proxy's `content --strip-codecs` removes the `CODECS=` attribute entirely. After strip:

- **AVPlayer**: variant becomes ineligible. Player picks only variants that still have CODECS.
- **ExoPlayer / hls.js / Shaka**: variant remains eligible (these tolerate missing CODECS).
- **Roku**: variant becomes ineligible.

Operator implication: if you `content --strip-codecs` on a master that has all variants with HEVC, AVPlayer will refuse the whole asset. Use selectively (strip on lower variants, leave HEVC ones).

## Variant-eligibility chain

A variant is eligible for selection iff:

1. Its CODECS string is parseable and supported by the player.
2. Its BANDWIDTH is below the player's current estimate × safety factor.
3. Its RESOLUTION is below or at the player's display cap (AVPlayer is strict; some Android implementations aren't).
4. The variant's playlist hasn't 404'd.

Each step filters the set. A common confusion: "why isn't AVPlayer picking the 2160p variant?" Answer is usually #1 (HEVC stripped) or #3 (iPad has 1366×1024 panel, won't pick 2160p typically).

## Common mistakes when reading manifest issues

- Assuming all players accept all codec strings the same way. Apple is strict; Roku is stricter.
- Stripping CODECS to "test ABR" — it doesn't just hide info, it makes Apple-side variants unplayable.
- Reading a strange variant choice and concluding "ABR is broken" without checking codec eligibility first.

## See also

- `.claude/standards/hls-taxonomy.md` — manifest tag reference
- `.claude/standards/abr-decision-model.md` — how the eligibility-filtered set is then ranked
