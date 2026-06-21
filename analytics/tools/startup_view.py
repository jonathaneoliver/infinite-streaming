#!/usr/bin/env python3
"""startup_view.py — repeatable client-side startup timeline for one play.

Renders the per-segment delivery view we kept rebuilding by hand:

    t(s)   stream    seg     KB    dur     rate       ttfb
    0.00   aud init    -      1   0.10s   0.06Mbps    104
    0.09   v360 init   -      1   0.21s   0.03Mbps    193
    2.67   aud       00034  146   2.67s   0.45Mbps      3
    ...

WHY THIS TOOL
-------------
The delivery dur/rate/ttfb here are *real client-side* numbers — they come
from iOS AVMetrics `AVMetricHLSMediaSegmentRequestEvent` rows, whose
`raw_json` carries app-computed `derived_*` fields (bytes / transfer_ms /
ttfb_ms / mbps) measured from `networkTransactionMetrics`. They are NOT
go-proxy's `total_ms`, which only measures proxy<->local-upstream and reads
sub-millisecond on a sim. See:
  .claude/.../memory/reference_avmetric_byterange_zero.md
  .claude/.../memory/reference_network_bitrate_responsiveness.md

The archive holds these rows for ~30 days, so this works for any past play
from any shell with no device attached — that's the repeatability win over
grepping an ephemeral device log.

The exact server segment number (#00034) is NOT in the AVMetric event; it is
recovered best-effort by zipping per-stream against go-proxy `network` rows
(which carry the segment URL). Pass --no-seq to skip the join.

UUID case: iOS emits UPPERCASE play_ids; the harness parses case-insensitively
and we lowercase before display, so the [a-f0-9]-grep bug can't recur.
"""

import argparse
import json
import os
import re
import subprocess
import sys

HARNESS = os.environ.get("HARNESS_BIN", "harness")
# Default base dodges the test-dev cert SAN mismatch (cert is for
# dev.jeoliver.com, not the .local host the harness defaults to).
DEFAULT_BASE = os.environ.get("HARNESS_BASE_URL", "https://dev.jeoliver.com:21000")

SEG_EVENT = "AVMetricHLSMediaSegmentRequestEvent"
SEG_NUM_RE = re.compile(r"(?:segment[_-]?|_)0*(\d+)\.(?:m4s|ts|mp4)", re.I)
VARIANT_RE = re.compile(r"(\d+)p", re.I)


def run_harness(base, insecure, args):
    cmd = [HARNESS, "--base", base, "--json"]
    if insecure:
        cmd.append("--insecure")
    cmd += args
    out = subprocess.run(cmd, capture_output=True, text=True)
    if out.returncode != 0:
        sys.stderr.write(out.stderr or out.stdout)
        sys.exit(out.returncode)
    rows = []
    for line in out.stdout.splitlines():
        line = line.strip()
        if not line:
            continue
        obj = json.loads(line)
        # bounded reads wrap rows in {"items":[...]}; SSE-ish reads are NDJSON
        rows.extend(obj["items"] if isinstance(obj, dict) and "items" in obj else [obj])
    return rows


def stream_label(url):
    """playlist_6s_360p.m3u8 -> v360 ; .../audio/... -> aud."""
    if not url:
        return "?"
    if "audio" in url.lower():
        return "aud"
    m = VARIANT_RE.search(url)
    return f"v{m.group(1)}" if m else "vid"


def fnum(d, k):
    try:
        return float(d[k])
    except (KeyError, TypeError, ValueError):
        return None


