#!/usr/bin/env python3
"""
Check go-live LL-HLS playlists for window sizing and loop discontinuity.

Usage:
  python3 tools/playlist_check.py http://localhost:8010/go-live/<content>/master.m3u8
"""

import re
import sys
import time
from urllib.parse import urljoin
from urllib.request import urlopen, Request


SEG_RE = re.compile(r"segment_(\d+)\.m4s")


def fetch_text(url):
    req = Request(url, headers={"Cache-Control": "no-cache"})
    with urlopen(req, timeout=5) as resp:
        return resp.read().decode("utf-8", errors="replace")


def parse_master(master_text):
    variants = []
    for line in master_text.splitlines():
        line = line.strip()
        if line and not line.startswith("#"):
            variants.append(line)

        if line.startswith("#EXT-X-MEDIA:") and "TYPE=AUDIO" in line:
            uri = extract_attribute_value(line, "URI")
            if uri:
                variants.append(uri)
    return variants


def extract_attribute_value(line, key):
    search = key + "="
    idx = line.find(search)
    if idx == -1:
        return ""
    rest = line[idx + len(search):]
    if not rest:
        return ""
    if rest[0] == "\"":
        rest = rest[1:]
        end = rest.find("\"")
        return rest[:end] if end != -1 else ""
    end = rest.find(",")
    return rest[:end] if end != -1 else rest


def parse_variant(text):
    lines = text.splitlines()
    discontinuities = [i for i, l in enumerate(lines) if l.strip() == "#EXT-X-DISCONTINUITY"]
    segments = [int(m.group(1)) for m in SEG_RE.finditer(text)]
    ordered_tokens = []
    expect_segment = False
    for line in lines:
        stripped = line.strip()
        if stripped == "#EXT-X-DISCONTINUITY":
            ordered_tokens.append("DIS")
            continue
        if stripped.startswith("#EXTINF:"):
            expect_segment = True
            continue
        if expect_segment:
            match = SEG_RE.search(line)
            if match:
                ordered_tokens.append(int(match.group(1)))
                expect_segment = False
    window = 0.0
    for line in lines:
        if line.startswith("#EXTINF:"):
            dur = line.split(":", 1)[1].split(",", 1)[0]
            try:
                window += float(dur)
            except ValueError:
                pass
    return {
        "segments": segments,
        "unique_segments": sorted(set(segments)),
        "ordered_tokens": ordered_tokens,
        "discontinuities": discontinuities,
        "window_seconds": window,
    }


def summarize_variant(url, info):
    segments = info["segments"]
    unique = info["unique_segments"]
    print(f"- {url}", flush=True)
    print(f"  window_seconds: {info['window_seconds']:.3f}", flush=True)
    print(f"  discontinuities: {len(info['discontinuities'])}", flush=True)

    ordered = info["ordered_tokens"]
    if ordered:
        print(f"  ordered: {ordered}", flush=True)


def main():
    if len(sys.argv) < 2:
        print("Usage: python3 tools/playlist_check.py <master.m3u8 URL>", flush=True)
        return 2

    master_url = sys.argv[1]
    while True:
        master_text = fetch_text(master_url)
        variants = parse_master(master_text)
        if not variants:
            print("No variants found in master playlist.", flush=True)
            return 1

        print(f"Master: {master_url}", flush=True)
        print(f"Variants: {len(variants)}", flush=True)
        print(f"Checked at: {time.strftime('%Y-%m-%d %H:%M:%S')}", flush=True)
        print("", flush=True)

        audio_variant = None
        video_variants = []
        for variant in variants:
            if variant.startswith("audio/") and audio_variant is None:
                audio_variant = variant
            elif not variant.startswith("audio/"):
                video_variants.append(variant)

        targets = []
        if audio_variant:
            targets.append(audio_variant)
        if video_variants:
            targets.append(video_variants[0])

        for variant in targets:
            variant_url = urljoin(master_url, variant)
            try:
                variant_text = fetch_text(variant_url)
            except Exception as exc:
                print(f"- {variant_url}", flush=True)
                print(f"  error: {exc}", flush=True)
                continue
            info = parse_variant(variant_text)
            summarize_variant(variant_url, info)
            print("", flush=True)

        if len(video_variants) > 1:
            baseline_text = None
            try:
                baseline_text = fetch_text(urljoin(master_url, video_variants[0]))
            except Exception:
                baseline_text = None
            if baseline_text is not None:
                baseline_info = parse_variant(baseline_text)
                for variant in video_variants[1:]:
                    variant_url = urljoin(master_url, variant)
                    try:
                        variant_text = fetch_text(variant_url)
                    except Exception as exc:
                        print(f"- {variant_url}", flush=True)
                        print(f"  error: {exc}", flush=True)
                        continue
                    info = parse_variant(variant_text)
                    if info["ordered_tokens"] != baseline_info["ordered_tokens"] or abs(info["window_seconds"] - baseline_info["window_seconds"]) > 0.01:
                        summarize_variant(variant_url, info)
                        print("", flush=True)

        time.sleep(1)


if __name__ == "__main__":
    raise SystemExit(main())
