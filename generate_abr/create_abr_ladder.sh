#!/usr/bin/env bash

################################################################################
# ABR Ladder Generator - H.265 + H.264 + AV1 with Timecode Burn-in
# Creates DASH and HLS packages from any video source
#
# Features:
# - Universal input support: MP4, MKV, MOV, M3U8, TS, and more
# - Adaptive quality levels: up to 6 tiers (360p through 2160p)
# - Dynamic frame rate detection and GOP calculation
# - Multi codec support: H.265 (HEVC), H.264 (AVC), and AV1
# - Optional hardware encoding: VideoToolbox (macOS) with 5-13x speedup
# - Software encoding by default (libx265/libx264)
# - SMPTE timecode burn-in per variant
# - Closed GOPs with fixed keyframe intervals
# - 4-second segment duration
# - DASH output with SegmentList format
# - HLS output reusing same segments
# - Organized output with resolution subdirectories
################################################################################

# Note: set -e removed to prevent premature script exit during Phase 6
# Individual phase failures are handled explicitly
set -o pipefail

################################################################################
# Usage Function
################################################################################

usage() {
    cat << EOF
Usage: $0 --input <file_or_m3u8> [OPTIONS]
       $0 --resume-package-from <abr_ladder_dir> [OPTIONS]

Create ABR ladder with multiple resolutions and codecs from any video source.

Required Arguments:
  One of:
    --input <path>                 Input video file (.mp4, .mkv, .mov) or HLS playlist (.m3u8)
    --resume-package-from <path>   Resume from an existing abr_ladder temp dir and run
                                   packaging/manifests only (skip re-encoding)

Optional Arguments:
  --output <name>         Base name for output files (default: derived from input filename)
  --output-dir <path>     Base output directory (default: current working directory)
                          Relative paths are resolved from where you run the script
  --resume-package-from <path> Resume from existing temp dir with encoded files
                         (expects files like h264_360p.mp4/hevc_360p.mp4 and optional audio.mp4)
  --codec <hevc|h264|av1|both|all> Codec selection (default: both)
  --no-hevc              Skip HEVC encoding (same as --codec h264)
  --no-h264              Skip H.264 encoding (same as --codec hevc)
  --no-av1               Skip AV1 encoding (same as --codec both)
  --time <seconds>       Limit video duration (e.g., --time 30 for 30 seconds)
  --force-software       Force software encoding (default behavior)
  --force-hardware       Force hardware encoding via VideoToolbox (macOS)
  --max-res <resolution> Limit maximum resolution tier encoded
                         Valid: 360p, 540p, 720p, 1080p, 1440p, 2160p
                         Example: --max-res 1080p (skips 1440p, 2160p)
                         Useful for bandwidth/storage constraints
  --padding              Enable padding to segment boundaries with BLACK frames
                         (Adds "PADDING" label to padded frames)
  --padding-pink         Enable padding to segment boundaries with PINK frames
                         (Adds "PADDING" label to padded frames)
  --no-padding           Disable automatic padding to segment boundaries
                         (padding is OFF by default)
  --hls-format <format>  HLS output format (default: fmp4)
                         Options:
                           fmp4 - fragmented MP4 segments (default, modern)
                           ts   - MPEG-TS segments (legacy, maximum compatibility)
                           both - Generate both fMP4 and TS variants
  --segment-duration <s> Segment duration in seconds (default: 6)
  --partial-duration <s> Partial/GOP duration in seconds (default: 0.2)
  --gop-duration <s>     GOP/keyframe duration in seconds (default: 1.0)
  --bitrate-override-hevc <map> Override HEVC ladder kbps by resolution (e.g. 360p=1367,540p=2617)
  --bitrate-override-h264 <map> Override H264 ladder kbps by resolution (e.g. 360p=1421,540p=2762)
  --vmaf-lookup-csv <p>  CSV from crf_bandwidth_sweep.py for estimated VMAF burn-in
                         (default: ./crf_bandwidth_sweep_newer.csv next to this script, if present)
  --vmaf-lookup-mode <m> Lookup mode: auto|sw|hw|hwmatch (default: auto)
  --keep-mezzanine       Preserve intermediate encoded files
  --help                 Show this help message

Hardware Encoding:
  By default, software encoders are used:
  - libx265 / libx264 (software)
  - Use --force-hardware to enable VideoToolbox on macOS
  - If --force-hardware is set but unavailable, the script exits with error

Optional Hardware Encoding:
  - macOS: VideoToolbox (hevc_videotoolbox, h264_videotoolbox)
  - Provides 5-13x faster encoding with similar quality
  - Enable with --force-hardware

Examples:
  # From MP4 file (outputs to current directory, software encoding default)
  $0 --input video.mp4

  # From MKV file with custom output name
  $0 --input movie.mkv --output my_video

  # From MKV file with custom output directory (absolute path)
  $0 --input movie.mkv --output-dir /path/to/output

  # Relative output directory (resolved from current working directory)
  $0 --input movie.mkv --output-dir ./streams

  # From HLS playlist
  $0 --input playlist.m3u8

  # HEVC only
  $0 --input video.mp4 --codec hevc
  
  # H.264 only
  $0 --input video.mp4 --no-hevc

  # AV1 only
  $0 --input video.mp4 --codec av1

  # Convert only first 30 seconds
  $0 --input video.mp4 --time 30
  
  # Limit to 1080p maximum (save time/storage on 4K source)
  $0 --input 4k_video.mp4 --max-res 1080p
  
  # Force software encoding explicitly (same as default)
  $0 --input video.mp4 --force-software
  
  # Force hardware encoding (VideoToolbox, macOS)
  $0 --input video.mp4 --force-hardware
  
  # Enable padding with black frames
  $0 --input video.mp4 --padding
  
  # Enable padding with pink frames (easier to spot padding)
  $0 --input video.mp4 --padding-pink
  
  # Disable padding completely (default behavior)
  $0 --input video.mp4 --no-padding

  # Preserve intermediate encoded files (mezzanine + per-variant MP4s)
  $0 --input video.mp4 --keep-mezzanine

  # Burn estimated VMAF from a prior sweep CSV
  $0 --input video.mp4 --vmaf-lookup-csv /path/to/crf_bandwidth_sweep_new.csv

  # Resume at packaging/manifests phase from a prior run temp directory
  $0 --resume-package-from ./my_video_tmp/abr_ladder_12345 --codec h264

Output Behavior:
  - Output files are created in your current working directory by default
  - Run from anywhere: ../path/to/$0 --input video.mp4
    Creates: ./video_hevc/ and ./video_h264/ (in your current directory)

Output Structure:
  <basename>_hevc/    (if HEVC enabled)
  <basename>_h264/    (if H.264 enabled)
  Each contains DASH manifest + HLS playlists with up to 6 resolution tiers

Supported Input Formats:
  - Video files: .mp4, .mkv, .mov, .avi, .webm, .ts
  - HLS playlists: .m3u8 (media playlists or master playlists with variants)
  - Any format supported by FFmpeg
  
  Note: For multi-variant HLS master playlists, the highest bitrate variant will be selected

EOF
}

################################################################################
# Parse Arguments
################################################################################

INPUT_FILE=""
OUTPUT_BASE_NAME=""  # Custom base name for output files
OUTPUT_BASE_DIR=""
CODEC_SELECTION="both"  # both, hevc, h264, av1, all
CODEC_SELECTION_EXPLICIT=false
TIME_LIMIT=""  # Optional duration limit in seconds
FORCE_SOFTWARE=false  # Force software encoding (disables hardware)
FORCE_HARDWARE=false  # Force hardware encoding (VideoToolbox)
HLS_FORMAT="fmp4"  # fmp4, ts, both
PAD_TO_SEGMENT_BOUNDARY=false  # Padding is disabled by default
MAX_RESOLUTION_HEIGHT=""  # Optional max resolution limit (e.g., "1080p")
PADDING_COLOR="black"  # Color for padding frames (black or pink)
KEEP_MEZZANINE=false  # Preserve intermediate encoded files
VMAF_LOOKUP_CSV=""    # Optional CSV for estimated VMAF burn-in label
VMAF_LOOKUP_MODE="auto"  # auto|sw|hw|hwmatch
BITRATE_OVERRIDE_HEVC=""  # Optional map: "360p=1367,540p=2617,..."
BITRATE_OVERRIDE_H264=""  # Optional map: "360p=1421,540p=2762,..."
SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
DEFAULT_VMAF_LOOKUP_CSV="${SCRIPT_DIR}/crf_bandwidth_sweep_newer.csv"
RESUME_PACKAGE_FROM=""    # Optional path to existing abr_ladder temp dir
RESUME_MODE=false
FRAGMENT_PARSER_SCRIPT="" # Auto-detected path to parse_fmp4_fragments.py

while [[ $# -gt 0 ]]; do
    case $1 in
        --input)
            INPUT_FILE="$2"
            shift 2
            ;;
        --output)
            OUTPUT_BASE_NAME="$2"
            shift 2
            ;;
        --output-dir)
            OUTPUT_BASE_DIR="$2"
            shift 2
            ;;
        --resume-package-from)
            RESUME_PACKAGE_FROM="$2"
            RESUME_MODE=true
            shift 2
            ;;
        --codec)
            CODEC_SELECTION="$2"
            CODEC_SELECTION_EXPLICIT=true
            shift 2
            ;;
        --no-hevc)
            CODEC_SELECTION="h264"
            CODEC_SELECTION_EXPLICIT=true
            shift
            ;;
        --no-h264)
            CODEC_SELECTION="hevc"
            CODEC_SELECTION_EXPLICIT=true
            shift
            ;;
        --no-av1)
            CODEC_SELECTION="both"
            CODEC_SELECTION_EXPLICIT=true
            shift
            ;;
        --time)
            TIME_LIMIT="$2"
            shift 2
            ;;
        --force-software)
            FORCE_SOFTWARE=true
            shift
            ;;
        --force-hardware)
            FORCE_HARDWARE=true
            shift
            ;;
        --no-padding)
            PAD_TO_SEGMENT_BOUNDARY=false
            shift
            ;;
        --padding)
            PAD_TO_SEGMENT_BOUNDARY=true
            PADDING_COLOR="black"
            shift
            ;;
        --padding-pink)
            PAD_TO_SEGMENT_BOUNDARY=true
            PADDING_COLOR="pink"
            shift
            ;;
        --max-res)
            MAX_RESOLUTION_HEIGHT="$2"
            shift 2
            ;;
        --hls-format)
            HLS_FORMAT="$2"
            shift 2
            ;;
        --segment-duration)
            SEGMENT_DURATION="$2"
            shift 2
            ;;
        --partial-duration)
            PARTIAL_DURATION="$2"
            shift 2
            ;;
        --gop-duration)
            GOP_DURATION="$2"
            shift 2
            ;;
        --bitrate-override-hevc)
            BITRATE_OVERRIDE_HEVC="$2"
            shift 2
            ;;
        --bitrate-override-h264)
            BITRATE_OVERRIDE_H264="$2"
            shift 2
            ;;
        --vmaf-lookup-csv)
            VMAF_LOOKUP_CSV="$2"
            shift 2
            ;;
        --vmaf-lookup-mode)
            VMAF_LOOKUP_MODE="$2"
            shift 2
            ;;
        --help|-h)
            usage
            exit 0
            ;;
        --keep-mezzanine)
            KEEP_MEZZANINE=true
            shift
            ;;
        *)
            echo "Error: Unknown option $1"
            usage
            exit 1
            ;;
    esac
done

# Validate required arguments
if [[ "$RESUME_MODE" == "false" ]] && [[ -z "$INPUT_FILE" ]]; then
    echo "Error: either --input or --resume-package-from is required"
    usage
    exit 1
fi

