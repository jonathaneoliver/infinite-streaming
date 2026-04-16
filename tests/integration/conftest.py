"""Pytest configuration and shared fixtures for HLS failure injection tests."""
import json
import os
import subprocess
import time
import uuid
import webbrowser
import re
from typing import Optional

import pytest
import urllib.request
import urllib.parse


# Pytest configuration
def pytest_configure(config):
    """Register custom markers."""
    config.addinivalue_line("markers", "integration: Integration tests")
    config.addinivalue_line("markers", "http: HTTP-level failure tests")
    config.addinivalue_line("markers", "socket: Socket-level failure tests")
    config.addinivalue_line("markers", "transport: Transport-level failure tests")
    config.addinivalue_line("markers", "corruption: Content corruption tests")
    config.addinivalue_line("markers", "manifest: Manifest failure tests")
    config.addinivalue_line("markers", "segment: Segment failure tests")
    config.addinivalue_line("markers", "master: Master manifest failure tests")
    config.addinivalue_line("markers", "variant: Variant scoping tests")
    config.addinivalue_line("markers", "slow: Slow-running tests (>30s)")
    config.addinivalue_line("markers", "smoke: Quick smoke tests")
    config.addinivalue_line("markers", "regression: Regression tests")
    config.addinivalue_line("markers", "abrchar: Player ABR characterization tests")


