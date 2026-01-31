#!/usr/bin/env python3
"""
Convert DASH MPD from SegmentTemplate to SegmentList

Usage:
    ./convert_to_segmentlist.py <path_to_manifest.mpd>

This script converts a DASH manifest from using SegmentTemplate (with URL patterns)
to using SegmentList (explicit list of segment URLs).

Benefits of SegmentList:
- Better CDN caching (explicit URLs)
- Easier debugging (see all segments in manifest)
- Some players prefer explicit segment lists
- Works better with static/VOD content
"""

import sys
import xml.etree.ElementTree as ET
import os
import glob
import argparse
from pathlib import Path


def convert_manifest(manifest_path, backup=True):
    """
    Convert MPD manifest from SegmentTemplate to SegmentList

    Args:
        manifest_path: Path to the manifest.mpd file
        backup: Create .bak backup before modifying
    """
    manifest_path = Path(manifest_path)
    output_dir = manifest_path.parent

    if not manifest_path.exists():
        print(f"Error: Manifest not found: {manifest_path}")
        sys.exit(1)

    # Backup original
    if backup:
        backup_path = manifest_path.with_suffix(".mpd.template.bak")
        manifest_path.replace(backup_path)
        print(f"✓ Backup created: {backup_path}")
        # Copy back for modification
        import shutil

        shutil.copy(backup_path, manifest_path)

    # Parse MPD
    ET.register_namespace("", "urn:mpeg:dash:schema:mpd:2011")
    ET.register_namespace("xsi", "http://www.w3.org/2001/XMLSchema-instance")

    try:
        tree = ET.parse(manifest_path)
    except ET.ParseError as e:
        print(f"Error parsing manifest: {e}")
        sys.exit(1)

    root = tree.getroot()
    ns = {"mpd": "urn:mpeg:dash:schema:mpd:2011"}

    conversions = 0
    normalized = 0

    # First pass: Normalize width/height attributes
    # When single-variant, DASH-IOP spec allows dimensions at AdaptationSet level
    # Copy them to Representation level for uniformity
    for adaptation_set in root.findall(".//mpd:AdaptationSet", ns):
        as_width = adaptation_set.get("width")
        as_height = adaptation_set.get("height")

        # Only normalize if AdaptationSet has dimensions
        if as_width or as_height:
            for rep in adaptation_set.findall("mpd:Representation", ns):
                # Check if this is a video representation (skip audio)
                mime_type = rep.get("mimeType", "")
                content_type = adaptation_set.get("contentType", "")
                is_video = "video" in mime_type or content_type == "video"

                if is_video:
                    # Copy width/height from AdaptationSet if not present on Representation
                    rep_width = rep.get("width")
                    rep_height = rep.get("height")

                    if not rep_width and as_width:
                        rep.set("width", as_width)
                        normalized += 1
                        print(
                            f"  ✓ Normalized width for Representation {rep.get('id', 'unknown')}: {as_width}"
                        )

                    if not rep_height and as_height:
                        rep.set("height", as_height)
                        if not (
                            not rep_width and as_width
                        ):  # Only increment if we didn't already count above
                            normalized += 1
                        print(
                            f"  ✓ Normalized height for Representation {rep.get('id', 'unknown')}: {as_height}"
                        )

    # Second pass: Convert SegmentTemplate to SegmentList
    for rep in root.findall(".//mpd:Representation", ns):
        seg_template = rep.find("mpd:SegmentTemplate", ns)

        if seg_template is not None:
            # Get template attributes
            init_seg = seg_template.get("initialization")
            media_template = seg_template.get("media")
            timescale = seg_template.get("timescale")
            start_number = int(seg_template.get("startNumber", "1"))
            duration = seg_template.get("duration")

            # Check for SegmentTimeline (more complex)
            seg_timeline = seg_template.find("mpd:SegmentTimeline", ns)

            if media_template:
                # Resolve media segments by finding actual files
                # Replace template variables with glob pattern
                base_pattern = media_template
                base_pattern = base_pattern.replace("$Number%05d$", "*")
                base_pattern = base_pattern.replace("$Number$", "*")
                base_pattern = base_pattern.replace(
                    "$RepresentationID$", rep.get("id", "*")
                )

                # Find all matching segments
                segment_files = sorted(glob.glob(str(output_dir / base_pattern)))

                if segment_files:
                    print(
                        f"  Converting {rep.get('id', 'unknown')}: {len(segment_files)} segments"
                    )

                    # Remove SegmentTemplate
                    rep.remove(seg_template)

                    # Create SegmentList
                    seg_list = ET.SubElement(rep, "SegmentList")
                    if timescale:
                        seg_list.set("timescale", timescale)
                    if duration:
                        seg_list.set("duration", duration)

                    # Add Initialization (preserving directory structure if present)
                    if init_seg:
                        initialization = ET.SubElement(seg_list, "Initialization")
                        # init_seg already contains path from Shaka Packager (e.g., "1080p/init.mp4")
                        initialization.set("sourceURL", init_seg)

                    # Add SegmentTimeline if it existed
                    if seg_timeline is not None:
                        seg_list.append(seg_timeline)

                    # Add SegmentURLs
                    for seg_file in segment_files:
                        # Preserve directory structure relative to manifest
                        seg_path = Path(seg_file).relative_to(output_dir)
                        seg_url = ET.SubElement(seg_list, "SegmentURL")
                        seg_url.set("media", str(seg_path))

                    conversions += 1
                else:
                    print(f"  Warning: No segments found for pattern: {base_pattern}")

    if conversions == 0:
        print("No SegmentTemplate elements found to convert")
        return

    # Prettify and write updated manifest
    import xml.dom.minidom as minidom

    # Convert to string
    xml_str = ET.tostring(root, encoding="UTF-8")

    # Parse and prettify
    dom = minidom.parseString(xml_str)
    pretty_xml = dom.toprettyxml(indent="  ", encoding="UTF-8")

    # Remove extra blank lines
    lines = [line for line in pretty_xml.decode("UTF-8").split("\n") if line.strip()]

    # Write prettified XML
    with open(str(manifest_path), "w", encoding="UTF-8") as f:
        f.write("\n".join(lines) + "\n")

    if normalized > 0:
        print(
            f"\n✓ Normalized {normalized} representation(s) (copied dimensions from AdaptationSet)"
        )
    print(f"✓ Converted {conversions} representation(s) to SegmentList")
    print(f"✓ Updated manifest: {manifest_path}")


def main():
    parser = argparse.ArgumentParser(
        description="Convert DASH MPD from SegmentTemplate to SegmentList",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog=__doc__,
    )
    parser.add_argument("manifest", help="Path to manifest.mpd file")
    parser.add_argument(
        "--no-backup", action="store_true", help="Skip creating backup file"
    )

    args = parser.parse_args()

    print("DASH Manifest Converter: SegmentTemplate → SegmentList")
    print("=" * 60)

    convert_manifest(args.manifest, backup=not args.no_backup)


if __name__ == "__main__":
    main()