def fetch_segments(base, insecure, play_id, limit):
    rows = run_harness(base, insecure, [
        "query", "avmetrics", play_id, "--event-type", SEG_EVENT, "--limit", str(limit),
    ])
    segs = []
    for r in rows:
        rj = json.loads(r.get("raw_json") or "{}")
        ts = r.get("event_ts_ms")
        segs.append({
            "ts_ms": int(ts) if ts is not None else None,
            "stream": stream_label(rj.get("indexFileURL")),
            "is_map": rj.get("isMapSegment") == "1",
            "kb": (fnum(rj, "derived_bytes") or 0) / 1024.0,
            "dur_ms": fnum(rj, "derived_transfer_ms"),
            "mbps": fnum(rj, "derived_mbps"),
            "ttfb_ms": fnum(rj, "derived_ttfb_ms"),
            "seg_dur": fnum(rj, "segmentDuration"),
            "from_cache": rj.get("derived_from_cache") == "1",
        })
    segs = [s for s in segs if s["ts_ms"] is not None]
    segs.sort(key=lambda s: s["ts_ms"])
    return segs


def attach_seq(base, insecure, play_id, segs):
    """Best-effort: zip per-stream avmetric segments against network-row
    segment URLs (time-ordered) to recover the server segment number."""
    net = run_harness(base, insecure, ["query", "network", play_id, "--limit", "2000"])
    by_stream = {}
    for r in net:
        if r.get("request_kind") not in ("segment", "partial"):
            continue
        m = SEG_NUM_RE.search(r.get("url") or "")
        if not m:
            continue
        by_stream.setdefault(stream_label(r.get("url")), []).append(
            (r.get("ts"), int(m.group(1)))
        )
    for v in by_stream.values():
        v.sort()
    cursor = {k: 0 for k in by_stream}
    for s in segs:
        if s["is_map"]:
            continue
        lst = by_stream.get(s["stream"])
        i = cursor.get(s["stream"], 0)
        if lst and i < len(lst):
            s["seg"] = lst[i][1]
            cursor[s["stream"]] = i + 1


def marker_ms(base, insecure, play_id, until):
    """Epoch-ms of the startup-complete marker, or None if not found.

      keepup  — AVMetricPlayerItemInitialLikelyToKeepUpEvent (engine predicts
                it can play through). The canonical "startup done" signal.
      playing — first RateChange to rate=1 (playhead actually starts moving).
                Coincides with keepup on a clean start but is the ground-truth
                "playing" signal when they diverge.
    """
    if until == "keepup":
        rows = run_harness(base, insecure, [
            "query", "avmetrics", play_id,
            "--event-type", "AVMetricPlayerItemInitialLikelyToKeepUpEvent", "--limit", "10",
        ])
        ts = [int(r["event_ts_ms"]) for r in rows if r.get("event_ts_ms")]
        return min(ts) if ts else None
    if until == "playing":
        rows = run_harness(base, insecure, [
            "query", "avmetrics", play_id,
            "--event-type", "AVMetricPlayerItemRateChangeEvent", "--limit", "50",
        ])
        play = [int(r["event_ts_ms"]) for r in rows
                if r.get("event_ts_ms") and json.loads(r.get("raw_json") or "{}").get("rate") == "1"]
        return min(play) if play else None
    return None


def startup_marks(base, insecure, play_id):
    """TTFF + videoWillKeepUp as (seconds_from_start, label) from player_metrics.

      TTFF            — video_first_frame_time_ms (first frame rendered)
      videoWillKeepUp — video_start_time_ms (playback start ≈ likely-to-keep-up)

    Both fields are already relative to the play's start, so they drop straight
    onto the segment timeline's t-axis (which is anchored at the first request).
    """
    rows = run_harness(base, insecure, ["query", "events", play_id, "--limit", "1000"])
    pms = [r["player_metrics"] for r in rows if isinstance(r, dict) and r.get("player_metrics")]

    def first_nonzero(field):
        for m in pms:
            if m.get(field):
                return m[field]
        return None

    marks = []
    ttff = first_nonzero("video_first_frame_time_ms")
    keep = first_nonzero("video_start_time_ms")
    if ttff:
        marks.append((ttff / 1000.0, ">>> TTFF (first frame)"))
    if keep:
        marks.append((keep / 1000.0, ">>> videoWillKeepUp (video-start)"))
    return marks