def pytest_addoption(parser):
    """Add custom command-line options."""
    parser.addoption("--host", default="lenovo", help="Server host")
    parser.addoption("--scheme", default="http", help="http or https")
    parser.addoption("--api-port", type=int, default=30000, help="API/UI port")
    parser.addoption("--hls-port", type=int, default=30081, help="HLS port")
    parser.addoption("--api-base", help="Override API base URL")
    parser.addoption("--hls-base", help="Override HLS base URL")
    parser.addoption("--test-seconds", type=int, default=12, help="Per-test duration")
    parser.addoption("--req-timeout", type=int, default=20, help="Request timeout seconds")
    parser.addoption("--restore-mbps", type=float, default=1000.0, help="Rate to restore after throttle")
    parser.addoption("--throttle-mbps", type=float, default=1.0, help="Mbps threshold for throttle test")
    parser.addoption("--expect-http", type=int, default=1, help="Min HTTP 4xx/5xx count")
    parser.addoption("--expect-timeouts", type=int, default=1, help="Min timeout count")
    parser.addoption("--expect-resets", type=int, default=1, help="Min connection reset count")
    parser.addoption("--expect-drop-reject", type=int, default=1, help="Min drop/reject count")
    parser.addoption("--expect-throttle", type=int, default=1, help="Min throttle count")
    parser.addoption("--expect-corrupted", type=int, default=1, help="Min corrupted segment count")
    parser.addoption("--url", help="Specific stream URL to test")
    parser.addoption(
        "--content-name",
        default="INSANE_FPV_NEW_p200_h264",
        help="Preferred content name for auto-discovery (exact match, fallback to first compatible content)",
    )
    parser.addoption(
        "--follow-redirects",
        action="store_true",
        default=True,
        help="Follow HTTP redirects during stream/API probing (default: enabled)",
    )
    parser.addoption("--abrchar-hold-seconds", type=int, default=8, help="Hold seconds per characterization step")
    parser.addoption(
        "--abrchar-smooth-step-seconds",
        type=int,
        default=None,
        help="Smooth mode only: seconds each smooth step should last (overrides --abrchar-hold-seconds for smooth mode)",
    )
    parser.addoption(
        "--abrchar-step-gap-seconds",
        type=float,
        default=0.0,
        help="Extra seconds to wait between consecutive limit changes",
    )
    parser.addoption("--abrchar-settle-timeout", type=int, default=30, help="Seconds to wait for throughput settle per step")
    parser.addoption("--abrchar-settle-tolerance", type=float, default=0.25, help="Settle tolerance ratio (e.g. 0.25 = ±25%%)")
    parser.addoption(
        "--abrchar-test-mode",
        default="smooth",
        choices=[
            "smooth",
            "steps",
            "transient-shock",
            "startup-caps",
            "downshift-severity",
            "hysteresis-gap",
            "emergency-downshift",
            "throughput-accuracy",
            "throughput-calcs",
        ],
        help="Characterization schedule mode",
    )
    parser.addoption(
        "--abrchar-accuracy-max-limit-mbps",
        type=float,
        default=100.0,
        help="throughput-accuracy mode: maximum shaping limit to test (Mbps)",
    )
    parser.addoption(
        "--abrchar-accuracy-sparse-variants",
        type=int,
        default=2,
        help="throughput-accuracy mode: emulate sparse ladder using this many representative variants (default: 2)",
    )
    parser.addoption(
        "--abrchar-content-variant-mode",
        default="off",
        choices=["off", "all", "sparse"],
        help="Pre-test content tab variant filtering mode: off (no change), all (clear filter), sparse (select representative subset)",
    )
    parser.addoption(
        "--abrchar-content-sparse-variants",
        type=int,
        default=2,
        help="When --abrchar-content-variant-mode=sparse, how many variants to allow",
    )
    parser.addoption(
        "--abrchar-stream-profile",
        default="6s",
        choices=["ll", "2s", "6s"],
        help="HLS stream profile to use for auto-discovery: ll (master.m3u8), 2s (master_2s.m3u8), or 6s (master_6s.m3u8)",
    )
    parser.addoption(
        "--net-overhead",
        type=int,
        choices=[5, 10],
        help="Network overhead percent used for shaping target conversion (JS parity: 5 or 10)",
    )
    parser.addoption("--abrchar-overhead-pct", type=float, default=10.0, help="Network overhead percent used to convert ladder Mbps to wire Mbps")
    parser.addoption("--abrchar-max-steps", type=int, default=0, help="Maximum number of characterization steps (0 = unlimited)")
    parser.addoption(
        "--abrchar-repeat-count",
        type=int,
        default=10,
        help="How many times to repeat the characterization step schedule",
    )
    parser.addoption("--abrchar-run-name", default="", help="Optional user-friendly name for this characterization run")
    parser.addoption(
        "--abrchar-plot-logs",
        action="store_true",
        default=False,
        help="Emit ABRCHAR_PLOT structured telemetry lines for offline charting (default: disabled)",
    )
    parser.addoption(
        "--abrchar-open-browser",
        action="store_true",
        dest="abrchar_open_browser",
        default=True,
        help="Open dashboard testing-session page to start browser playback session (default: enabled)",
    )
    parser.addoption(
        "--no-abrchar-open-browser",
        action="store_false",
        dest="abrchar_open_browser",
        help="Disable browser launch and attach to an existing player/session",
    )
    parser.addoption(
        "--abrchar-browser-wait",
        type=float,
        default=2.5,
        help="Seconds to wait after opening browser before polling /api/sessions",
    )
    parser.addoption(
        "--abrchar-live-offset-seconds",
        type=float,
        default=0.0,
        help="When opening testing-session.html, seek this many seconds behind live edge (e.g. 30)",
    )
    parser.addoption(
        "--abrchar-safari-native",
        action="store_true",
        default=False,
        help="Launch testing-session.html in Safari and force player=native",
    )
    parser.addoption("--abrchar-session-id", default="", help="Attach ABR characterization to an existing session_id")
    parser.addoption("--abrchar-player-id", default="", help="Attach ABR characterization to an existing player_id (e.g. iPad simulator app)")
    parser.addoption(
        "--abrchar-attach-timeout",
        type=float,
        default=60.0,
        help="Seconds to wait when attaching to an existing player/session",
    )
    parser.addoption(
        "--abrchar-launch-ios-simulator",
        action="store_true",
        default=False,
        help="If attach IDs are not found, try launching InfiniteStreamPlayer in iOS Simulator before browser fallback",
    )
    # Loop health monitoring
    parser.addoption("--loop-count", type=int, default=3, help="Number of loops to observe before reporting")
    parser.addoption("--loop-timeout", type=int, default=300, help="Max seconds to wait for loops")
    parser.addoption("--loop-player-id", default="", help="Filter by player_id (default: first active session)")
    parser.addoption("--loop-session-id", default="", help="Filter by session_id")


