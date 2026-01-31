#!/usr/bin/env python3
"""
Check go-live DASH MPDs for moving SegmentList windows and period splits.

Usage:
  python3 tools/mpd_check.py http://localhost:20081/go-live/<content>/manifest.mpd
"""

import hashlib
import re
import sys
import time
import xml.etree.ElementTree as ET
from urllib.error import HTTPError
from urllib.parse import urlencode, urlparse, urlunparse, parse_qsl
from urllib.request import Request, urlopen


SEG_RE = re.compile(r"segment_(\d+)\.(?:m4s|mp4|m4a|cmfv|cmfa|webm|m4v)")
DASH_NS = "urn:mpeg:dash:schema:mpd:2011"


def fetch_text(url):
    req = Request(url, headers={"Cache-Control": "no-cache"})
    try:
        with urlopen(req, timeout=5) as resp:
            data = resp.read().decode("utf-8", errors="replace")
            return data, resp.headers, resp.status
    except HTTPError as err:
        data = err.read().decode("utf-8", errors="replace")
        return data, err.headers, err.code


def with_cache_buster(url):
    parsed = urlparse(url)
    query = dict(parse_qsl(parsed.query))
    query["_ts"] = str(int(time.time() * 1000))
    new_query = urlencode(query)
    return urlunparse(parsed._replace(query=new_query))


def parse_mpd(xml_text):
    root = ET.fromstring(xml_text)
    periods = root.findall(f"{{{DASH_NS}}}Period")

    def is_audio(adaptation):
        content_type = adaptation.attrib.get("contentType", "")
        mime_type = adaptation.attrib.get("mimeType", "")
        return content_type == "audio" or "audio" in mime_type

    def is_video(adaptation):
        content_type = adaptation.attrib.get("contentType", "")
        mime_type = adaptation.attrib.get("mimeType", "")
        return content_type == "video" or "video" in mime_type

    audio_rep = None
    video_rep = None

    for period in periods:
        for adaptation in period.findall(f".//{{{DASH_NS}}}AdaptationSet"):
            reps = adaptation.findall(f".//{{{DASH_NS}}}Representation")
            if not reps:
                continue
            if audio_rep is None and is_audio(adaptation):
                audio_rep = (adaptation, reps[0])
            if video_rep is None and is_video(adaptation):
                video_rep = (adaptation, reps[0])
        if audio_rep and video_rep:
            break

    return periods, audio_rep, video_rep


def extract_ordered_segments(periods, rep_id):
    ordered = []
    per_period = []
    for period in periods:
        period_segments = []
        rep = period.find(
            f".//{{{DASH_NS}}}Representation[@id='{rep_id}']"
        )
        if rep is None:
            per_period.append(period_segments)
            continue
        seg_list = rep.find(f".//{{{DASH_NS}}}SegmentList")
        if seg_list is None:
            per_period.append(period_segments)
            continue
        for seg_url in seg_list.findall(f".//{{{DASH_NS}}}SegmentURL"):
            media = seg_url.attrib.get("media", "")
            match = SEG_RE.search(media)
            if match:
                period_segments.append(int(match.group(1)))
                ordered.append(int(match.group(1)))
        per_period.append(period_segments)
    return ordered, per_period


def summarize_rep(label, rep_id, ordered, per_period, prev_ordered):
    print(f"- {label} rep_id={rep_id}", flush=True)
    if ordered:
        print(f"  ordered: {ordered}", flush=True)
    else:
        print("  ordered: []", flush=True)

    if per_period:
        for idx, segment_list in enumerate(per_period):
            print(f"  period_{idx}: {segment_list}", flush=True)

    if prev_ordered is not None:
        changes = []
        max_len = max(len(prev_ordered), len(ordered))
        for i in range(max_len):
            old = prev_ordered[i] if i < len(prev_ordered) else None
            new = ordered[i] if i < len(ordered) else None
            if old != new:
                changes.append((i, old, new))
        if changes:
            preview = ", ".join(
                f"{idx}:{old}->{new}" for idx, old, new in changes[:12]
            )
            if len(changes) > 12:
                preview += f", ... (+{len(changes) - 12} more)"
            print(f"  changes: {preview}", flush=True)
        else:
            print("  changes: none", flush=True)


