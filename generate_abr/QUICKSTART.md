# generate_abr Quickstart

```bash
./create_abr_ladder.sh --input /boss/originals/video.mp4 --codec h264
```

Outputs:
- DASH manifest: `manifest.mpd`
- HLS master: `master.m3u8`

Helpers:
- `create_hls_manifests.py`
- `convert_to_segmentlist.py`