@pytest.fixture(scope="session")
def config(request):
    """Provide test configuration from command-line options."""
    return type('Config', (), {
        'host': request.config.getoption("--host"),
        'scheme': request.config.getoption("--scheme"),
        'api_port': request.config.getoption("--api-port"),
        'hls_port': request.config.getoption("--hls-port"),
        'api_base': request.config.getoption("--api-base"),
        'hls_base': request.config.getoption("--hls-base"),
        'test_seconds': request.config.getoption("--test-seconds"),
        'timeout': request.config.getoption("--req-timeout"),
        'restore_mbps': request.config.getoption("--restore-mbps"),
        'throttle_mbps': request.config.getoption("--throttle-mbps"),
        'expect_http': request.config.getoption("--expect-http"),
        'expect_timeouts': request.config.getoption("--expect-timeouts"),
        'expect_resets': request.config.getoption("--expect-resets"),
        'expect_drop_reject': request.config.getoption("--expect-drop-reject"),
        'expect_throttle': request.config.getoption("--expect-throttle"),
        'expect_corrupted': request.config.getoption("--expect-corrupted"),
        'url': request.config.getoption("--url"),
        'content_name': request.config.getoption("--content-name"),
        'follow_redirects': request.config.getoption("--follow-redirects"),
        'abrchar_hold_seconds': request.config.getoption("--abrchar-hold-seconds"),
        'abrchar_smooth_step_seconds': request.config.getoption("--abrchar-smooth-step-seconds"),
        'abrchar_step_gap_seconds': request.config.getoption("--abrchar-step-gap-seconds"),
        'abrchar_settle_timeout': request.config.getoption("--abrchar-settle-timeout"),
        'abrchar_settle_tolerance': request.config.getoption("--abrchar-settle-tolerance"),
        'abrchar_test_mode': request.config.getoption("--abrchar-test-mode"),
        'abrchar_accuracy_max_limit_mbps': request.config.getoption("--abrchar-accuracy-max-limit-mbps"),
        'abrchar_accuracy_sparse_variants': request.config.getoption("--abrchar-accuracy-sparse-variants"),
        'abrchar_content_variant_mode': request.config.getoption("--abrchar-content-variant-mode"),
        'abrchar_content_sparse_variants': request.config.getoption("--abrchar-content-sparse-variants"),
        'abrchar_stream_profile': request.config.getoption("--abrchar-stream-profile"),
        'net_overhead_pct': request.config.getoption("--net-overhead"),
        'abrchar_overhead_pct': request.config.getoption("--abrchar-overhead-pct"),
        'abrchar_max_steps': request.config.getoption("--abrchar-max-steps"),
        'abrchar_repeat_count': request.config.getoption("--abrchar-repeat-count"),
        'abrchar_run_name': request.config.getoption("--abrchar-run-name"),
        'abrchar_plot_logs': request.config.getoption("--abrchar-plot-logs"),
        'abrchar_open_browser': request.config.getoption("abrchar_open_browser"),
        'abrchar_browser_wait': request.config.getoption("--abrchar-browser-wait"),
        'abrchar_live_offset_seconds': request.config.getoption("--abrchar-live-offset-seconds"),
        'abrchar_safari_native': request.config.getoption("--abrchar-safari-native"),
        'abrchar_session_id': request.config.getoption("--abrchar-session-id"),
        'abrchar_player_id': request.config.getoption("--abrchar-player-id"),
        'abrchar_attach_timeout': request.config.getoption("--abrchar-attach-timeout"),
        'abrchar_launch_ios_simulator': request.config.getoption("--abrchar-launch-ios-simulator"),
        'loop_count': request.config.getoption("--loop-count"),
        'loop_timeout': request.config.getoption("--loop-timeout"),
        'loop_player_id': request.config.getoption("--loop-player-id") or None,
        'loop_session_id': request.config.getoption("--loop-session-id") or None,
        'verbose': request.config.getoption("-v") > 0,
    })()


