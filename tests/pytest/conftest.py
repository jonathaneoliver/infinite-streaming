"""Pytest configuration and shared fixtures for HLS failure injection tests."""
import json
import os
import time
import uuid
from typing import Optional

import pytest
import urllib.request
import urllib.parse


# Pytest configuration
def pytest_configure(config):
    """Register custom markers."""
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


def pytest_addoption(parser):
    """Add custom command-line options."""
    parser.addoption("--host", default="lenovo", help="Server host")
    parser.addoption("--scheme", default="http", help="http or https")
    parser.addoption("--api-port", type=int, default=30000, help="API/UI port")
    parser.addoption("--hls-port", type=int, default=30081, help="HLS port")
    parser.addoption("--api-base", help="Override API base URL")
    parser.addoption("--hls-base", help="Override HLS base URL")
    parser.addoption("--test-seconds", type=int, default=12, help="Per-test duration")
    parser.addoption("--timeout", type=int, default=20, help="Request timeout seconds")
    parser.addoption("--restore-mbps", type=float, default=1000.0, help="Rate to restore after throttle")
    parser.addoption("--throttle-mbps", type=float, default=1.0, help="Mbps threshold for throttle test")
    parser.addoption("--expect-http", type=int, default=1, help="Min HTTP 4xx/5xx count")
    parser.addoption("--expect-timeouts", type=int, default=1, help="Min timeout count")
    parser.addoption("--expect-resets", type=int, default=1, help="Min connection reset count")
    parser.addoption("--expect-drop-reject", type=int, default=1, help="Min drop/reject count")
    parser.addoption("--expect-throttle", type=int, default=1, help="Min throttle count")
    parser.addoption("--expect-corrupted", type=int, default=1, help="Min corrupted segment count")
    parser.addoption("--url", help="Specific stream URL to test")


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
        'timeout': request.config.getoption("--timeout"),
        'restore_mbps': request.config.getoption("--restore-mbps"),
        'throttle_mbps': request.config.getoption("--throttle-mbps"),
        'expect_http': request.config.getoption("--expect-http"),
        'expect_timeouts': request.config.getoption("--expect-timeouts"),
        'expect_resets': request.config.getoption("--expect-resets"),
        'expect_drop_reject': request.config.getoption("--expect-drop-reject"),
        'expect_throttle': request.config.getoption("--expect-throttle"),
        'expect_corrupted': request.config.getoption("--expect-corrupted"),
        'url': request.config.getoption("--url"),
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
def player_id():
    """Generate unique player ID for this test session."""
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
        return _prepare_provided_url(config.url, player_id)

    return _auto_select_stream(api_base, hls_base, player_id, config.timeout, config.verbose)


def _prepare_provided_url(url, player_id):
    """Prepare provided URL for testing."""
    from .helpers import ensure_player_id, http_get_text, parse_master_variants, pick_best_variant

    url = ensure_player_id(url, player_id)
    status, text, _, err = http_get_text(url, timeout=20, verbose=False)

    if status != 200:
        pytest.exit(f"Failed to fetch provided URL: {url}")

    master_url = None
    media_url = url
    variants = []

    if "#EXT-X-STREAM-INF" in text:
        master_url = url
        variants = parse_master_variants(text, url, player_id)
        media_url, _ = pick_best_variant(text, url)
        if not media_url:
            pytest.exit("Could not select variant from master manifest")

    return {
        'master_url': master_url,
        'media_url': media_url,
        'content_name': None,
        'has_dash': False,
        'variants': variants,
    }


def _auto_select_stream(api_base, hls_base, player_id, timeout, verbose):
    """Auto-select H264 HLS stream from content API."""
    from .helpers import ensure_player_id, http_get_text, parse_master_variants, is_h264_master

    content_url = f"{api_base}/api/content"
    status, text, _, err = http_get_text(content_url, timeout=timeout, verbose=verbose)

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

    # Find first H264 6s content
    for item in candidates:
        name = item.get("name")
        if not name:
            continue

        safe_name = urllib.parse.quote(name, safe="")
        master_url = f"{hls_base}/go-live/{safe_name}/master_6s.m3u8"
        master_url = ensure_player_id(master_url, player_id)

        status, master_text, _, _ = http_get_text(master_url, timeout=timeout, verbose=verbose)
        if status != 200 or not is_h264_master(master_text):
            continue

        from .helpers import pick_best_variant, parse_master_variants

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

    pytest.exit("No suitable H264 HLS content found")


@pytest.fixture(scope="session")
def session_id(api_base, stream_info, player_id, config):
    """
    Find or create test session by making initial request.

    Returns session_id string.
    """
    from .helpers import http_get_text, find_session_by_player_id

    # Warm up session
    http_get_text(stream_info['media_url'], timeout=config.timeout, verbose=config.verbose)
    time.sleep(1)

    # Find session
    session = find_session_by_player_id(api_base, player_id, timeout=12, verbose=config.verbose)

    if not session or not session.get('session_id'):
        pytest.exit("Failed to locate session via /api/sessions")

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
