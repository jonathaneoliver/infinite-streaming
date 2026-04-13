"""
Loop boundary health monitor.

Monitors an active browser session via SSE and checks for anomalies
around video loop boundaries (stalls, buffer drain, frame drops, etc.).

Requires a browser actively playing a stream — this test is passive,
it only watches SSE data.

Usage:
    # Monitor any active session on the Ubuntu test instance
    pytest test_loop_health.py -m loop -v \
      --host jonathanoliver-ubuntu.local --api-port 22000

    # Monitor a specific player_id
    pytest test_loop_health.py -m loop -v \
      --host jonathanoliver-ubuntu.local --api-port 22000 \
      --loop-player-id abc123

    # Wait for 5 loops with 10 minute timeout
    pytest test_loop_health.py -m loop -v \
      --host jonathanoliver-ubuntu.local --api-port 22000 \
      --loop-count 5 --loop-timeout 600
"""

import json
import time
import urllib.request
import pytest

pytestmark = pytest.mark.loop

UA = "loop-health-test/1.0"

ANOMALY_WINDOW_S = 5.0
BUFFER_LOW_THRESHOLD_S = 0.5
FRAMES_STALL_THRESHOLD_S = 3.0
BUFFERED_END_STALL_THRESHOLD_S = 3.0
SEGMENT_GAP_THRESHOLD_S = 6.0
BITRATE_DROP_PCT = 0.50
LOOP_DIVERGENCE_TIMEOUT_S = 10.0


def utc_now_iso():
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


class Sample:
    __slots__ = (
        "ts", "loop_server", "loop_player", "stall_count", "stall_time",
        "buffer_depth", "buffer_end", "frames_displayed", "frames_dropped",
        "bitrate", "segments_count",
    )

    def __init__(self, ts, session):
        self.ts = ts
        self.loop_server = _num(session, "loop_count_server", 0)
        self.loop_player = _num(session, "player_metrics_loop_count_player", 0)
        self.stall_count = _num(session, "player_metrics_stall_count", 0)
        self.stall_time = _num(session, "player_metrics_stall_time_s", 0)
        self.buffer_depth = _num(session, "player_metrics_buffer_depth_s", None)
        self.buffer_end = _num(session, "player_metrics_buffer_end_s", None)
        self.frames_displayed = _num(session, "player_metrics_frames_displayed", None)
        self.frames_dropped = _num(session, "player_metrics_dropped_frames", None)
        self.bitrate = _num(session, "player_metrics_video_bitrate_mbps", None)
        self.segments_count = _num(session, "segments_count", 0)


def _num(d, key, default=None):
    v = d.get(key)
    if v is None:
        return default
    try:
        f = float(v)
        return f if f == f else default  # NaN check
    except (TypeError, ValueError):
        return default


def detect_loop_events(samples):
    """Return list of (sample_index, timestamp) where loop_count_server incremented."""
    events = []
    for i in range(1, len(samples)):
        if samples[i].loop_server > samples[i - 1].loop_server:
            events.append((i, samples[i].ts))
    return events


def samples_in_window(samples, center_ts, window_s):
    """Return samples within ±window_s of center_ts."""
    return [s for s in samples if abs(s.ts - center_ts) <= window_s]