@pytest.fixture(scope="session")
def api_base(config):
    """Provide API base URL."""
    if config.api_base:
        return config.api_base.rstrip("/")
    return f"{config.scheme}://{config.host}:{config.api_port}"


@pytest.fixture(scope="session")
def hls_base(config):
    """Provide HLS base URL."""
    if config.hls_base:
        return config.hls_base.rstrip("/")
    return f"{config.scheme}://{config.host}:{config.hls_port}"


@pytest.fixture(scope="session")
def player_id(config):
    """Generate unique player ID for this test session."""
    override = str(getattr(config, "abrchar_player_id", "") or "").strip()
    if override:
        return override
    return str(uuid.uuid4())


@pytest.fixture(scope="session")
def stream_info(config, api_base, hls_base, player_id):
    """
    Auto-select and prepare stream for testing.

    Returns dict with:
    - master_url: URL to master playlist
    - media_url: URL to selected variant playlist
    - content_name: Content name
    - has_dash: Whether DASH is available
    - variants: List of variant info
    """
    if config.url:
        return _prepare_provided_url(
            config.url,
            player_id,
            config.timeout,
            config.verbose,
            config.follow_redirects,
            str(getattr(config, "abrchar_stream_profile", "6s") or "6s"),
        )

    return _auto_select_stream(
        api_base,
        hls_base,
        player_id,
        config.timeout,
        config.verbose,
        config.follow_redirects,
        str(getattr(config, "abrchar_stream_profile", "6s") or "6s"),
        str(getattr(config, "content_name", "") or "").strip(),
    )


def _prepare_provided_url(url, player_id, timeout, verbose, follow_redirects, stream_profile):
    """Prepare provided URL for testing."""
    from .helpers import ensure_player_id, http_get_text, parse_master_variants, pick_best_variant

    profile = str(stream_profile or "6s").strip().lower()
    master_by_profile = {
        "ll": "master.m3u8",
        "2s": "master_2s.m3u8",
        "6s": "master_6s.m3u8",
    }
    if profile not in master_by_profile:
        pytest.exit(f"Invalid --abrchar-stream-profile value: {stream_profile}")

    def rewrite_master_profile(input_url):
        text = str(input_url or "")
        if not text:
            return text
        target = master_by_profile[profile]
        # Rewrite any known go-live master filename to selected profile.
        return re.sub(r"/(master(?:_[26]s)?\.m3u8)(?=([?#]|$))", f"/{target}", text)

    def force_player_id(input_url):
        split = urllib.parse.urlsplit(str(input_url or ""))
        query = urllib.parse.parse_qs(split.query, keep_blank_values=True)
        query["player_id"] = [player_id]
        return urllib.parse.urlunsplit(
            (split.scheme, split.netloc, split.path, urllib.parse.urlencode(query, doseq=True), split.fragment)
        )

    provided_url = force_player_id(rewrite_master_profile(url))
    status, text, _, err = http_get_text(
        provided_url,
        timeout=timeout,
        verbose=verbose,
        follow_redirects=follow_redirects,
    )

    if status != 200:
        reason = f"status={status}"
        if err:
            reason = f"{reason} err={err}"
        if status == 429:
            pytest.exit(
                f"Failed to fetch provided URL due to rate limiting ({reason}): {provided_url}. "
                "Try again shortly or provide a less-loaded stream URL."
            )
        pytest.exit(f"Failed to fetch provided URL ({reason}): {provided_url}")

    master_url = None
    media_url = force_player_id(provided_url)
    variants = []

    if "#EXT-X-STREAM-INF" in text:
        master_url = force_player_id(provided_url)
        variants = parse_master_variants(text, master_url, player_id)
        media_url, _ = pick_best_variant(text, master_url)
        if not media_url:
            pytest.exit("Could not select variant from master manifest")
        media_url = force_player_id(media_url)

    return {
        'master_url': master_url,
        'media_url': media_url,
        'content_name': None,
        'has_dash': False,
        'variants': variants,
    }


