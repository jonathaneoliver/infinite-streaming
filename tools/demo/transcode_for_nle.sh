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
# CODEC CHOICE: H.264 in a .mov, hardware-encoded.
#   - ProRes was tried and is wrong at this frame size: 78MB -> 4.9GB at a
#     QUARTER of these dimensions, so a full-height take would run to tens of
#     gigabytes.
#   - `-g 25` puts a keyframe every second. H.264's ~250-frame default GOP is
#     what makes scrubbing in an NLE feel stuck, and that responsiveness is
#     most of what ProRes would have bought.
#   - VideoToolbox because this is a mechanical conversion, measured here at
#     ~3.5x less CPU than libx264 for the same job.
set -eu

TAKE="${1:?usage: transcode_for_nle.sh <take-dir>}"
BITRATE="${NLE_BITRATE:-60M}"

WEBM=$(ls "$TAKE"/page@*.webm 2>/dev/null | head -1)
[ -n "$WEBM" ] || { echo "no browser recording in $TAKE" >&2; exit 1; }
OUT="$TAKE/browser-nle.mov"

if [ -f "$OUT" ]; then
    echo "already have $OUT"
else
    echo "browser: $(basename "$WEBM") -> browser-nle.mov  (H.264 $BITRATE, 1s GOP)"
    ffmpeg -y -v error -stats -i "$WEBM" \
        -c:v h264_videotoolbox -b:v "$BITRATE" -g 25 \
        -pix_fmt yuv420p -movflags +faststart -an "$OUT"
fi

echo
echo "--- ready for Resolve ---"
for f in "$OUT" "$TAKE/phone.mov"; do
    [ -f "$f" ] || continue
    ffprobe -v error -select_streams v:0 \
        -show_entries stream=codec_name,width,height \
        -show_entries format=duration -of default=nw=1 "$f" \
        | tr '\n' ' '
    echo " <- $(basename "$f")"
done
echo
echo "Import in Resolve: open a project, then File > Import > Media."
echo "(Import PROJECT on the Project Manager only accepts .drp and will grey"
echo " these out — that is the trap, not a permissions problem.)"
