#!/usr/bin/env python3
"""
parse_fmp4_fragments.py - Extract fragment byte ranges from fMP4 segments

Parses fragmented MP4 (fMP4) segments to identify moof+mdat pairs
(fragments) and extracts their byte offsets, lengths, and keyframe info
for LL-HLS byte-range partial segment support.

Usage:
    python3 parse_fmp4_fragments.py segment_00001.m4s
    python3 parse_fmp4_fragments.py --verbose segment_00001.m4s

Output:
    segment_00001.m4s.byteranges (JSON file with fragment metadata)

Format:
    {
        "fragments": [
            {"offset": 0, "length": 150234, "independent": true},
            {"offset": 150234, "length": 148921, "independent": false},
            ...
        ]
    }
"""

import struct
import sys
import json
import os
import argparse
import subprocess

# Global verbose flag (set by command-line argument)
verbose_mode = False


def read_box_header(f):
    """
    Read MP4 box header (size + type).

    MP4 boxes have format:
        4 bytes: size (big-endian uint32)
        4 bytes: type (ASCII fourcc)
        [size-8 bytes: box payload]

    Returns:
        (box_type, box_size, box_offset) or (None, None, None) if EOF
    """
    data = f.read(8)
    if len(data) < 8:
        return None, None, None

    size = struct.unpack(">I", data[:4])[0]
    box_type = data[4:8].decode("ascii", errors="ignore")
    offset = f.tell() - 8

    # Handle extended size (size == 1 means 64-bit size follows)
    if size == 1:
        extended_size_data = f.read(8)
        if len(extended_size_data) < 8:
            return None, None, None
        size = struct.unpack(">Q", extended_size_data)[0]
        # Adjust offset accounting for extended size
        f.seek(offset + 16)  # Skip main header + extended size

    return box_type, size, offset


def read_tfhd_default_flags(f, traf_offset, traf_size):
    """
    Read default_sample_flags from tfhd box within traf.

    tfhd structure:
        [4 bytes] version + flags
        [4 bytes] track_id
        ... optional fields based on flags

    tfhd flags:
        0x000001 = base-data-offset-present
        0x000002 = sample-description-index-present
        0x000008 = default-sample-duration-present
        0x000010 = default-sample-size-present
        0x000020 = default-sample-flags-present  <-- what we need

    Returns:
        int or None: default_sample_flags if present, None otherwise
    """
    saved_pos = f.tell()

    try:
        f.seek(traf_offset + 8)  # Skip traf header
        traf_end = traf_offset + traf_size

        while f.tell() < traf_end:
            box_type, box_size, box_offset = read_box_header(f)

            if (
                box_type is None
                or box_size is None
                or box_offset is None
                or box_size < 8
            ):
                break

            if box_type == "tfhd":
                # Found track fragment header
                version_flags_data = f.read(4)
                if len(version_flags_data) < 4:
                    break

                version_flags = struct.unpack(">I", version_flags_data)[0]
                flags = version_flags & 0xFFFFFF

                # Skip track_id
                track_id_data = f.read(4)
                if len(track_id_data) < 4:
                    break

                # Navigate through optional fields to reach default-sample-flags
                if flags & 0x000001:  # base-data-offset-present
                    f.read(8)  # skip 64-bit offset

                if flags & 0x000002:  # sample-description-index-present
                    f.read(4)

                if flags & 0x000008:  # default-sample-duration-present
                    f.read(4)

                if flags & 0x000010:  # default-sample-size-present
                    f.read(4)

                if flags & 0x000020:  # default-sample-flags-present
                    default_flags_data = f.read(4)
                    if len(default_flags_data) < 4:
                        break
                    default_sample_flags = struct.unpack(">I", default_flags_data)[0]
                    f.seek(saved_pos)
                    return default_sample_flags

                # No default-sample-flags present
                f.seek(saved_pos)
                return None

            # Skip to next box
            f.seek(box_offset + box_size)

    except Exception as e:
        if verbose_mode:
            print(f"Debug: Error reading tfhd: {e}", file=sys.stderr)

    finally:
        f.seek(saved_pos)