def col(v, width, prec=None, dash="-"):
    """Right-justified fixed-width cell; `dash` when the value is missing."""
    if v is None:
        return f"{dash:>{width}}"
    return f"{v:>{width}.{prec}f}" if prec is not None else f"{v:>{width}d}"


def main():
    ap = argparse.ArgumentParser(description="Client-side startup timeline for one play.")
    ap.add_argument("play_id")
    ap.add_argument("--base", default=DEFAULT_BASE, help=f"harness base URL (default {DEFAULT_BASE})")
    ap.add_argument("--insecure", action="store_true", help="skip TLS verify (self-signed test-dev)")
    ap.add_argument("--until", choices=["keepup", "playing", "time"], default="keepup",
                    help="stop boundary: keepup=likely-to-keep-up (default), "
                         "playing=first rate=1, time=use --window")
    ap.add_argument("--window", type=float, default=25.0,
                    help="seconds to show when --until=time, or fallback if the marker is absent (default 25)")
    ap.add_argument("--vseg", type=int, default=None, metavar="N",
                    help="stop after the Nth video segment completes (overrides --until); "
                         "interleaves TTFF + videoWillKeepUp markers")
    ap.add_argument("--all", action="store_true", help="whole play, not just startup")
    ap.add_argument("--limit", type=int, default=600, help="max avmetric events to scan")
    ap.add_argument("--no-seq", action="store_true", help="skip the network join for segment numbers")
    ap.add_argument("--verbose", action="store_true", help="extra columns: seg-duration, from-cache")
    ap.add_argument("--chunks", action="store_true",
                    help="intra-segment chunk view: LL-HLS partials from the network log, "
                         "or a LocalProxy device-log capture via --log")
    ap.add_argument("--log", help="path to a captured LocalProxy device log (for --chunks)")
    ap.add_argument("--json", action="store_true", help="emit rows as JSON instead of a table")
    args = ap.parse_args()

    if args.chunks:
        render_chunks(args)
        return

    play_id = args.play_id.lower()
    segs = fetch_segments(args.base, args.insecure, play_id, args.limit)
    if not segs:
        sys.stderr.write(f"no {SEG_EVENT} rows for {play_id}\n")
        sys.exit(2)
    if not args.no_seq:
        attach_seq(args.base, args.insecure, play_id, segs)

    t0 = segs[0]["ts_ms"]
    for s in segs:
        s["t"] = (s["ts_ms"] - t0) / 1000.0

    # TTFF + videoWillKeepUp markers, interleaved into the timeline below.
    marks = startup_marks(args.base, args.insecure, play_id)

    cutoff, boundary = None, None
    if args.vseg:
        # Boundary = the Nth *video* segment completing: keep every row (incl. the
        # segment's partials) up to where the (N+1)th distinct video segment begins.
        seen = []
        for s in segs:
            if s["stream"].startswith("v") and not s["is_map"] and s.get("seg") not in seen:
                seen.append(s.get("seg"))
                if len(seen) > args.vseg:
                    cutoff = s["t"]
                    break
        if cutoff is None:
            cutoff = segs[-1]["t"] + 0.001
        segs = [s for s in segs if s["t"] < cutoff]
        last_v = max((s["t"] for s in segs if s["stream"].startswith("v") and not s["is_map"]),
                     default=cutoff)
        boundary = f"video segment {args.vseg} complete @ {last_v:.2f}s"
    elif not args.all:
        if args.until in ("keepup", "playing"):
            ms = marker_ms(args.base, args.insecure, play_id, args.until)
            if ms is not None:
                cutoff = (ms - t0) / 1000.0
                boundary = f"{args.until} @ {cutoff:.2f}s"
            else:
                cutoff, boundary = args.window, f"{args.until} not found — fell back to --window {args.window}s"
        else:
            cutoff, boundary = args.window, f"window {args.window}s"
        segs = [s for s in segs if s["t"] <= cutoff]

    if args.json:
        print(json.dumps(segs, indent=2))
        return

    if boundary:
        print(f"# startup → {boundary}")
    # Units live in the headers so the numeric cells line up cleanly.
    hdr = f'{"t(s)":>6}  {"stream":<9}  {"seg":>5}  {"KB":>6}  {"dur(s)":>7}  {"Mbps":>6}  {"ttfb(ms)":>8}'
    if args.verbose:
        hdr += f'  {"segdur":>7}  {"cache":>5}'
    print(hdr)
    print("# each row = one segment-request completion at t (status col); "
          "seg N.k = kth 2s sub-segment byte-ranged into 6s container N")
    # The 2s playlist byte-ranges into 6s .m4s files, so consecutive 2s segments
    # share a container number — sub-index them .1/.2/.3 so each completion is distinct.
    total = {}
    for s in segs:
        if not s["is_map"] and s.get("seg") is not None:
            total[(s["stream"], s["seg"])] = total.get((s["stream"], s["seg"]), 0) + 1
    occ = {}
    timeline = []
    for s in segs:
        stream = s["stream"] + (" init" if s["is_map"] else "")
        if s["is_map"] or s.get("seg") is None:
            seg_cell = "-"
        else:
            key = (s["stream"], s["seg"])
            occ[key] = occ.get(key, 0) + 1
            seg_cell = f'{s["seg"]}.{occ[key]}' if total[key] > 1 else str(s["seg"])
        if s["is_map"]:
            status = "init"
        elif s["kb"] > 0:
            status = "✓complete"
        elif s["from_cache"]:
            status = "✓cache"
        else:
            status = "·no-bytes"
        dur_s = s["dur_ms"] / 1000.0 if s["dur_ms"] is not None else None
        line = (
            f'{s["t"]:>6.2f}  {stream:<9}  {seg_cell:>5}  {col(s["kb"], 6, 0)}  '
            f'{col(dur_s, 7, 2)}  {col(s["mbps"], 6, 2)}  {col(s["ttfb_ms"], 8, 0)}'
        )
        if args.verbose:
            line += f'  {col(s["seg_dur"], 7, 2)}  {"yes" if s["from_cache"] else "no":>5}'
        line += f"  {status}"
        timeline.append((s["t"], line))
    for mt, ml in marks:
        if cutoff is None or mt <= cutoff + 0.5:
            timeline.append((mt, f"{mt:>6.2f}  {ml}"))
    timeline.sort(key=lambda x: x[0])
    for _, line in timeline:
        print(line)


