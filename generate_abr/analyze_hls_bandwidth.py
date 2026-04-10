#!/usr/bin/env python3
"""
Analyze HLS master/media playlists and report measured average + peak bandwidth.

The script inspects actual media bytes referenced by variant/audio playlists and
computes:
  - average bitrate (bits over media duration)
  - peak bitrate (max overlapping segment-rate sum over timeline)

It compares those measured values with declared BANDWIDTH and AVERAGE-BANDWIDTH
from #EXT-X-STREAM-INF lines in master.m3u8.
"""

from __future__ import annotations

import argparse
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Dict, List, Optional, Tuple


@dataclass
class Chunk:
    start_s: float
    end_s: float
    duration_s: float
    bytes_len: int

    @property
    def bitrate_bps(self) -> float:
        if self.duration_s <= 0:
            return 0.0
        return (self.bytes_len * 8.0) / self.duration_s


@dataclass
class PlaylistStats:
    path: Path
    map_bytes: int
    chunks: List[Chunk]

    @property
    def total_duration_s(self) -> float:
        return sum(c.duration_s for c in self.chunks)

    @property
    def media_bytes(self) -> int:
        return sum(c.bytes_len for c in self.chunks)

    def average_bps(self, include_map: bool = True) -> float:
        total_bytes = self.media_bytes + (self.map_bytes if include_map else 0)
        if self.total_duration_s <= 0:
            return 0.0
        return (total_bytes * 8.0) / self.total_duration_s

    def peak_segment_bps(self) -> float:
        if not self.chunks:
            return 0.0
        return max(c.bitrate_bps for c in self.chunks)


@dataclass
class Variant:
    uri: str
    attrs: Dict[str, str]


def parse_attr_list(raw: str) -> Dict[str, str]:
    out: Dict[str, str] = {}
    key = []
    val = []
    in_key = True
    in_quote = False
    i = 0
    while i < len(raw):
        ch = raw[i]
        if in_key:
            if ch == "=":
                in_key = False
            else:
                key.append(ch)
        else:
            if ch == '"':
                in_quote = not in_quote
                val.append(ch)
            elif ch == "," and not in_quote:
                k = "".join(key).strip()
                v = "".join(val).strip()
                if k:
                    out[k] = strip_quotes(v)
                key, val = [], []
                in_key = True
            else:
                val.append(ch)
        i += 1
    if key:
        k = "".join(key).strip()
        v = "".join(val).strip()
        if k:
            out[k] = strip_quotes(v)
    return out


def strip_quotes(v: str) -> str:
    if len(v) >= 2 and v[0] == '"' and v[-1] == '"':
        return v[1:-1]
    return v


def parse_master(master_path: Path) -> Tuple[List[Variant], Dict[str, str]]:
    lines = master_path.read_text(encoding="utf-8", errors="replace").splitlines()
    variants: List[Variant] = []
    audio_groups: Dict[str, str] = {}

    pending_variant_attrs: Optional[Dict[str, str]] = None
    for raw in lines:
        line = raw.strip()
        if not line:
            continue
        if line.startswith("#EXT-X-MEDIA:"):
            attrs = parse_attr_list(line.split(":", 1)[1])
            if attrs.get("TYPE", "").upper() == "AUDIO":
                group = attrs.get("GROUP-ID")
                uri = attrs.get("URI")
                if group and uri:
                    # Keep first for a given group; usually DEFAULT=YES track.
                    audio_groups.setdefault(group, uri)
            continue
        if line.startswith("#EXT-X-STREAM-INF:"):
            pending_variant_attrs = parse_attr_list(line.split(":", 1)[1])
            continue
        if line.startswith("#"):
            continue
        if pending_variant_attrs is not None:
            variants.append(Variant(uri=line, attrs=pending_variant_attrs))
            pending_variant_attrs = None

    return variants, audio_groups


def parse_byterange(text: str) -> Tuple[int, Optional[int]]:
    parts = text.split("@", 1)
    length = int(parts[0])
    offset = int(parts[1]) if len(parts) == 2 else None
    return length, offset


def resolve_range_size(
    file_path: Path,
    byterange: Optional[str],
    last_range_end_by_uri: Dict[str, int],
) -> int:
    if byterange is None:
        return file_path.stat().st_size
    length, offset = parse_byterange(byterange)
    key = str(file_path)
    if offset is None:
        offset = last_range_end_by_uri.get(key, 0)
    last_range_end_by_uri[key] = offset + length
    return length


