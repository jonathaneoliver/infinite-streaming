# generate_abr

**Status (Aug 2026): largely superseded.** This is the FALLBACK encoder, not the primary one.

Production content is now produced by the separate **Encoder** project
(`infinite-streaming-encoder`, `~/Projects/Encoder`) and copied onto the
infinite-streaming server. `create_abr_ladder.sh` is kept working, and kept
*aligned* with that project, for the cases where standing the Encoder up is not
worth it — a quick one-off clip, a box without it, or a change to the pipeline
itself.

Do not add features here that belong in the Encoder project. Do keep the two
aligned: content produced by either must be the same shape, or a clip encoded
here cannot be compared against one encoded there — which is the entire reason
this fallback still exists.

## The normal path

```
Encoder project  →  $ENCODE_STAGING_DIR  →  rsync  →  server  →  /media/dynamic_content/
```

The Encoder writes finished packages to the staging directory
(`$ENCODE_STAGING_DIR`, e.g. `/Volumes/4TB/media/encode-staging`), and they are
rsynced to the server's content volume. On the server that volume is `CONTENT_DIR`,
bind-mounted to `/media` inside the container, so a package must land under
`/media/dynamic_content/<content>/` to be discovered by `/api/content` and served
by go-live.

Content discovery is a plain directory scan gated on a manifest being present, so
a package appears in the catalogue as soon as it is in place — no registration
step, no restart.

## What the Encoder produces that this script does not

- **Self-describing manifests.** `manifest.mpd` is rewritten in place at fragment
  granularity, one `<SegmentURL @media @mediaRange>` per fragment, so the fragment
  index lives in the manifest. No `.byteranges` sidecars at serve time (Encoder
  #282, consumed by #986 on this side). This script still generates sidecars,
  consumes them to inject `#EXT-X-PART` into the HLS playlists, then prunes them —
  so its DASH output has no per-fragment ranges.
- Distributed/chunked encoding, VMAF audit, and the ladder store + UI.

## Alignment with the Encoder

Defaults here match the Encoder's default delivery profile, `apple-uniq-live-xs`:

| | |
|---|---|
| Ladder | `--ladder apple-uniq-live-xs`, 12 rungs per codec (h264 to 4K) |
| VBV | maxrate 100% of target, bufsize 0.25x |
| Passes | two-pass software encode |
| Audio | AAC-LC 96k stereo 48kHz |
| Timing | 6s segment, 200ms partial, 1s GOP |
| Output tag | `_xs` → `<stem>_p200_<codec>_xs` |

The ladder is a *delivery profile*, not just a bitrate table. Its VBV is what makes
ONE encode safe for go-live to re-chop into LL/2s/6s:

```
peak/avg = maxrate%/100 + bufsize/T   =   1.04x (6s) / 1.13x (2s) / 1.25x (1s)
```

The 1s case lands exactly on Apple's live/linear 1.25x bound and longer variants sit
below it. Two-pass is not optional at that buffer size: single-pass x265 undershoots
`-b:v` by ~17%, so the published bitrates are only truthful with it.

Other ladders (`legacy`, `apple`, `apple-uniq`) are retained for comparison runs and
stay untagged, so pre-existing content keeps its names.

## Active scripts

- `create_abr_ladder.sh` — main pipeline (ffmpeg + shaka-packager)
- `create_hls_manifests.py` — HLS manifest generation
- `convert_to_segmentlist.py` — SegmentTemplate → SegmentList
- `backfill_thumbnails.sh` — thumbnails for existing content (`make backfill-thumbnails`)
- `ladder_audit.py` — spacing / peak-accuracy / VMAF checks over a finished package

## Notes

- Output defaults to `$ENCODE_STAGING_DIR` when set, else the current directory —
  so encodes do not scatter across whatever directory they were launched from.
- Legacy test scripts (avsync, LL-HLS tests, etc.) were removed.
- `ladder_audit.py` enforces a >=1.5x *combined* (video+audio) spacing rule inherited
  from the legacy ladder. Apple's published ladder does not satisfy it by design, so
  `tight_spacing` is expected on the apple ladders and is not a regression.
- Encoding characterization runs and raw sweep tables: `ENCODING_CHARACTERIZATION.md`.