def _auto_select_stream(api_base, hls_base, player_id, timeout, verbose, follow_redirects, stream_profile, preferred_content_name):
    """Auto-select H264 HLS stream from content API."""
    from .helpers import ensure_player_id, http_get_text, parse_master_variants, is_h264_master

    profile = str(stream_profile or "6s").strip().lower()
    master_by_profile = {
        "ll": "master.m3u8",
        "2s": "master_2s.m3u8",
        "6s": "master_6s.m3u8",
    }
    if profile not in master_by_profile:
        pytest.exit(f"Invalid --abrchar-stream-profile value: {stream_profile}")
    master_filename = master_by_profile[profile]

    content_url = f"{api_base}/api/content"
    status, text, _, err = http_get_text(
        content_url,
        timeout=timeout,
        verbose=verbose,
        follow_redirects=follow_redirects,
    )

    if status != 200:
        pytest.exit(f"Failed to fetch content list from {content_url}")

    try:
        items = json.loads(text)
    except json.JSONDecodeError:
        pytest.exit(f"Invalid JSON from {content_url}")

    if not isinstance(items, list):
        pytest.exit(f"Unexpected content format from {content_url}")

    candidates = [x for x in items if x.get("has_hls")]
    candidates.sort(key=lambda x: x.get("name", ""))

    if not candidates:
        pytest.exit("No HLS content found")

    preferred_name = str(preferred_content_name or "").strip()
    if preferred_name:
        preferred = [item for item in candidates if str(item.get("name") or "") == preferred_name]
        if preferred:
            others = [item for item in candidates if str(item.get("name") or "") != preferred_name]
            candidates = preferred + others
            if verbose:
                print(f"Preferred content requested: {preferred_name}", flush=True)
        elif verbose:
            print(
                f"Preferred content not found in API list, falling back to first compatible stream: {preferred_name}",
                flush=True,
            )

    # Find first H264 content for the selected stream profile.
    # Probe without player_id to avoid creating many player-bound sessions during
    # discovery (which can trigger HTTP 429 rate limiting on busy hosts).
    for item in candidates:
        name = item.get("name")
        if not name:
            continue

        safe_name = urllib.parse.quote(name, safe="")
        master_probe_url = f"{hls_base}/go-live/{safe_name}/{master_filename}"

        status, master_text, _, _ = http_get_text(
            master_probe_url,
            timeout=timeout,
            verbose=verbose,
            follow_redirects=follow_redirects,
        )
        if status == 429:
            # Gentle backoff while searching to avoid amplifying throttling.
            time.sleep(0.05)
            continue
        if status != 200 or not is_h264_master(master_text):
            continue

        from .helpers import pick_best_variant, parse_master_variants

        # Once a candidate is selected, bind player_id for actual playback/test traffic.
        master_url = ensure_player_id(master_probe_url, player_id)

        variants = parse_master_variants(master_text, master_url, player_id)
        media_url, _ = pick_best_variant(master_text, master_url)

        if not media_url:
            continue

        return {
            'master_url': master_url,
            'media_url': media_url,
            'content_name': name,
            'has_dash': bool(item.get("has_dash")),
            'variants': variants,
        }

    pytest.exit(
        f"No suitable H264 HLS content found for profile '{profile}' (or discovery was throttled with HTTP 429). "
        "Try passing --url with a known-good master/variant URL."
    )


