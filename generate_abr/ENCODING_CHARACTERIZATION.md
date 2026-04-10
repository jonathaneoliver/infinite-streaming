# Encoding Characterization

This document records measured encoding results produced by:

- `generate_abr/crf_bandwidth_sweep.py`

Source used for this run:

- `your-source-video.mkv`

Run shape:

- Duration: `18s`
- Codecs: `h264`, `hevc`
- Modes: `sw`, `hw`, `hwmatch`
- Resolutions: `360p,540p,720p,1080p,1440p,2160p`
- CRF list: `18,20,22,24,26`
- VMAF: enabled

## `HW_Q` Meaning

`HW_Q` is the hardware encoder quality value (`-q:v`) used for VideoToolbox runs.

- It applies only to `mode=hw` rows (`h264_videotoolbox`, `hevc_videotoolbox`).
- `mode=sw` rows show `-` in `HW_Q` because software uses true `CRF`.
- In this sweep script, `HW_Q` is derived from CRF with:
  - `HW_Q = round((52 - CRF) * 1.5)`, clamped to `1..100`.
- Higher `HW_Q` means higher hardware quality and usually higher bitrate.

Important:

- `HW_Q` is not standardized across encoders and is not a true CRF equivalent.
- It is a practical mapping to make hardware and software sweeps comparable by intent.

## HW Match Results Summary

The combined CSV below includes baseline `sw`, baseline `hw`, and `hwmatch` rows:

- `generate_abr/output/crf_sweep_vmaf/crf_bandwidth_sweep_new.csv`

Coverage:

- Total rows: `180` (`2 codecs x 3 modes x 6 resolutions x 5 CRFs`)
- `hwmatch` rows: `60`

Bitrate-match accuracy (`achieved avg Mbps / target avg Mbps`) for `hwmatch`:

- `h264`: `0.844` (avg `9.253` / target `10.960` Mbps)
- `hevc`: `0.848` (avg `7.307` / target `8.621` Mbps)

Quality impact from `hw` to `hwmatch` and gap to `sw`:

- `h264`:
  - VMAF gain `hw -> hwmatch`: `+8.01`
  - Remaining VMAF gap `sw - hwmatch`: `6.52`
  - Avg bitrate increase `hw -> hwmatch`: `+4.301 Mbps`
- `hevc`:
  - VMAF gain `hw -> hwmatch`: `+11.31`
  - Remaining VMAF gap `sw - hwmatch`: `4.91`
  - Avg bitrate increase `hw -> hwmatch`: `+3.670 Mbps`

Observed behavior:

- `hwmatch` materially improves quality over baseline hardware mode.
- `hwmatch` still tends to under-hit target average bitrate at top rungs, most notably `2160p`.
- Lower rungs (`360p` to `720p`) are closer to target than higher rungs.

## Raw Sweep Results

Source of truth for the latest combined run (including `sw`, `hw`, and `hwmatch`) is:

- `generate_abr/output/crf_sweep_vmaf/crf_bandwidth_sweep_new.csv`

This file contains all 180 rows:

- `2 codecs x 3 modes (sw, hw, hwmatch) x 6 resolutions x 5 CRFs`

To view the full raw results:

```bash
cat generate_abr/output/crf_sweep_vmaf/crf_bandwidth_sweep_new.csv
```

Preview (header + first 15 rows):

```text
resolution,width,height,codec,mode,encoder,crf,hw_quality_qv,target_avg_mbps,avg_bandwidth_mbps,peak_bandwidth_mbps,vmaf_mean
360p,640,360,h264,hw,h264_videotoolbox,18,51,,1.188,2.587,86.092
360p,640,360,h264,hw,h264_videotoolbox,20,48,,1.068,2.325,84.396
360p,640,360,h264,hw,h264_videotoolbox,22,45,,0.812,1.752,78.082
360p,640,360,h264,hw,h264_videotoolbox,24,42,,0.710,1.524,74.945
360p,640,360,h264,hw,h264_videotoolbox,26,39,,0.640,1.361,72.180
540p,960,540,h264,hw,h264_videotoolbox,18,51,,2.336,5.253,86.373
540p,960,540,h264,hw,h264_videotoolbox,20,48,,2.116,4.749,84.725
540p,960,540,h264,hw,h264_videotoolbox,22,45,,1.632,3.621,78.405
540p,960,540,h264,hw,h264_videotoolbox,24,42,,1.437,3.175,75.351
540p,960,540,h264,hw,h264_videotoolbox,26,39,,1.300,2.860,72.524
720p,1280,720,h264,hw,h264_videotoolbox,18,51,,3.622,8.182,86.587
720p,1280,720,h264,hw,h264_videotoolbox,20,48,,3.294,7.429,84.960
720p,1280,720,h264,hw,h264_videotoolbox,22,45,,2.561,5.737,78.750
720p,1280,720,h264,hw,h264_videotoolbox,24,42,,2.267,5.059,75.779
720p,1280,720,h264,hw,h264_videotoolbox,26,39,,2.061,4.580,72.937
```