# Validate resume path if requested
if [[ "$RESUME_MODE" == "true" ]]; then
    if [[ -z "$RESUME_PACKAGE_FROM" ]]; then
        echo "Error: --resume-package-from requires a path"
        exit 1
    fi
    if [[ "$RESUME_PACKAGE_FROM" != /* ]]; then
        RESUME_PACKAGE_FROM="$(cd "$(dirname "$RESUME_PACKAGE_FROM")" && pwd)/$(basename "$RESUME_PACKAGE_FROM")"
    fi
    if [[ ! -d "$RESUME_PACKAGE_FROM" ]]; then
        echo "Error: resume directory not found: $RESUME_PACKAGE_FROM"
        exit 1
    fi
fi

if [[ "$RESUME_MODE" == "false" ]]; then
    if [[ ! -f "$INPUT_FILE" ]]; then
        echo "Error: Input file not found: $INPUT_FILE"
        exit 1
    fi

    # Convert to absolute path (needed for M3U8 files with relative segment paths)
    if [[ "$INPUT_FILE" != /* ]]; then
        INPUT_FILE="$(cd "$(dirname "$INPUT_FILE")" && pwd)/$(basename "$INPUT_FILE")"
    fi
fi

# Validate codec selection
if [[ "$CODEC_SELECTION" != "both" ]] && [[ "$CODEC_SELECTION" != "all" ]] && [[ "$CODEC_SELECTION" != "hevc" ]] && [[ "$CODEC_SELECTION" != "h264" ]] && [[ "$CODEC_SELECTION" != "av1" ]]; then
    echo "Error: --codec must be 'both', 'all', 'hevc', 'h264', or 'av1'"
    exit 1
fi

# Validate time limit if provided
if [[ -n "$TIME_LIMIT" ]]; then
    if ! [[ "$TIME_LIMIT" =~ ^[0-9]+$ ]]; then
        echo "Error: --time must be a positive integer (seconds)"
        exit 1
    fi
    if [[ "$TIME_LIMIT" -le 0 ]]; then
        TIME_LIMIT=""
    fi
fi

# Validate HLS format
if [[ "$HLS_FORMAT" != "fmp4" ]] && [[ "$HLS_FORMAT" != "ts" ]] && [[ "$HLS_FORMAT" != "both" ]]; then
    echo "Error: --hls-format must be 'fmp4', 'ts', or 'both'"
    exit 1
fi

# Validate mutually exclusive hardware/software force flags
if [[ "$FORCE_SOFTWARE" == "true" ]] && [[ "$FORCE_HARDWARE" == "true" ]]; then
    echo "Error: --force-software and --force-hardware cannot be used together"
    exit 1
fi

# Validate VMAF lookup mode
if [[ "$VMAF_LOOKUP_MODE" != "auto" ]] && [[ "$VMAF_LOOKUP_MODE" != "sw" ]] && [[ "$VMAF_LOOKUP_MODE" != "hw" ]] && [[ "$VMAF_LOOKUP_MODE" != "hwmatch" ]]; then
    echo "Error: --vmaf-lookup-mode must be 'auto', 'sw', 'hw', or 'hwmatch'"
    exit 1
fi

# Use default lookup CSV if user did not pass one explicitly.
if [[ -z "$VMAF_LOOKUP_CSV" ]] && [[ -f "$DEFAULT_VMAF_LOOKUP_CSV" ]]; then
    VMAF_LOOKUP_CSV="$DEFAULT_VMAF_LOOKUP_CSV"
fi

# Validate VMAF lookup CSV if provided
if [[ -n "$VMAF_LOOKUP_CSV" ]]; then
    if [[ ! -f "$VMAF_LOOKUP_CSV" ]]; then
        echo "Error: --vmaf-lookup-csv file not found: $VMAF_LOOKUP_CSV"
        exit 1
    fi
    # Convert to absolute path for consistent access
    if [[ "$VMAF_LOOKUP_CSV" != /* ]]; then
        VMAF_LOOKUP_CSV="$(cd "$(dirname "$VMAF_LOOKUP_CSV")" && pwd)/$(basename "$VMAF_LOOKUP_CSV")"
    fi
fi

# Validate max-res if provided
if [[ -n "$MAX_RESOLUTION_HEIGHT" ]]; then
    case "$MAX_RESOLUTION_HEIGHT" in
        360p|540p|720p|1080p|1440p|2160p)
            # Valid resolution
            ;;
        *)
            echo "Error: --max-res must be one of: 360p, 540p, 720p, 1080p, 1440p, 2160p"
            echo "Example: --max-res 1080p"
            exit 1
            ;;
    esac
fi

################################################################################
# Configuration
################################################################################

# Allow TMPDIR override via environment variable (defaults to /tmp)
TEMP_BASE="${TMPDIR:-/tmp}"
# Allow TMPDIR-OUTPUT override via environment variable (defaults to output dir)
TMPDIR_OUTPUT="${TMPDIR_OUTPUT:-}"
TEMP_DIR="${TEMP_BASE}/abr_ladder_$$"
mkdir -p "$TEMP_DIR"
# Use Alpine Linux font path if macOS font doesn't exist
if [[ -f "/System/Library/Fonts/Supplemental/Courier New Bold.ttf" ]]; then
    FONT="/System/Library/Fonts/Supplemental/Courier New Bold.ttf"
else
    FONT="/usr/share/fonts/dejavu/DejaVuSansCondensed-Bold.ttf"
    if [[ ! -f "$FONT" ]]; then
        FONT="/usr/share/fonts/ttf-dejavu/DejaVuSansCondensed-Bold.ttf"
    fi
fi
LOG_FILE="$TEMP_DIR/encoding.log"
SKIP_AUDIO=false

# Padding configuration
SEGMENT_DURATION="${SEGMENT_DURATION:-6}"    # Target segment duration in seconds
PARTIAL_DURATION="${PARTIAL_DURATION:-0.2}"  # Partial fragment duration in seconds
GOP_DURATION="${GOP_DURATION:-1.0}"          # GOP/keyframe duration in seconds
MAXRATE_PERCENT="${MAXRATE_PERCENT:-124}"    # Peak cap percentage of target bitrate (<125% guidance)
MULTI_DURATION_LCM=12           # LCM of 2s/4s/6s for multi-duration support
PADDING_THRESHOLD=0.1           # Minimum remainder to trigger padding (seconds)
PADDING_WARNING_RATIO=50        # Warn if padding exceeds this % of total duration
VIDEO_PADDING_DURATION=0        # Calculated video padding duration (applied during encoding)
AUDIO_PADDING_DURATION=0        # Calculated audio padding duration (applied during encoding)

# Report generation and tracking
GENERATE_ENCODING_REPORT=true
ENCODING_START_TIME=""
ENCODING_INFOS=()       # Array to collect info messages
ENCODING_WARNINGS=()    # Array to collect warnings
ENCODING_ERRORS=()      # Array to collect errors

# Variant tracking for reports (associative arrays)
declare -A VARIANT_ENCODE_TIMES     # Key: "hevc_1080p" -> Value: "12"
declare -A VARIANT_FILE_SIZES       # Key: "hevc_1080p" -> Value: "77MB"
declare -A VARIANT_SPEEDS           # Key: "hevc_1080p" -> Value: "17.7x"

# Output directories (will be set later)
OUTPUT_DIR_HEVC=""
OUTPUT_DIR_H264=""
OUTPUT_DIR_AV1=""

# HLS master playlist handling
USE_MASTER_PLAYLIST=false
HIGHEST_VIDEO_STREAM_INDEX=""

# Color codes
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

# All possible resolution tiers (filtered based on source resolution)
# Format: name:width:height:bitrate_h265_kbps:bitrate_h264_kbps:bitrate_av1_kbps:preset:fontsize_tc:fontsize_label:x:y_tc:y_label
declare -a ALL_RESOLUTION_TIERS=(
    "360p:640:360:300:600:300:medium:20:16:10:10:30"
    "540p:960:540:900:1200:900:medium:24:20:10:10:34"
    "720p:1280:720:1500:2400:1500:medium:28:24:10:10:38"
    "1080p:1920:1080:4500:5000:4500:medium:36:32:10:10:45"
    "1440p:2560:1440:7500:11000:7500:medium:42:36:10:10:52"
    "2160p:3840:2160:15000:21700:15000:medium:54:48:10:10:64"
)

# Will be populated after source detection
declare -a PROFILES=()

 # Temp directory will be created after output directories are derived

################################################################################
# Hardware Encoder Detection and Configuration
################################################################################

# Hardware encoding configuration
USE_HARDWARE="no"  # yes|no
HARDWARE_AVAILABLE=false
HARDWARE_ENCODER_HEVC="hevc_videotoolbox"
HARDWARE_ENCODER_H264="h264_videotoolbox"

detect_hardware_encoders() {
    # Check if running on macOS
    if [[ "$(uname)" != "Darwin" ]]; then
        return 1
    fi
    
    # Check if FFmpeg supports VideoToolbox
    if ! ffmpeg -hide_banner -encoders 2>/dev/null | grep -q hevc_videotoolbox; then
        return 1
    fi
    
    if ! ffmpeg -hide_banner -encoders 2>/dev/null | grep -q h264_videotoolbox; then
        return 1
    fi
    
    return 0
}

detect_fragment_parser_script() {
    if [[ -f "/sbin/parse_fmp4_fragments.py" ]]; then
        FRAGMENT_PARSER_SCRIPT="/sbin/parse_fmp4_fragments.py"
        return 0
    fi
    if [[ -f "$SCRIPT_DIR/../parse_fmp4_fragments.py" ]]; then
        FRAGMENT_PARSER_SCRIPT="$SCRIPT_DIR/../parse_fmp4_fragments.py"
        return 0
    fi
    if [[ -f "$SCRIPT_DIR/parse_fmp4_fragments.py" ]]; then
        FRAGMENT_PARSER_SCRIPT="$SCRIPT_DIR/parse_fmp4_fragments.py"
        return 0
    fi
    FRAGMENT_PARSER_SCRIPT=""
    return 1
}

# Auto-detect hardware encoders
if detect_hardware_encoders; then
    HARDWARE_AVAILABLE=true
    ENCODING_INFOS+=("Hardware encoders available: VideoToolbox (HEVC & H.264)")
fi

# Override encode mode based on user flags (software is default).
if [[ "$FORCE_HARDWARE" == "true" ]]; then
    USE_HARDWARE="yes"
    ENCODING_INFOS+=("Hardware encoding forced via --force-hardware flag")
elif [[ "$FORCE_SOFTWARE" == "true" ]]; then
    USE_HARDWARE="no"
    ENCODING_INFOS+=("Software encoding forced via --force-software flag")
else
    ENCODING_INFOS+=("Using software encoding by default (set --force-hardware to enable VideoToolbox)")
fi

select_encoder() {
    local codec=$1  # hevc or h264
    if [[ "$codec" == "av1" ]]; then
        echo "software"
        return
    fi
    
    # If user forced software encoding
    if [[ "$USE_HARDWARE" == "no" ]]; then
        echo "software"
        return
    fi
    
    # If user forced hardware but it's not available
    if [[ "$USE_HARDWARE" == "yes" ]] && [[ "$HARDWARE_AVAILABLE" == "false" ]]; then
        log_error "Hardware encoding requested but not available"
        exit 1
    fi
    
    # Default mode: software encoding.
    echo "software"
}

################################################################################
# Helper Functions
################################################################################

log() {
    echo -e "${GREEN}[$(date +'%H:%M:%S')]${NC} $1" | tee -a "$LOG_FILE"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1" | tee -a "$LOG_FILE"
}

log_success() {
    echo -e "${GREEN}✓${NC} $1" | tee -a "$LOG_FILE"
}

log_warn() {
    echo -e "${YELLOW}⚠${NC} $1" | tee -a "$LOG_FILE"
}

ffmpeg() {
    log "FFmpeg command: ffmpeg $*"
    # For encoding calls (those with -i input), add -progress pipe:1 so progress
    # lines are emitted unbuffered. ffmpeg's normal stderr progress is block-
    # buffered when piped, which makes the dashboard meter jump in chunks.
    local has_input=false
    for arg in "$@"; do
        if [ "$arg" = "-i" ]; then
            has_input=true
            break
        fi
    done
    if $has_input; then
        command ffmpeg -progress pipe:1 -stats_period 0.5 "$@"
    else
        command ffmpeg "$@"
    fi
    local rc=$?
    if [[ $rc -eq 0 ]]; then
        log "FFmpeg finished (rc=0)"
    else
        log_error "FFmpeg failed (rc=$rc)"
    fi
    return $rc
}

print_header() {
    echo "" | tee -a "$LOG_FILE"
    echo -e "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}" | tee -a "$LOG_FILE"
    echo -e "${CYAN}$1${NC}" | tee -a "$LOG_FILE"
    echo -e "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}" | tee -a "$LOG_FILE"
    echo "" | tee -a "$LOG_FILE"
}

get_resolution_name() {
    local width=$1
    case $width in
        480)  echo "270p" ;;
        1920) echo "1080p" ;;
        1280) echo "720p" ;;
        960)  echo "540p" ;;
        640)  echo "360p" ;;
        2560) echo "1440p" ;;
        3840) echo "2160p" ;;
        *)    echo "${width}p" ;;
    esac
}

get_resolution_height() {
    local res_name=$1
    case "$res_name" in
        "360p") echo "360" ;;
        "540p") echo "540" ;;
        "720p") echo "720" ;;
        "1080p") echo "1080" ;;
        "1440p") echo "1440" ;;
        "2160p") echo "2160" ;;
        *) echo "0" ;;
    esac
}

resolve_bitrate_override() {
    local mapping="$1"
    local res_name="$2"
    local default_kbps="$3"

    if [[ -z "$mapping" ]]; then
        echo "$default_kbps"
        return 0
    fi

    local entry=""
    IFS=',' read -ra entries <<< "$mapping"
    for entry in "${entries[@]}"; do
        # Strip whitespace to accept formats like "360p = 1421"
        local normalized="${entry//[[:space:]]/}"
        if [[ "$normalized" =~ ^([^=]+)=([0-9]+)$ ]]; then
            local key="${BASH_REMATCH[1]}"
            local val="${BASH_REMATCH[2]}"
            if [[ "$key" == "$res_name" ]]; then
                echo "$val"
                return 0
            fi
        fi
    done

    echo "$default_kbps"
    return 0
}

estimate_vmaf_from_lookup() {
    local codec="$1"
    local encoder_type="$2"  # software|hardware
    local res_name="$3"
    local bitrate_kbps="$4"

    if [[ -z "$VMAF_LOOKUP_CSV" ]] || [[ ! -f "$VMAF_LOOKUP_CSV" ]]; then
        return 0
    fi
    # AV1 is not part of current characterization CSV in this workflow.
    if [[ "$codec" == "av1" ]]; then
        return 0
    fi

    local candidate_modes=()
    if [[ "$VMAF_LOOKUP_MODE" != "auto" ]]; then
        candidate_modes=("$VMAF_LOOKUP_MODE")
    else
        if [[ "$encoder_type" == "software" ]]; then
            candidate_modes=("sw")
        else
            # Hardware bitrate-control production runs are closest to hwmatch.
            candidate_modes=("hwmatch" "hw")
        fi
    fi

    local target_mbps
    target_mbps=$(awk -v kbps="$bitrate_kbps" 'BEGIN{printf "%.6f", kbps/1000.0}')

    local mode
    for mode in "${candidate_modes[@]}"; do
        local pairs
        pairs=$(awk -F, -v c="$codec" -v m="$mode" -v r="$res_name" '
            NR == 1 { next }
            $4 == c && $5 == m && $1 == r && $10 != "" && $12 != "" {
                print $10 "," $12
            }
        ' "$VMAF_LOOKUP_CSV" | sort -t, -k1,1n)

        if [[ -z "$pairs" ]]; then
            continue
        fi

        local prev_avg=""
        local prev_vmaf=""
        local est=""
        while IFS=, read -r avg_mbps vmaf; do
            if awk -v t="$target_mbps" -v a="$avg_mbps" 'BEGIN{exit !(t==a)}'; then
                est="$vmaf"
                break
            fi
            if awk -v t="$target_mbps" -v a="$avg_mbps" 'BEGIN{exit !(t<a)}'; then
                if [[ -n "$prev_avg" ]]; then
                    est=$(awk -v t="$target_mbps" -v a0="$prev_avg" -v v0="$prev_vmaf" -v a1="$avg_mbps" -v v1="$vmaf" '
                        BEGIN {
                            if (a1 == a0) { printf "%.3f", v1; exit }
                            ratio = (t - a0) / (a1 - a0)
                            printf "%.3f", v0 + ratio * (v1 - v0)
                        }')
                else
                    # Target is below first sample; use first sample as conservative nearest estimate.
                    est="$vmaf"
                fi
                break
            fi
            prev_avg="$avg_mbps"
            prev_vmaf="$vmaf"
        done <<< "$pairs"

        if [[ -z "$est" ]] && [[ -n "$prev_vmaf" ]]; then
            # Target above highest sampled bitrate; clamp to highest sample.
            est="$prev_vmaf"
        fi

        if [[ -n "$est" ]]; then
            echo "$est"
            return 0
        fi
    done

    return 0
}

estimate_average_bandwidth_from_lookup() {
    local codec="$1"
    local encoder_type="$2"  # software|hardware
    local res_name="$3"
    local bitrate_kbps="$4"

    if [[ -z "$VMAF_LOOKUP_CSV" ]] || [[ ! -f "$VMAF_LOOKUP_CSV" ]]; then
        return 0
    fi
    # AV1 is not part of current characterization CSV in this workflow.
    if [[ "$codec" == "av1" ]]; then
        return 0
    fi

    local candidate_modes=()
    if [[ "$VMAF_LOOKUP_MODE" != "auto" ]]; then
        candidate_modes=("$VMAF_LOOKUP_MODE")
    else
        if [[ "$encoder_type" == "software" ]]; then
            candidate_modes=("sw")
        else
            # Hardware bitrate-control production runs are closest to hwmatch.
            candidate_modes=("hwmatch" "hw")
        fi
    fi

    local target_mbps
    target_mbps=$(awk -v kbps="$bitrate_kbps" 'BEGIN{printf "%.6f", kbps/1000.0}')

    local mode
    for mode in "${candidate_modes[@]}"; do
        local pairs
        # X axis: target_avg_mbps when available (hwmatch), else avg_bandwidth_mbps.
        # Y axis: measured avg_bandwidth_mbps.
        pairs=$(awk -F, -v c="$codec" -v m="$mode" -v r="$res_name" '
            NR == 1 { next }
            $4 == c && $5 == m && $1 == r && $10 != "" {
                x = $9
                if (x == "") x = $10
                if (x != "") print x "," $10
            }
        ' "$VMAF_LOOKUP_CSV" | sort -t, -k1,1n)

        if [[ -z "$pairs" ]]; then
            continue
        fi

        local prev_x=""
        local prev_y=""
        local est=""
        while IFS=, read -r x y; do
            if awk -v t="$target_mbps" -v cur="$x" 'BEGIN{exit !(t==cur)}'; then
                est="$y"
                break
            fi
            if awk -v t="$target_mbps" -v cur="$x" 'BEGIN{exit !(t<cur)}'; then
                if [[ -n "$prev_x" ]]; then
                    est=$(awk -v t="$target_mbps" -v x0="$prev_x" -v y0="$prev_y" -v x1="$x" -v y1="$y" '
                        BEGIN {
                            if (x1 == x0) { printf "%.6f", y1; exit }
                            ratio = (t - x0) / (x1 - x0)
                            printf "%.6f", y0 + ratio * (y1 - y0)
                        }')
                else
                    est="$y"
                fi
                break
            fi
            prev_x="$x"
            prev_y="$y"
        done <<< "$pairs"

        if [[ -z "$est" ]] && [[ -n "$prev_y" ]]; then
            est="$prev_y"
        fi

        if [[ -n "$est" ]]; then
            echo "$est"
            return 0
        fi
    done

    return 0
}

prepare_resume_packaging_context() {
    print_header "Phase 0: Resume Packaging Setup"

    TEMP_DIR="$RESUME_PACKAGE_FROM"
    LOG_FILE="$TEMP_DIR/encoding.log"
    TEMP_BASE="$(dirname "$TEMP_DIR")"

    # Ensure log file exists so downstream log calls can append safely.
    touch "$LOG_FILE" 2>/dev/null || {
        echo "Error: Cannot write log file in resume directory: $LOG_FILE"
        exit 1
    }

    if [[ -n "$INPUT_FILE" ]]; then
        log_warn "--input is ignored in resume mode"
    fi
    INPUT_FILE="$RESUME_PACKAGE_FROM"
    INPUT_SIZE="N/A (resume mode)"
    INPUT_DURATION="N/A (resume mode)"

    # Derive defaults from the previous run path if caller did not provide output naming.
    local resume_parent_name
    resume_parent_name="$(basename "$(dirname "$RESUME_PACKAGE_FROM")")"
    local resume_base_name=""
    if [[ "$resume_parent_name" == *_tmp ]]; then
        resume_base_name="${resume_parent_name%_tmp}"
    else
        resume_base_name="$(basename "$RESUME_PACKAGE_FROM")"
    fi

    if [[ -z "$OUTPUT_BASE_NAME" ]]; then
        OUTPUT_BASE_NAME="${resume_base_name}_resume"
    fi
    if [[ -z "$OUTPUT_BASE_DIR" ]]; then
        OUTPUT_BASE_DIR="$(dirname "$(dirname "$RESUME_PACKAGE_FROM")")"
    fi

    local has_hevc_any=false
    local has_h264_any=false
    local has_av1_any=false

    for tier in "${ALL_RESOLUTION_TIERS[@]}"; do
        IFS=':' read -r name _ _ _ _ _ _ _ _ _ _ <<< "$tier"
        if [[ -s "$TEMP_DIR/hevc_${name}.mp4" ]]; then
            has_hevc_any=true
        fi
        if [[ -s "$TEMP_DIR/h264_${name}.mp4" ]]; then
            has_h264_any=true
        fi
        if [[ -s "$TEMP_DIR/av1_${name}.mp4" ]]; then
            has_av1_any=true
        fi
    done

    if [[ "$CODEC_SELECTION_EXPLICIT" == "false" ]]; then
        if [[ "$has_hevc_any" == "true" ]] && [[ "$has_h264_any" == "true" ]] && [[ "$has_av1_any" == "true" ]]; then
            CODEC_SELECTION="all"
        elif [[ "$has_hevc_any" == "true" ]] && [[ "$has_h264_any" == "true" ]]; then
            CODEC_SELECTION="both"
        elif [[ "$has_hevc_any" == "true" ]]; then
            CODEC_SELECTION="hevc"
        elif [[ "$has_h264_any" == "true" ]]; then
            CODEC_SELECTION="h264"
        elif [[ "$has_av1_any" == "true" ]]; then
            CODEC_SELECTION="av1"
        else
            log_error "No encoded variant MP4s found in resume directory: $TEMP_DIR"
            exit 1
        fi
        log "Auto-selected codec mode for resume: $CODEC_SELECTION"
    fi

    local need_hevc=false
    local need_h264=false
    local need_av1=false
    case "$CODEC_SELECTION" in
        both)
            need_hevc=true
            need_h264=true
            ;;
        hevc)
            need_hevc=true
            ;;
        h264)
            need_h264=true
            ;;
        av1)
            need_av1=true
            ;;
        all)
            need_hevc=true
            need_h264=true
            need_av1=true
            ;;
    esac

    if [[ "$need_hevc" == "true" ]] && [[ "$has_hevc_any" != "true" ]]; then
        log_error "Resume mode requested HEVC packaging, but no hevc_*.mp4 files were found in $TEMP_DIR"
        exit 1
    fi
    if [[ "$need_h264" == "true" ]] && [[ "$has_h264_any" != "true" ]]; then
        log_error "Resume mode requested H.264 packaging, but no h264_*.mp4 files were found in $TEMP_DIR"
        exit 1
    fi
    if [[ "$need_av1" == "true" ]] && [[ "$has_av1_any" != "true" ]]; then
        log_error "Resume mode requested AV1 packaging, but no av1_*.mp4 files were found in $TEMP_DIR"
        exit 1
    fi

    PROFILES=()
    for tier in "${ALL_RESOLUTION_TIERS[@]}"; do
        IFS=':' read -r name width height bitrate_h265 bitrate_h264 bitrate_av1 preset fontsize_tc fontsize_label x y_tc y_label <<< "$tier"
        local include_tier=true
        if [[ "$need_hevc" == "true" ]] && [[ ! -s "$TEMP_DIR/hevc_${name}.mp4" ]]; then
            include_tier=false
        fi
        if [[ "$need_h264" == "true" ]] && [[ ! -s "$TEMP_DIR/h264_${name}.mp4" ]]; then
            include_tier=false
        fi
        if [[ "$need_av1" == "true" ]] && [[ ! -s "$TEMP_DIR/av1_${name}.mp4" ]]; then
            include_tier=false
        fi
        if [[ "$include_tier" == "true" ]]; then
            PROFILES+=("$width:$height:$bitrate_h265:$bitrate_h264:$bitrate_av1:$preset:$fontsize_tc:$fontsize_label:$x:$y_tc:$y_label")
            log_success "Resume tier detected: $name"
        fi
    done

    if [[ ${#PROFILES[@]} -eq 0 ]]; then
        log_error "No common tiers found for selected codec mode '$CODEC_SELECTION' in $TEMP_DIR"
        exit 1
    fi

    if [[ -s "$TEMP_DIR/audio.mp4" ]]; then
        SKIP_AUDIO=false
        log_success "Audio mezzanine found for resume: audio.mp4"
    else
        SKIP_AUDIO=true
        log_warn "audio.mp4 not found in resume directory; packaging video-only output"
    fi

    # Minimal source metadata for summary output in resume mode.
    local probe_codec="h264"
    if [[ "$need_hevc" == "true" ]]; then
        probe_codec="hevc"
    elif [[ "$need_av1" == "true" ]]; then
        probe_codec="av1"
    fi
    local first_profile="${PROFILES[0]}"
    IFS=':' read -r first_w _ _ _ _ _ _ _ _ _ _ <<< "$first_profile"
    local first_res
    first_res="$(get_resolution_name "$first_w")"
    local probe_file="$TEMP_DIR/${probe_codec}_${first_res}.mp4"
    if [[ -f "$probe_file" ]]; then
        SOURCE_WIDTH=$(ffprobe -v error -select_streams v:0 -show_entries stream=width -of default=noprint_wrappers=1:nokey=1 "$probe_file" 2>/dev/null)
        SOURCE_HEIGHT=$(ffprobe -v error -select_streams v:0 -show_entries stream=height -of default=noprint_wrappers=1:nokey=1 "$probe_file" 2>/dev/null)
        SOURCE_FPS=$(ffprobe -v error -select_streams v:0 -show_entries stream=r_frame_rate -of default=noprint_wrappers=1:nokey=1 "$probe_file" 2>/dev/null)
        SOURCE_FPS_DECIMAL=$(echo "$SOURCE_FPS" | awk -F/ '{if ($2=="" || $2==0) printf "0.00"; else printf "%.2f", $1/$2}')
        if [[ -z "$SOURCE_WIDTH" ]]; then SOURCE_WIDTH="N/A"; fi
        if [[ -z "$SOURCE_HEIGHT" ]]; then SOURCE_HEIGHT="N/A"; fi
        if [[ -z "$SOURCE_FPS" ]]; then SOURCE_FPS="N/A"; fi
    else
        SOURCE_WIDTH="N/A"
        SOURCE_HEIGHT="N/A"
        SOURCE_FPS="N/A"
        SOURCE_FPS_DECIMAL="0.00"
    fi

    log "Resume source directory: $TEMP_DIR"
    log "Resume codec selection: $CODEC_SELECTION"
    log "Resume tiers selected: ${#PROFILES[@]}"
    echo ""
}

################################################################################
# Derive Output Directory Names
################################################################################

derive_output_directories() {
    local input=$1
    local base_name
    
    # Determine base name
    if [[ -n "$OUTPUT_BASE_NAME" ]]; then
        # User specified custom base name
        base_name="$OUTPUT_BASE_NAME"
    else
        # Auto-derive from input filename (strip extension)
        base_name=$(basename "$input" | sed -E 's/\.(mp4|mkv|mov|avi|webm|m3u8|ts)$//')
    fi
    
    # Determine output directory
    if [[ -z "$OUTPUT_BASE_DIR" ]]; then
        # Default to current working directory (where script was run from)
        OUTPUT_BASE_DIR="$PWD/$base_name"
    else
        # User specified directory - make it absolute if relative
        if [[ "$OUTPUT_BASE_DIR" != /* ]]; then
            # Resolve relative paths from current working directory
            OUTPUT_BASE_DIR="$PWD/$OUTPUT_BASE_DIR"
        fi
        # Append base name to the directory path
        OUTPUT_BASE_DIR="$OUTPUT_BASE_DIR/$base_name"
    fi
    
    # Handle existing directories with timestamp
    if [[ "$CODEC_SELECTION" == "both" ]] || [[ "$CODEC_SELECTION" == "all" ]] || [[ "$CODEC_SELECTION" == "hevc" ]]; then
        OUTPUT_DIR_HEVC="${OUTPUT_BASE_DIR}_hevc"
        if [[ -d "$OUTPUT_DIR_HEVC" ]]; then
            TIMESTAMP=$(date +%Y%m%d_%H%M%S)
            OUTPUT_DIR_HEVC="${OUTPUT_BASE_DIR}_hevc_${TIMESTAMP}"
            log_warn "Output directory exists, using: $(basename $OUTPUT_DIR_HEVC)"
        fi
        mkdir -p "$OUTPUT_DIR_HEVC"
        
        # Create TS package directory if TS format is requested
        if [[ "$HLS_FORMAT" == "ts" ]] || [[ "$HLS_FORMAT" == "both" ]]; then
            OUTPUT_DIR_HEVC_TS="${OUTPUT_BASE_DIR}_hevc_ts"
            if [[ -d "$OUTPUT_DIR_HEVC_TS" ]]; then
                OUTPUT_DIR_HEVC_TS="${OUTPUT_BASE_DIR}_hevc_ts_${TIMESTAMP}"
                log_warn "Output directory exists, using: $(basename $OUTPUT_DIR_HEVC_TS)"
            fi
            mkdir -p "$OUTPUT_DIR_HEVC_TS"
        fi
    fi
    
    if [[ "$CODEC_SELECTION" == "both" ]] || [[ "$CODEC_SELECTION" == "all" ]] || [[ "$CODEC_SELECTION" == "h264" ]]; then
        OUTPUT_DIR_H264="${OUTPUT_BASE_DIR}_h264"
        if [[ -d "$OUTPUT_DIR_H264" ]]; then
            TIMESTAMP=$(date +%Y%m%d_%H%M%S)
            OUTPUT_DIR_H264="${OUTPUT_BASE_DIR}_h264_${TIMESTAMP}"
            log_warn "Output directory exists, using: $(basename $OUTPUT_DIR_H264)"
        fi
        mkdir -p "$OUTPUT_DIR_H264"
        
        # Create TS package directory if TS format is requested
        if [[ "$HLS_FORMAT" == "ts" ]] || [[ "$HLS_FORMAT" == "both" ]]; then
            OUTPUT_DIR_H264_TS="${OUTPUT_BASE_DIR}_h264_ts"
            if [[ -d "$OUTPUT_DIR_H264_TS" ]]; then
                OUTPUT_DIR_H264_TS="${OUTPUT_BASE_DIR}_h264_ts_${TIMESTAMP}"
                log_warn "Output directory exists, using: $(basename $OUTPUT_DIR_H264_TS)"
            fi
            mkdir -p "$OUTPUT_DIR_H264_TS"
        fi
    fi

    if [[ "$CODEC_SELECTION" == "all" ]] || [[ "$CODEC_SELECTION" == "av1" ]]; then
        OUTPUT_DIR_AV1="${OUTPUT_BASE_DIR}_av1"
        if [[ -d "$OUTPUT_DIR_AV1" ]]; then
            TIMESTAMP=$(date +%Y%m%d_%H%M%S)
            OUTPUT_DIR_AV1="${OUTPUT_BASE_DIR}_av1_${TIMESTAMP}"
            log_warn "Output directory exists, using: $(basename $OUTPUT_DIR_AV1)"
        fi
        mkdir -p "$OUTPUT_DIR_AV1"
    fi
}

################################################################################
# Helper: Detect and Select Best HLS Variant
################################################################################

select_best_hls_variant() {
    local m3u8_file=$1
    
    # Check if this is a master playlist (contains #EXT-X-STREAM-INF)
    if grep -q "#EXT-X-STREAM-INF" "$m3u8_file"; then
        log "Multi-variant HLS master playlist detected" >&2
        
        # Check if playlist has separate audio renditions
        if grep -q "#EXT-X-MEDIA:TYPE=AUDIO" "$m3u8_file"; then
            log "Detected separate audio renditions in master playlist" >&2
            
            # Parse to find highest bandwidth variant
            local best_bandwidth=0
            local current_bandwidth=0
            local variant_index=0
            local best_variant_index=0
            
            while IFS= read -r line; do
                # Trim whitespace and carriage returns
                line=$(echo "$line" | tr -d '\r' | xargs)
                
                if [[ "$line" =~ ^#EXT-X-STREAM-INF ]]; then
                    # Extract BANDWIDTH value
                    if [[ "$line" =~ BANDWIDTH=([0-9]+) ]]; then
                        current_bandwidth="${BASH_REMATCH[1]}"
                    fi
                elif [[ "$line" != \#* ]] && [[ -n "$line" ]]; then
                    # This is the variant URI
                    if [[ $current_bandwidth -gt $best_bandwidth ]]; then
                        best_bandwidth=$current_bandwidth
                        best_variant_index=$variant_index
                    fi
                    variant_index=$((variant_index + 1))
                    current_bandwidth=0
                fi
            done < "$m3u8_file"
            
            local bandwidth_mbps=$(echo "$best_bandwidth" | awk '{printf "%.2f", $1/1000000}')
            log_success "Using master playlist to preserve audio streams" >&2
            log_success "Highest bitrate variant: ${bandwidth_mbps} Mbps (video stream index: $best_variant_index)" >&2
            
            # Set global flags for ffmpeg command building
            USE_MASTER_PLAYLIST=true
            HIGHEST_VIDEO_STREAM_INDEX=$best_variant_index
            
            # Return master playlist
            echo "$m3u8_file"
            return 0
        fi
        
        log "Parsing variant streams..." >&2
        
        # Extract bandwidth and URI pairs
        local best_bandwidth=0
        local best_variant=""
        local current_bandwidth=0
        
        while IFS= read -r line; do
            # Trim whitespace and carriage returns
            line=$(echo "$line" | tr -d '\r' | xargs)
            
            if [[ "$line" =~ ^#EXT-X-STREAM-INF ]]; then
                # Extract BANDWIDTH value
                if [[ "$line" =~ BANDWIDTH=([0-9]+) ]]; then
                    current_bandwidth="${BASH_REMATCH[1]}"
                fi
            elif [[ "$line" != \#* ]] && [[ -n "$line" ]]; then
                # This is the variant URI
                if [[ $current_bandwidth -gt $best_bandwidth ]]; then
                    best_bandwidth=$current_bandwidth
                    best_variant="$line"
                fi
                current_bandwidth=0
            fi
        done < "$m3u8_file"
        
        if [[ -z "$best_variant" ]]; then
            log_error "Could not parse variants from master playlist" >&2
            exit 1
        fi
        
        # Convert relative path to absolute
        local m3u8_dir="$(dirname "$m3u8_file")"
        if [[ "$best_variant" != /* ]]; then
            best_variant="$m3u8_dir/$best_variant"
        fi
        
        # Verify the variant file exists
        if [[ ! -f "$best_variant" ]]; then
            log_error "Best variant playlist not found: $best_variant" >&2
            exit 1
        fi
        
        local bandwidth_mbps=$(echo "$best_bandwidth" | awk '{printf "%.2f", $1/1000000}')
        log_success "Selected highest bitrate variant: ${bandwidth_mbps} Mbps" >&2
        log "Variant path: $best_variant" >&2
        
        # Return the path to the best variant
        echo "$best_variant"
    else
        # Not a master playlist, return original file
        echo "$m3u8_file"
    fi
}

################################################################################
# Phase 1: Input Validation
################################################################################

validate_input() {
    local input=$1
    
    print_header "Phase 1: Input Validation"
    
    log "Analyzing input file: $(basename $input)"
    
    # Check if file is readable
    if [[ ! -r "$input" ]]; then
        log_error "Cannot read input file: $input"
        exit 1
    fi
    
    # If M3U8, check for multi-variant playlist and select best variant
    if [[ "$input" == *.m3u8 ]]; then
        # Redirect stdout to variable while allowing function to set global variables
        # Use a temp file to capture the output while preserving global variable changes
        local temp_variant_file="/tmp/variant_$$"
        select_best_hls_variant "$input" > "$temp_variant_file"
        local selected_variant=$(cat "$temp_variant_file")
        rm -f "$temp_variant_file"
        
        if [[ "$selected_variant" != "$input" ]]; then
            log "Using variant: $(basename $selected_variant)"
            INPUT_FILE="$selected_variant"
            input="$selected_variant"
        fi
    fi
    
    # Probe with ffprobe
    if ! ffprobe -v error "$input" > /dev/null 2>&1; then
        log_error "Invalid or corrupted video file: $input"
        exit 1
    fi
    
    # Check for video stream
    HAS_VIDEO=$(ffprobe -v error -select_streams v:0 -show_entries stream=codec_type \
                -of default=noprint_wrappers=1:nokey=1 "$input" 2>/dev/null | head -1)
    
    if [[ "$HAS_VIDEO" != "video" ]]; then
        log_error "No video stream found in input file"
        exit 1
    fi
    log_success "Video stream detected"
    
    # Check for audio stream
    HAS_AUDIO=$(ffprobe -v error -select_streams a:0 -show_entries stream=codec_type \
                -of default=noprint_wrappers=1:nokey=1 "$input" 2>/dev/null | head -1)
    
    if [[ "$HAS_AUDIO" != "audio" ]]; then
        log_warn "No audio stream found - will create video-only output"
        SKIP_AUDIO=true
        ENCODING_WARNINGS+=("No audio stream found in source - creating video-only output")
    else
        log_success "Audio stream detected"
        SKIP_AUDIO=false
    fi

    # Default to first audio stream; refine below if multiple are available.
    INPUT_AUDIO_STREAM_INDEX=0
    MULTIPLE_AUDIO_STREAMS=false
    if [[ "$SKIP_AUDIO" != "true" ]]; then
        # Probe all audio streams: index,channels[,language]
        local audio_streams_info
        audio_streams_info=$(ffprobe -v error -select_streams a \
            -show_entries stream=index,channels:stream_tags=language \
            -of csv=p=0 "$input" 2>/dev/null)
        local audio_count
        audio_count=$(echo "$audio_streams_info" | /usr/bin/grep -c '^[0-9]')
        if [[ "$audio_count" -gt 1 ]]; then
            MULTIPLE_AUDIO_STREAMS=true
            log "Source has $audio_count audio streams — preferring 2-channel (stereo)"
            # Find the first audio stream with channels=2 (logical index in :a sequence)
            local logical_idx=0
            local stereo_logical_idx=""
            while IFS=, read -r abs_idx channels lang; do
                # ffprobe csv yields trailing fields; accept any line starting with a number
                [[ ! "$abs_idx" =~ ^[0-9]+$ ]] && continue
                if [[ "$channels" = "2" && -z "$stereo_logical_idx" ]]; then
                    stereo_logical_idx=$logical_idx
                fi
                log "  a:$logical_idx (abs $abs_idx) channels=$channels lang=${lang:-und}"
                logical_idx=$((logical_idx + 1))
            done <<< "$audio_streams_info"
            if [[ -n "$stereo_logical_idx" ]]; then
                INPUT_AUDIO_STREAM_INDEX=$stereo_logical_idx
                log_success "Selected audio a:$INPUT_AUDIO_STREAM_INDEX (stereo)"
            else
                log "No 2-channel audio found; falling back to a:0"
            fi
        fi
    fi
    
    # Get input details
    # If using master playlist with separate audio, probe the specific video stream
    local stream_selector="v:0"
    if [[ "$USE_MASTER_PLAYLIST" == "true" ]] && [[ -n "$HIGHEST_VIDEO_STREAM_INDEX" ]]; then
        stream_selector="v:${HIGHEST_VIDEO_STREAM_INDEX}"
        log "Probing highest quality video stream (v:${HIGHEST_VIDEO_STREAM_INDEX})"
    fi
    
    INPUT_WIDTH=$(ffprobe -v error -select_streams "$stream_selector" -show_entries stream=width \
                  -of default=noprint_wrappers=1:nokey=1 "$input" 2>/dev/null | head -1)
    INPUT_HEIGHT=$(ffprobe -v error -select_streams "$stream_selector" -show_entries stream=height \
                   -of default=noprint_wrappers=1:nokey=1 "$input" 2>/dev/null | head -1)
    INPUT_FPS=$(ffprobe -v error -select_streams "$stream_selector" -show_entries stream=r_frame_rate \
                -of default=noprint_wrappers=1:nokey=1 "$input" 2>/dev/null | head -1)
    INPUT_FPS_DECIMAL=$(echo "$INPUT_FPS" | awk -F/ '{printf "%.2f", $1/$2}')
    INPUT_DURATION=$(ffprobe -v error -show_entries format=duration \
                     -of default=noprint_wrappers=1:nokey=1 "$input" 2>/dev/null | cut -d. -f1)
    INPUT_SIZE=$(du -h "$input" | cut -f1)
    
    log "Resolution: ${INPUT_WIDTH}×${INPUT_HEIGHT}"
    log "Frame rate: ${INPUT_FPS_DECIMAL} fps"
    log "Duration: ${INPUT_DURATION}s"
    log "File size: ${INPUT_SIZE}"
    
    # Validate minimum resolution
    if [[ $INPUT_WIDTH -lt 640 ]] || [[ $INPUT_HEIGHT -lt 360 ]]; then
        log_error "Input resolution too low (minimum 640×360)"
        exit 1
    fi
    
    echo ""
}

################################################################################
# Phase 1b: Prerequisites Check
################################################################################

check_prerequisites() {
    print_header "Phase 1b: Tool Checks"
    
    log "Checking for required tools..."
    
    # Check ffmpeg
    if ! command -v ffmpeg &> /dev/null; then
        log_error "ffmpeg not found. Please install ffmpeg."
        exit 1
    fi
    log_success "ffmpeg: $(ffmpeg -version | head -1)"
    
    # Check for encoders and show which will be used.
    # Only resolve HEVC/H.264 encoder mode when those codecs are requested.
    local encoder_type="software"
    if [[ "$CODEC_SELECTION" == "both" ]] || [[ "$CODEC_SELECTION" == "all" ]] || [[ "$CODEC_SELECTION" == "hevc" ]] || [[ "$CODEC_SELECTION" == "h264" ]]; then
        encoder_type=$(select_encoder "hevc")
    fi
    
    if [[ "$CODEC_SELECTION" == "both" ]] || [[ "$CODEC_SELECTION" == "all" ]] || [[ "$CODEC_SELECTION" == "hevc" ]]; then
        if [[ "$encoder_type" == "hardware" ]]; then
            log_success "HEVC encoder: VideoToolbox (hardware) - 13x faster"
        else
            # Capture encoder list to avoid pipefail issues
            local encoders=$(ffmpeg -encoders 2>&1)
            if ! echo "$encoders" | grep -q libx265; then
                log_error "libx265 encoder not found"
                exit 1
            fi
            log_success "HEVC encoder: libx265 (software)"
        fi
    fi
    
    if [[ "$CODEC_SELECTION" == "both" ]] || [[ "$CODEC_SELECTION" == "all" ]] || [[ "$CODEC_SELECTION" == "h264" ]]; then
        if [[ "$encoder_type" == "hardware" ]]; then
            log_success "H.264 encoder: VideoToolbox (hardware) - 5x faster"
        else
            # Capture encoder list to avoid pipefail issues
            local encoders=$(ffmpeg -encoders 2>&1)
            if ! echo "$encoders" | grep -q libx264; then
                log_error "libx264 encoder not found"
                exit 1
            fi
            log_success "H.264 encoder: libx264 (software)"
        fi
    fi

    if [[ "$CODEC_SELECTION" == "all" ]] || [[ "$CODEC_SELECTION" == "av1" ]]; then
        local encoders=$(ffmpeg -encoders 2>&1)
        if ! echo "$encoders" | grep -q libsvtav1; then
            log_error "libsvtav1 encoder not found"
            exit 1
        fi
        log_success "AV1 encoder: libsvtav1 (software)"
    fi
    
    # Check Shaka Packager
    if [[ -x "/usr/local/bin/packager" ]]; then
        log_success "Shaka Packager: $(/usr/local/bin/packager --version 2>&1 | head -1)"
    elif command -v packager &> /dev/null; then
        log_success "Shaka Packager: $(packager --version 2>&1 | head -1)"
    else
        log_warn "Shaka Packager not found"
        log_warn "DASH packaging will be skipped"
    fi
    
    # Check font
    if [[ ! -f "$FONT" ]]; then
        log_error "Font not found: $FONT"
        exit 1
    fi
    log_success "Font available"

    # Check fMP4 fragment parser helper (used in Phase 6).
    if detect_fragment_parser_script; then
        log_success "Fragment parser: $FRAGMENT_PARSER_SCRIPT"
    else
        log_warn "Fragment parser script not found (parse_fmp4_fragments.py)"
        log_warn "Phase 6 (.byteranges generation) will be skipped"
    fi
    
    echo ""
}

################################################################################
# Phase 2: Create Mezzanine (Universal)
################################################################################

create_mezzanine() {
    print_header "Phase 2: Creating Mezzanine File"
    
    # Use .mp4 container for mezzanine to ensure AV1 compatibility
    MEZZANINE="$TEMP_DIR/mezzanine.mp4"
    
    log "Converting input to mezzanine format..."
    log "Input: $(basename $INPUT_FILE)"
    
    if [[ -n "$TIME_LIMIT" ]]; then
        log "Duration limit: ${TIME_LIMIT}s"
    fi
    
    START_TIME=$(date +%s)
    
    # Build ffmpeg command with optional time limit and stream mapping
    local ffmpeg_cmd="ffmpeg -i \"$INPUT_FILE\""
    
    # If using master playlist with separate audio, map specific streams
    if [[ "$USE_MASTER_PLAYLIST" == "true" ]] && [[ -n "$HIGHEST_VIDEO_STREAM_INDEX" ]]; then
        log "Mapping audio stream a:${INPUT_AUDIO_STREAM_INDEX:-0} and highest quality video stream (v:${HIGHEST_VIDEO_STREAM_INDEX})"
        ffmpeg_cmd="$ffmpeg_cmd -map 0:a:${INPUT_AUDIO_STREAM_INDEX:-0} -map 0:v:${HIGHEST_VIDEO_STREAM_INDEX}"
    elif [[ "$MULTIPLE_AUDIO_STREAMS" == "true" ]]; then
        # Source has multiple audio streams; explicitly pick the chosen one
        # (default ffmpeg stream selection prefers highest channel count).
        log "Mapping audio stream a:${INPUT_AUDIO_STREAM_INDEX} (stereo) from multi-track source"
        ffmpeg_cmd="$ffmpeg_cmd -map 0:v -map 0:a:${INPUT_AUDIO_STREAM_INDEX}"
    fi
    
    if [[ -n "$TIME_LIMIT" ]]; then
        ffmpeg_cmd="$ffmpeg_cmd -t $TIME_LIMIT"
    fi
    
    # Always use stream copy - .mp4 container supports AV1
    ffmpeg_cmd="$ffmpeg_cmd -c copy \"$MEZZANINE\" -loglevel error -stats"
    
    # Universal input handling - ffmpeg detects format automatically
    # Supports: MP4, MKV, MOV, M3U8, TS, AVI, WebM, etc.
    eval "$ffmpeg_cmd" 2>&1 | tee -a "$LOG_FILE"
    
    END_TIME=$(date +%s)
    DURATION=$((END_TIME - START_TIME))
    
    if [[ ! -f "$MEZZANINE" ]]; then
        log_error "Mezzanine creation failed"
        exit 1
    fi
    
    MEZ_SIZE=$(du -h "$MEZZANINE" | cut -f1)
    MEZ_DURATION=$(ffprobe -v error -show_entries format=duration \
                   -of default=noprint_wrappers=1:nokey=1 "$MEZZANINE" 2>/dev/null | cut -d. -f1)
    
    log_success "Mezzanine created in ${DURATION}s: $MEZ_SIZE, ${MEZ_DURATION}s duration"
    
    # Apply padding to align with segment boundaries (if enabled)
    check_and_apply_padding
    if [ $? -ne 0 ]; then
        log_error "Padding failed"
        exit 1
    fi
    
    # Detect source characteristics from mezzanine
    detect_source_characteristics
    
    echo ""
}

# ============================================================================
# PADDING TO SEGMENT BOUNDARIES (PHASE 1)
# ============================================================================

check_and_apply_padding() {
    if [ "$PAD_TO_SEGMENT_BOUNDARY" != "true" ]; then
        log "Padding disabled via --no-padding flag"
        VIDEO_PADDING_DURATION=0
        AUDIO_PADDING_DURATION=0
        return 0
    fi
    
    log "Checking if padding is needed (multi-duration LCM: ${MULTI_DURATION_LCM}s for 2s/4s/6s support)..."
    
    # Get precise durations for both video and audio streams from mezzanine
    local video_duration=$(ffprobe -v error -select_streams v:0 \
        -show_entries stream=duration \
        -of default=noprint_wrappers=1:nokey=1 "$MEZZANINE" 2>/dev/null)
    
    local audio_duration=$(ffprobe -v error -select_streams a:0 \
        -show_entries stream=duration \
        -of default=noprint_wrappers=1:nokey=1 "$MEZZANINE" 2>/dev/null)
    
    if [ -z "$video_duration" ]; then
        log_error "Could not detect video duration for padding calculation"
        return 1
    fi
    
    # If no audio stream, use video duration for audio as well
    if [ -z "$audio_duration" ]; then
        log_warning "No audio stream detected, using video duration"
        audio_duration="$video_duration"
    fi
    
    log "Video duration: ${video_duration}s, Audio duration: ${audio_duration}s"
    
    # Get the maximum duration (we'll pad both to align with this)
    local max_duration=$(awk -v v="$video_duration" -v a="$audio_duration" \
        'BEGIN {print (v > a) ? v : a}')
    
    log "Maximum stream duration: ${max_duration}s"
    
    # Store content duration (before padding) for use in burn-in filters
    # This allows us to show "PADDING" label only during padded frames
    CONTENT_DURATION="$max_duration"
    
    # Calculate remainder when dividing by multi-duration LCM (12s)
    local remainder=$(awk -v dur="$max_duration" -v lcm="$MULTI_DURATION_LCM" \
        'BEGIN {printf "%.3f", dur - int(dur/lcm)*lcm}')
    
    log "Remainder when divided by ${MULTI_DURATION_LCM}s: ${remainder}s"
    
    # Check if remainder is below threshold (effectively already aligned)
    local below_threshold=$(awk -v rem="$remainder" -v thresh="$PADDING_THRESHOLD" \
        'BEGIN {print (rem < thresh) ? 1 : 0}')
    
    if [ "$below_threshold" -eq 1 ]; then
        log_success "Duration already aligned to ${MULTI_DURATION_LCM}s boundaries (remainder < ${PADDING_THRESHOLD}s)"
        ENCODING_INFOS+=("Duration already aligned, no padding needed (remainder: ${remainder}s)")
        VIDEO_PADDING_DURATION=0
        AUDIO_PADDING_DURATION=0
        return 0
    fi
    
    # Calculate target duration (next 12s boundary)
    # Manual ceiling: if there's any remainder, round up
    local target_duration=$(awk -v dur="$max_duration" -v lcm="$MULTI_DURATION_LCM" \
        'BEGIN {
            quotient = int(dur/lcm);
            if (dur > quotient * lcm) quotient++;
            printf "%.3f", quotient * lcm;
        }')
    
    log "Target aligned duration: ${target_duration}s (next ${MULTI_DURATION_LCM}s boundary)"
    
    # Calculate individual padding for video and audio
    VIDEO_PADDING_DURATION=$(awk -v target="$target_duration" -v current="$video_duration" \
        'BEGIN {printf "%.3f", target - current}')
    
    AUDIO_PADDING_DURATION=$(awk -v target="$target_duration" -v current="$audio_duration" \
        'BEGIN {printf "%.3f", target - current}')
    
    log "Video padding: ${VIDEO_PADDING_DURATION}s (${video_duration}s → ${target_duration}s)"
    log "Audio padding: ${AUDIO_PADDING_DURATION}s (${audio_duration}s → ${target_duration}s)"
    log "✓ Padding will be applied during variant encoding (no separate padded mezzanine)"
    
    # Check if padding ratio exceeds warning threshold
    local padding_ratio=$(awk -v vpad="$VIDEO_PADDING_DURATION" -v apad="$AUDIO_PADDING_DURATION" -v dur="$max_duration" \
        'BEGIN {max_pad = (vpad > apad) ? vpad : apad; printf "%.1f", (max_pad/dur)*100}')
    
    local exceeds_warning=$(awk -v ratio="$padding_ratio" -v warn="$PADDING_WARNING_RATIO" \
        'BEGIN {print (ratio > warn) ? 1 : 0}')
    
    if [ "$exceeds_warning" -eq 1 ]; then
        log_warning "Padding ratio is ${padding_ratio}% (exceeds ${PADDING_WARNING_RATIO}% threshold)"
        ENCODING_WARNINGS+=("High padding ratio: ${padding_ratio}% of total duration")
    fi
    
    ENCODING_INFOS+=("Will apply video padding ${VIDEO_PADDING_DURATION}s and audio padding ${AUDIO_PADDING_DURATION}s to align with ${MULTI_DURATION_LCM}s boundary")
    
    return 0
}

apply_padding() {
    local padding_duration=$1
    local original_duration=$2
    
    log "Applying ${padding_duration}s padding to mezzanine..."
    
    # Detect if source has audio
    local audio_streams=$(ffprobe -v error -select_streams a \
        -show_entries stream=index \
        -of csv=p=0 "$MEZZANINE" 2>/dev/null | wc -l)
    
    local has_audio=false
    if [ "$audio_streams" -gt 0 ]; then
        has_audio=true
    fi
    
    # Get sample rate if audio exists
    local sample_rate=48000  # default
    if [ "$has_audio" = true ]; then
        sample_rate=$(ffprobe -v error -select_streams a:0 \
            -show_entries stream=sample_rate \
            -of default=noprint_wrappers=1:nokey=1 "$MEZZANINE" 2>/dev/null)
        
        if [ -z "$sample_rate" ]; then
            sample_rate=48000  # fallback
        fi
    fi
    
    # Calculate padding samples for audio
    local padding_samples=$(awk -v dur="$padding_duration" -v rate="$sample_rate" \
        'BEGIN {printf "%d", dur * rate}')
    
    # Create padded version
    local padded_file="${MEZZANINE}.padded.mov"
    
    log "Padding: ${padding_duration}s video + audio (${padding_samples} samples @ ${sample_rate}Hz)"
    
    # Build FFmpeg filter
    local video_filter="tpad=stop_mode=clone:stop_duration=${padding_duration}"
    local audio_filter=""
    
    if [ "$has_audio" = true ]; then
        audio_filter="apad=pad_dur=${padding_duration}"
    fi
    
    # Run FFmpeg with appropriate filters
    # Use H.264 with high quality preset to keep file size reasonable
    # ~50Mbps for 4K should be plenty for a mezzanine (vs 400Mbps ProRes)
    if [ "$has_audio" = true ]; then
        ffmpeg -hide_banner -loglevel warning -stats \
            -i "$MEZZANINE" \
            -filter_complex "[0:v]${video_filter}[v];[0:a]${audio_filter}[a]" \
            -map "[v]" -map "[a]" \
            -c:v libx264 -preset slow -crf 18 -g 48 -keyint_min 48 -sc_threshold 0 \
            -c:a aac -b:a 192k -ac 2 \
            -movflags +faststart \
            "$padded_file"
    else
        # Video only
        ffmpeg -hide_banner -loglevel warning -stats \
            -i "$MEZZANINE" \
            -vf "${video_filter}" \
            -c:v libx264 -preset slow -crf 18 -g 48 -keyint_min 48 -sc_threshold 0 \
            -movflags +faststart \
            "$padded_file"
    fi
    
    local ffmpeg_result=$?
    
    if [ $ffmpeg_result -ne 0 ]; then
        log_error "Failed to apply padding"
        rm -f "$padded_file"
        return 1
    fi
    
    # Verify padded duration
    local padded_duration=$(ffprobe -v error -select_streams v:0 \
        -show_entries format=duration \
        -of default=noprint_wrappers=1:nokey=1 "$padded_file" 2>/dev/null)
    
    log "Original duration: ${original_duration}s → Padded duration: ${padded_duration}s"
    
    # Replace original with padded version
    mv "$padded_file" "$MEZZANINE"
    
    # Update MEZ_DURATION global variable
    MEZ_DURATION="$padded_duration"
    
    log_success "Padding applied successfully"
    return 0
}

detect_source_characteristics() {
    log "Detecting source characteristics..."
    log "Configured segment duration: ${SEGMENT_DURATION}s"
    log "Configured partial duration: ${PARTIAL_DURATION}s"
    log "Configured GOP duration: ${GOP_DURATION}s"
    
    # Resolution
    SOURCE_WIDTH=$(ffprobe -v error -select_streams v:0 \
        -show_entries stream=width \
        -of default=noprint_wrappers=1:nokey=1 "$MEZZANINE" 2>/dev/null)
    
    SOURCE_HEIGHT=$(ffprobe -v error -select_streams v:0 \
        -show_entries stream=height \
        -of default=noprint_wrappers=1:nokey=1 "$MEZZANINE" 2>/dev/null)
    
    # Frame rate (as fraction)
    SOURCE_FPS=$(ffprobe -v error -select_streams v:0 \
        -show_entries stream=r_frame_rate \
        -of default=noprint_wrappers=1:nokey=1 "$MEZZANINE" 2>/dev/null)
    
    # Calculate decimal FPS for display
    SOURCE_FPS_DECIMAL=$(echo "$SOURCE_FPS" | awk -F/ '{printf "%.2f", $1/$2}')
    
    # Calculate GOP (keyframe interval = fps * partial duration)
    KEYINT=$(awk -v fps="$SOURCE_FPS" -v gop="$GOP_DURATION" 'BEGIN {split(fps,a,"/"); if (a[2]=="" ) a[2]=1; printf "%.0f", (a[1]/a[2])*gop}')
    if [[ "$KEYINT" -lt 1 ]]; then
        KEYINT=1
    fi
    
    # Aspect ratio check
    SOURCE_ASPECT=$(echo "$SOURCE_WIDTH $SOURCE_HEIGHT" | awk '{printf "%.3f", $1/$2}')
    
    if [[ "${SOURCE_ASPECT:0:4}" != "1.77" ]]; then
        log_warn "Non-16:9 aspect ratio detected: ${SOURCE_WIDTH}×${SOURCE_HEIGHT} (${SOURCE_ASPECT})"
        log_warn "Output will be scaled to 16:9 (may introduce letterboxing/pillarboxing)"
    fi
    
    log_success "Source: ${SOURCE_WIDTH}×${SOURCE_HEIGHT} @ ${SOURCE_FPS_DECIMAL} fps (${SOURCE_FPS})"
    log_success "GOP structure: ${KEYINT} frames per GOP (${GOP_DURATION}s closed GOPs)"
}

select_resolution_tiers() {
    print_header "Phase 2b: Selecting Resolution Tiers"
    
    log "Source resolution: ${SOURCE_WIDTH}×${SOURCE_HEIGHT}"
    
    # Show max-res limit if set
    if [[ -n "$MAX_RESOLUTION_HEIGHT" ]]; then
        log "Maximum resolution limit: ${MAX_RESOLUTION_HEIGHT}"
    fi
    
    log "Selecting appropriate encoding tiers..."
    echo ""
    
    local selected_count=0
    local skipped_by_maxres=0
    local max_res_height=99999  # Default: no limit
    
    # Convert max-res to numeric height if specified
    if [[ -n "$MAX_RESOLUTION_HEIGHT" ]]; then
        max_res_height=$(get_resolution_height "$MAX_RESOLUTION_HEIGHT")
    fi
    
    for tier in "${ALL_RESOLUTION_TIERS[@]}"; do
        IFS=':' read -r name width height bitrate_h265 bitrate_h264 bitrate_av1 preset fontsize_tc fontsize_label x y_tc y_label <<< "$tier"
        
        # Check source resolution limit (existing logic)
        if [[ $width -gt $SOURCE_WIDTH ]] || [[ $height -gt $SOURCE_HEIGHT ]]; then
            log "  ✗ $name (${width}×${height}) - exceeds source resolution"
            continue
        fi
        
        # Check max-res limit (NEW)
        if [[ $height -gt $max_res_height ]]; then
            log "  ✗ $name (${width}×${height}) - exceeds max-resolution limit"
            ((skipped_by_maxres++)) || true
            continue
        fi
        
        # Tier is within both limits - include it
        PROFILES+=("$width:$height:$bitrate_h265:$bitrate_h264:$bitrate_av1:$preset:$fontsize_tc:$fontsize_label:$x:$y_tc:$y_label")
        log_success "✓ $name (${width}×${height})"
        ((selected_count++)) || true
    done
    
    echo ""
    log_success "Selected $selected_count resolution tiers for encoding"
    
    # Add info message to report if max-res limited encoding
    if [[ -n "$MAX_RESOLUTION_HEIGHT" ]] && [[ $skipped_by_maxres -gt 0 ]]; then
        ENCODING_INFOS+=("Maximum resolution limited to ${MAX_RESOLUTION_HEIGHT} (${skipped_by_maxres} higher tiers skipped)")
    fi
    
    echo ""
}

################################################################################
# Phase 3: Encode Variants
################################################################################

encode_variant() {
    local codec=$1
    local width=$2
    local height=$3
    local bitrate_kbps=$4
    local preset=$5
    local fontsize_tc=$6
    local fontsize_label=$7
    local x_offset=$8
    local y_tc=$9
    local y_label=${10}
    
    local res_name=$(get_resolution_name $width)
    local output_file="$TEMP_DIR/${codec}_${res_name}.mp4"
    
    # Build labels
    local codec_upper=$(echo $codec | tr '[:lower:]' '[:upper:]')
    
    # Convert bitrate to Mbps for display (2 decimal places)
    local bitrate_mbps=$(echo "$bitrate_kbps" | awk '{printf "%.2f", $1/1000}')
    
    # Create separate labels
    local bitrate_label="${bitrate_mbps}Mbps"
    local codec_res_fps_label="${codec_upper} ${res_name} | ${SOURCE_FPS_DECIMAL}fps"
    
    # Determine encoder type
    local encoder_type=$(select_encoder "$codec")
    local encoder_name="software"
    local encoder_label=""
    if [[ "$encoder_type" == "hardware" ]]; then
        encoder_name="VideoToolbox (hardware)"
        encoder_label="HW"
    else
        if [[ "$codec" == "av1" ]]; then
            encoder_name="libsvtav1 (software)"
        else
            encoder_name="libx265/libx264 (software)"
        fi
        encoder_label="SW"
    fi

    # Optional estimated VMAF label from characterization CSV lookup.
    local vmaf_label=""
    local vmaf_estimate=""
    if [[ -n "$VMAF_LOOKUP_CSV" ]]; then
        vmaf_estimate=$(estimate_vmaf_from_lookup "$codec" "$encoder_type" "$res_name" "$bitrate_kbps")
        if [[ -n "$vmaf_estimate" ]]; then
            vmaf_label="VMAF~${vmaf_estimate}"
        fi
    fi
    # Average label defaults to target bitrate; hardware can optionally use CSV estimate.
    local avg_bandwidth_label="AVG~${bitrate_mbps}Mbps"
    local avg_bandwidth_estimate=""
    local avg_for_peak_mbps="$bitrate_mbps"
    if [[ -n "$VMAF_LOOKUP_CSV" ]] && [[ "$encoder_type" == "hardware" ]]; then
        avg_bandwidth_estimate=$(estimate_average_bandwidth_from_lookup "$codec" "$encoder_type" "$res_name" "$bitrate_kbps")
        if [[ -n "$avg_bandwidth_estimate" ]]; then
            local avg_bandwidth_mbps
            avg_bandwidth_mbps=$(awk -v m="$avg_bandwidth_estimate" 'BEGIN{printf "%.2f", m+0}')
            avg_bandwidth_label="AVG~${avg_bandwidth_mbps}Mbps"
            avg_for_peak_mbps="$avg_bandwidth_mbps"
        fi
    fi
    local peak_bandwidth_label=""
    local peak_cap_mbps
    peak_cap_mbps=$(awk -v kbps="$bitrate_kbps" -v pct="$MAXRATE_PERCENT" -v avg="$avg_for_peak_mbps" 'BEGIN{p=(kbps * pct / 100.0) / 1000.0; if (p < avg) p=avg; printf "%.2f", p}')
    peak_bandwidth_label="PEAK<=${peak_cap_mbps}Mbps"
    local avg_peak_bandwidth_label=""
    if [[ -n "$avg_bandwidth_label" ]] && [[ -n "$peak_bandwidth_label" ]]; then
        avg_peak_bandwidth_label="${avg_bandwidth_label} / ${peak_bandwidth_label}"
    elif [[ -n "$avg_bandwidth_label" ]]; then
        avg_peak_bandwidth_label="$avg_bandwidth_label"
    elif [[ -n "$peak_bandwidth_label" ]]; then
        avg_peak_bandwidth_label="$peak_bandwidth_label"
    fi
    local rate_burnin_label="$bitrate_label"
    if [[ -n "$avg_peak_bandwidth_label" ]]; then
        rate_burnin_label="$avg_peak_bandwidth_label"
    fi
    
    log "Encoding: ${codec_res_fps_label} @ ${bitrate_label} (${bitrate_kbps}kbps target, preset $preset) - $encoder_name"
    log "  Resolution: ${width}x${height}"
    log "  Timecode: ${fontsize_tc}px at ($x_offset, $y_tc)"
    if [[ -n "$avg_peak_bandwidth_label" ]]; then
        if [[ -n "$avg_bandwidth_estimate" ]]; then
            log "  Avg/Peak bandwidth: ${avg_peak_bandwidth_label} (AVG from $VMAF_LOOKUP_CSV, PEAK from maxrate cap)"
        else
            log "  Avg/Peak bandwidth: ${avg_peak_bandwidth_label} (from target bitrate + maxrate cap)"
        fi
    fi
    if [[ -n "$vmaf_label" ]]; then
        log "  VMAF estimate: ${vmaf_label} (from $VMAF_LOOKUP_CSV)"
    fi
    
    # Build filter based on encoder type
    local filter=""
    if [ "$encoder_type" = "software" ]; then
        # 5-layer burn-in for software: timecode, bitrate, codec+res+fps, encoder, watermark
        log "  Rate burn-in: ${rate_burnin_label} (${fontsize_label}px at ($x_offset, $y_label))"
        
        local y_bitrate=$((y_tc + fontsize_tc + 5))
        local y_next=$((y_bitrate + fontsize_label + 5))
        local draw_vmaf_filter=""
        # VMAF burn-in intentionally disabled.
        # if [[ -n "$vmaf_label" ]]; then
        #     local y_vmaf=$y_next
        #     draw_vmaf_filter="drawtext=fontfile='${FONT}':text='${vmaf_label}':fontsize=${fontsize_label}:fontcolor=lime:box=1:boxcolor=black@0.7:boxborderw=5:x=${x_offset}:y=${y_vmaf},\\"
        #     y_next=$((y_vmaf + fontsize_label + 5))
        #     log "  VMAF burn-in: ${fontsize_label}px at ($x_offset, $y_vmaf)"
        # fi
        local y_codec_res_fps=$y_next
        local y_encoder=$((y_codec_res_fps + fontsize_label + 5))
        local y_watermark=$((y_encoder + fontsize_label + 5))
        
        log "  Codec+Res+FPS: ${fontsize_label}px at ($x_offset, $y_codec_res_fps)"
        log "  Encoder: ${encoder_label} at ($x_offset, $y_encoder)"
        log "  Watermark: JEO at ($x_offset, $y_watermark)"
        
        # Start filter chain with scale, then add padding BEFORE drawtext so burn-ins appear on padded frames
        filter="scale=${width}:${height}"
        
        # Add padding if needed (must come before drawtext to allow burn-ins on padded frames)
        local padding_enabled=0
        if (( $(awk -v pad="$VIDEO_PADDING_DURATION" 'BEGIN {print (pad > 0) ? 1 : 0}') )); then
            # Use stop_mode=add (not clone) to allow timecode to continue incrementing
            filter="${filter},tpad=stop_mode=add:stop_duration=${VIDEO_PADDING_DURATION}:color=${PADDING_COLOR}"
            padding_enabled=1
        fi
        
        # Add burn-in overlays (applied to all frames including padded ones)
        filter="${filter},\
drawtext=fontfile='${FONT}':timecode='00\\:00\\:00\\:00':rate=${SOURCE_FPS}:fontsize=${fontsize_tc}:fontcolor=yellow:box=1:boxcolor=black@1.0:boxborderw=5:x=${x_offset}:y=${y_tc},\
drawtext=fontfile='${FONT}':text='${rate_burnin_label}':fontsize=${fontsize_label}:fontcolor=cyan:box=1:boxcolor=black@0.7:boxborderw=5:x=${x_offset}:y=${y_bitrate},\
${draw_vmaf_filter}\
drawtext=fontfile='${FONT}':text='${codec_res_fps_label}':fontsize=${fontsize_label}:fontcolor=cyan:box=1:boxcolor=black@0.7:boxborderw=5:x=${x_offset}:y=${y_codec_res_fps},\
drawtext=fontfile='${FONT}':text='${encoder_label}':fontsize=${fontsize_label}:fontcolor=orange:box=1:boxcolor=black@0.7:boxborderw=5:x=${x_offset}:y=${y_encoder},\
drawtext=fontfile='${FONT}':text='JEO':fontsize=${fontsize_label}:fontcolor=white:box=1:boxcolor=black@0.7:boxborderw=5:x=${x_offset}:y=${y_watermark}"
        
        # Add PADDING label on top right (visible on all padding frames)
        if [ "$padding_enabled" -eq 1 ]; then
            local fontsize_padding=$((fontsize_tc * 2))  # Make it twice the size of timecode
            local x_padding="w-tw-10"  # Top right: width - text_width - 10px margin
            local y_padding=10
            # Show PADDING label on all padding frames (enable when time >= MEZ_DURATION)
            filter="${filter},drawtext=fontfile='${FONT}':text='PADDING':fontsize=${fontsize_padding}:fontcolor=red:box=1:boxcolor=black@0.9:boxborderw=8:x=${x_padding}:y=${y_padding}:enable='gte(t,${MEZ_DURATION})'"
            log "  Padding label: ${fontsize_padding}px at top-right (all padding frames)"
        fi
    else
        # Hardware layout mirrors software vertical order at top:
        # timecode -> rate (AVG/PEAK when available) -> codec+res+fps -> encoder -> watermark
        local y_bitrate=$((y_tc + fontsize_tc + 5))
        local y_next=$((y_bitrate + fontsize_label + 5))
        local draw_rate_filter="drawtext=fontfile='${FONT}':text='${rate_burnin_label}':fontsize=${fontsize_label}:fontcolor=cyan:box=1:boxcolor=black@0.7:boxborderw=5:x=${x_offset}:y=${y_bitrate},\\"
        log "  Rate burn-in: ${rate_burnin_label} (${fontsize_label}px at ($x_offset, $y_bitrate))"
        local draw_vmaf_filter=""
        # VMAF burn-in intentionally disabled.
        # if [[ -n "$vmaf_label" ]]; then
        #     local y_vmaf=$y_next
        #     draw_vmaf_filter="drawtext=fontfile='${FONT}':text='${vmaf_label}':fontsize=${fontsize_label}:fontcolor=lime:box=1:boxcolor=black@0.7:boxborderw=5:x=${x_offset}:y=${y_vmaf},\\"
        #     y_next=$((y_vmaf + fontsize_label + 5))
        #     log "  VMAF burn-in: ${fontsize_label}px at ($x_offset, $y_vmaf)"
        # fi
        local y_codec_res_fps=$y_next
        local y_encoder=$((y_codec_res_fps + fontsize_label + 5))
        local y_watermark=$((y_encoder + fontsize_label + 5))
        
        log "  Codec+Res+FPS: ${fontsize_label}px at ($x_offset, $y_codec_res_fps)"
        log "  Encoder: ${encoder_label} at ($x_offset, $y_encoder)"
        log "  Watermark: JEO at ($x_offset, $y_watermark)"
        
        # Start filter chain with scale, then add padding BEFORE drawtext so burn-ins appear on padded frames
        filter="scale=${width}:${height}"
        
        # Add padding if needed (must come before drawtext to allow burn-ins on padded frames)
        local padding_enabled=0
        if (( $(awk -v pad="$VIDEO_PADDING_DURATION" 'BEGIN {print (pad > 0) ? 1 : 0}') )); then
            # Use stop_mode=add (not clone) to allow timecode to continue incrementing
            filter="${filter},tpad=stop_mode=add:stop_duration=${VIDEO_PADDING_DURATION}:color=${PADDING_COLOR}"
            padding_enabled=1
        fi
        
        # Add burn-in overlays (applied to all frames including padded ones)
        filter="${filter},\
drawtext=fontfile='${FONT}':timecode='00\\:00\\:00\\:00':rate=${SOURCE_FPS}:fontsize=${fontsize_tc}:fontcolor=yellow:box=1:boxcolor=black@1.0:boxborderw=5:x=${x_offset}:y=${y_tc},\
${draw_rate_filter}\
${draw_vmaf_filter}\
drawtext=fontfile='${FONT}':text='${codec_res_fps_label}':fontsize=${fontsize_label}:fontcolor=cyan:box=1:boxcolor=black@0.7:boxborderw=5:x=${x_offset}:y=${y_codec_res_fps},\
drawtext=fontfile='${FONT}':text='${encoder_label}':fontsize=${fontsize_label}:fontcolor=orange:box=1:boxcolor=black@0.7:boxborderw=5:x=${x_offset}:y=${y_encoder},\
drawtext=fontfile='${FONT}':text='JEO':fontsize=${fontsize_label}:fontcolor=white:box=1:boxcolor=black@0.7:boxborderw=5:x=${x_offset}:y=${y_watermark}"
        
        # Add PADDING label on top right (visible on all padding frames)
        if [ "$padding_enabled" -eq 1 ]; then
            local fontsize_padding=$((fontsize_tc * 2))  # Make it twice the size of timecode
            local x_padding="w-tw-10"  # Top right: width - text_width - 10px margin
            local y_padding=10
            # Show PADDING label on all padding frames (enable when time >= MEZ_DURATION)
            filter="${filter},drawtext=fontfile='${FONT}':text='PADDING':fontsize=${fontsize_padding}:fontcolor=red:box=1:boxcolor=black@0.9:boxborderw=8:x=${x_padding}:y=${y_padding}:enable='gte(t,${MEZ_DURATION})'"
            log "  Padding label: ${fontsize_padding}px at top-right (all padding frames)"
        fi
    fi
    
    START_TIME=$(date +%s)
    
    # Execute encoding with encoder-specific commands
    if [ "$codec" = "hevc" ]; then
        if [ "$encoder_type" = "hardware" ]; then
            # VideoToolbox HEVC with bitrate control
            ffmpeg -i "$MEZZANINE" \
                   -vf "$filter" \
                   -c:v hevc_videotoolbox \
                   -allow_sw 1 \
                   -b:v "${bitrate_kbps}k" \
                   -maxrate "$((bitrate_kbps * MAXRATE_PERCENT / 100))k" \
                   -bufsize "$((bitrate_kbps * 2))k" \
                   -g "$KEYINT" \
                   -force_key_frames "expr:gte(n,n_forced*$KEYINT)" \
                   -tag:v hvc1 \
                   -pix_fmt nv12 \
                   -an \
                   -movflags empty_moov+default_base_moof -frag_duration 1000000 \
                   "$output_file" \
                   -loglevel warning -stats 2>&1 | tee -a "$LOG_FILE"
        else
            # libx265 software with bitrate control
            ffmpeg -i "$MEZZANINE" \
                   -vf "$filter" \
                   -c:v libx265 \
                   -b:v "${bitrate_kbps}k" \
                   -maxrate "$((bitrate_kbps * MAXRATE_PERCENT / 100))k" \
                   -bufsize "$((bitrate_kbps * 2))k" \
                   -preset "$preset" \
                   -threads 0 \
                   -x265-params "keyint=${KEYINT}:min-keyint=${KEYINT}:scenecut=0:open-gop=0:pools=+:frame-threads=0" \
                   -tag:v hvc1 \
                   -pix_fmt yuv420p \
                   -an \
                   -movflags empty_moov+default_base_moof -frag_duration 1000000 \
                   "$output_file" \
                   -loglevel warning -stats 2>&1 | tee -a "$LOG_FILE"
        fi
    elif [ "$codec" = "h264" ]; then
        if [ "$encoder_type" = "hardware" ]; then
            # VideoToolbox H.264 with bitrate control
            ffmpeg -i "$MEZZANINE" \
                   -vf "$filter" \
                   -c:v h264_videotoolbox \
                   -allow_sw 1 \
                   -b:v "${bitrate_kbps}k" \
                   -maxrate "$((bitrate_kbps * MAXRATE_PERCENT / 100))k" \
                   -bufsize "$((bitrate_kbps * 2))k" \
                   -g "$KEYINT" \
                   -force_key_frames "expr:gte(n,n_forced*$KEYINT)" \
                   -tag:v avc1 \
                   -pix_fmt nv12 \
                   -an \
                   -movflags empty_moov+default_base_moof -frag_duration 1000000 \
                   "$output_file" \
                   -loglevel warning -stats 2>&1 | tee -a "$LOG_FILE"
        else
            # libx264 software with bitrate control
            ffmpeg -i "$MEZZANINE" \
                   -vf "$filter" \
                   -c:v libx264 \
                   -b:v "${bitrate_kbps}k" \
                   -maxrate "$((bitrate_kbps * MAXRATE_PERCENT / 100))k" \
                   -bufsize "$((bitrate_kbps * 2))k" \
                   -preset "$preset" \
                   -threads 0 \
                   -x264-params "keyint=${KEYINT}:min-keyint=${KEYINT}:scenecut=0:open-gop=0" \
                   -tag:v avc1 \
                   -pix_fmt yuv420p \
                   -an \
                   -movflags empty_moov+default_base_moof -frag_duration 1000000 \
                   "$output_file" \
                   -loglevel warning -stats 2>&1 | tee -a "$LOG_FILE"
        fi
    else
        # AV1 (libsvtav1) software encoding
        ffmpeg -i "$MEZZANINE" \
               -vf "$filter" \
               -c:v libsvtav1 \
               -preset 8 \
               -b:v "${bitrate_kbps}k" \
               -maxrate "$((bitrate_kbps * MAXRATE_PERCENT / 100))k" \
               -bufsize "$((bitrate_kbps * 2))k" \
               -g "$KEYINT" \
               -force_key_frames "expr:gte(n,n_forced*$KEYINT)" \
               -svtav1-params "keyint=${KEYINT}:scd=0" \
               -pix_fmt yuv420p \
               -tag:v av01 \
               -an \
               -movflags empty_moov+default_base_moof -frag_duration 1000000 \
               "$output_file" \
               -loglevel warning -stats 2>&1 | tee -a "$LOG_FILE"
    fi
    
    END_TIME=$(date +%s)
    DURATION=$((END_TIME - START_TIME))
    
    if [[ ! -f "$output_file" ]]; then
        log_error "Encoding failed for $bitrate_label"
        ENCODING_ERRORS+=("Encoding failed for ${codec_upper} ${res_name} @ ${bitrate_label}")
        exit 1
    fi
    
    FILE_SIZE=$(du -h "$output_file" | cut -f1)
    BITRATE=$(ffprobe -v error -show_entries format=bit_rate -of default=noprint_wrappers=1:nokey=1 "$output_file" 2>/dev/null)
    BITRATE_KBPS=$((BITRATE / 1000))
    
    # Calculate encoding speed (duration / encode time)
    local encode_speed=$(awk -v mez="$MEZ_DURATION" -v dur="$DURATION" 'BEGIN {printf "%.2f", mez/dur}')
    
    # Track variant stats for report
    local variant_key="${codec_upper}_${res_name}"
    VARIANT_ENCODE_TIMES["$variant_key"]="$DURATION"
    VARIANT_FILE_SIZES["$variant_key"]="$FILE_SIZE"
    VARIANT_SPEEDS["$variant_key"]="${encode_speed}x"
    
    log_success "Encoded in ${DURATION}s: $FILE_SIZE @ ${BITRATE_KBPS} kbps (${encode_speed}x realtime)"
    echo ""
}

encode_all_variants() {
    print_header "Phase 3: Encoding Video Variants"
    
    # Calculate variant count
    local codec_multiplier=1
    if [[ "$CODEC_SELECTION" == "both" ]]; then
        codec_multiplier=2
    elif [[ "$CODEC_SELECTION" == "all" ]]; then
        codec_multiplier=3
    fi
    local variant_count=$((${#PROFILES[@]} * codec_multiplier))
    log "Encoding ${#PROFILES[@]} quality levels × $codec_multiplier codec(s) = $variant_count variants"
    echo ""
    
    # Parse profiles and encode
    for profile in "${PROFILES[@]}"; do
        IFS=':' read -r width height bitrate_h265 bitrate_h264 bitrate_av1 preset fontsize_tc fontsize_label x y_tc y_label <<< "$profile"
        local res_name=$(get_resolution_name "$width")
        local bitrate_h265_effective
        local bitrate_h264_effective
        bitrate_h265_effective=$(resolve_bitrate_override "$BITRATE_OVERRIDE_HEVC" "$res_name" "$bitrate_h265")
        bitrate_h264_effective=$(resolve_bitrate_override "$BITRATE_OVERRIDE_H264" "$res_name" "$bitrate_h264")
        
        # Encode HEVC variant if requested
        if [[ "$CODEC_SELECTION" == "both" ]] || [[ "$CODEC_SELECTION" == "all" ]] || [[ "$CODEC_SELECTION" == "hevc" ]]; then
            encode_variant "hevc" "$width" "$height" "$bitrate_h265_effective" "$preset" "$fontsize_tc" "$fontsize_label" "$x" "$y_tc" "$y_label"
        fi
        
        # Encode H.264 variant if requested
        if [[ "$CODEC_SELECTION" == "both" ]] || [[ "$CODEC_SELECTION" == "all" ]] || [[ "$CODEC_SELECTION" == "h264" ]]; then
            encode_variant "h264" "$width" "$height" "$bitrate_h264_effective" "$preset" "$fontsize_tc" "$fontsize_label" "$x" "$y_tc" "$y_label"
        fi

        # Encode AV1 variant if requested
        if [[ "$CODEC_SELECTION" == "all" ]] || [[ "$CODEC_SELECTION" == "av1" ]]; then
            encode_variant "av1" "$width" "$height" "$bitrate_av1" "$preset" "$fontsize_tc" "$fontsize_label" "$x" "$y_tc" "$y_label"
        fi
    done
}

################################################################################
# Phase 4: Create Audio Mezzanine
################################################################################

create_audio_mezzanine() {
    if [[ "$SKIP_AUDIO" == "true" ]]; then
        log_warn "Skipping audio (no audio stream in source)"
        return 0
    fi
    
    print_header "Phase 4: Creating Audio Mezzanine"
    
    log "Extracting audio from mezzanine file..."

    # Detect audio codec, profile, and channel count.
    AUDIO_CODEC=$(ffprobe -v error -select_streams a:0 -show_entries stream=codec_name \
                  -of default=noprint_wrappers=1:nokey=1 "$MEZZANINE" 2>/dev/null | head -1)
    AUDIO_PROFILE=$(ffprobe -v error -select_streams a:0 -show_entries stream=profile \
                  -of default=noprint_wrappers=1:nokey=1 "$MEZZANINE" 2>/dev/null | head -1)
    AUDIO_CHANNELS=$(ffprobe -v error -select_streams a:0 -show_entries stream=channels \
                  -of default=noprint_wrappers=1:nokey=1 "$MEZZANINE" 2>/dev/null | head -1)

    log "Source audio codec: $AUDIO_CODEC, profile: ${AUDIO_PROFILE:-?}, channels: ${AUDIO_CHANNELS:-?}"

    START_TIME=$(date +%s)

    # Determine if we need to apply padding
    local needs_padding=$(awk -v pad="$AUDIO_PADDING_DURATION" 'BEGIN {print (pad > 0) ? 1 : 0}')

    if [ "$needs_padding" -eq 1 ]; then
        log "Audio padding: ${AUDIO_PADDING_DURATION}s will be applied to reach aligned duration"
    fi

    # Force stereo output for player compatibility (HLS.js and several other
    # players have trouble with multichannel AAC). Triggers a transcode whenever
    # the source has more than 2 channels.
    local needs_downmix=0
    if [[ -n "$AUDIO_CHANNELS" && "$AUDIO_CHANNELS" =~ ^[0-9]+$ && "$AUDIO_CHANNELS" -gt 2 ]]; then
        needs_downmix=1
        log "Downmixing ${AUDIO_CHANNELS}-channel source to 2-channel stereo for player compatibility"
        ENCODING_INFOS+=("Audio downmixed from ${AUDIO_CHANNELS}-channel to stereo")
    fi

    # Always transcode to AAC-LC 192k stereo 48kHz. Single canonical
    # audio output for the whole catalogue, no codec/profile gating
    # decisions, every master.m3u8 carries `CODECS="...,mp4a.40.2"`,
    # every player on every platform decodes it. Trade is one extra
    # audio re-encode per clip — cheap (audio is a few MB), worth the
    # certainty across Apple AVPlayer / Android ExoPlayer / hls.js /
    # Roku.
    log "Transcoding audio (source codec: $AUDIO_CODEC, profile: ${AUDIO_PROFILE:-?}) → AAC-LC 192k stereo 48kHz"
    ENCODING_INFOS+=("Audio transcoded to AAC-LC 192k stereo 48kHz (source: ${AUDIO_CODEC})")

    if [ "$needs_padding" -eq 1 ]; then
        ffmpeg -i "$MEZZANINE" \
               -vn -c:a aac -b:a 192k -ar 48000 -ac 2 \
               -af "apad=pad_dur=${AUDIO_PADDING_DURATION}" \
               -movflags empty_moov+default_base_moof -frag_duration 1000000 \
               "$TEMP_DIR/audio.mp4" \
               -loglevel error -stats 2>&1 | tee -a "$LOG_FILE"
    else
        ffmpeg -i "$MEZZANINE" \
               -vn -c:a aac -b:a 192k -ar 48000 -ac 2 \
               -movflags empty_moov+default_base_moof -frag_duration 1000000 \
               "$TEMP_DIR/audio.mp4" \
               -loglevel error -stats 2>&1 | tee -a "$LOG_FILE"
    fi
    
    END_TIME=$(date +%s)
    DURATION=$((END_TIME - START_TIME))
    
    if [[ ! -f "$TEMP_DIR/audio.mp4" ]] || [[ ! -s "$TEMP_DIR/audio.mp4" ]]; then
        log_error "Audio mezzanine creation failed"
        exit 1
    fi
    
    AUDIO_SIZE=$(du -h "$TEMP_DIR/audio.mp4" | cut -f1)
    AUDIO_DURATION=$(ffprobe -v error -show_entries format=duration \
                     -of default=noprint_wrappers=1:nokey=1 \
                     "$TEMP_DIR/audio.mp4" 2>/dev/null | cut -d. -f1)
    
    log_success "Audio mezzanine created in ${DURATION}s: $AUDIO_SIZE, ${AUDIO_DURATION}s duration"
    echo ""
}

################################################################################
# Phase 5: Package to DASH
################################################################################

convert_to_segmentlist() {
    local output_dir=$1
    local manifest="$output_dir/manifest.mpd"
    
    log "Converting SegmentTemplate to SegmentList..."
    
    # Use external conversion script for proper SegmentTimeline support
    if [[ -f "$SCRIPT_DIR/convert_to_segmentlist.py" ]]; then
        python3 "$SCRIPT_DIR/convert_to_segmentlist.py" "$manifest" 2>&1 | grep -v "^DASH" | grep -v "^===" | grep -v "^$" | tee -a "$LOG_FILE"
        log_success "Manifest converted to SegmentList"
    else
        log_warn "convert_to_segmentlist.py not found - manifest remains in SegmentTemplate format"
    fi
}

################################################################################
# Phase 5: Package with ffmpeg (frag_keyframe for LL-HLS)
################################################################################

package_with_ffmpeg() {
    local codec=$1
    local output_dir=$2
    
    local codec_upper=$(echo "$codec" | tr '[:lower:]' '[:upper:]')
    print_header "Phase 5: Segmenting $codec_upper with ffmpeg (frag_keyframe)"
    
    log "Creating ${SEGMENT_DURATION}s segments with fragments at each keyframe (${PARTIAL_DURATION}s GOPs)..."
    log "This preserves GOP boundaries for proper LL-HLS with partials per segment..."
    
    # Create resolution subdirectories
    for profile in "${PROFILES[@]}"; do
        IFS=':' read -r width height _ _ _ _ _ _ _ _ <<< "$profile"
        local res_name=$(get_resolution_name $width)
        mkdir -p "$output_dir/$res_name"
    done
    
    # Create audio subdirectory only if audio exists
    if [[ "$SKIP_AUDIO" != "true" ]]; then
        mkdir -p "$output_dir/audio"
    fi
    
    # Segment each video resolution
    for profile in "${PROFILES[@]}"; do
        IFS=':' read -r width height _ _ _ _ _ _ _ _ <<< "$profile"
        local res_name=$(get_resolution_name $width)
        local input_file="$TEMP_DIR/${codec}_${res_name}.mp4"
        
        log "Segmenting $res_name..."
        
        # Extract init segment
        ffmpeg -y -i "$input_file" \
            -c copy \
            -t 0 \
            -movflags frag_keyframe+empty_moov+default_base_moof+separate_moof \
            "$output_dir/$res_name/init.mp4" \
            -loglevel error 2>&1 | tee -a "$LOG_FILE"
        
        # Create segments with fragment at each keyframe
        ffmpeg -y -i "$input_file" \
            -c copy \
            -map 0 \
            -f segment \
            -segment_format mp4 \
            -segment_time "$SEGMENT_DURATION" \
            -segment_format_options movflags=frag_keyframe+empty_moov+default_base_moof+separate_moof \
            -reset_timestamps 1 \
            "$output_dir/$res_name/segment_%05d.m4s" \
            -loglevel error -stats 2>&1 | tee -a "$LOG_FILE"
        
        local seg_count=$(ls -1 "$output_dir/$res_name"/segment_*.m4s 2>/dev/null | wc -l)
        log_success "$res_name: $seg_count segments created"
    done
    
    # Segment audio (if present)
    if [[ "$SKIP_AUDIO" != "true" ]]; then
        log "Segmenting audio..."
        
        # Extract init segment
        ffmpeg -y -i "$TEMP_DIR/audio.mp4" \
            -c copy \
            -t 0 \
            -movflags frag_keyframe+empty_moov+default_base_moof+separate_moof \
            "$output_dir/audio/init.mp4" \
            -loglevel error 2>&1 | tee -a "$LOG_FILE"
        
        # Create segments
        ffmpeg -y -i "$TEMP_DIR/audio.mp4" \
            -c copy \
            -map 0 \
            -f segment \
            -segment_format mp4 \
            -segment_time "$SEGMENT_DURATION" \
            -segment_format_options movflags=frag_keyframe+empty_moov+default_base_moof+separate_moof \
            -reset_timestamps 1 \
            "$output_dir/audio/segment_%05d.m4s" \
            -loglevel error -stats 2>&1 | tee -a "$LOG_FILE"
        
        local seg_count=$(ls -1 "$output_dir/audio"/segment_*.m4s 2>/dev/null | wc -l)
        log_success "audio: $seg_count segments created"
    fi
    
    # Generate HLS manifests
    log "Creating HLS manifests..."
    
    # Create variant playlists for each video resolution
    for profile in "${PROFILES[@]}"; do
        IFS=':' read -r width height _ _ _ _ _ _ _ _ <<< "$profile"
        local res_name=$(get_resolution_name $width)
        local playlist="$output_dir/$res_name/playlist.m3u8"
        
        # Count segments
        local seg_count=$(ls -1 "$output_dir/$res_name"/segment_*.m4s 2>/dev/null | wc -l)
        
        # Get duration of first segment to set target duration
        local first_seg="$output_dir/$res_name/segment_00001.m4s"
        local target_duration=3
        if [ -f "$first_seg" ]; then
            local duration=$(ffprobe -v error -show_entries format=duration \
                -of default=noprint_wrappers=1:nokey=1 "$first_seg" 2>/dev/null)
            if [ -n "$duration" ] && [ "$duration" != "N/A" ]; then
                target_duration=$(awk -v dur="$duration" 'BEGIN {printf "%.0f", dur + 1}')
            fi
        fi
        
        # Create variant playlist header
        cat > "$playlist" << EOF
#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:$target_duration
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-MAP:URI="init.mp4"
EOF
        
        # Add segment entries
        for segment in "$output_dir/$res_name"/segment_*.m4s; do
            if [ -f "$segment" ]; then
                local seg_name=$(basename "$segment")
                local seg_duration=$(ffprobe -v error -show_entries format=duration \
                    -of default=noprint_wrappers=1:nokey=1 "$segment" 2>/dev/null)
                if [ -z "$seg_duration" ] || [ "$seg_duration" = "N/A" ]; then
                    seg_duration=$(awk -v dur="$SEGMENT_DURATION" 'BEGIN {printf "%.6f", dur}')
                fi
                echo "#EXTINF:$seg_duration," >> "$playlist"
                echo "$seg_name" >> "$playlist"
            fi
        done
        
        echo "#EXT-X-ENDLIST" >> "$playlist"
        log_success "$res_name: playlist.m3u8 created ($seg_count segments)"
    done
    
    # Create audio variant playlist (if present)
    if [[ "$SKIP_AUDIO" != "true" ]]; then
        local audio_playlist="$output_dir/audio/playlist.m3u8"
        local seg_count=$(ls -1 "$output_dir/audio"/segment_*.m4s 2>/dev/null | wc -l)
        
        # Get duration of first segment
        local first_seg="$output_dir/audio/segment_00001.m4s"
        local target_duration=3
        if [ -f "$first_seg" ]; then
            local duration=$(ffprobe -v error -show_entries format=duration \
                -of default=noprint_wrappers=1:nokey=1 "$first_seg" 2>/dev/null)
            if [ -n "$duration" ] && [ "$duration" != "N/A" ]; then
                target_duration=$(awk -v dur="$duration" 'BEGIN {printf "%.0f", dur + 1}')
            fi
        fi
        
        # Create audio playlist header
        cat > "$audio_playlist" << EOF
#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:$target_duration
#EXT-X-PLAYLIST-TYPE:VOD
#EXT-X-INDEPENDENT-SEGMENTS
#EXT-X-MAP:URI="init.mp4"
EOF
        
        # Add segment entries
        for segment in "$output_dir/audio"/segment_*.m4s; do
            if [ -f "$segment" ]; then
                local seg_name=$(basename "$segment")
                local seg_duration=$(ffprobe -v error -show_entries format=duration \
                    -of default=noprint_wrappers=1:nokey=1 "$segment" 2>/dev/null)
                if [ -z "$seg_duration" ] || [ "$seg_duration" = "N/A" ]; then
                    seg_duration=$(awk -v dur="$SEGMENT_DURATION" 'BEGIN {printf "%.6f", dur}')
                fi
                echo "#EXTINF:$seg_duration," >> "$audio_playlist"
                echo "$seg_name" >> "$audio_playlist"
            fi
        done
        
        echo "#EXT-X-ENDLIST" >> "$audio_playlist"
        log_success "audio: playlist.m3u8 created ($seg_count segments)"
    fi
    
    # Create master playlist
    local master_file="$output_dir/master.m3u8"

    cat > "$master_file" << 'EOF'
#EXTM3U
#EXT-X-VERSION:7

EOF
    
    # Add audio rendition if exists
    if [[ "$SKIP_AUDIO" != "true" ]] && [[ -f "$output_dir/audio/playlist.m3u8" ]]; then
        cat >> "$master_file" << 'EOF'
# Audio
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",NAME="English",DEFAULT=YES,AUTOSELECT=YES,LANGUAGE="en",URI="audio/playlist.m3u8"

EOF
    fi
    
    # Add video variants
    echo "# Video Variants" >> "$master_file"

    # Measure default audio rendition average bitrate once for BANDWIDTH/AVERAGE-BANDWIDTH.
    local audio_average_bandwidth=0
    if [[ "$SKIP_AUDIO" != "true" ]] && [[ -f "$output_dir/audio/playlist.m3u8" ]]; then
        local audio_total_bytes=0
        local audio_total_duration=$(awk -F: '/^#EXTINF:/{gsub(/,.*/, "", $2); s+=$2} END{printf "%.6f", s+0}' "$output_dir/audio/playlist.m3u8")
        if [[ -f "$output_dir/audio/init.mp4" ]]; then
            local audio_init_size=$(stat -c %s "$output_dir/audio/init.mp4" 2>/dev/null || stat -f %z "$output_dir/audio/init.mp4" 2>/dev/null)
            audio_total_bytes=$((audio_total_bytes + audio_init_size))
        fi
        for audio_seg in "$output_dir/audio"/segment_*.m4s; do
            if [[ -f "$audio_seg" ]]; then
                local audio_seg_size=$(stat -c %s "$audio_seg" 2>/dev/null || stat -f %z "$audio_seg" 2>/dev/null)
                audio_total_bytes=$((audio_total_bytes + audio_seg_size))
            fi
        done
        if awk -v d="$audio_total_duration" 'BEGIN{exit !(d>0)}'; then
            audio_average_bandwidth=$(awk -v bytes="$audio_total_bytes" -v dur="$audio_total_duration" 'BEGIN {printf "%.0f", (bytes * 8) / dur}')
        fi
    fi
    
    # Process each resolution in reverse order (highest first)
    for profile in "${PROFILES[@]}"; do
        IFS=':' read -r width height _ _ _ _ _ _ _ _ <<< "$profile"
        local res_name=$(get_resolution_name $width)
        local variant_playlist="$output_dir/$res_name/playlist.m3u8"
        
        if [[ ! -f "$variant_playlist" ]]; then
            continue
        fi
        
        # Get video metadata from init.mp4
        local init_mp4="$output_dir/$res_name/init.mp4"
        if [[ ! -f "$init_mp4" ]]; then
            log_warn "init.mp4 not found for $res_name, skipping from master playlist"
            continue
        fi
        
        # Extract codec information
        local codec_name=$(ffprobe -v error -select_streams v:0 -show_entries stream=codec_name \
            -of default=noprint_wrappers=1:nokey=1 "$init_mp4" 2>/dev/null)
        local profile_name=$(ffprobe -v error -select_streams v:0 -show_entries stream=profile \
            -of default=noprint_wrappers=1:nokey=1 "$init_mp4" 2>/dev/null | head -1)
        local level=$(ffprobe -v error -select_streams v:0 -show_entries stream=level \
            -of default=noprint_wrappers=1:nokey=1 "$init_mp4" 2>/dev/null | head -1)
        local framerate=$(ffprobe -v error -select_streams v:0 -show_entries stream=r_frame_rate \
            -of default=noprint_wrappers=1:nokey=1 "$init_mp4" 2>/dev/null)
        
        # Convert framerate to decimal
        if [[ "$framerate" =~ ^([0-9]+)/([0-9]+)$ ]]; then
            framerate=$(awk -v num="${BASH_REMATCH[1]}" -v den="${BASH_REMATCH[2]}" 'BEGIN {printf "%.3f", num/den}')
        fi
        
        # Derive codec string
        local video_codecs=""
        if [[ "$codec_name" == "h264" ]]; then
            video_codecs=$(derive_codec_string "h264" "$profile_name" "$level")
        elif [[ "$codec_name" == "hevc" ]]; then
            video_codecs=$(derive_codec_string "hevc" "$profile_name" "$level")
        fi
        
        # Calculate bandwidth from all segments
        local total_bytes=0
        local segment_count=0
        for seg_file in "$output_dir/$res_name"/segment_*.m4s; do
            if [[ -f "$seg_file" ]]; then
                local file_size=$(stat -c %s "$seg_file" 2>/dev/null || stat -f %z "$seg_file" 2>/dev/null)
                total_bytes=$((total_bytes + file_size))
                segment_count=$((segment_count + 1))
            fi
        done
        
        # Add init.mp4 size
        if [[ -f "$init_mp4" ]]; then
            local init_size=$(stat -c %s "$init_mp4" 2>/dev/null || stat -f %z "$init_mp4" 2>/dev/null)
            total_bytes=$((total_bytes + init_size))
        fi
        
        # Calculate average video bitrate (bits per second) from generated media.
        local duration=$(awk -F: '/^#EXTINF:/{gsub(/,.*/, "", $2); s+=$2} END{printf "%.6f", s+0}' "$variant_playlist")
        if ! awk -v d="$duration" 'BEGIN{exit !(d>0)}'; then
            duration=$(awk -v count="$segment_count" -v seg="$SEGMENT_DURATION" 'BEGIN {printf "%.6f", count * seg}')
        fi
        local video_average_bandwidth=$(awk -v bytes="$total_bytes" -v dur="$duration" 'BEGIN {printf "%.0f", (bytes * 8) / dur}')
        local video_peak_bandwidth
        video_peak_bandwidth=$(awk -v avg="$video_average_bandwidth" -v pct="$MAXRATE_PERCENT" 'BEGIN {p=(avg*pct/100.0); if (p<avg) p=avg; printf "%.0f", p}')
        local bandwidth="$video_peak_bandwidth"
        
        # Build CODECS string
        local codecs_str="$video_codecs"
        if [[ "$SKIP_AUDIO" != "true" ]]; then
            codecs_str="${video_codecs},mp4a.40.2"
        fi
        
        # Include default audio rendition bitrate in both BANDWIDTH and AVERAGE-BANDWIDTH.
        local average_bandwidth="$video_average_bandwidth"
        if [[ "$audio_average_bandwidth" -gt 0 ]]; then
            bandwidth=$((bandwidth + audio_average_bandwidth))
            average_bandwidth=$((average_bandwidth + audio_average_bandwidth))
        fi

        # Write variant line
        if [[ "$SKIP_AUDIO" != "true" ]]; then
            echo "#EXT-X-STREAM-INF:BANDWIDTH=$bandwidth,AVERAGE-BANDWIDTH=$average_bandwidth,RESOLUTION=${width}x${height},CODECS=\"$codecs_str\",AUDIO=\"audio\",FRAME-RATE=$framerate" >> "$master_file"
        else
            echo "#EXT-X-STREAM-INF:BANDWIDTH=$bandwidth,AVERAGE-BANDWIDTH=$average_bandwidth,RESOLUTION=${width}x${height},CODECS=\"$codecs_str\",FRAME-RATE=$framerate" >> "$master_file"
        fi
        echo "$res_name/playlist.m3u8" >> "$master_file"
    done
    
    log_success "master.m3u8 created"
    
    # Count total segments
    local total_segments=$(find "$output_dir" -name "*.m4s" | wc -l | tr -d ' ')
    log_success "ffmpeg segmentation complete: $total_segments segments"
    log_success "Location: $output_dir"
    echo ""
}

