#!/usr/bin/env python3
"""
Generate HLS manifests from existing DASH segments.
Reuses the same .m4s segments and init.mp4 files.
"""

import os
import sys
import json
from pathlib import Path
import xml.etree.ElementTree as ET


def parse_dash_manifest(mpd_path):
    """Parse DASH manifest to extract segment info."""
    ET.register_namespace("", "urn:mpeg:dash:schema:mpd:2011")
    tree = ET.parse(mpd_path)
    root = tree.getroot()
    ns = {"mpd": "urn:mpeg:dash:schema:mpd:2011"}

    # Get duration
    duration = root.get("mediaPresentationDuration")
    if duration:
        # Parse PT210.279999S format
        duration = duration.replace("PT", "").replace("S", "")
        duration_sec = float(duration)
    else:
        duration_sec = 210.28  # fallback

    representations = []

    for adaptation_set in root.findall(".//mpd:AdaptationSet", ns):
        content_type = adaptation_set.get("contentType", "video")

        # Get AdaptationSet-level width/height (fallback for single-variant case)
        as_width = adaptation_set.get("width")
        as_height = adaptation_set.get("height")

        for rep in adaptation_set.findall(".//mpd:Representation", ns):
            rep_id = rep.get("id")
            bandwidth = int(rep.get("bandwidth", 0))
            codecs = rep.get("codecs", "")
            mime_type = rep.get("mimeType", "")

            # Get resolution for video - check Representation first, fallback to AdaptationSet
            # (Single-variant: dimensions at AdaptationSet level per DASH-IOP spec)
            # (Multi-variant: dimensions at Representation level)
            width = rep.get("width") or as_width
            height = rep.get("height") or as_height

            # Get segment info
            seg_list = rep.find("mpd:SegmentList", ns)
            if seg_list is not None:
                timescale = int(seg_list.get("timescale", 1))

                # Get init segment
                init_elem = seg_list.find("mpd:Initialization", ns)
                init_url = init_elem.get("sourceURL") if init_elem is not None else None

                # Get segment timeline
                timeline = seg_list.find("mpd:SegmentTimeline", ns)
                segments = []

                if timeline is not None:
                    # Parse SegmentTimeline
                    for s_elem in timeline.findall("mpd:S", ns):
                        t = int(s_elem.get("t", 0))
                        d = int(s_elem.get("d", 0))
                        r = int(s_elem.get("r", 0))

                        # Duration in seconds
                        duration_s = d / timescale

                        # Repeat count
                        for _ in range(r + 1):
                            segments.append(
                                {"duration": duration_s, "timeline_duration": d}
                            )

                # Get segment URLs
                seg_urls = []
                for seg_url_elem in seg_list.findall("mpd:SegmentURL", ns):
                    media = seg_url_elem.get("media")
                    if media:
                        seg_urls.append(media)

                # Match segments with URLs
                for i, seg_url in enumerate(seg_urls):
                    if i < len(segments):
                        segments[i]["url"] = seg_url
                    else:
                        segments.append(
                            {
                                "url": seg_url,
                                "duration": 4.0,  # default
                            }
                        )

                representations.append(
                    {
                        "id": rep_id,
                        "content_type": content_type,
                        "bandwidth": bandwidth,
                        "codecs": codecs,
                        "mime_type": mime_type,
                        "width": width,
                        "height": height,
                        "init_segment": init_url,
                        "segments": segments,
                        "timescale": timescale,
                    }
                )

    return {"duration": duration_sec, "representations": representations}


def load_byteranges(segment_path):
    """Load fragment byterange metadata from .byteranges file."""
    byteranges_path = Path(str(segment_path) + ".byteranges")

    if not byteranges_path.exists():
        return None

    try:
        with open(byteranges_path, "r") as f:
            data = json.load(f)
            return data.get("fragments", [])
    except (json.JSONDecodeError, IOError):
        return None