def parse_media_playlist(playlist_path: Path) -> PlaylistStats:
    lines = playlist_path.read_text(encoding="utf-8", errors="replace").splitlines()
    base_dir = playlist_path.parent
    map_bytes = 0
    chunks: List[Chunk] = []
    t = 0.0
    pending_extinf: Optional[float] = None
    pending_byterange: Optional[str] = None
    last_range_end_by_uri: Dict[str, int] = {}

    for raw in lines:
        line = raw.strip()
        if not line:
            continue
        if line.startswith("#EXT-X-MAP:"):
            attrs = parse_attr_list(line.split(":", 1)[1])
            uri = attrs.get("URI")
            if uri:
                map_path = base_dir / uri
                if map_path.exists():
                    map_bytes = resolve_range_size(
                        map_path, attrs.get("BYTERANGE"), last_range_end_by_uri
                    )
            continue
        if line.startswith("#EXT-X-BYTERANGE:"):
            pending_byterange = line.split(":", 1)[1].strip()
            continue
        if line.startswith("#EXTINF:"):
            dur_txt = line.split(":", 1)[1].split(",", 1)[0].strip()
            pending_extinf = float(dur_txt)
            continue
        if line.startswith("#EXT-X-PART:"):
            # We intentionally ignore partial segments to avoid double-counting
            # when full EXTINF segments are also present.
            continue
        if line.startswith("#"):
            continue

        if pending_extinf is None:
            continue
        seg_path = base_dir / line
        if not seg_path.exists():
            pending_extinf = None
            pending_byterange = None
            t += 0.0
            continue

        seg_bytes = resolve_range_size(seg_path, pending_byterange, last_range_end_by_uri)
        start = t
        end = t + max(0.0, pending_extinf)
        chunks.append(
            Chunk(start_s=start, end_s=end, duration_s=max(0.0, pending_extinf), bytes_len=seg_bytes)
        )
        t = end
        pending_extinf = None
        pending_byterange = None

    return PlaylistStats(path=playlist_path, map_bytes=map_bytes, chunks=chunks)


def peak_sum_bps(a_chunks: List[Chunk], b_chunks: List[Chunk]) -> float:
    events: List[Tuple[float, int, float]] = []
    for c in a_chunks + b_chunks:
        rate = c.bitrate_bps
        if rate <= 0 or c.end_s <= c.start_s:
            continue
        # end before start at same timestamp
        events.append((c.start_s, 1, rate))
        events.append((c.end_s, 0, -rate))

    if not events:
        return 0.0

    events.sort(key=lambda x: (x[0], x[1]))
    cur = 0.0
    peak = 0.0
    for _, _, delta in events:
        cur += delta
        if cur > peak:
            peak = cur
    return peak


def mbps(v_bps: float) -> float:
    return v_bps / 1_000_000.0


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Measure average + peak bandwidth from an HLS master playlist"
    )
    parser.add_argument(
        "master",
        type=Path,
        help="Path to master.m3u8",
    )
    parser.add_argument(
        "--exclude-map",
        action="store_true",
        help="Exclude EXT-X-MAP init bytes from average bitrate calculations",
    )
    args = parser.parse_args()

    master_path = args.master.expanduser().resolve()
    if not master_path.exists():
        raise SystemExit(f"master playlist not found: {master_path}")

    variants, audio_groups = parse_master(master_path)
    if not variants:
        raise SystemExit(f"no variants found in: {master_path}")

    base_dir = master_path.parent
    print(f"MASTER: {master_path}")
    print(
        "NOTE: measured values are media-payload bitrate from files; "
        "they exclude HTTP/TCP/TLS overhead."
    )
    print()
    print(
        f"{'VARIANT':<10} {'DECLARED':>10} {'DECL_AVG':>10} "
        f"{'MEAS_VIDEO':>11} {'MEAS_V+A':>11} {'PEAK_VIDEO':>11} {'PEAK_V+A':>11}"
    )
    print(
        f"{'-'*10:<10} {'-'*10:>10} {'-'*10:>10} "
        f"{'-'*11:>11} {'-'*11:>11} {'-'*11:>11} {'-'*11:>11}"
    )

    for v in variants:
        video_pl = (base_dir / v.uri).resolve()
        if not video_pl.exists():
            print(f"{v.uri:<10} {'missing':>10}")
            continue
        vstats = parse_media_playlist(video_pl)
        declared = float(v.attrs.get("BANDWIDTH", "0") or 0)
        declared_avg = float(v.attrs.get("AVERAGE-BANDWIDTH", "0") or 0)

        audio_chunks: List[Chunk] = []
        audio_avg_bps = 0.0
        audio_group = v.attrs.get("AUDIO")
        if audio_group and audio_group in audio_groups:
            apl = (base_dir / audio_groups[audio_group]).resolve()
            if apl.exists():
                astats = parse_media_playlist(apl)
                audio_chunks = astats.chunks
                audio_avg_bps = astats.average_bps(include_map=not args.exclude_map)

        video_avg = vstats.average_bps(include_map=not args.exclude_map)
        combined_avg = video_avg + audio_avg_bps
        peak_video = vstats.peak_segment_bps()
        peak_combined = peak_sum_bps(vstats.chunks, audio_chunks)

        variant_label = Path(v.uri).parent.name or v.uri
        print(
            f"{variant_label:<10} "
            f"{mbps(declared):10.3f} "
            f"{mbps(declared_avg):10.3f} "
            f"{mbps(video_avg):11.3f} "
            f"{mbps(combined_avg):11.3f} "
            f"{mbps(peak_video):11.3f} "
            f"{mbps(peak_combined):11.3f}"
        )

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