package_dash() {
    local codec=$1
    local output_dir=$2
    
    local codec_upper=$(echo "$codec" | tr '[:lower:]' '[:upper:]')
    print_header "Phase 5: Packaging $codec_upper to DASH"
    
    if ! command -v packager &> /dev/null; then
        log_warn "Shaka Packager not found - skipping DASH packaging"
        log_warn "Encoded files available in: $TEMP_DIR"
        return
    fi
    
    log "Creating DASH package with ${SEGMENT_DURATION}s segments (${PARTIAL_DURATION}s fragments for LL-HLS)..."
    log "Organizing output with resolution subdirectories..."
    
    # Create resolution subdirectories
    for profile in "${PROFILES[@]}"; do
        IFS=':' read -r width height _ _ _ _ _ _ _ _ <<< "$profile"
        local res_name=$(get_resolution_name $width)
        mkdir -p "$output_dir/$res_name"
    done
    
    # Create audio subdirectory only if audio exists
    if [[ "$SKIP_AUDIO" != "true" ]]; then
        mkdir -p "$output_dir/audio"
    fi
    
    # Build packager command
    local cmd="packager"
    
    # Add video streams
    for profile in "${PROFILES[@]}"; do
        IFS=':' read -r width height _ _ _ _ _ _ _ _ <<< "$profile"
        local res_name=$(get_resolution_name $width)
        local input_file="$TEMP_DIR/${codec}_${res_name}.mp4"
        
        cmd="$cmd 'in=${input_file},stream=video,init_segment=${res_name}/init.mp4,segment_template=${res_name}/segment_\$Number%05d\$.m4s'"
    done
    
    # Add audio stream only if present
    if [[ "$SKIP_AUDIO" != "true" ]]; then
        cmd="$cmd 'in=$TEMP_DIR/audio.mp4,stream=audio,init_segment=audio/init.mp4,segment_template=audio/segment_\$Number%05d\$.m4s'"
    fi
    
    # Add packaging options (segment/fragment durations for LL-HLS support)
    cmd="$cmd --segment_duration $SEGMENT_DURATION"
    cmd="$cmd --fragment_duration $PARTIAL_DURATION"
    cmd="$cmd --fragment_sap_aligned=false"
    cmd="$cmd --mpd_output manifest.mpd"
    cmd="$cmd --generate_static_live_mpd"  # This helps with SegmentList generation
    
    # Execute in output directory
    cd "$output_dir"
    eval $cmd 2>&1 | tee -a "$LOG_FILE"
    local packager_exit_code=$?
    cd "$SCRIPT_DIR"
    
    # Check if manifest.mpd was created successfully
    # If not, Shaka Packager may have left it in a temp file due to filesystem issues
    if [[ ! -f "$output_dir/manifest.mpd" ]]; then
        log_warn "manifest.mpd not found - checking for Shaka Packager temp files..."
        
        # Look for the largest packager temp file created after this encode started
        # (largest = most complete, as partial manifests are smaller)
        local temp_matches
        temp_matches=$(find "${TEMP_BASE}/encoding" -name "packager-tempfile-*" -type f -newer "$output_dir" 2>/dev/null)
        local temp_manifest=""
        if [[ -n "$temp_matches" ]]; then
            temp_manifest=$(printf '%s\n' "$temp_matches" | xargs ls -S 2>/dev/null | head -1)
        fi

        if [[ -n "$temp_manifest" ]] && [[ -f "$temp_manifest" ]]; then
            log "Found temp manifest: $temp_manifest"
            cp "$temp_manifest" "$output_dir/manifest.mpd"
            log_success "Recovered manifest.mpd from temp file"
        else
            log_error "Failed to create manifest.mpd and no temp file found"
            return 1
        fi
    fi
    
    # Convert SegmentTemplate to SegmentList
    convert_to_segmentlist "$output_dir"
    
    # Count segments
    local total_segments=$(find "$output_dir" -name "*.m4s" | wc -l | tr -d ' ')
    local manifest_size=$(du -h "$output_dir/manifest.mpd" | cut -f1)
    
    log_success "DASH package created: $total_segments segments"
    log_success "Manifest: $manifest_size (SegmentList)"
    log_success "Location: $output_dir"
    echo ""
}

