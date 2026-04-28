#!/usr/bin/env bash
#
# Walk an InfiniteStream content directory and emit thumbnails for any
# entry that's missing them. Skips dirs that already have a thumbnail
# unless --force is passed.
#
# Source preference order (per content):
#   1. /data/originals/{stem}.{mp4,mov,mkv,...}   — pre-burnin upload,
#      matched by content-name stem (everything before "_p200_codec").
#      Set --originals to override the path.
#   2. {dir}/360p/init.mp4 + segment_00001.m4s    — last-resort fallback;
#      these segments have the AVG/PEAK/codec burn-in applied.
#
# Output per content: thumbnail-small.jpg (320 w), thumbnail.jpg (640 w),
# thumbnail-large.jpg (1280 w). Single ffmpeg pass via the `thumbnail`
# filter so we skip black / mostly-black frames.
#
# Usage:
#   ./backfill_thumbnails.sh /media/dynamic_content
#   ./backfill_thumbnails.sh /media/dynamic_content --force
#   ./backfill_thumbnails.sh /media/dynamic_content --originals /custom/path
set -euo pipefail

CONTENT_DIR=""
ORIGINALS_DIR="${INFINITE_STREAM_SOURCES_DIR:-/data/originals}"
FORCE="false"

while [[ $# -gt 0 ]]; do
    case "$1" in
        --force) FORCE="true"; shift ;;
        --originals) ORIGINALS_DIR="$2"; shift 2 ;;
        --help|-h)
            sed -n '2,18p' "$0"; exit 0 ;;
        -*)
            echo "Unknown flag: $1" >&2; exit 64 ;;
        *)
            if [[ -z "$CONTENT_DIR" ]]; then CONTENT_DIR="$1"; shift
            else echo "Unexpected arg: $1" >&2; exit 64; fi ;;
    esac
done

if [[ -z "$CONTENT_DIR" || ! -d "$CONTENT_DIR" ]]; then
    echo "Usage: $0 <content_dir> [--force] [--originals <dir>]" >&2
    exit 64
fi
if ! command -v ffmpeg >/dev/null 2>&1; then
    echo "ffmpeg not found in PATH" >&2
    exit 69
fi

log() { echo "[backfill] $*"; }

# Strip _p200_{h264|hevc|av1}[_TIMESTAMP] from a content name to recover
# the original-source stem we'd expect in /data/originals.
strip_codec() {
    local n=$1
    # case-insensitive lowercase first
    n=$(echo "$n" | tr '[:upper:]' '[:lower:]')
    # strip optional trailing _YYYYMMDD_HHMMSS, then _h264|_hevc|_av1, then _p200
    n=$(echo "$n" | sed -E 's/_[0-9]{8}_[0-9]{6}$//')
    n=$(echo "$n" | sed -E 's/_(h264|hevc|h265|av1)$//')
    n=$(echo "$n" | sed -E 's/_p200$//')
    printf '%s' "$n"
}

# Best-effort lookup of an /originals/ file matching a stripped content stem.
# Tries exact stem first, then case-insensitive contains-match.
find_original() {
    local stem=$1
    [[ -d "$ORIGINALS_DIR" ]] || return 1
    local found=""
    for ext in mp4 mov mkv m4v webm; do
        for f in "$ORIGINALS_DIR"/"$stem".$ext "$ORIGINALS_DIR"/"$stem".${ext^^}; do
            [[ -f "$f" ]] && { printf '%s' "$f"; return 0; }
        done
    done
    # Loose match: any file whose lowercased stem contains the content stem.
    found=$(ls "$ORIGINALS_DIR" 2>/dev/null \
        | awk -v s="$stem" 'BEGIN{IGNORECASE=1} index(tolower($0), s){print; exit}')
    if [[ -n "$found" ]]; then
        printf '%s' "$ORIGINALS_DIR/$found"
        return 0
    fi
    return 1
}

# Single-decode 3-output ffmpeg pass with the `thumbnail` filter to dodge
# black / mostly-black frames. Returns 0 on success, 1 on failure.
extract_thumbs() {
    local src=$1
    local dir=$2
    local seek=${3:-10}
    local fc="[0:v]thumbnail=300,split=3[a][b][c];"
    fc+="[a]scale='min(320,iw)':-2[s];"
    fc+="[b]scale='min(640,iw)':-2[m];"
    fc+="[c]scale='min(1280,iw)':-2[l]"
    local seek_args=()
    [[ "$seek" -gt 0 ]] && seek_args=(-ss "$seek")
    ffmpeg -nostdin -y -loglevel error \
        "${seek_args[@]}" -i "$src" \
        -filter_complex "$fc" \
        -map "[s]" -frames:v 1 -q:v 4 "${dir}/thumbnail-small.jpg" \
        -map "[m]" -frames:v 1 -q:v 4 "${dir}/thumbnail.jpg" \
        -map "[l]" -frames:v 1 -q:v 4 "${dir}/thumbnail-large.jpg"
}

made=0
skipped=0
failed=0

for dir in "$CONTENT_DIR"/*/; do
    name="$(basename "$dir")"
    case "$name" in .*|_*) continue ;; esac

    if [[ -f "${dir}thumbnail.jpg" && "$FORCE" != "true" ]]; then
        skipped=$((skipped+1))
        continue
    fi

    stem=$(strip_codec "$name")
    src=""
    src_kind=""

    # 1. Try the pre-burnin original.
    if original=$(find_original "$stem"); then
        src="$original"
        src_kind="original"
    fi

    # 2. Fall back to the smallest available rendition's first segment.
    if [[ -z "$src" ]]; then
        for res_dir in "${dir}360p" "${dir}540p" "${dir}480p" "${dir}720p" "${dir}1080p" "${dir}2160p"; do
            init="${res_dir}/init.mp4"
            seg="${res_dir}/segment_00001.m4s"
            if [[ -f "$init" && -f "$seg" ]]; then
                tmp_input="${dir}.thumb.tmp.mp4"
                cat "$init" "$seg" >"$tmp_input"
                src="$tmp_input"
                src_kind="segment-fallback"
                break
            fi
        done
    fi

    if [[ -z "$src" ]]; then
        log "skip $name (no source found)"
        skipped=$((skipped+1))
        continue
    fi

    # First attempt with seek=10. If clip is shorter than 10 s it'll fail;
    # retry from the start.
    if extract_thumbs "$src" "$dir" 10 || extract_thumbs "$src" "$dir" 0; then
        log "made $name ($src_kind)"
        made=$((made+1))
    else
        log "fail $name"
        failed=$((failed+1))
    fi

    [[ "$src_kind" == "segment-fallback" ]] && rm -f "$src"
done

log "thumbnails: $made made, $skipped skipped, $failed failed"
