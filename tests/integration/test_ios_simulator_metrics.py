"""Sample integration test for iOS simulator + SSE/session metrics."""
from __future__ import annotations

import json
import os
import subprocess
import threading
import time
from dataclasses import dataclass, field
from typing import Dict, List, Optional

import pytest

from . import helpers


@dataclass
class MetricsSample:
    ts: float
    video_quality_pct: Optional[float] = None
    stall_count: Optional[int] = None
    stall_time_s: Optional[float] = None
    last_event: Optional[str] = None


@dataclass
class MonitorState:
    samples: List[MetricsSample] = field(default_factory=list)
    last_session: Dict[str, object] = field(default_factory=dict)


class SseSessionMonitor:
    def __init__(
        self,
        api_base: str,
        session_id: str,
        stop_event: threading.Event,
        verbose: bool,
    ):
        self.api_base = api_base.rstrip("/")
        self.session_id = session_id
        self.stop_event = stop_event
        self.verbose = verbose
        self.state = MonitorState()
        self.thread = threading.Thread(target=self._run, daemon=True)

    def start(self) -> None:
        self.thread.start()

    def join(self, timeout: Optional[float] = None) -> None:
        self.thread.join(timeout=timeout)

    def _run(self) -> None:
        url = f"{self.api_base}/api/sessions/stream"
        req = helpers.urllib.request.Request(url, headers={"Accept": "text/event-stream"})
        try:
            with helpers.urllib.request.urlopen(req, timeout=30) as resp:
                buffer: List[str] = []
                while not self.stop_event.is_set():
                    line = resp.readline().decode("utf-8", errors="replace")
                    if not line:
                        break
                    line = line.rstrip("\r\n")
                    if not line:
                        self._handle_event(buffer)
                        buffer = []
                        continue
                    buffer.append(line)
        except Exception:
            return

    def _handle_event(self, lines: List[str]) -> None:
        data_lines = [line[5:] for line in lines if line.startswith("data:")]
        if not data_lines:
            return
        try:
            payload = json.loads("\n".join(data_lines))
        except json.JSONDecodeError:
            return
        sessions = payload.get("sessions") if isinstance(payload, dict) else None
        if not isinstance(sessions, list):
            return
        for session in sessions:
            if not isinstance(session, dict):
                continue
            if session.get("session_id") != self.session_id:
                continue
            self.state.last_session = session
            if self.verbose:
                print(
                    "SSE session update "
                    f"event={session.get('player_metrics_last_event')} "
                    f"quality={session.get('player_metrics_video_quality_pct')} "
                    f"stalls={session.get('player_metrics_stall_count')} "
                    f"stall_time_s={session.get('player_metrics_stall_time_s')}",
                    flush=True,
                )
            self.state.samples.append(
                MetricsSample(
                    ts=time.time(),
                    video_quality_pct=_to_float(session.get("player_metrics_video_quality_pct")),
                    stall_count=_to_int(session.get("player_metrics_stall_count")),
                    stall_time_s=_to_float(session.get("player_metrics_stall_time_s")),
                    last_event=_to_str(session.get("player_metrics_last_event")),
                )
            )
            break


def _to_float(value: object) -> Optional[float]:
    try:
        if value is None:
            return None
        return float(value)
    except (TypeError, ValueError):
        return None


def _to_int(value: object) -> Optional[int]:
    try:
        if value is None:
            return None
        return int(value)
    except (TypeError, ValueError):
        return None


def _to_str(value: object) -> Optional[str]:
    if value is None:
        return None
    return str(value)


def _run(cmd: List[str], check: bool = True) -> subprocess.CompletedProcess:
    return subprocess.run(cmd, check=check, capture_output=True, text=True)


def _log(verbose: bool, message: str) -> None:
    if verbose:
        print(message, flush=True)


def _boot_simulator(device: str, verbose: bool) -> None:
    _log(verbose, f"Booting simulator: {device}")
    _run(["xcrun", "simctl", "boot", device], check=False)
    _run(["xcrun", "simctl", "bootstatus", device, "-b"], check=True)