################################################################################
# Phase 6: Parse fMP4 Fragments (Generate .byteranges for LL-HLS)
################################################################################

parse_fmp4_fragments() {
    print_header "Phase 6: Generating Fragment Metadata"
    
    log "Parsing fMP4 segments to generate .byteranges files for LL-HLS support..."

    if ! detect_fragment_parser_script; then
        log_warn "Skipping Phase 6: parser helper not found"
        echo ""
        return 0
    fi
    log "Using fragment parser: $FRAGMENT_PARSER_SCRIPT"
    
    local total_parsed=0
    local total_failed=0
    local hevc_count=0
    local h264_count=0
    local av1_count=0
    local first_failure_logged=false
    
    # Process HEVC package if it exists
    if [[ -n "$OUTPUT_DIR_HEVC" ]] && [[ -d "$OUTPUT_DIR_HEVC" ]]; then
        log "Processing HEVC segments..."
        while IFS= read -r -d '' segment; do
            # Detect track type from directory path
            local track_type="video"
            if [[ "$segment" == */audio/* ]]; then
                track_type="audio"
            fi
            
            # Run parser without stdout/stderr to reduce pipe traffic
            if python3 "$FRAGMENT_PARSER_SCRIPT" --track-type "$track_type" --segment-duration "$SEGMENT_DURATION" --gop-duration "$GOP_DURATION" "$segment" >/dev/null 2>&1; then
                ((total_parsed++))
                ((hevc_count++))
            else
                ((total_failed++))
                if [[ "$first_failure_logged" == "false" ]]; then
                    local err_sample
                    err_sample=$(python3 "$FRAGMENT_PARSER_SCRIPT" --track-type "$track_type" --segment-duration "$SEGMENT_DURATION" --gop-duration "$GOP_DURATION" "$segment" 2>&1 | head -1)
                    log_warn "Sample parser error: ${err_sample:-unknown}"
                    first_failure_logged=true
                fi
            fi
        done < <(find "$OUTPUT_DIR_HEVC" -name "*.m4s" -print0 | sort -z)
        log_success "HEVC: Parsed $hevc_count segments"
    fi
    
    # Process H.264 package if it exists
    if [[ -n "$OUTPUT_DIR_H264" ]] && [[ -d "$OUTPUT_DIR_H264" ]]; then
        log "Processing H.264 segments..."
        while IFS= read -r -d '' segment; do
            # Detect track type from directory path
            local track_type="video"
            if [[ "$segment" == */audio/* ]]; then
                track_type="audio"
            fi
            
            # Run parser without stdout/stderr to reduce pipe traffic
            if python3 "$FRAGMENT_PARSER_SCRIPT" --track-type "$track_type" --segment-duration "$SEGMENT_DURATION" --gop-duration "$GOP_DURATION" "$segment" >/dev/null 2>&1; then
                ((total_parsed++))
                ((h264_count++))
            else
                ((total_failed++))
                if [[ "$first_failure_logged" == "false" ]]; then
                    local err_sample
                    err_sample=$(python3 "$FRAGMENT_PARSER_SCRIPT" --track-type "$track_type" --segment-duration "$SEGMENT_DURATION" --gop-duration "$GOP_DURATION" "$segment" 2>&1 | head -1)
                    log_warn "Sample parser error: ${err_sample:-unknown}"
                    first_failure_logged=true
                fi
            fi
        done < <(find "$OUTPUT_DIR_H264" -name "*.m4s" -print0 | sort -z)
        log_success "H.264: Parsed $h264_count segments"
    fi

    # Process AV1 package if it exists
    if [[ -n "$OUTPUT_DIR_AV1" ]] && [[ -d "$OUTPUT_DIR_AV1" ]]; then
        log "Processing AV1 segments..."
        while IFS= read -r -d '' segment_file; do
            local track_type="video"
            if [[ "$segment_file" == */audio/* ]]; then
                track_type="audio"
            fi
            if python3 "$FRAGMENT_PARSER_SCRIPT" --track-type "$track_type" --segment-duration "$SEGMENT_DURATION" --gop-duration "$GOP_DURATION" "$segment_file" >/dev/null 2>&1; then
                ((total_parsed++)) || true
                ((av1_count++)) || true
            else
                ((total_failed++)) || true
                if [[ "$first_failure_logged" == "false" ]]; then
                    local err_sample
                    err_sample=$(python3 "$FRAGMENT_PARSER_SCRIPT" --track-type "$track_type" --segment-duration "$SEGMENT_DURATION" --gop-duration "$GOP_DURATION" "$segment_file" 2>&1 | head -1)
                    log_warn "Sample parser error: ${err_sample:-unknown}"
                    first_failure_logged=true
                fi
            fi
        done < <(find "$OUTPUT_DIR_AV1" -name "*.m4s" -print0 | sort -z)
        log_success "AV1: Parsed $av1_count segments"
    fi
    
    # Summary
    if [[ $total_parsed -gt 0 ]]; then
        log_success "Total: Parsed $total_parsed segments successfully"
        if [[ $total_failed -gt 0 ]]; then
            log_warn "Failed to parse $total_failed segments"
        fi
        log "Fragment metadata saved as .byteranges files alongside segments"
    else
        log_warn "No segments parsed - .byteranges files not generated"
        if [[ $total_failed -gt 0 ]]; then
            log_error "Failed to parse $total_failed segments"
        fi
    fi
    
    echo ""
}

################################################################################
# Phase 7: Generate HLS Manifests
################################################################################

generate_hls_manifests() {
    print_header "Phase 7: Generating HLS Manifests"
    
    log "Creating HLS manifests from DASH packages..."
    
    # Generate HLS for each created package
    local hls_success_count=0
    local hls_total_count=0
    
    if [[ -n "$OUTPUT_DIR_HEVC" ]] && [[ -f "$OUTPUT_DIR_HEVC/manifest.mpd" ]]; then
        ((hls_total_count++)) || true
        log "Generating HLS for HEVC package..."
        if python3 -c "
import sys
sys.path.insert(0, '$SCRIPT_DIR')
from create_hls_manifests import create_hls_for_dash_package
create_hls_for_dash_package('$OUTPUT_DIR_HEVC')
" 2>&1 | tee -a "$LOG_FILE"; then
            ((hls_success_count++)) || true
            log_success "HEVC HLS manifests created"
        else
            log_warn "HEVC HLS generation failed"
        fi
    fi
    
    if [[ -n "$OUTPUT_DIR_H264" ]] && [[ -f "$OUTPUT_DIR_H264/manifest.mpd" ]]; then
        ((hls_total_count++)) || true
        log "Generating HLS for H.264 package..."
        if python3 -c "
import sys
sys.path.insert(0, '$SCRIPT_DIR')
from create_hls_manifests import create_hls_for_dash_package
create_hls_for_dash_package('$OUTPUT_DIR_H264')
" 2>&1 | tee -a "$LOG_FILE"; then
            ((hls_success_count++)) || true
            log_success "H.264 HLS manifests created"
        else
            log_warn "H.264 HLS generation failed"
        fi
    fi

    if [[ -n "$OUTPUT_DIR_AV1" ]] && [[ -f "$OUTPUT_DIR_AV1/manifest.mpd" ]]; then
        ((hls_total_count++)) || true
        log "Generating HLS for AV1 package..."
        if python3 -c "
import sys
sys.path.insert(0, '$SCRIPT_DIR')
from create_hls_manifests import create_hls_for_dash_package
create_hls_for_dash_package('$OUTPUT_DIR_AV1')
" 2>&1 | tee -a "$LOG_FILE"; then
            ((hls_success_count++)) || true
            log_success "AV1 HLS manifests created"
        else
            log_warn "AV1 HLS generation failed"
        fi
    fi
    
    if [[ $hls_success_count -gt 0 ]]; then
        log_success "Created HLS manifests for $hls_success_count/$hls_total_count packages"
    fi
    
    echo ""
}

# ============================================================================
# TS HLS GENERATION (PHASE 2)
# ============================================================================

generate_hls_ts_segments() {
    local temp_dir=$1
    local ts_output_dir=$2
    local codec=$3
    local dash_manifest_dir=$4
    
    if [ "$HLS_FORMAT" != "ts" ] && [ "$HLS_FORMAT" != "both" ]; then
        return 0  # Skip TS generation if not requested
    fi
    
    print_header "Phase 7b: Generating HLS Transport Stream Package"
    
    log "Creating HLS TS package: $(basename $ts_output_dir)"
    log "Using encoded files from: $temp_dir"
    
    # Generate TS segments from the original encoded MP4 files in temp directory
    # These files have complete video/audio data, unlike the fragmented init.mp4 files
    
    for profile in "${PROFILES[@]}"; do
        IFS=':' read -r width height bitrate_h265 bitrate_h264 preset fontsize_tc fontsize_label x y_tc y_label <<< "$profile"
        
        local res_name=$(get_resolution_name $width)
        local source_mp4="$temp_dir/${codec}_${res_name}.mp4"
        
        if [ ! -f "$source_mp4" ]; then
            log_warn "Source file not found: $source_mp4"
            continue
        fi
        
        log "Processing ${res_name}..."
        
        # Create resolution directory (flat structure, no ts/ subdirectory)
        local res_dir="${ts_output_dir}/${res_name}"
        mkdir -p "$res_dir"
        
        log "  Source: $(basename $source_mp4)"
        log "  Creating TS segments in: ${res_dir}/"
        
        # Generate TS segments using FFmpeg HLS muxer
        # Use the same 4-second segment duration
        ffmpeg -i "$source_mp4" \
            -c copy \
            -f hls \
            -hls_time $SEGMENT_DURATION \
            -hls_segment_type mpegts \
            -hls_segment_filename "${res_dir}/segment_%03d.ts" \
            -hls_playlist_type vod \
            "${res_dir}/playlist.m3u8" \
            -loglevel warning -stats 2>&1 | tee -a "$LOG_FILE"
        
        if [ $? -eq 0 ]; then
            local segment_count=$(ls -1 "${res_dir}"/segment_*.ts 2>/dev/null | wc -l | tr -d ' ')
            log_success "  Created ${segment_count} TS segments for ${res_name}"
        else
            log_error "  Failed to create TS segments for ${res_name}"
            ENCODING_ERRORS+=("TS segment generation failed for ${res_name}")
        fi
    done
    
    # Handle audio TS segments if audio exists
    if [ "$SKIP_AUDIO" != "true" ] && [ -f "$temp_dir/audio.mp4" ]; then
        log "Processing audio..."
        
        local audio_dir="${ts_output_dir}/audio"
        mkdir -p "$audio_dir"
        
        log "  Source: audio.mp4"
        log "  Creating TS segments in: ${audio_dir}/"
        
        # Re-encode audio to AAC to ensure perfect segment boundary alignment
        # Using -c copy can cause fractional frame misalignment issues
        ffmpeg -i "$temp_dir/audio.mp4" \
            -c:a aac -b:a 192k -ar 48000 -ac 2 \
            -f hls \
            -hls_time $SEGMENT_DURATION \
            -hls_segment_type mpegts \
            -hls_flags independent_segments \
            -hls_segment_filename "${audio_dir}/segment_%03d.ts" \
            -hls_playlist_type vod \
            "${audio_dir}/playlist.m3u8" \
            -loglevel warning -stats 2>&1 | tee -a "$LOG_FILE"
        
        if [ $? -eq 0 ]; then
            local segment_count=$(ls -1 "${audio_dir}"/segment_*.ts 2>/dev/null | wc -l | tr -d ' ')
            log_success "  Created ${segment_count} TS segments for audio"
            
            # Validate audio/video segment count match
            local video_segment_count=$(ls -1 "${ts_output_dir}"/720p/segment_*.ts 2>/dev/null | wc -l | tr -d ' ')
            if [ "$segment_count" != "$video_segment_count" ]; then
                log_warn "  Audio/video segment count mismatch! Audio: ${segment_count}, Video: ${video_segment_count}"
                ENCODING_WARNINGS+=("Audio/video segment count mismatch: Audio=${segment_count}, Video=${video_segment_count}")
            else
                log_success "  Audio/video segment counts match: ${segment_count} segments"
            fi
        else
            log_error "  Failed to create TS segments for audio"
            ENCODING_ERRORS+=("TS segment generation failed for audio")
        fi
    fi
    
    # Generate TS master playlist
    generate_ts_master_playlist "$ts_output_dir" "$codec" "$dash_manifest_dir" "$temp_dir"
    
    echo ""
}

# Helper function to derive HLS codec string from profile and level
derive_codec_string() {
    local codec=$1
    local profile=$2
    local level=$3
    
    if [ "$codec" = "h264" ]; then
        # Convert profile name to hex
        case "$profile" in
            "High") local profile_hex="64" ;;
            "Main") local profile_hex="4d" ;;
            "Baseline") local profile_hex="42" ;;
            *) local profile_hex="64" ;;  # Default to High
        esac
        
        # Convert level to hex (e.g., 30 -> 1e, 31 -> 1f, 40 -> 28)
        local level_hex=$(printf "%02x" "$level")
        echo "avc1.${profile_hex}00${level_hex}"
    elif [ "$codec" = "hevc" ]; then
        # HEVC codec strings: hvc1.PROFILE.FLAGS.LEVEL.CONSTRAINTS
        # For simplicity, derive basic string from level
        # Level 30 (1.0) -> L30, Level 93 (3.1) -> L93, etc.
        case "$level" in
            30) echo "hvc1.1.6.L30.90" ;;
            60) echo "hvc1.1.6.L60.90" ;;
            63) echo "hvc1.1.6.L63.90" ;;
            90) echo "hvc1.1.6.L90.90" ;;
            93) echo "hvc1.1.6.L93.90" ;;
            120) echo "hvc1.1.6.L120.b0" ;;
            123) echo "hvc1.1.6.L123.b0" ;;
            150) echo "hvc1.1.6.L150.b0" ;;
            153) echo "hvc1.1.6.L153.b0" ;;
            *) echo "hvc1.1.6.L93.90" ;;  # Default to 720p level
        esac
    elif [ "$codec" = "av1" ]; then
        # AV1 codec strings: av01.P.LL.BB (approximate, 8-bit main profile)
        # Use a conservative level based on resolution
        if [[ "$level" -ge 150 ]]; then
            echo "av01.0.08M.08"
        elif [[ "$level" -ge 93 ]]; then
            echo "av01.0.06M.08"
        else
            echo "av01.0.05M.08"
        fi
    else
        echo "unknown"
    fi
}

generate_ts_master_playlist() {
    local ts_output_dir=$1
    local codec=$2
    local dash_manifest_dir=$3
    local temp_dir=$4
    local master_file="${ts_output_dir}/master.m3u8"
    local dash_manifest="${dash_manifest_dir}/manifest.mpd"
    
    log "Creating TS master playlist: $(basename $master_file)"
    
    # Start master playlist
    cat > "$master_file" << 'EOF'
#EXTM3U
#EXT-X-VERSION:3
EOF
    
    # Add audio rendition if exists
    if [ "$SKIP_AUDIO" != "true" ] && [ -f "${ts_output_dir}/audio/playlist.m3u8" ]; then
        cat >> "$master_file" << 'EOF'

# Audio
#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="audio",NAME="English",DEFAULT=YES,AUTOSELECT=YES,LANGUAGE="en",URI="audio/playlist.m3u8"
EOF
    fi
    
    # Add video variants
    echo "" >> "$master_file"
    echo "# Video Variants" >> "$master_file"
    
    # Find all resolution directories and sort them
    local res_dirs=$(find "$ts_output_dir" -mindepth 1 -maxdepth 1 -type d ! -name "audio" | sort -r)

    # Measure default audio rendition average bitrate once for BANDWIDTH/AVERAGE-BANDWIDTH.
    local audio_average_bandwidth=0
    if [ "$SKIP_AUDIO" != "true" ] && [ -f "${ts_output_dir}/audio/playlist.m3u8" ]; then
        local audio_total_bytes=0
        local audio_total_duration=$(awk -F: '/^#EXTINF:/{gsub(/,.*/, "", $2); s+=$2} END{printf "%.6f", s+0}' "${ts_output_dir}/audio/playlist.m3u8")
        for audio_ts in "${ts_output_dir}/audio"/segment_*.ts; do
            if [ -f "$audio_ts" ]; then
                local audio_seg_size=$(stat -c %s "$audio_ts" 2>/dev/null || stat -f %z "$audio_ts" 2>/dev/null)
                audio_total_bytes=$((audio_total_bytes + audio_seg_size))
            fi
        done
        if awk -v d="$audio_total_duration" 'BEGIN{exit !(d>0)}'; then
            audio_average_bandwidth=$(awk -v bytes="$audio_total_bytes" -v dur="$audio_total_duration" 'BEGIN {printf "%.0f", (bytes * 8) / dur}')
        fi
    fi
    
    # Check if DASH manifest exists for metadata extraction
    local use_dash_metadata=true
    if [ ! -f "$dash_manifest" ]; then
        log_warn "DASH manifest not found at $dash_manifest"
        log_warn "Will extract metadata directly from source MP4 files and TS segments"
        use_dash_metadata=false
    fi
    
    for res_dir in $res_dirs; do
        local res_name=$(basename "$res_dir")
        local ts_playlist="${res_dir}/playlist.m3u8"
        
        if [ ! -f "$ts_playlist" ]; then
            continue
        fi
        
        local width=""
        local height=""
        local bandwidth=""
        local video_codecs=""
        local audio_codecs="mp4a.40.2"  # Default AAC
        
        if [ "$use_dash_metadata" = true ]; then
            # EXISTING PATH: Extract from DASH manifest
            log "Extracting metadata from DASH manifest for $res_name"
            
            # Get resolution from DASH package init.mp4
            local init_mp4="${dash_manifest_dir}/${res_name}/init.mp4"
            width=$(ffprobe -v error -select_streams v:0 -show_entries stream=width \
                -of default=noprint_wrappers=1:nokey=1 "$init_mp4" 2>/dev/null)
            height=$(ffprobe -v error -select_streams v:0 -show_entries stream=height \
                -of default=noprint_wrappers=1:nokey=1 "$init_mp4" 2>/dev/null)
            
            # Extract bandwidth and codecs for this resolution from DASH manifest
            # Try matching by height first (most reliable)
            local res_height=$(echo "$res_name" | sed 's/[^0-9]//g')
            bandwidth=$(grep "height=\"${res_height}\"" "$dash_manifest" | \
                             sed -n 's/.*bandwidth="\([0-9]*\)".*/\1/p' | head -1)
            video_codecs=$(grep "height=\"${res_height}\"" "$dash_manifest" | \
                                sed -n 's/.*codecs="\([^"]*\)".*/\1/p' | head -1)
            
            # Fallback: try matching by width x height
            if [ -z "$bandwidth" ] && [ -n "$width" ] && [ -n "$height" ]; then
                bandwidth=$(grep "width=\"${width}\" height=\"${height}\"" "$dash_manifest" | \
                           sed -n 's/.*bandwidth="\([0-9]*\)".*/\1/p' | head -1)
                video_codecs=$(grep "width=\"${width}\" height=\"${height}\"" "$dash_manifest" | \
                              sed -n 's/.*codecs="\([^"]*\)".*/\1/p' | head -1)
            fi
            
            # Get audio codecs from DASH manifest
            local audio_codecs_from_dash=$(grep "contentType=\"audio\"" "$dash_manifest" -A 2 | \
                                grep "codecs=" | head -1 | \
                                sed -n 's/.*codecs="\([^"]*\)".*/\1/p')
            if [ -n "$audio_codecs_from_dash" ]; then
                audio_codecs="$audio_codecs_from_dash"
            fi
            
        else
            # NEW FALLBACK PATH: Extract from source MP4 and calculate from TS files
            log "Extracting metadata from source files for $res_name"
            
            # Find source MP4 in temp directory
            local source_mp4=""
            if [ -n "$temp_dir" ] && [ -f "$temp_dir/${codec}_${res_name}.mp4" ]; then
                source_mp4="$temp_dir/${codec}_${res_name}.mp4"
            fi
            
            if [ -z "$source_mp4" ] || [ ! -f "$source_mp4" ]; then
                log_warn "Source MP4 not found for $res_name (expected: $temp_dir/${codec}_${res_name}.mp4), skipping"
                continue
            fi
            
            # Get resolution from source MP4
            width=$(ffprobe -v error -select_streams v:0 -show_entries stream=width \
                -of default=noprint_wrappers=1:nokey=1 "$source_mp4" 2>/dev/null)
            height=$(ffprobe -v error -select_streams v:0 -show_entries stream=height \
                -of default=noprint_wrappers=1:nokey=1 "$source_mp4" 2>/dev/null)
            
            # Get codec profile and level from source MP4
            local profile=$(ffprobe -v error -select_streams v:0 -show_entries stream=profile \
                -of default=noprint_wrappers=1:nokey=1 "$source_mp4" 2>/dev/null | head -1)
            local level=$(ffprobe -v error -select_streams v:0 -show_entries stream=level \
                -of default=noprint_wrappers=1:nokey=1 "$source_mp4" 2>/dev/null | head -1)
            
            # Derive codec string from profile and level
            video_codecs=$(derive_codec_string "$codec" "$profile" "$level")
            
            # Calculate bandwidth from TS segment file sizes
            local total_bytes=0
            local segment_count=0
            for ts_file in "$res_dir"/segment_*.ts; do
                if [ -f "$ts_file" ]; then
                    local file_size=$(stat -c %s "$ts_file" 2>/dev/null || stat -f %z "$ts_file" 2>/dev/null)
                    total_bytes=$((total_bytes + file_size))
                    segment_count=$((segment_count + 1))
                fi
            done
            
            # Get duration from source MP4
            local duration=$(ffprobe -v error -show_entries format=duration \
                -of default=noprint_wrappers=1:nokey=1 "$source_mp4" 2>/dev/null)
            
            # Calculate bandwidth (bits per second)
            if [ -n "$duration" ] && [ "$duration" != "N/A" ] && [ "$total_bytes" -gt 0 ]; then
                bandwidth=$(awk -v bytes="$total_bytes" -v dur="$duration" 'BEGIN {printf "%.0f", bytes * 8 / dur}')
            else
                # Fallback: estimate from segment count and average bitrate
                log_warn "Could not determine duration, estimating bandwidth"
                bandwidth=$((total_bytes * 8 / (segment_count * SEGMENT_DURATION)))
            fi
            
            log "  Resolution: ${width}x${height}"
            log "  Profile/Level: $profile/$level"
            log "  Codec: $video_codecs"
            log "  Bandwidth: $bandwidth bps (calculated from $segment_count segments, $total_bytes bytes)"
        fi
        
        # Validate we have required values
        if [ -z "$bandwidth" ] || [ -z "$width" ] || [ -z "$height" ]; then
            log_warn "Could not extract complete metadata for $res_name, skipping"
            continue
        fi
        
        # Validate video codecs were found
        if [ -z "$video_codecs" ] || [ "$video_codecs" = "unknown" ]; then
            log_warn "Could not extract video codecs for $res_name, skipping"
            continue
        fi
        
        # Measure average video bitrate from generated TS media playlist.
        local video_total_bytes=0
        local video_total_duration=$(awk -F: '/^#EXTINF:/{gsub(/,.*/, "", $2); s+=$2} END{printf "%.6f", s+0}' "$ts_playlist")
        for ts_file in "$res_dir"/segment_*.ts; do
            if [ -f "$ts_file" ]; then
                local file_size=$(stat -c %s "$ts_file" 2>/dev/null || stat -f %z "$ts_file" 2>/dev/null)
                video_total_bytes=$((video_total_bytes + file_size))
            fi
        done
        local video_average_bandwidth="$bandwidth"
        local average_bandwidth="$video_average_bandwidth"
        if awk -v d="$video_total_duration" 'BEGIN{exit !(d>0)}'; then
            video_average_bandwidth=$(awk -v bytes="$video_total_bytes" -v dur="$video_total_duration" 'BEGIN {printf "%.0f", (bytes * 8) / dur}')
            average_bandwidth="$video_average_bandwidth"
        fi
        local video_peak_bandwidth
        video_peak_bandwidth=$(awk -v avg="$video_average_bandwidth" -v pct="$MAXRATE_PERCENT" 'BEGIN {p=(avg*pct/100.0); if (p<avg) p=avg; printf "%.0f", p}')
        bandwidth="$video_peak_bandwidth"
        # Include default audio rendition bitrate in both BANDWIDTH and AVERAGE-BANDWIDTH.
        if [ "$audio_average_bandwidth" -gt 0 ]; then
            bandwidth=$((video_peak_bandwidth + audio_average_bandwidth))
            average_bandwidth=$((average_bandwidth + audio_average_bandwidth))
        fi

        # Write variant entry with actual codecs
        if [ "$SKIP_AUDIO" != "true" ]; then
            echo "#EXT-X-STREAM-INF:BANDWIDTH=${bandwidth},AVERAGE-BANDWIDTH=${average_bandwidth},RESOLUTION=${width}x${height},CODECS=\"${video_codecs},${audio_codecs}\",AUDIO=\"audio\"" >> "$master_file"
        else
            echo "#EXT-X-STREAM-INF:BANDWIDTH=${bandwidth},AVERAGE-BANDWIDTH=${average_bandwidth},RESOLUTION=${width}x${height},CODECS=\"${video_codecs}\"" >> "$master_file"
        fi
        echo "${res_name}/playlist.m3u8" >> "$master_file"
    done
    
    log_success "TS master playlist created"
    ENCODING_INFOS+=("HLS Transport Stream package generated (master.m3u8)")
}