def main():
    if len(sys.argv) < 2:
        print("Usage: python3 tools/mpd_check.py <manifest.mpd URL>", flush=True)
        return 2

    mpd_url = sys.argv[1]
    ll_url = None
    if "/go-live/" in mpd_url:
        ll_url = mpd_url.replace("/go-live/", "/go-live/")
    prev_audio = None
    prev_video = None
    prev_hash = None
    prev_ll_hash = None

    while True:
        request_url = with_cache_buster(mpd_url)
        xml_text, headers, status = fetch_text(request_url)
        if status >= 400:
            print(f"MPD: {mpd_url}", flush=True)
            print(f"Status: {status}", flush=True)
            print(f"Body (first 400 chars): {xml_text[:400]}", flush=True)
            time.sleep(1)
            continue
        try:
            periods, audio_rep, video_rep = parse_mpd(xml_text)
        except ET.ParseError as exc:
            print(f"MPD: {mpd_url}", flush=True)
            print(f"Status: {status}", flush=True)
            print(f"Parse error: {exc}", flush=True)
            lines = xml_text.splitlines()
            if hasattr(exc, "position"):
                line_no, col = exc.position
                if 1 <= line_no <= len(lines):
                    bad_line = lines[line_no - 1]
                    print(f"Error line {line_no}:{col}: {bad_line!r}", flush=True)
            print(f"Body (first 400 chars): {xml_text[:400]}", flush=True)
            time.sleep(1)
            continue
        content_hash = hashlib.sha1(xml_text.encode("utf-8")).hexdigest()

        served_by = headers.get("X-Served-By", "unknown")
        print(f"MPD: {mpd_url}", flush=True)
        print(f"Served-By: {served_by}", flush=True)
        if prev_hash is not None:
            print(f"Body changed: {'yes' if content_hash != prev_hash else 'no'}", flush=True)
        prev_hash = content_hash
        print(f"Periods: {len(periods)}", flush=True)
        print(f"Checked at: {time.strftime('%Y-%m-%d %H:%M:%S')}", flush=True)
        print("", flush=True)

        go_audio = None
        go_video = None
        if audio_rep:
            _, rep = audio_rep
            rep_id = rep.attrib.get("id", "")
            ordered, per_period = extract_ordered_segments(periods, rep_id)
            summarize_rep("audio", rep_id, ordered, per_period, prev_audio)
            prev_audio = ordered
            go_audio = ordered
            print("", flush=True)

        if video_rep:
            _, rep = video_rep
            rep_id = rep.attrib.get("id", "")
            ordered, per_period = extract_ordered_segments(periods, rep_id)
            summarize_rep("video", rep_id, ordered, per_period, prev_video)
            prev_video = ordered
            go_video = ordered
            print("", flush=True)

        if ll_url:
            print(f"LL MPD: {ll_url}", flush=True)
            try:
                ll_request = with_cache_buster(ll_url)
                ll_text, ll_headers, ll_status = fetch_text(ll_request)
                if ll_status >= 400:
                    print(f"LL Status: {ll_status}", flush=True)
                    print(f"LL Body (first 400 chars): {ll_text[:400]}", flush=True)
                    time.sleep(1)
                    print("", flush=True)
                    continue

                try:
                    ll_periods, ll_audio, ll_video = parse_mpd(ll_text)
                except ET.ParseError as exc:
                    print(f"LL Parse error: {exc}", flush=True)
                    print(f"LL Body (first 400 chars): {ll_text[:400]}", flush=True)
                    time.sleep(1)
                    print("", flush=True)
                    continue

                ll_hash = hashlib.sha1(ll_text.encode("utf-8")).hexdigest()
                ll_served_by = ll_headers.get("X-Served-By", "unknown")

                print(f"Served-By: {ll_served_by}", flush=True)
                if prev_ll_hash is not None:
                    print(f"Body changed: {'yes' if ll_hash != prev_ll_hash else 'no'}", flush=True)
                prev_ll_hash = ll_hash
                print(f"Periods: {len(ll_periods)}", flush=True)

                ll_audio_list = None
                ll_video_list = None
                if ll_audio:
                    _, rep = ll_audio
                    rep_id = rep.attrib.get("id", "")
                    ordered, per_period = extract_ordered_segments(ll_periods, rep_id)
                    summarize_rep("ll-audio", rep_id, ordered, per_period, None)
                    ll_audio_list = ordered
                if ll_video:
                    _, rep = ll_video
                    rep_id = rep.attrib.get("id", "")
                    ordered, per_period = extract_ordered_segments(ll_periods, rep_id)
                    summarize_rep("ll-video", rep_id, ordered, per_period, None)
                    ll_video_list = ordered

                if go_video is not None and ll_video_list is not None:
                    print("Compare: go-live vs ll-live video", flush=True)
                    diff = diff_segments(go_video, ll_video_list)
                    print(f"  {diff}", flush=True)
                if go_audio is not None and ll_audio_list is not None:
                    print("Compare: go-live vs ll-live audio", flush=True)
                    diff = diff_segments(go_audio, ll_audio_list)
                    print(f"  {diff}", flush=True)
            except Exception as exc:
                print(f"Error fetching LL MPD: {exc}", flush=True)

            print("", flush=True)

        time.sleep(1)


def diff_segments(a, b):
    max_len = max(len(a), len(b))
    changes = []
    for i in range(max_len):
        old = a[i] if i < len(a) else None
        new = b[i] if i < len(b) else None
        if old != new:
            changes.append((i, old, new))
    if not changes:
        return "no differences"
    preview = ", ".join(f"{idx}:{old}->{new}" for idx, old, new in changes[:12])
    if len(changes) > 12:
        preview += f", ... (+{len(changes) - 12} more)"
    return preview


if __name__ == "__main__":
    raise SystemExit(main())