def _install_and_launch_app(
    device: str,
    bundle_id: str,
    app_path: Optional[str],
    launch_args: Optional[List[str]],
    verbose: bool,
) -> None:
    if app_path:
        _log(verbose, f"Installing app: {app_path}")
        _run(["xcrun", "simctl", "install", device, app_path], check=True)
    _log(verbose, f"Launching app: {bundle_id}")
    _run(["xcrun", "simctl", "terminate", device, bundle_id], check=False)
    cmd = ["xcrun", "simctl", "launch", device, bundle_id]
    if launch_args:
        cmd += ["--args"] + launch_args
    _run(cmd, check=True)


def _wait_for_new_session(
    api_base: str,
    existing_ids: set,
    timeout_s: int = 45,
    verbose: bool = False,
    baseline_ts: Optional[float] = None,
) -> Dict[str, object]:
    deadline = time.time() + timeout_s
    while time.time() < deadline:
        _log(verbose, "Polling /api/sessions for new session...")
        sessions = helpers.api_request_json(f"{api_base}/api/sessions", timeout=10, verbose=False)
        if isinstance(sessions, list):
            newest = None
            newest_ts = -1.0
            for session in sessions:
                if not isinstance(session, dict):
                    continue
                session_id = session.get("session_id")
                if session_id and session_id not in existing_ids:
                    _log(verbose, f"New session found: {session_id}")
                    return session
                ts = _to_float(session.get("bytes_last_ts")) or 0.0
                if ts > newest_ts:
                    newest_ts = ts
                    newest = session
            if baseline_ts is not None and newest and newest_ts >= baseline_ts:
                _log(verbose, f"Reusing active session: {newest.get('session_id')}")
                return newest
        time.sleep(1)
    raise RuntimeError("Timed out waiting for new session")


def _apply_pyramid_pattern(
    api_base: str,
    port: str,
    steps: List[Dict[str, object]],
    verbose: bool,
) -> None:
    shape_url = f"{api_base}/api/nftables/shape/{port}"
    pattern_url = f"{api_base}/api/nftables/pattern/{port}"
    _log(verbose, f"Applying pyramid pattern on port {port}")
    helpers.api_request_json(
        shape_url,
        method="POST",
        payload={"rate_mbps": 8.0, "delay_ms": 0, "loss_pct": 0.0},
        timeout=10,
        verbose=False,
    )
    helpers.api_request_json(
        pattern_url,
        method="POST",
        payload={
            "steps": steps,
            "segment_duration_seconds": 6,
            "default_segments": 2,
            "default_step_seconds": 12,
            "template_mode": "pyramid",
            "template_margin_pct": 25,
            "delay_ms": 0,
            "loss_pct": 0.0,
        },
        timeout=10,
        verbose=False,
    )


def _score_session(samples: List[MetricsSample]) -> float:
    quality_samples = [s.video_quality_pct for s in samples if s.video_quality_pct is not None]
    avg_quality = sum(quality_samples) / len(quality_samples) if quality_samples else 0.0
    stall_count = max((s.stall_count or 0) for s in samples) if samples else 0
    stall_time = max((s.stall_time_s or 0.0) for s in samples) if samples else 0.0
    return round(avg_quality - (stall_count * 5.0) - (stall_time * 0.5), 2)


def _summarize_samples(samples: List[MetricsSample]) -> Dict[str, float]:
    quality_samples = [s.video_quality_pct for s in samples if s.video_quality_pct is not None]
    avg_quality = sum(quality_samples) / len(quality_samples) if quality_samples else 0.0
    min_quality = min(quality_samples) if quality_samples else 0.0
    max_quality = max(quality_samples) if quality_samples else 0.0
    stall_count = max((s.stall_count or 0) for s in samples) if samples else 0
    stall_time = max((s.stall_time_s or 0.0) for s in samples) if samples else 0.0
    return {
        "samples": float(len(samples)),
        "avg_quality": round(avg_quality, 2),
        "min_quality": round(min_quality, 2),
        "max_quality": round(max_quality, 2),
        "stall_count": float(stall_count),
        "stall_time_s": round(stall_time, 3),
    }