def generate_media_playlist(rep_info, output_path, package_dir):
    """Generate LL-HLS media playlist with EXT-X-PART tags (variant-specific .m3u8)."""

    segments = rep_info["segments"]
    if not segments:
        return

    # Calculate target duration (max segment duration rounded up)
    max_duration = max(seg.get("duration", 4.0) for seg in segments)
    target_duration = int(max_duration) + 1

    # Detect if this is video or audio
    content_type = rep_info.get("content_type", "video")
    is_video = content_type == "video"

    lines = [
        "#EXTM3U",
        "#EXT-X-VERSION:10",  # Version 10 for LL-HLS
        f"#EXT-X-TARGETDURATION:{target_duration}",
        "#EXT-X-PLAYLIST-TYPE:VOD",
        "#EXT-X-INDEPENDENT-SEGMENTS",
    ]

    # Add LL-HLS part info (1-second target for partials)
    lines.append("#EXT-X-PART-INF:PART-TARGET=1.0")

    # Add init segment map
    # Init segment path is already relative to package root (e.g., "1080p/init.mp4")
    # Playlist will be in same folder, so just use filename
    if rep_info["init_segment"]:
        init_path = Path(rep_info["init_segment"])
        # If playlist is in resolution folder, init is just "init.mp4"
        init_filename = init_path.name
        lines.append(f'#EXT-X-MAP:URI="{init_filename}"')

    # Add segments with LL-HLS partials
    for seg in segments:
        duration = seg.get("duration", 4.0)
        url = seg.get("url", "")

        if url:
            # Segment URL like "1080p/segment_00001.m4s"
            # Playlist is in same folder, so just use filename
            seg_path = Path(url)
            seg_filename = seg_path.name

            # Full path to segment file (for loading byteranges)
            segment_full_path = package_dir / url

            # Load fragment metadata
            fragments = load_byteranges(segment_full_path)

            if fragments and len(fragments) > 0:
                # Generate EXT-X-PART tags for each fragment
                for i, fragment in enumerate(fragments):
                    offset = fragment["offset"]
                    length = fragment["length"]
                    independent = "YES" if fragment.get("independent", False) else "NO"

                    # Calculate fragment duration (divide segment duration by fragment count)
                    fragment_duration = duration / len(fragments)

                    # BYTERANGE format: length@offset
                    byterange = f"{length}@{offset}"

                    lines.append(
                        f"#EXT-X-PART:DURATION={fragment_duration:.6f},"
                        f'URI="{seg_filename}",BYTERANGE="{byterange}",INDEPENDENT={independent}'
                    )

                # Add full segment reference after all partials
                lines.append(f"#EXTINF:{duration:.6f},")
                lines.append(seg_filename)
            else:
                # No fragments found, fallback to standard HLS
                lines.append(f"#EXTINF:{duration:.6f},")
                lines.append(seg_filename)

    lines.append("#EXT-X-ENDLIST")

    with open(output_path, "w") as f:
        f.write("\n".join(lines) + "\n")


def get_resolution_name(width, height):
    """Get friendly resolution name."""
    if height:
        return f"{height}p"
    elif width:
        if int(width) == 1920:
            return "1080p"
        elif int(width) == 1280:
            return "720p"
        elif int(width) == 960:
            return "540p"
        elif int(width) == 640:
            return "360p"
        elif int(width) == 2560:
            return "1440p"
        elif int(width) == 3840:
            return "2160p"
    return "unknown"


