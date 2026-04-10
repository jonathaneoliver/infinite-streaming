# Playback Test README

**Status (Jan 2026):** Use `/go-live/...` endpoints.

## Common URLs

- HLS LL: `/go-live/{content}/master.m3u8`
- HLS 2s: `/go-live/{content}/master_2s.m3u8`
- HLS 6s: `/go-live/{content}/master_6s.m3u8`
- DASH LL: `/go-live/{content}/manifest.mpd`
- DASH 2s: `/go-live/{content}/manifest_2s.mpd`
- DASH 6s: `/go-live/{content}/manifest_6s.mpd`

## UI Pages

- `/testing/go-live-videojs.html`
- `/dashboard/segment-duration-comparison.html`
- `/dashboard/grid.html`
- `/dashboard/quartet.html`

## Player Characterization Overhead Model

When using Player Characterization in the testing dashboard:

- Network overhead can be selected as `5%` or `10%`.
- ABR limit ramps are generated from overhead-adjusted wire bitrate targets, not raw media bitrates.
- For each adjacent ladder pair, interpolation points are:
	- `0%, 5%, 10%, 25%, 50%, 75%, 90%, 95%`

Formula:

- `wire_variant_mbps = media_variant_mbps / (1 - overhead_pct)`