# ============================================================================
# ENCODING REPORT GENERATION (PHASE 3)
# ============================================================================

generate_encoding_report() {
    local output_dir=$1
    local codec=$2
    local report_file="${output_dir}/ENCODING_REPORT.md"
    
    log "Generating encoding report: $report_file"
    
    # Calculate total encoding duration
    local total_encoding_time=$((TOTAL_END - TOTAL_START))
    local total_minutes=$((total_encoding_time / 60))
    local total_seconds=$((total_encoding_time % 60))
    
    # Get current timestamp
    local timestamp=$(date "+%Y-%m-%d %H:%M:%S %Z")
    
    # Get codec-specific variants
    local codec_upper=$(echo $codec | tr '[:lower:]' '[:upper:]')
    
    # Start building report
    cat > "$report_file" << EOF
# Encoding Report

**Generated:** ${timestamp}  
**Input:** $(basename "$INPUT_FILE")  
**Codec:** ${codec_upper}  
**Output Directory:** ${output_dir}

---

## Summary

- **Source Resolution:** ${SOURCE_WIDTH}×${SOURCE_HEIGHT} @ ${SOURCE_FPS_DECIMAL} fps
- **Source Duration:** ${MEZ_DURATION}s
EOF

    # Add max-resolution info if set
    if [[ -n "$MAX_RESOLUTION_HEIGHT" ]]; then
        cat >> "$report_file" << EOF
- **Maximum Resolution:** ${MAX_RESOLUTION_HEIGHT} (user limit)
EOF
    fi

    cat >> "$report_file" << EOF
- **Variants Encoded:** $(echo "${!VARIANT_ENCODE_TIMES[@]}" | tr ' ' '\n' | grep -c "^${codec_upper}_")
- **Total Encoding Time:** ${total_minutes}m ${total_seconds}s
- **Package Size:** $(du -sh "$output_dir" 2>/dev/null | cut -f1 || echo "N/A")
- **Segment Duration:** ${SEGMENT_DURATION}s
- **Partial Duration:** ${PARTIAL_DURATION}s
- **GOP Duration:** ${GOP_DURATION}s
- **GOP Keyint:** ${KEYINT} frames

---

## Variant Performance

| Variant | Resolution | Encode Time | File Size | Speed |
|---------|------------|-------------|-----------|-------|
EOF

    if [[ "$KEEP_MEZZANINE" == "true" ]]; then
        local mezz_dir="${output_dir}/_mezzanine"
        if [[ -d "$mezz_dir" ]]; then
            cat >> "$report_file" << EOF

## Intermediate Files

- **Mezzanine Directory:** ${mezz_dir}

EOF
        fi
    fi
    
    # Add variant rows sorted by resolution
    for key in $(echo "${!VARIANT_ENCODE_TIMES[@]}" | tr ' ' '\n' | grep "^${codec_upper}_" | sort); do
        local res_name=$(echo "$key" | sed "s/${codec_upper}_//")
        local encode_time="${VARIANT_ENCODE_TIMES[$key]}"
        local file_size="${VARIANT_FILE_SIZES[$key]}"
        local speed="${VARIANT_SPEEDS[$key]}"
        
        # Get resolution dimensions from res_name
        local resolution=$(get_resolution_dimensions "$res_name")
        
        echo "| ${res_name} | ${resolution} | ${encode_time}s | ${file_size} | ${speed} |" >> "$report_file"
    done
    
    # Add events section
    cat >> "$report_file" << EOF

---

## Events

### Info ℹ️

EOF
    
    if [ ${#ENCODING_INFOS[@]} -eq 0 ]; then
        echo "- *No info events*" >> "$report_file"
    else
        for info in "${ENCODING_INFOS[@]}"; do
            echo "- $info" >> "$report_file"
        done
    fi
    
    cat >> "$report_file" << EOF

### Warnings ⚠️

EOF
    
    if [ ${#ENCODING_WARNINGS[@]} -eq 0 ]; then
        echo "- *No warnings*" >> "$report_file"
    else
        for warning in "${ENCODING_WARNINGS[@]}"; do
            echo "- $warning" >> "$report_file"
        done
    fi
    
    cat >> "$report_file" << EOF

### Errors ❌

EOF
    
    if [ ${#ENCODING_ERRORS[@]} -eq 0 ]; then
        echo "- *No errors*" >> "$report_file"
    else
        for error in "${ENCODING_ERRORS[@]}"; do
            echo "- $error" >> "$report_file"
        done
    fi
    
    # Add validation section
    cat >> "$report_file" << EOF

---

## Validation

EOF
    
    # Check DASH manifest
    if [ -f "${output_dir}/manifest.mpd" ]; then
        echo "- ✅ DASH manifest created" >> "$report_file"
    else
        echo "- ❌ DASH manifest missing" >> "$report_file"
    fi
    
    # Check HLS manifest
    if [ -f "${output_dir}/master.m3u8" ]; then
        echo "- ✅ HLS master playlist created" >> "$report_file"
    else
        echo "- ⚠️  HLS master playlist missing" >> "$report_file"
    fi
    
    # Count segment files
    local segment_count=$(find "$output_dir" -name "*.m4s" -o -name "*.mp4" 2>/dev/null | wc -l | tr -d ' ')
    echo "- ℹ️  Segment files found: ${segment_count}" >> "$report_file"
    
    # Add HLS TS package info if it exists
    local ts_output_dir=""
    if [[ "$codec" == "hevc" ]] && [[ -n "$OUTPUT_DIR_HEVC_TS" ]]; then
        ts_output_dir="$OUTPUT_DIR_HEVC_TS"
    elif [[ "$codec" == "h264" ]] && [[ -n "$OUTPUT_DIR_H264_TS" ]]; then
        ts_output_dir="$OUTPUT_DIR_H264_TS"
    fi
    
    if [[ -n "$ts_output_dir" ]] && [ -d "$ts_output_dir" ]; then
        cat >> "$report_file" << EOF

---

## HLS Transport Stream Package

EOF
        
        if [ -f "${ts_output_dir}/master.m3u8" ]; then
            local ts_size=$(du -sh "$ts_output_dir" 2>/dev/null | cut -f1 || echo "N/A")
            local ts_segment_count=$(find "$ts_output_dir" -name "*.ts" 2>/dev/null | wc -l | tr -d ' ')
            
            cat >> "$report_file" << EOF
- **Location:** ${ts_output_dir}
- **Package Size:** ${ts_size}
- **TS Segments:** ${ts_segment_count}
- ✅ HLS TS master playlist created
- **URL:** http://localhost:8000/$(basename "$ts_output_dir")/master.m3u8
EOF
        else
            echo "- ⚠️  HLS TS package directory exists but master.m3u8 missing" >> "$report_file"
        fi
    fi
    
    # Add footer
    cat >> "$report_file" << EOF

---

*Report generated by ABR Ladder Generator*
EOF
    
    log_success "Report generated: $report_file"
}

get_resolution_dimensions() {
    local res_name=$1
    case "$res_name" in
        "2160p") echo "3840×2160" ;;
        "1440p") echo "2560×1440" ;;
        "1080p") echo "1920×1080" ;;
        "720p") echo "1280×720" ;;
        "540p") echo "960×540" ;;
        "432p") echo "768×432" ;;
        "360p") echo "640×360" ;;
        *) echo "Unknown" ;;
    esac
}