def analyze_loop(samples, loop_idx, loop_ts, all_samples, window_s=ANOMALY_WINDOW_S):
    """Analyze a single loop event for anomalies."""
    window = samples_in_window(all_samples, loop_ts, window_s)
    if len(window) < 2:
        return {"loop": loop_idx, "ts": loop_ts, "status": "INSUFFICIENT_DATA", "details": []}

    anomalies = []

    # 1. Stall detection — did stall_count increase in the window?
    stall_counts = [s.stall_count for s in window if s.stall_count is not None]
    if len(stall_counts) >= 2 and max(stall_counts) > min(stall_counts):
        delta = max(stall_counts) - min(stall_counts)
        stall_times = [s.stall_time for s in window if s.stall_time is not None]
        stall_delta = (max(stall_times) - min(stall_times)) if len(stall_times) >= 2 else 0
        anomalies.append(f"stall_count +{delta} (stall_time +{stall_delta:.1f}s)")

    # 2. Buffer drain — did buffer_depth drop below threshold?
    depths = [s.buffer_depth for s in window if s.buffer_depth is not None]
    if depths:
        min_depth = min(depths)
        if min_depth < BUFFER_LOW_THRESHOLD_S:
            anomalies.append(f"buffer_depth dropped to {min_depth:.2f}s")

    # 3. Frames stall — did frames_displayed stop incrementing?
    frame_points = [(s.ts, s.frames_displayed) for s in window if s.frames_displayed is not None]
    if len(frame_points) >= 2:
        max_gap = 0
        for i in range(1, len(frame_points)):
            if frame_points[i][1] == frame_points[i - 1][1]:
                gap = frame_points[i][0] - frame_points[i - 1][0]
                max_gap = max(max_gap, gap)
        if max_gap >= FRAMES_STALL_THRESHOLD_S:
            anomalies.append(f"frames_displayed stalled for {max_gap:.1f}s")

    # 4. Buffered end freeze — did buffer_end stop changing?
    end_points = [(s.ts, s.buffer_end) for s in window if s.buffer_end is not None]
    if len(end_points) >= 2:
        max_freeze = 0
        for i in range(1, len(end_points)):
            if abs(end_points[i][1] - end_points[i - 1][1]) < 0.01:
                freeze = end_points[i][0] - end_points[i - 1][0]
                max_freeze = max(max_freeze, freeze)
        if max_freeze >= BUFFERED_END_STALL_THRESHOLD_S:
            anomalies.append(f"buffer_end frozen for {max_freeze:.1f}s")

    # 5. Bitrate drop >50%
    bitrates = [s.bitrate for s in window if s.bitrate is not None and s.bitrate > 0]
    if len(bitrates) >= 2:
        peak = max(bitrates)
        trough = min(bitrates)
        if peak > 0 and trough / peak < BITRATE_DROP_PCT:
            anomalies.append(f"bitrate dropped {peak:.1f} → {trough:.1f} Mbps ({trough/peak*100:.0f}%)")

    # 6. Segment gap — segments_count stalled
    seg_points = [(s.ts, s.segments_count) for s in window if s.segments_count is not None]
    if len(seg_points) >= 2:
        max_seg_gap = 0
        for i in range(1, len(seg_points)):
            if seg_points[i][1] == seg_points[i - 1][1]:
                gap = seg_points[i][0] - seg_points[i - 1][0]
                max_seg_gap = max(max_seg_gap, gap)
        if max_seg_gap >= SEGMENT_GAP_THRESHOLD_S:
            anomalies.append(f"segments_count stalled for {max_seg_gap:.1f}s")

    # Summary
    status = "ANOMALY" if anomalies else "CLEAN"
    details = []
    if not anomalies:
        depth_str = f"{min(depths):.1f}s" if depths else "?"
        bitrate_str = f"{bitrates[-1]:.1f}Mbps" if bitrates else "?"
        details.append(f"buffer_depth={depth_str} bitrate={bitrate_str}")
    else:
        details = anomalies

    return {"loop": loop_idx, "ts": loop_ts, "status": status, "details": details}