@pytest.fixture(scope="session")
def session_id(api_base, hls_base, stream_info, player_id, config):
    """
    Find or create test session by making initial request.

    Returns session_id string.
    """
    from .helpers import http_get_text, find_session_by_player_id, find_session_by_player_id_sse
    from .helpers import fetch_session_snapshot

    attach_session_id = str(getattr(config, "abrchar_session_id", "") or "").strip()
    attach_player_id = str(getattr(config, "abrchar_player_id", "") or "").strip()
    explicit_attach_requested = bool(attach_session_id or attach_player_id)
    attach_timeout = max(5.0, float(getattr(config, "abrchar_attach_timeout", 60.0) or 60.0))
    launch_ios_sim = bool(getattr(config, "abrchar_launch_ios_simulator", False))
    ios_bundle_id = "com.jeoliver.InfiniteStreamPlayer"

    def find_by_player(pid: str, timeout_s: float):
        if not pid:
            return None
        found = find_session_by_player_id_sse(api_base, pid, timeout=timeout_s, verbose=config.verbose)
        if not found:
            found = find_session_by_player_id(api_base, pid, timeout=timeout_s, verbose=config.verbose)
        return found

    def try_launch_ios_simulator() -> bool:
        try:
            cmd = ["xcrun", "simctl", "launch", "booted", ios_bundle_id]
            proc = subprocess.run(cmd, capture_output=True, text=True, timeout=20, check=False)
            if config.verbose:
                stdout = (proc.stdout or "").strip()
                stderr = (proc.stderr or "").strip()
                print(f"iOS simulator launch exit={proc.returncode} bundle={ios_bundle_id}", flush=True)
                if stdout:
                    print(f"iOS simulator launch stdout: {stdout}", flush=True)
                if stderr:
                    print(f"iOS simulator launch stderr: {stderr}", flush=True)
            return proc.returncode == 0
        except Exception as exc:
            if config.verbose:
                print(f"iOS simulator launch failed: {exc}", flush=True)
            return False

    if attach_session_id:
        snap = fetch_session_snapshot(api_base, attach_session_id, verbose=config.verbose)
        if snap and snap.get("session_id"):
            if config.verbose:
                print(f"Attached to existing session_id={attach_session_id}", flush=True)
            return str(snap.get("session_id"))
        if config.verbose:
            print(f"session_id={attach_session_id} not found", flush=True)

    if attach_player_id:
        if config.verbose:
            print(
                f"Attempting attach by player_id={attach_player_id} (timeout={attach_timeout}s)",
                flush=True,
            )
        session = find_by_player(attach_player_id, attach_timeout)
        if session and session.get("session_id"):
            if config.verbose:
                print(f"Attached to existing player_id={attach_player_id}", flush=True)
            return session["session_id"]
        if config.verbose:
            print(f"player_id={attach_player_id} not found", flush=True)

    if launch_ios_sim:
        launched = try_launch_ios_simulator()
        candidate_player = attach_player_id or player_id
        if launched and candidate_player:
            if config.verbose:
                print(
                    f"Waiting for simulator session via player_id={candidate_player} (timeout={attach_timeout}s)",
                    flush=True,
                )
            session = find_by_player(candidate_player, attach_timeout)
            if session and session.get("session_id"):
                if config.verbose:
                    print(f"Attached after simulator launch via player_id={candidate_player}", flush=True)
                return session["session_id"]
            if config.verbose:
                print("No session found after simulator launch; continuing with browser fallback", flush=True)
        elif config.verbose:
            print("Simulator launch not successful; continuing with browser fallback", flush=True)

    if explicit_attach_requested:
        pytest.exit(
            "Attach target was not found (session/player). Refusing browser fallback to avoid creating extra playback sessions. "
            "Verify --abrchar-session-id/--abrchar-player-id or run without attach flags."
        )

    # Warm up session by opening dashboard playback page (matches real UI flow).
    def force_player_id(input_url):
        split = urllib.parse.urlsplit(str(input_url or ""))
        query = urllib.parse.parse_qs(split.query, keep_blank_values=True)
        query["player_id"] = [player_id]
        return urllib.parse.urlunsplit(
            (split.scheme, split.netloc, split.path, urllib.parse.urlencode(query, doseq=True), split.fragment)
        )

    raw_playback_url = stream_info.get('master_url') or stream_info['media_url']
    playback_url = force_player_id(raw_playback_url)
    if config.abrchar_open_browser and playback_url:
        open_folds = 'network-shaping,bitrate-chart,player-characterization'
        launch_params = {
            'player_id': player_id,
            'url': playback_url,
            'open_folds': open_folds,
            'auto_recovery': '1',
        }
        live_offset_seconds = float(getattr(config, 'abrchar_live_offset_seconds', 0.0) or 0.0)
        if live_offset_seconds > 0:
            launch_params['live_offset_s'] = f"{live_offset_seconds:g}"
        if bool(getattr(config, 'abrchar_safari_native', False)):
            launch_params['player'] = 'native'
        launch_query = urllib.parse.urlencode(launch_params)
        browser_url = (
            f"{hls_base}/dashboard/testing-session.html?{launch_query}"
        )
        if config.verbose:
            print(f"Opening browser playback URL: {browser_url}", flush=True)
            existing_pid = urllib.parse.parse_qs(urllib.parse.urlsplit(str(raw_playback_url or '')).query).get('player_id', [''])[0]
            launch_pid = urllib.parse.parse_qs(urllib.parse.urlsplit(playback_url).query).get('player_id', [''])[0]
            print(
                f"Playback URL player_id normalization raw={existing_pid or '-'} normalized={launch_pid or '-'} expected={player_id}",
                flush=True,
            )
        try:
            if bool(getattr(config, 'abrchar_safari_native', False)):
                proc = subprocess.run(["open", "-a", "Safari", browser_url], capture_output=True, text=True, timeout=20, check=False)
                opened = proc.returncode == 0
                if config.verbose and (proc.stdout or proc.stderr):
                    stdout = (proc.stdout or "").strip()
                    stderr = (proc.stderr or "").strip()
                    if stdout:
                        print(f"Safari open stdout: {stdout}", flush=True)
                    if stderr:
                        print(f"Safari open stderr: {stderr}", flush=True)
            else:
                opened = webbrowser.open(browser_url, new=2, autoraise=True)
            if config.verbose:
                print(f"Browser open result: {opened}", flush=True)
        except Exception as exc:
            if config.verbose:
                print(f"Browser open failed ({exc}); falling back to HTTP warmup", flush=True)
            http_get_text(
                stream_info['media_url'],
                timeout=config.timeout,
                verbose=config.verbose,
                follow_redirects=config.follow_redirects,
            )
        time.sleep(max(0.5, float(config.abrchar_browser_wait)))
    else:
        http_get_text(
            stream_info['media_url'],
            timeout=config.timeout,
            verbose=config.verbose,
            follow_redirects=config.follow_redirects,
        )
        time.sleep(1)

    # Find session (prefer SSE stream subscription, then fallback polling API list).
    if config.verbose:
        print(f"Session discovery base (SSE + polling): {api_base}", flush=True)
    session = find_session_by_player_id_sse(api_base, player_id, timeout=12, verbose=config.verbose)
    if not session:
        session = find_session_by_player_id(api_base, player_id, timeout=12, verbose=config.verbose)

    if not session or not session.get('session_id'):
        pytest.exit("Failed to locate session via SSE or /api/sessions")

    return session['session_id']


