#!/bin/sh
# Make a take's RAW tracks editable in DaVinci Resolve.
#
#   tools/demo/transcode_for_nle.sh /Volumes/4TB/smashing-demo/take6
#
# WHY: Playwright records the browser as VP8 in WebM, which Resolve cannot
# decode on macOS. Its import log says "File not found in search directories",
# which reads like a path problem and is not — the file is right there. Proven
# by importing both tracks in one XML: the phone's H.264 .mov linked and the
# .webm did not, same volume, same absolute path form.
#
# The phone recording is already H.264 from QuickTime, so it is left alone —
# re-encoding it would cost an hour and a generation of quality for nothing.
#
# CODEC CHOICE, and why it depends on the frame size:
#   - VideoToolbox's H.264 encoder refuses anything over 4096 in either
#     dimension: it fails to create a compression session (-12903) and the
#     error names bit_rate/rate/width/height, which sends you tuning the wrong
#     knob entirely. A full-height capture is 3200x6800, so it is always over.
#   - VideoToolbox's HEVC encoder takes that size, and Resolve on macOS decodes
#     hvc1 through the same framework. So: HEVC when the capture is tall,
#     H.264 when it fits, both hardware.
#   - libx264 is the last resort — correct at any size, just slow.
#   - ProRes was tried and is wrong here: 78MB -> 4.9GB at a QUARTER of these
#     dimensions, so a full-height take runs to tens of gigabytes.
#   - `-g 25` puts a keyframe every second. The ~250-frame default GOP is what
#     makes scrubbing in an NLE feel stuck, and that responsiveness is most of
#     what ProRes would have bought.
set -eu

TAKE="${1:?usage: transcode_for_nle.sh <take-dir>}"
BITRATE="${NLE_BITRATE:-60M}"

WEBM=$(ls "$TAKE"/page@*.webm 2>/dev/null | head -1)
[ -n "$WEBM" ] || { echo "no browser recording in $TAKE" >&2; exit 1; }
OUT="$TAKE/browser-nle.mov"

# A run that died mid-encode leaves a file behind. Trusting mere existence
# turned a hard failure into a permanent "already have": the 0-byte output was
# reported as success and would have been handed to Resolve as one. Require a
# readable video stream, not a directory entry.
if [ -f "$OUT" ] && ffprobe -v error -select_streams v:0 \
        -show_entries stream=codec_name -of csv=p=0 "$OUT" 2>/dev/null | grep -q .; then
    echo "already have $OUT"
    exit 0
fi
rm -f "$OUT"

W=$(ffprobe -v error -select_streams v:0 -show_entries stream=width -of csv=p=0 "$WEBM")
H=$(ffprobe -v error -select_streams v:0 -show_entries stream=height -of csv=p=0 "$WEBM")

if [ "$W" -le 4096 ] && [ "$H" -le 4096 ]; then
    CODEC="h264_videotoolbox"
    EXTRA=""
else
    CODEC="hevc_videotoolbox"
    EXTRA="-tag:v hvc1"
fi

TMP="$TAKE/.browser-nle.partial.mov"
rm -f "$TMP"

echo "browser: $(basename "$WEBM")  ${W}x${H} -> browser-nle.mov  ($CODEC $BITRATE, 1s GOP)"
if ! ffmpeg -y -v error -stats -i "$WEBM" \
        -c:v "$CODEC" -b:v "$BITRATE" $EXTRA -g 25 \
        -pix_fmt yuv420p -movflags +faststart -an "$TMP"; then
    echo "  $CODEC failed — falling back to libx264 (slower, always works)" >&2
    rm -f "$TMP"
    ffmpeg -y -v error -stats -i "$WEBM" \
        -c:v libx264 -preset veryfast -crf 18 -g 25 \
        -pix_fmt yuv420p -movflags +faststart -an "$TMP"
fi

# Prove it decodes before claiming it: ffmpeg can exit 0 having written a
# container with no packets in it, which is exactly how this failed silently.
ffprobe -v error -select_streams v:0 -show_entries stream=codec_name \
        -of csv=p=0 "$TMP" 2>/dev/null | grep -q . || {
    echo "encode produced no video stream — leaving $TMP for inspection" >&2
    exit 1
}

mv "$TMP" "$OUT"
echo "wrote $OUT  ($(du -h "$OUT" | cut -f1))"