# LocalProxy log line shapes (smashing-811-knobs LocalHTTPProxy.swift):
#   [NETCHUNK] <parent/seg.m4s> +<n>B cum=<bytes> t=<ms>ms  player_id=… play_id=…
#   [NETBYTES] <parent/seg.m4s> bytes=<n> dur=<s>s rate=<Mbps>Mbps ttfb=<ms>ms  player_id=… play_id=…
NETCHUNK_RE = re.compile(
    r"\[NETCHUNK\]\s+(?P<seg>\S+)\s+\+(?P<delta>\d+)B\s+cum=(?P<cum>\d+)\s+t=(?P<t>[\d.]+)ms")
NETBYTES_RE = re.compile(
    r"\[NETBYTES\]\s+(?P<seg>\S+)\s+bytes=(?P<bytes>\d+)\s+dur=(?P<dur>[\d.]+)s"
    r"\s+rate=(?P<rate>[\d.]+)Mbps\s+ttfb=(?P<ttfb>-?[\d.]+)ms")
PLAYID_RE = re.compile(r"play_id=([0-9A-Fa-f-]{36})")
# Leading wall-clock stamp emitted by `log show`/`log stream` (with or without tz).
LOGTS_RE = re.compile(r"^(\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}\.\d+)(?:\s*([+-]\d{4}))?")