@pytest.fixture(scope="session")
def session_port(api_base, session_id):
    """Get session port for network shaping tests."""
    from .helpers import fetch_session_snapshot

    snapshot = fetch_session_snapshot(api_base, session_id, verbose=False)
    return snapshot.get('x_forwarded_port') if snapshot else None


@pytest.fixture(scope="function")
def clean_session(api_base, session_id, session_port, config):
    """
    Reset session to clean state before each test.

    This fixture runs before each test function and ensures:
    - All failure settings are cleared
    - Network shaping is reset
    - Session is in known good state
    """
    from .helpers import apply_failure_settings, apply_shaping, base_failure_payload

    # Reset failures
    apply_failure_settings(api_base, session_id, base_failure_payload(), verbose=config.verbose)

    # Reset network shaping
    if session_port:
        apply_shaping(api_base, session_port, config.restore_mbps, verbose=config.verbose)

    yield

    # Cleanup after test
    apply_failure_settings(api_base, session_id, base_failure_payload(), verbose=config.verbose)
    if session_port:
        apply_shaping(api_base, session_port, config.restore_mbps, verbose=config.verbose)


@pytest.fixture(scope="function")
def validate_precheck(stream_info, config, clean_session):
    """
    Validate stream is healthy before running failure test.

    Returns True if precheck passes, otherwise fails the test.
    """
    from .helpers import run_probe_window

    warmup_seconds = min(4, config.test_seconds)

    counters = run_probe_window(
        stream_info['media_url'],
        config,
        warmup_seconds,
        verbose_label="precheck",
        timeout_override=min(config.timeout, max(2, warmup_seconds - 1)),
    )

    if counters.get("segment_success", 0) < 1:
        pytest.fail("Precheck failed: no successful segment responses")

    return True