def ffprobe_fragment_keyframe(segment_path, start_time, duration):
    """
    Use ffprobe to check if the first video frame in a time window is a keyframe.
    """
    cmd = [
        "ffprobe",
        "-v",
        "error",
        "-select_streams",
        "v:0",
        "-show_frames",
        "-read_intervals",
        f"{start_time}%+{duration}",
        "-show_entries",
        "frame=key_frame,pkt_pts_time",
        "-of",
        "json",
        segment_path,
    ]
    try:
        result = subprocess.run(cmd, capture_output=True, text=True, check=False)
        if result.returncode != 0:
            if verbose_mode:
                print(
                    f"Debug: ffprobe failed ({result.returncode}): {result.stderr.strip()}",
                    file=sys.stderr,
                )
            return False
        data = json.loads(result.stdout or "{}")
        frames = data.get("frames", [])
        if not frames:
            return False
        return frames[0].get("key_frame") == 1
    except Exception as e:
        if verbose_mode:
            print(f"Debug: ffprobe error: {e}", file=sys.stderr)
        return False

    return None


def has_keyframe(f, moof_offset, moof_size, fragment_index, track_type="auto"):
    """
    Check if a fragment (moof box) starts with a keyframe.

    Uses multi-method detection:
    1. Try trun first-sample-flags (primary method - ffmpeg style)
    2. Try trun per-sample-flags for first sample (Shaka Packager style)
    3. Try tfhd default-sample-flags (fallback for frag_duration encoding)
    4. Use position heuristic (first fragment = keyframe)

    Args:
        f: File handle
        moof_offset: Byte offset of moof box
        moof_size: Size of moof box in bytes
        fragment_index: Index of this fragment within segment (0-based)
        track_type: Track type hint ("audio", "video", or "auto")

    Returns:
        bool: True if fragment starts with keyframe, False otherwise
    """
    saved_pos = f.tell()
    detection_method = None

    try:
        # METHOD 1: Check trun first-sample-flags
        f.seek(moof_offset + 8)  # Skip moof header
        end_of_moof = moof_offset + moof_size

        traf_offset = None
        traf_size = None

        # First pass: find traf box
        while f.tell() < end_of_moof:
            box_type, box_size, box_offset = read_box_header(f)

            if (
                box_type is None
                or box_size is None
                or box_offset is None
                or box_size < 8
            ):
                break

            if box_type == "traf":
                traf_offset = box_offset
                traf_size = box_size
                break

            f.seek(box_offset + box_size)

        if traf_offset is None:
            if verbose_mode:
                print(
                    f"Debug: Fragment {fragment_index}: No traf box found",
                    file=sys.stderr,
                )
            f.seek(saved_pos)
            return fragment_index == 0  # Fallback heuristic

        # Second pass: look for trun within traf
        f.seek(traf_offset + 8)
        traf_end = traf_offset + traf_size

        while f.tell() < traf_end:
            trun_type, trun_size, trun_offset = read_box_header(f)

            if (
                trun_type is None
                or trun_size is None
                or trun_offset is None
                or trun_size < 8
            ):
                break

            if trun_type == "trun":
                # Parse trun box
                version_flags_data = f.read(4)
                if len(version_flags_data) < 4:
                    break

                version_flags = struct.unpack(">I", version_flags_data)[0]
                flags = version_flags & 0xFFFFFF

                sample_count_data = f.read(4)
                if len(sample_count_data) < 4:
                    break

                # Check for first-sample-flags-present (0x04)
                if flags & 0x000004:
                    # Skip data-offset if present (0x01)
                    if flags & 0x000001:
                        f.read(4)

                    # Read first_sample_flags
                    first_sample_flags_data = f.read(4)
                    if len(first_sample_flags_data) < 4:
                        break

                    first_sample_flags = struct.unpack(">I", first_sample_flags_data)[0]

                    # Extract sample_depends_on (bits 24-25)
                    # 00 = unknown, 01 = depends on others, 10 = independent (keyframe)
                    sample_depends_on = (first_sample_flags >> 24) & 0x03

                    is_keyframe = sample_depends_on == 2
                    detection_method = "trun-first-sample-flags"

                    if verbose_mode:
                        print(
                            f"Debug: Fragment {fragment_index}: {detection_method} -> independent={is_keyframe}",
                            file=sys.stderr,
                        )

                    f.seek(saved_pos)
                    return is_keyframe

                # METHOD 2: Check trun per-sample-flags (Shaka Packager style)
                # If sample-flags-present (0x200) AND sample-size-present (0x400), read first sample's flags
                # Note: Audio tracks may have 0x200 set but entries only contain sample_size (no flags)
                # Only proceed if BOTH 0x200 and 0x400 are set (video pattern)
                if (flags & 0x000200) and (flags & 0x000400):
                    # Reset to start of trun entries
                    f.seek(
                        trun_offset + 8 + 4 + 4
                    )  # Skip header + version/flags + sample_count

                    # Skip data-offset if present (0x01)
                    if flags & 0x000001:
                        f.read(4)

                    # Now at first sample entry
                    # Entry format depends on flags:
                    # 0x100 = sample-duration-present
                    # 0x200 = sample-flags-present
                    # 0x400 = sample-size-present (NOT 0x002!)
                    # 0x800 = sample-composition-time-offsets-present

                    # For Shaka (flags=0xe01): order is duration, size, flags, composition_offset
                    # But duration comes from tfhd default, so not in trun entries
                    # Actual order: size (0x400), flags (0x200), composition_offset (0x800)

                    # Skip sample-duration if present (0x100)
                    if flags & 0x000100:
                        f.read(4)

                    # Read sample-size if present (0x400)
                    if flags & 0x000400:
                        f.read(4)  # Skip sample-size

                    # Read sample-flags (0x200)
                    first_sample_flags_data = f.read(4)
                    if len(first_sample_flags_data) < 4:
                        break

                    first_sample_flags = struct.unpack(">I", first_sample_flags_data)[0]

                    # sample_flags interpretation:
                    # 0 = sync sample (keyframe)
                    # 0x10000 (65536) = non-sync sample
                    # Can also check sample_depends_on bits (24-25)
                    sample_depends_on = (first_sample_flags >> 24) & 0x03

                    is_keyframe = (first_sample_flags == 0) or (sample_depends_on == 2)
                    detection_method = "trun-per-sample-flags"

                    if verbose_mode:
                        print(
                            f"Debug: Fragment {fragment_index}: {detection_method} -> independent={is_keyframe} (flags=0x{first_sample_flags:x})",
                            file=sys.stderr,
                        )

                    f.seek(saved_pos)
                    return is_keyframe

                # No first-sample-flags or per-sample-flags, continue to next method
                break

            f.seek(trun_offset + trun_size)

        # METHOD 3: Check tfhd default-sample-flags
        if verbose_mode:
            print(
                f"Debug: Fragment {fragment_index}: Trying tfhd default flags...",
                file=sys.stderr,
            )

        default_flags = read_tfhd_default_flags(f, traf_offset, traf_size)

        if default_flags is not None:
            sample_depends_on = (default_flags >> 24) & 0x03

            # For audio tracks, sample_depends_on=0 typically means independent
            # Audio codecs (AAC, Opus, etc.) have all independent frames
            # but often don't set the sample_depends_on bits explicitly
            if track_type == "audio" and sample_depends_on == 0:
                is_keyframe = True
                detection_method = "tfhd-default-sample-flags (audio)"
            else:
                is_keyframe = sample_depends_on == 2
                detection_method = "tfhd-default-sample-flags"

            if verbose_mode:
                print(
                    f"Debug: Fragment {fragment_index}: {detection_method} -> independent={is_keyframe} (flags=0x{default_flags:x}, depends_on={sample_depends_on})",
                    file=sys.stderr,
                )

            f.seek(saved_pos)
            return is_keyframe

        # METHOD 4: Fallback heuristic - assume only first fragment is keyframe
        detection_method = "position-heuristic"
        is_keyframe = fragment_index == 0

        if verbose_mode:
            print(
                f"Debug: Fragment {fragment_index}: {detection_method} -> independent={is_keyframe} (conservative fallback)",
                file=sys.stderr,
            )

        f.seek(saved_pos)
        return is_keyframe

    except Exception as e:
        if verbose_mode:
            print(
                f"Debug: Fragment {fragment_index}: Error during detection: {e}",
                file=sys.stderr,
            )
        f.seek(saved_pos)
        # Conservative fallback
        return fragment_index == 0

    finally:
        f.seek(saved_pos)