################################################################################
# Summary & Cleanup
################################################################################

print_summary() {
    print_header "Encoding Complete!"
    
    echo -e "${CYAN}Input:${NC}"
    echo -e "  File: $(basename $INPUT_FILE)"
    echo -e "  Size: $INPUT_SIZE"
    echo ""
    
    echo -e "${CYAN}Source Characteristics:${NC}"
    echo -e "  Resolution: ${SOURCE_WIDTH}×${SOURCE_HEIGHT}"
    echo -e "  Frame Rate: ${SOURCE_FPS_DECIMAL} fps (${SOURCE_FPS})"
    echo -e "  GOP Keyint: ${KEYINT} frames (${GOP_DURATION}s closed GOPs)"
    echo -e "  Tiers: ${#PROFILES[@]} resolutions encoded"
    echo -e "  Audio: $([ "$SKIP_AUDIO" == "true" ] && echo "None (video-only)" || echo "AAC stereo")"
    echo ""
    
    echo -e "${CYAN}Output Packages:${NC}"
    echo ""
    
    if [[ -n "$OUTPUT_DIR_HEVC" ]] && [ -f "$OUTPUT_DIR_HEVC/manifest.mpd" ]; then
        local hevc_size=$(du -sh "$OUTPUT_DIR_HEVC" | cut -f1)
        echo -e "  ${GREEN}HEVC Package (DASH + HLS fMP4):${NC} $OUTPUT_DIR_HEVC ($hevc_size)"
        echo -e "    DASH: http://localhost:8000/$(basename $OUTPUT_DIR_HEVC)/manifest.mpd"
        if [ -f "$OUTPUT_DIR_HEVC/master.m3u8" ]; then
            echo -e "    HLS:  http://localhost:8000/$(basename $OUTPUT_DIR_HEVC)/master.m3u8"
        fi
        echo ""
    fi
    
    if [[ -n "$OUTPUT_DIR_HEVC_TS" ]] && [ -f "$OUTPUT_DIR_HEVC_TS/master.m3u8" ]; then
        local hevc_ts_size=$(du -sh "$OUTPUT_DIR_HEVC_TS" | cut -f1)
        echo -e "  ${GREEN}HEVC Package (HLS TS):${NC} $OUTPUT_DIR_HEVC_TS ($hevc_ts_size)"
        echo -e "    HLS:  http://localhost:8000/$(basename $OUTPUT_DIR_HEVC_TS)/master.m3u8"
        echo ""
    fi
    
    if [[ -n "$OUTPUT_DIR_H264" ]] && [ -f "$OUTPUT_DIR_H264/manifest.mpd" ]; then
        local h264_size=$(du -sh "$OUTPUT_DIR_H264" | cut -f1)
        echo -e "  ${GREEN}H.264 Package (DASH + HLS fMP4):${NC} $OUTPUT_DIR_H264 ($h264_size)"
        echo -e "    DASH: http://localhost:8000/$(basename $OUTPUT_DIR_H264)/manifest.mpd"
        if [ -f "$OUTPUT_DIR_H264/master.m3u8" ]; then
            echo -e "    HLS:  http://localhost:8000/$(basename $OUTPUT_DIR_H264)/master.m3u8"
        fi
        echo ""
    fi
    
    if [[ -n "$OUTPUT_DIR_H264_TS" ]] && [ -f "$OUTPUT_DIR_H264_TS/master.m3u8" ]; then
        local h264_ts_size=$(du -sh "$OUTPUT_DIR_H264_TS" | cut -f1)
        echo -e "  ${GREEN}H.264 Package (HLS TS):${NC} $OUTPUT_DIR_H264_TS ($h264_ts_size)"
        echo -e "    HLS:  http://localhost:8000/$(basename $OUTPUT_DIR_H264_TS)/master.m3u8"
        echo ""
    fi

    if [[ -n "$OUTPUT_DIR_AV1" ]] && [ -f "$OUTPUT_DIR_AV1/manifest.mpd" ]; then
        local av1_size=$(du -sh "$OUTPUT_DIR_AV1" | cut -f1)
        echo -e "  ${GREEN}AV1 Package (DASH + HLS fMP4):${NC} $OUTPUT_DIR_AV1 ($av1_size)"
        echo -e "    DASH: http://localhost:8000/$(basename $OUTPUT_DIR_AV1)/manifest.mpd"
        if [ -f "$OUTPUT_DIR_AV1/master.m3u8" ]; then
            echo -e "    HLS:  http://localhost:8000/$(basename $OUTPUT_DIR_AV1)/master.m3u8"
        fi
        echo ""
    fi
    
    if [[ "$KEEP_MEZZANINE" == "true" ]]; then
        echo -e "${CYAN}Encoded Files (preserved):${NC}"
        echo -e "  $TEMP_DIR"
        echo ""
    fi
    
    echo -e "${CYAN}Log File:${NC}"
    echo -e "  $LOG_FILE"
    echo ""
}

