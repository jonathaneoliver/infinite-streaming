# Content format

What a content package in `/media/dynamic_content/` must look like for the
catalogue (`/api/content`), go-live and the client apps to use it fully.

Read this before bringing your own encodes. The package **name**, the **file
layout** and the **partial-segment information in the manifests** are all part
of the contract: get the name wrong and the apps hide the clip; leave out the
partials and there is no LL and no real 1s/2s variant. Most of these failures
are silent.

## Where content comes from

| Source | When to use it |
|---|---|
| [infinite-streaming-encoder](https://github.com/jonathaneoliver/infinite-streaming-encoder) | **The normal path.** A parallel, chunking encoder (local farm or AWS Batch spot) whose output already matches this contract. Copy or `rsync` each finished `<content>/` directory into `$CONTENT_DIR/dynamic_content/`. |
| Bundled fallback, [`generate_abr/`](../generate_abr/README.md) | **Upload Content**, **Source Library** re-encodes, the first-run seed. Kept aligned with the Encoder's default profile. |
| Your own packager | Supported, if the output follows this document. |

A package joins the catalogue as soon as it is in place: discovery is a
directory scan on every `/api/content` request. There is no import step and no
restart. (Replacing the *files* of a package go-live has already served is
different; see [Known gaps](#known-gaps).)

## The directory name

One directory per codec:

```
<stem>_p200_<codec>[_<tag>][_<YYYYMMDD>_<HHMMSS>]
```

| Part | Example | Read by | What it drives |
|---|---|---|---|
| `<stem>` | `tears-of-steel-4k` | catalogue | The title. Lowercased, it becomes `clip_id`: packages sharing a `clip_id` are one clip in several codecs, so clients group them into one row. |
| `_p200` | `_p200` | catalogue, iOS, Android: **the literal `200`** | A marker for "the codec comes next". go-live does **not** read the number: partial duration comes from the manifests ([below](#partial-segment-information)). |
| `_<codec>` | `_h264`, `_hevc`, `_h265`, `_av1` | catalogue, iOS, Android | Sets `codec` in `/api/content`. The iOS and Android codec filters depend on it, so without it the clip is missing from the app's stream picker. |
| `_<tag>` | `_xs`, `_2s` | catalogue; go-live for pins | Distinguishes encodes of the same source. It stays in `clip_id`, so `fpv_p200_h264_xs` and `fpv_p200_h264_6s` are separate rows. |
| `_<YYYYMMDD>_<HHMMSS>` | `_20260101_010101` | catalogue | Added by the encoders when re-encoding into a name that already exists. Removed from `clip_id`. **Newest wins:** only the latest package per (`clip_id`, codec) is listed. A name without a timestamp uses the directory mtime. |

The server matches `_p200_(h264|hevc|h265|av1)(_|$)`, case-insensitively
([`go-upload/internal/util/content.go`](../go-upload/internal/util/content.go)).
The iOS and Android apps use the same pattern.

### Tags

| Tag | Meaning | Effect here |
|---|---|---|
| *(none)* | Older ladders (`legacy`, `apple`, `apple-uniq`) | Repackaged into every length (see [Re-segmentation](#re-segmentation-into-1s--2s--6s)). |
| `_xs` | The Encoder's default flexible base, `apple-uniq-live-xs`: encoded once with a VBV that stays within Apple's 1.25x peak/average bound down to 1s segments, so it is safe to re-chop. | **Nothing reads it.** It is unpinned only because it is not `_1s`/`_2s`/`_6s`. The VBV claim is the encoder's, and nothing here checks it. |
| `_1s` / `_2s` / `_6s` | **Pinned:** encoded natively at that one segment length. | go-live serves only that length, plus LL, and returns **404** for the others. The catalogue advertises only that length. The pin must be the **last** suffix: `clip_p200_h264_6s_20260101_010101` is not pinned. |
| `_ts` | MPEG-TS package from the fallback encoder | Nothing keys off the name; see [Not supported](#not-supported). |

### Names that break the contract

These still play from the dashboard, but `codec` is `""`, so the apps' codec
filters never match them:

| Name | Why |
|---|---|
| `my-show_h264` | no `_p200_` (v2.0.0's first-run seed looked like this) |
| `my-show_p100_h264` | only the literal `_p200_` is recognised |
| `my-show_p200_padblack_h264` | the Encoder's padding option inserts `_padblack` / `_padpink` **before** the codec |
| `my-show_p200_vp9` | codec not in the list |

Also:
- Directory names starting with `.` or `_` are ignored. The Encoder's
  `.archive/` relies on this.
- Two directories that lowercase to the same `clip_id` and codec hide each
  other: newest wins.

## Directory layout

```
my-show_p200_h264_xs/
├── master.m3u8              # required (see below)
├── manifest.mpd             # optional: DASH
├── 360p/ … 2160p/           # one directory per rendition
│   ├── playlist.m3u8        # media playlist, with #EXT-X-PART byte ranges
│   ├── init.mp4
│   └── segment_00001.m4s …
├── audio/                   # same shape as a rendition
│   ├── playlist.m3u8
│   ├── init.mp4
│   └── segment_00001.m4s …
├── thumbnail.jpg            # optional, 640 px wide
├── thumbnail-small.jpg      #   320 px
└── thumbnail-large.jpg      #   1280 px
```

| File | Required | Notes |
|---|---|---|
| `master.m3u8` | **Yes** | The catalogue lists a directory only if it has `master.m3u8` or `manifest.mpd`. In practice go-live needs `master.m3u8` even for DASH, because DASH refresh runs inside the HLS worker (see [Known gaps](#known-gaps)). |
| Media playlists | Yes | Found by following the master's `EXT-X-STREAM-INF` and `EXT-X-MEDIA:TYPE=AUDIO` URIs. go-live doesn't care about names. The catalogue does: see [Known gaps](#known-gaps). |
| `init.mp4` via `EXT-X-MAP` | Yes for fMP4 | Only `URI` is read; a `BYTERANGE` on the map is ignored. |
| `manifest.mpd` | For DASH | See [DASH](#dash). |
| `<segment>.m4s.byteranges` | No | Fallback partial info; see below. |
| Thumbnails | No | The catalogue checks only `thumbnail.jpg` and assumes the other two exist alongside it. |

**Never read:** `manifest.json`, `encode.json`, `run.json`,
`ENCODING_REPORT.md` and `_mezzanine/`. The encoders write them for their own
records.

### Encoder output states

An Encoder output directory can exist before it is complete. Copy only complete
ones:

| Sidecar present | State | What this server does |
|---|---|---|
| none | complete | serves it |
| `.pending.json` | encoded, never packaged: no manifests | skips it |
| `.remote.json` | manifests present, **media still in S3** | lists it; **every segment 404s** |

## HLS source requirements

go-live parses playlists with
[gohlslib](https://github.com/bluenviron/gohlslib), which is strict. A playlist
that fails to parse is skipped as a whole.

- **`master.m3u8`**:
  - Must be a multivariant playlist with at least one `EXT-X-STREAM-INF`.
  - The first line must be exactly `#EXTM3U`.
  - Each variant URI must be on the line **immediately** after its
    `EXT-X-STREAM-INF`, with no blank or comment line between them.
  - `EXT-X-VERSION` must be 10 or lower.
- **Audio** must be a separate `EXT-X-MEDIA:TYPE=AUDIO,…,URI=` rendition. go-live
  regroups and partials it exactly like video, so its segments and fragments
  should line up with video.
- **Media playlists** need `#EXT-X-TARGETDURATION`, at least one segment, and
  `#EXTINF:<duration>,` with the trailing comma. They should be VOD (the whole
  clip); go-live loops them into a live stream.
- **Segment and init URIs** should be plain filenames next to the playlist
  (`segment_00001.m4s`). Relative subpaths, `../` and absolute URLs are not
  resolved correctly.
- **One directory per variant.** Generated playlist names come from the
  variant's directory, so two variant playlists in the same directory overwrite
  each other.
- **Every variant and the audio need the same total duration and segment
  count.** go-live loops on the shortest one.

## Partial-segment information

LL-HLS, LL-DASH and the 1s and 2s variants are all built by slicing each source
segment into its fMP4 fragments (`moof`+`mdat` pairs). **go-live does not parse
the media to find those fragments. It reads their byte ranges from the
manifests.** The fragment length becomes the partial duration: 200 ms in
everything our encoders produce.

### In the HLS media playlists (primary)

One `#EXT-X-PART` per fragment, as a `BYTERANGE` inside the segment file:

```
#EXT-X-PART-INF:PART-TARGET=1.0
#EXT-X-MAP:URI="init.mp4"
#EXT-X-PART:DURATION=0.200200,URI="segment_00001.m4s",BYTERANGE="157047@432",INDEPENDENT=YES
#EXT-X-PART:DURATION=0.200200,URI="segment_00001.m4s",BYTERANGE="76727@157479",INDEPENDENT=NO
…
#EXTINF:6.006000,
segment_00001.m4s
```

Rules:
- **Parts are byte ranges of their parent segment.** Part URIs are ignored and
  the range is applied to the segment file, so separate part files
  (`segment_00001.part0.m4s`) break silently. Always give `length@offset`
  explicitly.
- **Parts must tile the segment:** contiguous, with no gaps. (The first
  fragment starts after the segment's own `styp`/`sidx` header, here byte
  432.)
- **Fragments must be equal length.** go-live takes the partial duration as
  *the first segment's `EXTINF` ÷ its number of parts* and applies it
  everywhere. It ignores `PART-TARGET`, each part's `DURATION` and the `_p200`
  in the name.
- **Mark keyframes with `INDEPENDENT=YES`.** In the example above, every fifth
  part is independent (a 1s GOP at 200 ms parts).

### In the DASH manifest

`SegmentList` with one `SegmentURL` per fragment, using an inclusive
`@mediaRange`:

```xml
<SegmentURL media="1080p/segment_00001.m4s" mediaRange="432-262133"/>
<SegmentURL media="1080p/segment_00001.m4s" mediaRange="262134-391644"/>
```

Consecutive entries with the same `@media` are merged back into one segment. The
`SegmentTimeline`, with `@r` repeats expanded, must have exactly one entry per
`SegmentURL` (per *fragment*), or go-live ignores the ranges. The Encoder writes this in place.
An older layout keeps the ranges in a sibling `manifest_fragmented.mpd`, which is
also accepted.

### `.byteranges` sidecars (fallback)

One JSON file per segment, named `<segment>.byteranges` (e.g.
`segment_00001.m4s.byteranges`), next to it:

```json
{"fragments":[{"offset":432,"length":157047,"independent":true}, …]}
```

- **HLS** uses sidecars **only if the playlist has no `#EXT-X-PART` at all.** One
  part tag anywhere disables them for the whole playlist.
- **DASH** uses them after inline and `manifest_fragmented.mpd` ranges.
- The fallback encoder writes sidecars, turns them into `#EXT-X-PART` tags, then
  keeps or prunes them. The Encoder does not write them.

### Without partial info

The package still serves, but:
- The LL playlist has no parts.
- The "1s" and "2s" variants contain whole native segments (e.g. 6s), with no
  error.
- The catalogue reports `has_ll: false` and no `1` in `segment_durations`.

## Re-segmentation into 1s / 2s / 6s

For an unpinned package, go-live builds each variant from the same encode:

| Variant | Built from |
|---|---|
| LL | the 200 ms parts, grouped into 1s segments |
| 1s / 2s | `round(target ÷ partial duration)` fragments per segment, never crossing a source segment boundary |
| 6s | the **native** segments as they are. "6s" means "native length", whatever that is. |

**go-live checks nothing about keyframes.** A 1s or 2s segment starts on
whatever fragment the arithmetic lands on, so the encode must make every such
start decodable:

- **Closed GOP of 1s (or a divisor of 1s), aligned to fragment boundaries.** A
  keyframe then begins every 1s and 2s group.
- **Partial duration divides 1s evenly** (200 ms does).
- **Native segment length a multiple of 2s** (6s is), so 2s groups don't leave a
  short last segment.
- **Bitrate peaks bounded at the shortest length served.** Re-chopping raises the
  peak/average ratio as segments get shorter. The `_xs` profile's VBV (maxrate
  100%, bufsize 0.25x) lands exactly on Apple's 1.25x bound at 1s. A looser
  encode plays, but its declared `BANDWIDTH` understates the 1s variant's real
  peaks.

Our encoders' defaults (6s segments, 200 ms partials, 1s GOP) meet all of these.

## DASH

- **Use `SegmentList` + `SegmentTimeline` + `SegmentURL@media`, with
  `Initialization@sourceURL`.** A `SegmentTemplate`-only MPD gets made-up
  `segment_%05d.m4s` URLs with no init segment, and no 1s/2s regrouping.
- Every `Representation` needs a unique `@id`; any without one are skipped.
- All representations should share segment count and duration.
- `@media` must be relative to the MPD's directory.

## Not supported

- **MPEG-TS partials.** `.ts` segments are served, but our TS packaging produces
  no part or sidecar info, so TS packages get native-length segments on every
  variant and no LL.
- **Single-file HLS** (one media file, with `EXT-X-BYTERANGE` per segment).
- **Separate part files** (see [the rules above](#in-the-hls-media-playlists-primary)).
- **`EXT-X-I-FRAME-STREAM-INF`, `TYPE=SUBTITLES`, `EXT-X-KEY`/encryption.** They
  are not carried into the generated playlists.
- **Fragments of varying length** within a rendition.

## Checklist

1. The directory name is `<stem>_p200_<h264|hevc|h265|av1>[_<tag>]`, with no
   padding suffix before the codec.
2. `master.m3u8` is at the top level. Variants and audio are in their own
   directories, with `init.mp4`, `playlist.m3u8` and segments side by side.
3. Every media playlist has `#EXT-X-PART … BYTERANGE="len@offset"` tiling each
   segment into equal fragments, with `INDEPENDENT=YES` on keyframes.
4. The encode has a 1s closed GOP aligned to fragments, and segments a multiple
   of 2s.
5. For DASH: `SegmentList` with a per-fragment `@mediaRange`.
6. For Encoder output: no `.pending.json` or `.remote.json` in the directory.

Then check what the server made of it:

```bash
curl -sk https://<host>:<port>/api/content \
  | jq '.[] | select(.name=="my-show_p200_h264_xs")
            | {name, codec, clip_id, has_hls, has_dash, has_ll, segment_durations, variants}'
```

Expect a non-empty `codec`, `has_ll: true`, `segment_durations: [1, 2, 6]` (or
just your pinned length), and your full ladder under `variants`.

## Known gaps

These are current limitations, not contract rules.

- **The catalogue looks for rendition directories by name.** `has_ll`,
  `segment_durations` and `segment_duration` are detected only from
  `1080p/`, `720p/`, `540p/` or `360p/` containing `playlist.m3u8`. A ladder with
  none of those plays fine through go-live but is reported with no LL and no 1s.
  Our encoders always produce at least one.
- **The codec comes from the name only**, not from the master's `CODECS`
  attribute.
- **DASH needs `master.m3u8` too.** Without it the live MPD is generated once and
  then stays frozen.
- **go-live caches the parsed DASH manifests, and the HLS master and variant
  list, for as long as a worker runs.** After replacing the files of a package
  that has already been played, restart go-live, or wait for the idle worker to
  shut down (DASH needs a restart).