def parse_fmp4_fragments(segment_path, track_type="auto"):
    """
    Parse fMP4 segment and extract fragment byte ranges.

    fMP4 structure:
        [ftyp] - file type box (container metadata)
        [styp] - segment type box (optional)
        [sidx] - segment index (optional)
        [moof] - movie fragment box (fragment 1 metadata)
        [mdat] - media data box (fragment 1 data)
        [moof] - movie fragment box (fragment 2 metadata)
        [mdat] - media data box (fragment 2 data)
        ...

    For LL-HLS with 1.0s GOP and 6s segments:
        Each segment contains 6 [moof+mdat] pairs
        Each pair = 1 partial (1.0s fragment)

    Args:
        segment_path: Path to the fMP4 segment file
        track_type: Track type hint ("audio", "video", or "auto")

    Returns:
        List of dicts: [{"offset": int, "length": int, "independent": bool}, ...]
    """
    fragments = []

    with open(segment_path, "rb") as f:
        # Process all boxes in file
        while True:
            box_type, box_size, box_offset = read_box_header(f)

            if box_type is None or box_size is None or box_offset is None:
                # EOF reached
                break

            if box_size < 8:
                # Invalid box size
                print(
                    f"Warning: Invalid box size {box_size} for type '{box_type}' at offset {box_offset}",
                    file=sys.stderr,
                )
                break

            if box_type in ["ftyp", "styp", "sidx"]:
                # Container metadata - skip
                f.seek(box_offset + box_size)
                continue

            if box_type == "moof":
                # Found fragment start (movie fragment box)
                moof_offset = box_offset
                moof_size = box_size

                # Seek to next box (should be mdat)
                f.seek(box_offset + box_size)

                mdat_type, mdat_size, mdat_offset = read_box_header(f)

                if (
                    mdat_type == "mdat"
                    and mdat_size is not None
                    and mdat_offset is not None
                ):
                    # Fragment = moof + mdat
                    fragment_offset = moof_offset
                    fragment_length = moof_size + mdat_size

                    # Check if this fragment has a keyframe
                    fragment_index = len(fragments)  # Current fragment number (0-based)
                    is_keyframe = has_keyframe(
                        f, moof_offset, moof_size, fragment_index, track_type
                    )

                    fragments.append(
                        {
                            "offset": fragment_offset,
                            "length": fragment_length,
                            "independent": is_keyframe,
                        }
                    )

                    # Seek to next box
                    f.seek(mdat_offset + mdat_size)
                else:
                    # Unexpected structure - moof not followed by mdat
                    print(
                        f"Warning: moof at {moof_offset} not followed by mdat (found '{mdat_type}')",
                        file=sys.stderr,
                    )
                    f.seek(moof_offset + moof_size)
            else:
                # Unknown/unexpected box type - skip
                f.seek(box_offset + box_size)

    return fragments