@pytest.fixture(scope="function")
def validate_postcheck(stream_info, api_base, session_id, config):
    """
    Validate stream recovers after failure test.

    This is a finalizer that runs after the test and validates recovery.
    """
    yield

    from .helpers import run_probe_window

    warmup_seconds = min(4, config.test_seconds)

    counters = run_probe_window(
        stream_info['media_url'],
        config,
        warmup_seconds,
        verbose_label="postcheck",
        timeout_override=min(config.timeout, max(2, warmup_seconds - 1)),
    )

    if counters.get("segment_success", 0) < 1:
        pytest.fail("Postcheck failed: stream did not recover")

    failure_count = (
        counters.get("segment_http_error", 0) +
        counters.get("segment_timeout", 0) +
        counters.get("segment_conn_reset", 0) +
        counters.get("segment_conn_refused", 0)
    )

    if failure_count > 0:
        pytest.fail(f"Postcheck failed: failures persisted ({failure_count} errors)")


@pytest.fixture
def failure_payload_factory(api_base, session_id, config):
    """
    Factory fixture for creating and applying failure payloads.

    Usage:
        payload = failure_payload_factory(segment_failure_type="404")
    """
    from .helpers import base_failure_payload, apply_failure_settings

    def _create_payload(**overrides):
        payload = base_failure_payload()
        payload.update(overrides)
        apply_failure_settings(api_base, session_id, payload, verbose=config.verbose)
        return payload

    return _create_payload


@pytest.fixture
def snapshot(api_base, session_id):
    """Get current session snapshot."""
    from .helpers import fetch_session_snapshot
    return fetch_session_snapshot(api_base, session_id, verbose=False)


# Hooks for test execution tracking
@pytest.hookimpl(tryfirst=True, hookwrapper=True)
def pytest_runtest_makereport(item, call):
    """Hook to track test results and generate detailed reports."""
    outcome = yield
    rep = outcome.get_result()

    # Store test result on the item for access in fixtures
    setattr(item, f"rep_{rep.when}", rep)

    # Add custom reporting metadata
    if rep.when == "call":
        if hasattr(item, 'test_metadata'):
            rep.metadata = item.test_metadata


def pytest_collection_modifyitems(config, items):
    """Modify test items after collection."""
    # Add markers based on test names
    for item in items:
        # Auto-mark based on test name patterns
        if "http_" in item.nodeid or "_404" in item.nodeid or "_500" in item.nodeid:
            item.add_marker(pytest.mark.http)

        if "socket_" in item.nodeid or "_reset" in item.nodeid or "_hang" in item.nodeid:
            item.add_marker(pytest.mark.socket)

        if "transport_" in item.nodeid or "_drop" in item.nodeid or "_reject" in item.nodeid:
            item.add_marker(pytest.mark.transport)

        if "corrupted" in item.nodeid:
            item.add_marker(pytest.mark.corruption)

        if "manifest" in item.nodeid:
            if "master" in item.nodeid:
                item.add_marker(pytest.mark.master)
            else:
                item.add_marker(pytest.mark.manifest)

        if "segment" in item.nodeid:
            item.add_marker(pytest.mark.segment)

        if "variant" in item.nodeid or "scope" in item.nodeid:
            item.add_marker(pytest.mark.variant)


@pytest.fixture(scope="session")
def test_results():
    """Track test results across session."""
    return {
        'passed': [],
        'failed': [],
        'skipped': [],
    }


def pytest_sessionfinish(session, exitstatus):
    """Generate summary report after all tests."""
    if hasattr(session.config, 'workerinput'):
        # Skip on xdist workers
        return

    # Generate HTML report if requested
    if session.config.getoption('--html', default=None):
        # pytest-html will handle this
        pass