@pytest.mark.integration
def test_ios_simulator_pyramid_metrics(api_base, config):
    if os.getenv("IOS_SIM_TEST_RUN") != "1":
        pytest.skip("Set IOS_SIM_TEST_RUN=1 to enable iOS simulator integration test")

    device = os.getenv("IOS_SIM_DEVICE", "iPad Pro (11-inch) (4th generation)")
    bundle_id = os.getenv("IOS_APP_BUNDLE_ID")
    app_path = os.getenv("IOS_APP_PATH")
    duration_s = max(900, int(os.getenv("IOS_METRICS_DURATION", "900")))
    snapshot_interval_s = int(os.getenv("IOS_SNAPSHOT_INTERVAL", "60"))
    score_min = os.getenv("IOS_SCORE_MIN")
    verbose = os.getenv("IOS_VERBOSE", "1") != "0"

    if not bundle_id:
        pytest.skip("Set IOS_APP_BUNDLE_ID (and optionally IOS_APP_PATH) to launch the app")

    existing_sessions = helpers.api_request_json(f"{api_base}/api/sessions", timeout=10, verbose=False)
    existing_ids = {s.get("session_id") for s in existing_sessions or [] if isinstance(s, dict)}
    baseline_ts = time.time()

    _boot_simulator(device, verbose)
    _install_and_launch_app(device, bundle_id or "", app_path, launch_args=None, verbose=verbose)

    session = _wait_for_new_session(
        api_base,
        existing_ids,
        timeout_s=60,
        verbose=verbose,
        baseline_ts=baseline_ts,
    )
    session_id = session.get("session_id")
    assert session_id

    port = session.get("x_forwarded_port_external") or session.get("x_forwarded_port")
    if not port:
        snapshot = helpers.fetch_session_snapshot(api_base, session_id, verbose=False)
        port = snapshot.get("x_forwarded_port_external") or snapshot.get("x_forwarded_port")

    steps = [
        {"rate_mbps": 3.0, "duration_seconds": 12, "enabled": True},
        {"rate_mbps": 10.0, "duration_seconds": 12, "enabled": True},
        {"rate_mbps": 3.0, "duration_seconds": 12, "enabled": True},
    ]
    if port:
        _apply_pyramid_pattern(api_base, str(port), steps, verbose)
    else:
        _log(verbose, "No port detected; skipping pyramid pattern")

    stop_event = threading.Event()
    sse = SseSessionMonitor(api_base, session_id, stop_event, verbose)
    sse.start()

    end_time = time.time() + duration_s
    next_snapshot = time.time()
    while time.time() < end_time:
        now = time.time()
        if now < next_snapshot:
            time.sleep(min(1, next_snapshot - now))
            continue
        snapshot = helpers.fetch_session_snapshot(api_base, session_id, verbose=False)
        if snapshot:
            if verbose:
                print(
                    "Session snapshot "
                    f"event={snapshot.get('player_metrics_last_event')} "
                    f"quality={snapshot.get('player_metrics_video_quality_pct')} "
                    f"stalls={snapshot.get('player_metrics_stall_count')} "
                    f"stall_time_s={snapshot.get('player_metrics_stall_time_s')}",
                    flush=True,
                )
            sse.state.samples.append(
                MetricsSample(
                    ts=time.time(),
                    video_quality_pct=_to_float(snapshot.get("player_metrics_video_quality_pct")),
                    stall_count=_to_int(snapshot.get("player_metrics_stall_count")),
                    stall_time_s=_to_float(snapshot.get("player_metrics_stall_time_s")),
                    last_event=_to_str(snapshot.get("player_metrics_last_event")),
                )
            )
        next_snapshot = time.time() + max(1, snapshot_interval_s)

    stop_event.set()
    sse.join(timeout=5)

    score = _score_session(sse.state.samples)
    summary = _summarize_samples(sse.state.samples)
    print(
        "Session summary "
        f"id={session_id} "
        f"score={score} "
        f"samples={int(summary['samples'])} "
        f"stalls={int(summary['stall_count'])} "
        f"stall_time_s={summary['stall_time_s']} "
        f"quality_avg={summary['avg_quality']} "
        f"quality_min={summary['min_quality']} "
        f"quality_max={summary['max_quality']}",
        flush=True,
    )

    if score_min is not None:
        assert score >= float(score_min)
