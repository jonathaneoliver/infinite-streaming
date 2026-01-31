# generate_abr

**Status (Jan 2026):** The active encoding pipeline is `create_abr_ladder.sh` with two Python helpers.

## Active scripts
- `create_abr_ladder.sh` (main pipeline)
- `create_hls_manifests.py` (HLS manifest generation)
- `convert_to_segmentlist.py` (SegmentTemplate → SegmentList)

## Notes
- Legacy test scripts (avsync, LL-HLS tests, etc.) were removed.
- Output is written under the target content directory in `dynamic_content`.