def generate_master_playlist(dash_info, output_dir, package_name):
    """Generate HLS master playlist."""

    reps = dash_info["representations"]

    # Separate video and audio
    video_reps = [r for r in reps if r["content_type"] == "video"]
    audio_reps = [r for r in reps if r["content_type"] == "audio"]

    lines = [
        "#EXTM3U",
        "#EXT-X-VERSION:7",
    ]

    # Add audio
    if audio_reps:
        lines.append("")
        lines.append("# Audio")
        for audio_rep in audio_reps:
            group_id = "audio"
            name = "Audio"
            language = "en"

            # Audio playlist goes in audio/ subdirectory
            audio_playlist = "audio/playlist.m3u8"

            lines.append(
                f'#EXT-X-MEDIA:TYPE=AUDIO,GROUP-ID="{group_id}",NAME="{name}",LANGUAGE="{language}",AUTOSELECT=YES,DEFAULT=YES,URI="{audio_playlist}"'
            )

            # Generate audio media playlist in audio/ folder
            audio_dir = output_dir / "audio"
            audio_dir.mkdir(exist_ok=True)
            audio_path = audio_dir / "playlist.m3u8"
            generate_media_playlist(audio_rep, audio_path, output_dir)

    # Add video variants
    lines.append("")
    lines.append("# Video variants")

    for video_rep in video_reps:
        bandwidth = video_rep["bandwidth"]
        codecs = video_rep["codecs"]
        width = video_rep["width"]
        height = video_rep["height"]

        # Determine codec type
        codec_name = "HEVC" if codecs.startswith("hvc1") else "AVC"
        res_name = get_resolution_name(width, height)

        # Variant playlist goes in resolution subdirectory
        variant_playlist = f"{res_name.lower()}/playlist.m3u8"

        # Build stream info
        stream_info = f"#EXT-X-STREAM-INF:BANDWIDTH={bandwidth}"

        if width and height:
            stream_info += f",RESOLUTION={width}x{height}"

        stream_info += f',CODECS="{codecs}"'

        if audio_reps:
            stream_info += ',AUDIO="audio"'

        # Add frame rate if available
        stream_info += ",FRAME-RATE=25.000"

        lines.append(stream_info)
        lines.append(variant_playlist)

        # Generate variant media playlist in resolution folder
        variant_dir = output_dir / res_name.lower()
        variant_dir.mkdir(exist_ok=True)
        variant_path = variant_dir / "playlist.m3u8"
        generate_media_playlist(video_rep, variant_path, output_dir)

    # Write master playlist
    master_path = output_dir / "master.m3u8"
    with open(master_path, "w") as f:
        f.write("\n".join(lines) + "\n")

    return master_path


def create_hls_for_dash_package(dash_dir):
    """Create HLS manifests for a DASH package directory."""

    dash_dir = Path(dash_dir)
    manifest_path = dash_dir / "manifest.mpd"

    if not manifest_path.exists():
        print(f"❌ No manifest.mpd found in {dash_dir}")
        return False

    print(f"\n📦 Processing: {dash_dir.name}")
    print(f"   Reading DASH manifest...")

    # Parse DASH manifest
    dash_info = parse_dash_manifest(manifest_path)

    print(f"   Found {len(dash_info['representations'])} representations")

    # Generate HLS master playlist
    print(f"   Generating HLS master playlist...")
    master_path = generate_master_playlist(dash_info, dash_dir, dash_dir.name)

    print(f"   ✅ Created: {master_path.name}")

    # Count media playlists (in subdirectories)
    media_playlists = list(dash_dir.glob("*/playlist.m3u8"))

    for playlist in media_playlists:
        rel_path = playlist.relative_to(dash_dir)
        print(f"   ✅ Created: {rel_path}")

    return True


def main():
    """Main entry point."""

    script_dir = Path(__file__).parent

    # Auto-discover DASH packages (directories ending with _hevc, _h264, or _av1)
    packages = []
    for item in script_dir.iterdir():
        if item.is_dir() and ("_hevc" in item.name or "_h264" in item.name or "_av1" in item.name):
            packages.append(item.name)

    # Sort for consistent ordering
    packages.sort()

    print("=" * 70)
    print("🎬 HLS Manifest Generator")
    print("=" * 70)
    print("\nGenerating HLS manifests from existing DASH segments...")

    if not packages:
        print("\n⚠️  No DASH packages found (*_hevc, *_h264, or *_av1 directories)")
        print("=" * 70)
        return

    success_count = 0

    for package in packages:
        package_dir = script_dir / package

        if not package_dir.exists():
            print(f"\n⚠️  Skipping {package} (directory not found)")
            continue

        if create_hls_for_dash_package(package_dir):
            success_count += 1

    print("\n" + "=" * 70)
    print(
        f"✅ Successfully created HLS manifests for {success_count}/{len(packages)} packages"
    )
    print("=" * 70)

    if success_count > 0:
        print("\n📺 Test HLS streams:")
        for package in packages:
            package_dir = script_dir / package
            if (package_dir / "master.m3u8").exists():
                print(f"   • {package}: http://localhost:8000/{package}/master.m3u8")

        print("\n💡 Test with:")
        print("   - VLC: Media > Open Network Stream")
        print("   - ffplay: ffplay http://localhost:8000/{package}/master.m3u8")
        print("   - Safari/iOS: Open URL directly in browser")
        print("   - HLS.js demo: https://hls-js.netlify.app/demo/")


if __name__ == "__main__":
    main()