def main():
    global verbose_mode

    parser = argparse.ArgumentParser(
        description="Extract fragment byte ranges from fMP4 segment file"
    )
    parser.add_argument("segment", help="Path to fMP4 segment file (.m4s)")
    parser.add_argument(
        "-v", "--verbose", action="store_true", help="Enable verbose debug logging"
    )
    parser.add_argument(
        "--track-type",
        choices=["audio", "video", "auto"],
        default="auto",
        help="Track type (audio/video/auto). For audio, treats sample_depends_on=0 as independent.",
    )
    parser.add_argument(
        "--segment-duration",
        type=float,
        default=None,
        help="Segment duration in seconds (used for GOP heuristic if keyframe flags are missing).",
    )
    parser.add_argument(
        "--gop-duration",
        type=float,
        default=None,
        help="GOP/keyframe duration in seconds (used for GOP heuristic if keyframe flags are missing).",
    )
    parser.add_argument(
        "--ffprobe-check",
        action="store_true",
        help="Use ffprobe to check if each fragment starts with a keyframe.",
    )

    args = parser.parse_args()

    verbose_mode = args.verbose
    segment_path = args.segment
    track_type = args.track_type

    if not os.path.exists(segment_path):
        print(f"Error: File not found: {segment_path}", file=sys.stderr)
        sys.exit(1)

    # Parse fragments
    try:
        fragments = parse_fmp4_fragments(segment_path, track_type)
    except Exception as e:
        print(f"Error: Failed to parse {segment_path}: {e}", file=sys.stderr)
        sys.exit(1)

    if not fragments:
        print(f"Warning: No fragments found in {segment_path}", file=sys.stderr)
    else:
        has_independent = any(f.get("independent", False) for f in fragments)
        if (
            track_type == "video"
            and not has_independent
            and args.segment_duration
            and args.gop_duration
        ):
            parts_per_gop = int(
                round(args.gop_duration / (args.segment_duration / len(fragments)))
            )
            if parts_per_gop < 1:
                parts_per_gop = 1
            for i, frag in enumerate(fragments):
                frag["independent"] = (i % parts_per_gop) == 0
            if verbose_mode:
                print(
                    f"Debug: Applied GOP heuristic (every {parts_per_gop} fragments)",
                    file=sys.stderr,
                )

        if args.ffprobe_check and args.segment_duration:
            fragment_duration = args.segment_duration / len(fragments)
            print("ffprobe keyframe check:", file=sys.stderr)
            for idx in range(len(fragments)):
                start_time = idx * fragment_duration
                is_keyframe = ffprobe_fragment_keyframe(
                    segment_path, start_time, fragment_duration
                )
                marker = "YES" if is_keyframe else "NO"
                print(
                    f"  fragment {idx:02d}: start={start_time:.3f}s keyframe={marker}",
                    file=sys.stderr,
                )
        elif args.ffprobe_check:
            print(
                "ffprobe check skipped (segment duration not provided)",
                file=sys.stderr,
            )

    # Output to JSON file
    output_path = segment_path + ".byteranges"
    output_data = {"fragments": fragments}
    with open(output_path, "w") as f:
        json.dump(output_data, f, indent=2)

    # Print summary
    print(f"Extracted {len(fragments)} fragments from {os.path.basename(segment_path)}")
    keyframe_count = sum(1 for f in fragments if f.get("independent", False))
    print(f"Independent fragments (keyframes): {keyframe_count}/{len(fragments)}")
    print(f"Metadata saved to: {os.path.basename(output_path)}")

    # Verbose output: show all fragments
    if verbose_mode and fragments:
        print("\nFragment details:")
        for i, frag in enumerate(fragments):
            size_kb = frag["length"] / 1024
            kf_marker = "🔑" if frag.get("independent", False) else "  "
            print(
                f"  {kf_marker} Fragment {i}: offset={frag['offset']}, length={frag['length']} ({size_kb:.1f} KB), independent={frag['independent']}"
            )
    elif fragments and len(fragments) <= 20:
        # Print details for small fragment counts (non-verbose)
        for i, frag in enumerate(fragments):
            size_kb = frag["length"] / 1024
            kf_marker = "🔑" if frag.get("independent", False) else "  "
            print(
                f"  {kf_marker} Fragment {i}: offset={frag['offset']}, length={frag['length']} ({size_kb:.1f} KB)"
            )
    elif fragments:
        # Just print first 3 for large counts
        print(f"  First 3 fragments:")
        for i in range(min(3, len(fragments))):
            frag = fragments[i]
            size_kb = frag["length"] / 1024
            kf_marker = "🔑" if frag.get("independent", False) else "  "
            print(
                f"    {kf_marker} Fragment {i}: offset={frag['offset']}, length={frag['length']} ({size_kb:.1f} KB)"
            )
        print(f"  ... and {len(fragments) - 3} more")


if __name__ == "__main__":
    main()