def log_epoch(line):
    """Epoch seconds from a `log` line's leading timestamp, or None."""
    from datetime import datetime
    m = LOGTS_RE.match(line)
    if not m:
        return None
    stamp, tz = m.group(1).replace("T", " "), m.group(2)
    try:
        if tz:
            return datetime.strptime(f"{stamp}{tz}", "%Y-%m-%d %H:%M:%S.%f%z").timestamp()
        # No tz → treat as UTC (matches the keep-up epoch, also UTC).
        from datetime import timezone
        return datetime.strptime(stamp, "%Y-%m-%d %H:%M:%S.%f").replace(tzinfo=timezone.utc).timestamp()
    except ValueError:
        return None


def parse_localproxy_log(path, play_id):
    """Walk a LocalProxy capture; return per-segment records (NETBYTES summary +
    the NETCHUNK arrivals that preceded it) for the matching play_id, in start
    order. A NETBYTES line closes a segment instance; chunks accumulate per
    seg-label so loop repeats (segment_00039 each loop) stay distinct."""
    pending = {}   # seg-label -> list of chunk dicts since its last NETBYTES
    segments = []
    for raw in open(path):
        line = raw.rstrip("\n")
        pid = PLAYID_RE.search(line)
        if not pid or pid.group(1).lower() != play_id:
            continue
        epoch = log_epoch(line)
        mc = NETCHUNK_RE.search(line)
        if mc:
            pending.setdefault(mc["seg"], []).append({
                "epoch": epoch, "delta": int(mc["delta"]),
                "cum": int(mc["cum"]), "t_ms": float(mc["t"]),
            })
            continue
        mb = NETBYTES_RE.search(line)
        if mb:
            chunks = pending.pop(mb["seg"], [])
            start = next((c["epoch"] for c in chunks if c["epoch"] is not None), epoch)
            segments.append({
                "seg": mb["seg"], "bytes": int(mb["bytes"]), "dur": float(mb["dur"]),
                "rate": float(mb["rate"]), "ttfb": float(mb["ttfb"]),
                "start": start, "chunks": chunks,
            })
    segments.sort(key=lambda s: (s["start"] is None, s["start"] or 0))
    return segments


def render_chunks(args):
    """Intra-segment chunk view. Preferred source is a LocalProxy capture
    (--log) carrying real per-chunk [NETCHUNK] arrivals; otherwise fall back to
    LL-HLS `partial` network rows, the finest archived granularity."""
    if args.log:
        render_chunks_from_log(args)
        return
    render_chunks_from_network(args)