print_resume_summary() {
    print_header "Resume Packaging Complete!"

    echo -e "${CYAN}Resume Source:${NC}"
    echo -e "  Directory: $RESUME_PACKAGE_FROM"
    echo -e "  Codec selection: $CODEC_SELECTION"
    echo -e "  Tiers packaged: ${#PROFILES[@]}"
    echo -e "  Audio: $([ "$SKIP_AUDIO" == "true" ] && echo "None (video-only)" || echo "Included")"
    echo ""

    echo -e "${CYAN}Output Packages:${NC}"
    echo ""

    if [[ -n "$OUTPUT_DIR_HEVC" ]] && [ -f "$OUTPUT_DIR_HEVC/manifest.mpd" ]; then
        local hevc_size=$(du -sh "$OUTPUT_DIR_HEVC" | cut -f1)
        echo -e "  ${GREEN}HEVC Package (DASH + HLS fMP4):${NC} $OUTPUT_DIR_HEVC ($hevc_size)"
        echo -e "    DASH: http://localhost:8000/$(basename "$OUTPUT_DIR_HEVC")/manifest.mpd"
        if [ -f "$OUTPUT_DIR_HEVC/master.m3u8" ]; then
            echo -e "    HLS:  http://localhost:8000/$(basename "$OUTPUT_DIR_HEVC")/master.m3u8"
        fi
        echo ""
    fi

    if [[ -n "$OUTPUT_DIR_HEVC_TS" ]] && [ -f "$OUTPUT_DIR_HEVC_TS/master.m3u8" ]; then
        local hevc_ts_size=$(du -sh "$OUTPUT_DIR_HEVC_TS" | cut -f1)
        echo -e "  ${GREEN}HEVC Package (HLS TS):${NC} $OUTPUT_DIR_HEVC_TS ($hevc_ts_size)"
        echo -e "    HLS:  http://localhost:8000/$(basename "$OUTPUT_DIR_HEVC_TS")/master.m3u8"
        echo ""
    fi

    if [[ -n "$OUTPUT_DIR_H264" ]] && [ -f "$OUTPUT_DIR_H264/manifest.mpd" ]; then
        local h264_size=$(du -sh "$OUTPUT_DIR_H264" | cut -f1)
        echo -e "  ${GREEN}H.264 Package (DASH + HLS fMP4):${NC} $OUTPUT_DIR_H264 ($h264_size)"
        echo -e "    DASH: http://localhost:8000/$(basename "$OUTPUT_DIR_H264")/manifest.mpd"
        if [ -f "$OUTPUT_DIR_H264/master.m3u8" ]; then
            echo -e "    HLS:  http://localhost:8000/$(basename "$OUTPUT_DIR_H264")/master.m3u8"
        fi
        echo ""
    fi

    if [[ -n "$OUTPUT_DIR_H264_TS" ]] && [ -f "$OUTPUT_DIR_H264_TS/master.m3u8" ]; then
        local h264_ts_size=$(du -sh "$OUTPUT_DIR_H264_TS" | cut -f1)
        echo -e "  ${GREEN}H.264 Package (HLS TS):${NC} $OUTPUT_DIR_H264_TS ($h264_ts_size)"
        echo -e "    HLS:  http://localhost:8000/$(basename "$OUTPUT_DIR_H264_TS")/master.m3u8"
        echo ""
    fi

    if [[ -n "$OUTPUT_DIR_AV1" ]] && [ -f "$OUTPUT_DIR_AV1/manifest.mpd" ]; then
        local av1_size=$(du -sh "$OUTPUT_DIR_AV1" | cut -f1)
        echo -e "  ${GREEN}AV1 Package (DASH + HLS fMP4):${NC} $OUTPUT_DIR_AV1 ($av1_size)"
        echo -e "    DASH: http://localhost:8000/$(basename "$OUTPUT_DIR_AV1")/manifest.mpd"
        if [ -f "$OUTPUT_DIR_AV1/master.m3u8" ]; then
            echo -e "    HLS:  http://localhost:8000/$(basename "$OUTPUT_DIR_AV1")/master.m3u8"
        fi
        echo ""
    fi

    echo -e "${CYAN}Log File:${NC}"
    echo -e "  $LOG_FILE"
    echo ""
}

