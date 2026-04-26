#!/usr/bin/env bash
#
# Walk an InfiniteStream content directory and emit a thumbnail.jpg for any
# entry that's missing one. Skips dirs that already have a thumbnail unless
# --force is passed.
#
# A "content entry" here is any subdirectory with at least one fMP4 init
# segment under {res}/init.mp4 — those exist for every encoded clip and
# avoid touching half-finished uploads.
#
# Usage:
#   ./backfill_thumbnails.sh /path/to/dynamic_content
#   ./backfill_thumbnails.sh /path/to/dynamic_content --force
#
# Run inside the container against /media/dynamic_content, or on the host
# against the bind-mounted output dir.
set -euo pipefail

CONTENT_DIR="${1:-}"
FORCE="false"
if [[ "${2:-}" == "--force" || "${1:-}" == "--force" ]]; then
    FORCE="true"
    [[ "${1:-}" == "--force" ]] && CONTENT_DIR="${2:-}"
fi

if [[ -z "$CONTENT_DIR" ]]; then
    echo "Usage: $0 <content_dir> [--force]" >&2
    exit 64
fi
if [[ ! -d "$CONTENT_DIR" ]]; then
    echo "Not a directory: $CONTENT_DIR" >&2
    exit 66
fi
if ! command -v ffmpeg >/dev/null 2>&1; then
    echo "ffmpeg not found in PATH" >&2
    exit 69
fi

made=0
skipped=0
failed=0

for dir in "$CONTENT_DIR"/*/; do
    name="$(basename "$dir")"
    case "$name" in .*|_*) continue ;; esac

    thumb="${dir}thumbnail.jpg"
    if [[ -f "$thumb" && "$FORCE" != "true" ]]; then
        skipped=$((skipped+1))
        continue
    fi

    # Pick the smallest available rendition's first segment as input — the
    # source upload may not be on disk anymore (post-encode cleanup), but
    # the per-rendition fMP4 fragments always are. Walk the resolutions in
    # ascending order so we read the cheapest one available.
    src=""
    for res_dir in "${dir}360p" "${dir}540p" "${dir}480p" "${dir}720p" "${dir}1080p" "${dir}2160p"; do
        init="${res_dir}/init.mp4"
        seg="${res_dir}/segment_00001.m4s"
        if [[ -f "$init" && -f "$seg" ]]; then
            # Concatenate init + first segment for ffmpeg via a temp file
            # (some ffmpeg builds choke on `concat:` with fMP4).
            tmp_input="${dir}.thumb.tmp.mp4"
            cat "$init" "$seg" >"$tmp_input"
            src="$tmp_input"
            break
        fi
    done

    if [[ -z "$src" ]]; then
        echo "skip $name (no rendition found)"
        skipped=$((skipped+1))
        continue
    fi

    if ffmpeg -nostdin -y -loglevel error \
        -i "$src" \
        -frames:v 1 -vf "scale='min(640,iw)':-2" \
        -q:v 4 "$thumb"; then
        echo "made $name"
        made=$((made+1))
    else
        echo "fail $name"
        failed=$((failed+1))
    fi
    rm -f "$src"
done

echo "thumbnails: $made made, $skipped skipped, $failed failed"