def render_chunks_from_log(args):
    play_id = args.play_id.lower()
    segs = parse_localproxy_log(args.log, play_id)
    if not segs:
        any_chunk = any("[NETCHUNK]" in l or "[NETBYTES]" in l for l in open(args.log))
        print(f"--log {args.log}: no [NETCHUNK]/[NETBYTES] lines for play {play_id}.")
        if any_chunk:
            print("  (the file has chunk lines, but none with this play_id — check the id; "
                  "iOS logs it UPPERCASE, matched case-insensitively here.)")
        return

    # Keep-up trim (default), unless --all. Needs the archive for the marker.
    cutoff, boundary = None, None
    if not args.all and args.until in ("keepup", "playing"):
        ms = marker_ms(args.base, args.insecure, play_id, args.until)
        have_ts = any(s["start"] is not None for s in segs)
        if ms is not None and have_ts:
            cutoff = ms / 1000.0
            boundary = f"{args.until} @ log wall-clock"
        elif not have_ts:
            boundary = "no parseable log timestamps — showing all (use --all to silence)"
        else:
            boundary = f"{args.until} marker not found — showing all"
    if cutoff is not None:
        segs = [s for s in segs if s["start"] is None or s["start"] <= cutoff]

    t0 = next((s["start"] for s in segs if s["start"] is not None), None)
    if boundary:
        print(f"# chunks → {boundary}  (play {play_id}, {len(segs)} segments)")
    print(f'{"t(s)":>6}  {"seg":<22}  {"KB":>6}  {"dur(s)":>7}  {"Mbps":>6}  {"ttfb(ms)":>8}  {"nchk":>4}')
    for s in segs:
        t = (s["start"] - t0) if (t0 is not None and s["start"] is not None) else None
        print(
            f'{col(t, 6, 2)}  {s["seg"]:<22}  {col(s["bytes"] / 1024.0, 6, 0)}  '
            f'{col(s["dur"], 7, 2)}  {col(s["rate"], 6, 2)}  {col(s["ttfb"], 8, 0)}  '
            f'{col(len(s["chunks"]), 4)}'
        )

    # --verbose: interleaved per-chunk stream — every chunk across all segments in
    # global wall-clock order, so the video and audio chunks are seen competing for
    # the throttle chunk-by-chunk (not grouped per segment as the table above is).
    if args.verbose:
        flat = []
        for s in segs:
            stream, _, seg = s["seg"].partition("/")
            segnum = seg.replace("segment_", "#").replace(".m4s", "")
            for c in s["chunks"]:
                if c["epoch"] is not None:
                    flat.append((c["epoch"], stream, segnum, c["delta"]))
        flat.sort(key=lambda x: x[0])
        if flat:
            ct0 = flat[0][0]
            print()
            print("# interleaved chunks (video ↔ audio, global wall-clock)")
            print(f'{"t(s)":>7}  {"stream":<6}  {"seg":<8}  {"+KB":>6}')
            for ep, stream, segnum, delta in flat:
                print(f'{ep - ct0:>7.2f}  {stream:<6}  {segnum:<8}  {delta / 1024.0:>6.1f}')


def render_chunks_from_network(args):
    play_id = args.play_id.lower()
    net = run_harness(args.base, args.insecure, ["query", "network", play_id, "--limit", "2000"])
    parts = [r for r in net if r.get("request_kind") == "partial"]
    if not parts:
        kinds = sorted({r.get("request_kind") for r in net})
        print(f"No chunk-level rows for {play_id}.")
        print(f"  request_kinds present: {', '.join(k for k in kinds if k)}")
        print("  This play delivers full segments (no LL-HLS partials). For real")
        print("  intra-segment chunks, capture the LocalProxy log from a device running")
        print("  the 811-knobs build and pass it via --log, e.g.:")
        print("    log stream --predicate 'eventMessage CONTAINS \"[NETCHUNK]\"' \\")
        print("      --style compact > cap.log   # then: --chunks --log cap.log")
        return

    parts.sort(key=lambda r: r.get("ts") or "")
    t0 = parts[0].get("ts")
    print(f'{"t(s)":>6}  {"stream":<9}  {"KB":>6}  {"xfer(ms)":>8}  {"ttfb(ms)":>8}  status  url')
    for r in parts:
        kb = (r.get("bytes_out") or 0) / 1024.0
        line = (
            f'{_elapsed_s(t0, r.get("ts")):>6.2f}  {stream_label(r.get("url")):<9}  '
            f'{col(kb, 6, 0)}  {col(r.get("transfer_ms"), 8, 0)}  {col(r.get("ttfb_ms"), 8, 0)}  '
            f'{str(r.get("status")):>6}  {r.get("url")}'
        )
        print(line)


def _elapsed_s(t0, ts):
    """Seconds between two ISO-8601 'YYYY-MM-DDTHH:MM:SS.sssZ' strings, naive."""
    from datetime import datetime
    fmt = "%Y-%m-%dT%H:%M:%S.%fZ"
    try:
        return (datetime.strptime(ts, fmt) - datetime.strptime(t0, fmt)).total_seconds()
    except (TypeError, ValueError):
        return 0.0


if __name__ == "__main__":
    main()