cleanup() {
    if [[ "$RESUME_MODE" == "true" ]]; then
        log "Resume mode: source temp directory retained: $TEMP_DIR"
    else
        log "Keeping encoded files for inspection: $TEMP_DIR"
        log "To remove: rm -rf $TEMP_DIR"
    fi
    
    # Clean up old Shaka Packager temp files (older than 1 day)
    local old_temp_files=$(find "${TEMP_BASE}/encoding" -name "packager-tempfile-*" -type f -mtime +1 2>/dev/null | wc -l | tr -d ' ')
    if [[ "$old_temp_files" -gt 0 ]]; then
        log "Cleaning up $old_temp_files old Shaka Packager temp files..."
        find "${TEMP_BASE}/encoding" -name "packager-tempfile-*" -type f -mtime +1 -delete 2>/dev/null
    fi
}

export_mezzanine_files() {
    if [[ "$KEEP_MEZZANINE" != "true" ]]; then
        return
    fi

    local export_for_codec="$1"
    local output_dir="$2"
    if [[ -z "$output_dir" ]] || [[ ! -d "$output_dir" ]]; then
        return
    fi

    local mezz_dir="${output_dir}/_mezzanine"
    mkdir -p "$mezz_dir"

    if [[ -f "$MEZZANINE" ]]; then
        cp -p "$MEZZANINE" "$mezz_dir/mezzanine.mov" 2>/dev/null || true
    fi

    if [[ -f "$TEMP_DIR/audio.mp4" ]]; then
        cp -p "$TEMP_DIR/audio.mp4" "$mezz_dir/audio.mp4" 2>/dev/null || true
    fi

    for mp4 in "$TEMP_DIR/${export_for_codec}_"*.mp4; do
        if [[ -f "$mp4" ]]; then
            cp -p "$mp4" "$mezz_dir/" 2>/dev/null || true
        fi
    done
}

################################################################################
# Main Execution
################################################################################

main() {
    clear

    TOTAL_START=$(date +%s)

    if [[ "$RESUME_MODE" == "true" ]]; then
        prepare_resume_packaging_context
        derive_output_directories "$RESUME_PACKAGE_FROM"
    else
        # Derive output directories
        derive_output_directories "$INPUT_FILE"

        # Place temp files alongside output by default
        if [[ -n "$TMPDIR_OUTPUT" ]]; then
            if [[ "$TMPDIR_OUTPUT" != /* ]]; then
                TMPDIR_OUTPUT="$PWD/$TMPDIR_OUTPUT"
            fi
            TEMP_BASE="$TMPDIR_OUTPUT"
        else
            TEMP_BASE="${OUTPUT_BASE_DIR}_tmp"
        fi
        TEMP_DIR="${TEMP_BASE}/abr_ladder_$$"
        LOG_FILE="$TEMP_DIR/encoding.log"
        mkdir -p "$TEMP_DIR"
    fi

    print_header "ABR Ladder Generator - Universal Input Support"
    log "Script: $0"
    if [[ "$RESUME_MODE" == "true" ]]; then
        log "Resume source: $RESUME_PACKAGE_FROM"
    else
        log "Input: $INPUT_FILE"
    fi
    log "Codec selection: $CODEC_SELECTION"
    if [[ -n "$BITRATE_OVERRIDE_HEVC" ]]; then
        log "HEVC bitrate overrides: $BITRATE_OVERRIDE_HEVC"
    fi
    if [[ -n "$BITRATE_OVERRIDE_H264" ]]; then
        log "H264 bitrate overrides: $BITRATE_OVERRIDE_H264"
    fi
    if [[ -n "$VMAF_LOOKUP_CSV" ]]; then
        log "Lookup CSV: $VMAF_LOOKUP_CSV"
    else
        log "Lookup CSV: disabled"
    fi
    log "Temp directory: $TEMP_DIR"
    echo ""

    if [[ "$RESUME_MODE" == "false" ]]; then
        # Validate input
        validate_input "$INPUT_FILE"
    fi

    # Check prerequisites
    check_prerequisites

    if [[ "$RESUME_MODE" == "false" ]]; then
        # Execute encode phases
        create_mezzanine
        select_resolution_tiers
        encode_all_variants
        create_audio_mezzanine
    else
        log "Resume mode: skipping mezzanine creation and variant encoding"
        echo ""
    fi
    
    # Package variants based on codec selection
    # Use Shaka Packager (package_dash) for Safari-compatible fMP4 structure
    if [[ "$CODEC_SELECTION" == "both" ]] || [[ "$CODEC_SELECTION" == "all" ]] || [[ "$CODEC_SELECTION" == "hevc" ]]; then
        package_dash "hevc" "$OUTPUT_DIR_HEVC"
    fi
    
    if [[ "$CODEC_SELECTION" == "both" ]] || [[ "$CODEC_SELECTION" == "all" ]] || [[ "$CODEC_SELECTION" == "h264" ]]; then
        package_dash "h264" "$OUTPUT_DIR_H264"
    fi
    
    if [[ "$CODEC_SELECTION" == "all" ]] || [[ "$CODEC_SELECTION" == "av1" ]]; then
        package_dash "av1" "$OUTPUT_DIR_AV1"
    fi

    # Export intermediate mezzanine files if requested
    if [[ "$CODEC_SELECTION" == "both" ]] || [[ "$CODEC_SELECTION" == "all" ]] || [[ "$CODEC_SELECTION" == "hevc" ]]; then
        export_mezzanine_files "hevc" "$OUTPUT_DIR_HEVC"
    fi
    if [[ "$CODEC_SELECTION" == "both" ]] || [[ "$CODEC_SELECTION" == "all" ]] || [[ "$CODEC_SELECTION" == "h264" ]]; then
        export_mezzanine_files "h264" "$OUTPUT_DIR_H264"
    fi
    if [[ "$CODEC_SELECTION" == "all" ]] || [[ "$CODEC_SELECTION" == "av1" ]]; then
        export_mezzanine_files "av1" "$OUTPUT_DIR_AV1"
    fi
    
    # Generate fragment metadata (.byteranges files) for LL-HLS support
    parse_fmp4_fragments
    
    # Generate HLS manifests
    generate_hls_manifests
    
    # Generate HLS Transport Stream segments (if requested)
    if [ "$HLS_FORMAT" = "ts" ] || [ "$HLS_FORMAT" = "both" ]; then
        if [[ "$CODEC_SELECTION" == "both" ]] || [[ "$CODEC_SELECTION" == "all" ]] || [[ "$CODEC_SELECTION" == "hevc" ]]; then
            generate_hls_ts_segments "$TEMP_DIR" "$OUTPUT_DIR_HEVC_TS" "hevc" "$OUTPUT_DIR_HEVC"
        fi
        
        if [[ "$CODEC_SELECTION" == "both" ]] || [[ "$CODEC_SELECTION" == "all" ]] || [[ "$CODEC_SELECTION" == "h264" ]]; then
            generate_hls_ts_segments "$TEMP_DIR" "$OUTPUT_DIR_H264_TS" "h264" "$OUTPUT_DIR_H264"
        fi
    fi
    
    TOTAL_END=$(date +%s)
    TOTAL_DURATION=$((TOTAL_END - TOTAL_START))
    TOTAL_MINUTES=$((TOTAL_DURATION / 60))
    TOTAL_SECONDS=$((TOTAL_DURATION % 60))
    
    if [[ "$RESUME_MODE" == "false" ]]; then
        # Generate encoding reports
        if [[ "$CODEC_SELECTION" == "both" ]] || [[ "$CODEC_SELECTION" == "all" ]] || [[ "$CODEC_SELECTION" == "hevc" ]]; then
            generate_encoding_report "$OUTPUT_DIR_HEVC" "hevc"
        fi
        
        if [[ "$CODEC_SELECTION" == "both" ]] || [[ "$CODEC_SELECTION" == "all" ]] || [[ "$CODEC_SELECTION" == "h264" ]]; then
            generate_encoding_report "$OUTPUT_DIR_H264" "h264"
        fi
        
        if [[ "$CODEC_SELECTION" == "all" ]] || [[ "$CODEC_SELECTION" == "av1" ]]; then
            generate_encoding_report "$OUTPUT_DIR_AV1" "av1"
        fi
        print_summary
    else
        print_resume_summary
    fi
    
    log_success "Total time: ${TOTAL_MINUTES}m ${TOTAL_SECONDS}s"
    echo ""
    
    cleanup
}

# Run main
main "$@"