def stream_sse_samples(api_base, session_id=None, player_id=None, loop_count=3, timeout=300, verbose=False):
    """Connect to SSE and collect samples until we observe loop_count loops."""
    url = f"{api_base}/api/sessions/stream"
    req = urllib.request.Request(
        url,
        headers={
            "User-Agent": UA,
            "Accept": "text/event-stream",
            "Cache-Control": "no-cache",
            "Pragma": "no-cache",
        },
    )
    deadline = time.time() + timeout
    samples = []
    initial_loop_count = None
    matched_session_id = session_id
    buffer = []

    print(f"\n{utc_now_iso()} Connecting to SSE: {url}", flush=True)
    if player_id:
        print(f"  Filtering by player_id: {player_id}", flush=True)
    elif session_id:
        print(f"  Filtering by session_id: {session_id}", flush=True)
    else:
        print(f"  Will use first active session", flush=True)
    print(f"  Waiting for {loop_count} loops (timeout {timeout}s)\n", flush=True)

    try:
        with urllib.request.urlopen(req, timeout=min(timeout + 10, 600)) as resp:
            while time.time() < deadline:
                raw = resp.readline()
                if not raw:
                    break
                line = raw.decode("utf-8", errors="replace").rstrip("\r\n")
                if line:
                    buffer.append(line)
                    continue

                # Parse SSE event
                data_lines = []
                for item in buffer:
                    if item.startswith("data:"):
                        data_lines.append(item[5:].lstrip())
                buffer = []

                if not data_lines:
                    continue
                try:
                    payload = json.loads("\n".join(data_lines))
                except json.JSONDecodeError:
                    continue

                sessions = payload.get("sessions") if isinstance(payload, dict) else None
                if not isinstance(sessions, list) or not sessions:
                    continue

                # Find our session
                session = None
                for s in sessions:
                    if not isinstance(s, dict):
                        continue
                    if matched_session_id and str(s.get("session_id")) == str(matched_session_id):
                        session = s
                        break
                    if player_id and str(s.get("player_id", "")).startswith(str(player_id)):
                        session = s
                        matched_session_id = s.get("session_id")
                        break

                # If no filter specified, use first session
                if session is None and not player_id and not session_id and sessions:
                    session = sessions[0]
                    matched_session_id = session.get("session_id")

                if session is None:
                    continue

                now = time.time()
                sample = Sample(now, session)
                samples.append(sample)

                if initial_loop_count is None:
                    initial_loop_count = sample.loop_server
                    print(
                        f"{utc_now_iso()} Monitoring session_id={matched_session_id} "
                        f"player_id={session.get('player_id')} "
                        f"initial_loop_count={initial_loop_count}",
                        flush=True,
                    )

                loops_observed = sample.loop_server - initial_loop_count
                if loops_observed >= loop_count:
                    print(f"{utc_now_iso()} Observed {loops_observed} loops, stopping collection", flush=True)
                    # Collect a few more seconds for the post-window
                    post_deadline = now + ANOMALY_WINDOW_S + 1
                    while time.time() < min(post_deadline, deadline):
                        raw = resp.readline()
                        if not raw:
                            break
                        line = raw.decode("utf-8", errors="replace").rstrip("\r\n")
                        if line:
                            buffer.append(line)
                            continue
                        data_lines = []
                        for item in buffer:
                            if item.startswith("data:"):
                                data_lines.append(item[5:].lstrip())
                        buffer = []
                        if not data_lines:
                            continue
                        try:
                            p = json.loads("\n".join(data_lines))
                        except json.JSONDecodeError:
                            continue
                        ss = p.get("sessions") if isinstance(p, dict) else None
                        if not isinstance(ss, list):
                            continue
                        for s in ss:
                            if isinstance(s, dict) and str(s.get("session_id")) == str(matched_session_id):
                                samples.append(Sample(time.time(), s))
                                break
                    break

    except Exception as exc:
        print(f"{utc_now_iso()} SSE error: {exc}", flush=True)

    return samples, matched_session_id


def format_report(samples, loop_events, results):
    """Format a human-readable report."""
    lines = []
    start_ts = samples[0].ts if samples else 0
    for r in results:
        elapsed = r["ts"] - start_ts
        status = r["status"]
        lines.append(f"Loop {r['loop']} @ {elapsed:.1f}s: {status}")
        for d in r["details"]:
            lines.append(f"  {d}")
        lines.append("")

    # Loop divergence check
    if samples:
        last = samples[-1]
        if last.loop_server != last.loop_player:
            lines.append(f"Loop divergence: server={last.loop_server} player={last.loop_player} — MISMATCH")
        else:
            lines.append(f"Loop divergence: server={last.loop_server} player={last.loop_player} — OK")

    clean = sum(1 for r in results if r["status"] == "CLEAN")
    total = len(results)
    anomalies = total - clean
    lines.append(f"\nSummary: {clean}/{total} clean loops, {anomalies} anomal{'y' if anomalies == 1 else 'ies'}")
    return "\n".join(lines)


@pytest.mark.loop
def test_loop_boundary_health(api_base, config):
    """Monitor SSE for anomalies at video loop boundaries."""
    loop_count = getattr(config, "loop_count", 3)
    loop_timeout = getattr(config, "loop_timeout", 300)
    loop_player_id = getattr(config, "loop_player_id", None)
    loop_session_id = getattr(config, "loop_session_id", None)

    samples, session_id = stream_sse_samples(
        api_base,
        session_id=loop_session_id,
        player_id=loop_player_id,
        loop_count=loop_count,
        timeout=loop_timeout,
        verbose=True,
    )

    assert len(samples) > 0, "No SSE samples collected — is a browser session active?"

    loop_events = detect_loop_events(samples)
    assert len(loop_events) > 0, (
        f"No server loop events detected in {len(samples)} samples over "
        f"{samples[-1].ts - samples[0].ts:.0f}s — content may not have looped yet"
    )

    results = []
    for idx, (sample_idx, loop_ts) in enumerate(loop_events, 1):
        result = analyze_loop(samples, idx, loop_ts, samples)
        results.append(result)

    report = format_report(samples, loop_events, results)
    print(f"\n{'='*60}")
    print("LOOP BOUNDARY HEALTH REPORT")
    print(f"{'='*60}")
    print(report)
    print(f"{'='*60}\n")

    anomaly_count = sum(1 for r in results if r["status"] == "ANOMALY")
    assert anomaly_count == 0, f"{anomaly_count} loop(s) had anomalies — see report above"
