"""
Pytest-based HLS failure injection tests.

This module contains comprehensive tests for HLS player resilience
under various failure conditions including HTTP errors, socket failures,
transport faults, and content corruption.

Test organization:
- TestHTTPFailures: HTTP-level failure tests (404, 500, timeout, etc.)
- TestSocketFailures: Socket-level failures (reset, hang, delay)
- TestTransportFailures: Transport-level faults (drop, reject)
- TestCorruption: Content corruption tests
- TestVariantScoping: Variant-specific failure tests
- TestRecovery: Recovery and resilience tests

Usage:
    # Run all tests
    pytest test_hls_failures.py

    # Run only HTTP tests
    pytest test_hls_failures.py -m http

    # Run only segment tests
    pytest test_hls_failures.py -m segment

    # Run with verbose output
    pytest test_hls_failures.py -v

    # Run smoke tests only
    pytest test_hls_failures.py -m smoke
"""
import pytest
from .helpers import (
    run_probe_window,
    run_manifest_window,
    run_simple_window,
    wait_for_transport_active,
)


# ============================================================================
# Test Data / Parameters
# ============================================================================

HTTP_FAILURE_TYPES = [
    pytest.param("404", id="404_not_found"),
    pytest.param("403", id="403_forbidden"),
    pytest.param("500", id="500_server_error"),
    pytest.param("timeout", id="timeout"),
    pytest.param("connection_refused", id="connection_refused"),
    pytest.param("dns_failure", id="dns_failure"),
    pytest.param("rate_limiting", id="rate_limiting"),
]

SOCKET_RESET_TYPES = [
    pytest.param("request_connect_reset", id="connect_reset"),
    pytest.param("request_first_byte_reset", id="first_byte_reset"),
    pytest.param("request_body_reset", id="body_reset"),
]

SOCKET_HANG_TYPES = [
    pytest.param("request_connect_hang", id="connect_hang"),
    pytest.param("request_first_byte_hang", id="first_byte_hang"),
    pytest.param("request_body_hang", id="body_hang"),
]

SOCKET_DELAY_TYPES = [
    pytest.param("request_connect_delayed", id="connect_delayed"),
    pytest.param("request_first_byte_delayed", id="first_byte_delayed"),
    pytest.param("request_body_delayed", id="body_delayed"),
]

FAILURE_MODES = [
    pytest.param(
        {"mode": "requests", "consecutive_units": "requests", "frequency_units": "requests"},
        id="requests_mode"
    ),
    pytest.param(
        {"mode": "seconds", "consecutive_units": "seconds", "frequency_units": "seconds"},
        id="seconds_mode"
    ),
    pytest.param(
        {"mode": "failures_per_seconds", "consecutive_units": "requests", "frequency_units": "seconds"},
        id="failures_per_seconds_mode"
    ),
]

FAILURE_SCHEDULES = [
    pytest.param({"consecutive": 0, "frequency": 0, "manual": True}, id="instant"),
    pytest.param({"consecutive": 1, "frequency": 0, "manual": False}, id="c1_f0"),
    pytest.param({"consecutive": 1, "frequency": 10, "manual": False}, id="c1_f10"),
]


# ============================================================================
# Test Classes
# ============================================================================

class TestHTTPFailures:
    """Test HTTP-level failures (404, 500, timeout, etc.)."""

    @pytest.mark.http
    @pytest.mark.segment
    @pytest.mark.smoke
    @pytest.mark.parametrize("failure_type", HTTP_FAILURE_TYPES)
    def test_segment_http_failure_instant(
        self,
        failure_type,
        failure_payload_factory,
        stream_info,
        config,
        validate_precheck,
        validate_postcheck,
    ):
        """Test segment HTTP failures with instant triggering."""
        # Apply failure
        failure_payload_factory(
            segment_failure_type=failure_type,
            segment_consecutive_failures=0,
            segment_failure_frequency=0,
            segment_failure_mode="requests",
            segment_consecutive_units="requests",
            segment_frequency_units="requests",
        )

        # Run test window
        counters = run_probe_window(
            stream_info['media_url'],
            config,
            config.test_seconds,
            verbose_label=f"segment_{failure_type}",
        )

        # Assertions
        expected_status = {
            "404": 404,
            "403": 403,
            "500": 500,
            "timeout": 504,
            "connection_refused": 503,
            "dns_failure": 502,
            "rate_limiting": 429,
        }.get(failure_type)

        assert counters.get("segment_http_error", 0) >= config.expect_http, \
            f"Expected at least {config.expect_http} HTTP errors, got {counters.get('segment_http_error', 0)}"

        if expected_status:
            status_key = f"segment_status_{expected_status}"
            assert counters.get(status_key, 0) >= 1, \
                f"Expected at least 1 request with status {expected_status}, got {counters.get(status_key, 0)}"

    @pytest.mark.http
    @pytest.mark.manifest
    @pytest.mark.smoke
    @pytest.mark.parametrize("failure_type", HTTP_FAILURE_TYPES)
    def test_manifest_http_failure_instant(
        self,
        failure_type,
        failure_payload_factory,
        stream_info,
        config,
        clean_session,
    ):
        """Test manifest HTTP failures with instant triggering."""
        # Apply failure
        failure_payload_factory(
            manifest_failure_type=failure_type,
            manifest_consecutive_failures=0,
            manifest_failure_frequency=0,
            manifest_failure_mode="requests",
            manifest_consecutive_units="requests",
            manifest_frequency_units="requests",
        )

        # Run test window
        counters = run_manifest_window(
            stream_info['media_url'],
            config,
            config.test_seconds,
            verbose_label=f"manifest_{failure_type}",
        )

        # Assertions
        assert counters.get("manifest_http_error", 0) >= config.expect_http, \
            f"Expected at least {config.expect_http} HTTP errors, got {counters.get('manifest_http_error', 0)}"

    @pytest.mark.http
    @pytest.mark.master
    @pytest.mark.parametrize("failure_type", HTTP_FAILURE_TYPES)
    def test_master_manifest_http_failure(
        self,
        failure_type,
        failure_payload_factory,
        stream_info,
        config,
        clean_session,
    ):
        """Test master manifest HTTP failures."""
        if not stream_info.get('master_url'):
            pytest.skip("No master manifest available")

        # Apply failure
        failure_payload_factory(
            master_manifest_failure_type=failure_type,
            master_manifest_consecutive_failures=0,
            master_manifest_failure_frequency=0,
            master_manifest_failure_mode="requests",
            master_manifest_consecutive_units="requests",
            master_manifest_frequency_units="requests",
        )

        # Run test window
        counters = run_simple_window(
            "master_manifest",
            stream_info['master_url'],
            config,
            config.test_seconds,
            verbose_label=f"master_{failure_type}",
        )

        # Assertions
        assert counters.get("master_manifest_http_error", 0) >= config.expect_http

    @pytest.mark.http
    @pytest.mark.segment
    @pytest.mark.parametrize("failure_type", ["404"])
    @pytest.mark.parametrize("mode", FAILURE_MODES)
    @pytest.mark.parametrize("schedule", FAILURE_SCHEDULES)
    def test_segment_http_failure_with_modes(
        self,
        failure_type,
        mode,
        schedule,
        failure_payload_factory,
        stream_info,
        config,
        validate_precheck,
        validate_postcheck,
    ):
        """Test segment HTTP failures with different modes and schedules."""
        # Apply failure
        failure_payload_factory(
            segment_failure_type=failure_type,
            segment_consecutive_failures=schedule["consecutive"],
            segment_failure_frequency=schedule["frequency"],
            segment_failure_mode=mode["mode"],
            segment_consecutive_units=mode["consecutive_units"],
            segment_frequency_units=mode["frequency_units"],
        )

        # Determine window size based on schedule
        stop_on_failure = schedule.get("manual", False)

        # Run test window
        counters = run_probe_window(
            stream_info['media_url'],
            config,
            config.test_seconds,
            verbose_label=f"segment_{failure_type}_{mode['mode']}_{schedule}",
            stop_on_failure=stop_on_failure,
        )

        # Assertions
        if schedule["frequency"] == 0:
            # Expect all failures
            assert counters.get("segment_http_error", 0) >= config.expect_http
        else:
            # Expect mixed success and failures
            assert counters.get("segment_success", 0) >= 1, \
                "Expected at least 1 successful request with frequency > 0"
            assert counters.get("segment_http_error", 0) >= 1, \
                "Expected at least 1 failed request"


class TestSocketFailures:
    """Test socket-level failures (reset, hang, delay)."""

    @pytest.mark.socket
    @pytest.mark.segment
    @pytest.mark.smoke
    @pytest.mark.parametrize("failure_type", SOCKET_RESET_TYPES)
    def test_segment_socket_reset(
        self,
        failure_type,
        failure_payload_factory,
        stream_info,
        config,
        snapshot,
        validate_precheck,
        validate_postcheck,
    ):
        """Test segment socket reset failures."""
        # Apply failure
        failure_payload_factory(
            segment_failure_type=failure_type,
            segment_consecutive_failures=0,
            segment_failure_frequency=0,
            segment_failure_mode="requests",
            segment_consecutive_units="requests",
            segment_frequency_units="requests",
        )

        # Run test window
        counters = run_probe_window(
            stream_info['media_url'],
            config,
            config.test_seconds,
            verbose_label=f"segment_{failure_type}",
        )

        # Check either client-side detection or server-side fault counter
        client_resets = counters.get("segment_conn_reset", 0)
        server_fault_key = f"fault_count_{failure_type}"

        # Fetch updated snapshot to check server counter
        from .helpers import fetch_session_snapshot
        final_snapshot = fetch_session_snapshot(config.api_base if hasattr(config, 'api_base') else None,
                                                 snapshot.get('session_id') if snapshot else None)
        server_faults = int(final_snapshot.get(server_fault_key, 0)) if final_snapshot else 0

        assert client_resets >= config.expect_resets or server_faults >= config.expect_resets, \
            f"Expected {config.expect_resets} resets (client: {client_resets}, server: {server_faults})"

    @pytest.mark.socket
    @pytest.mark.segment
    @pytest.mark.slow
    @pytest.mark.parametrize("failure_type", SOCKET_HANG_TYPES)
    def test_segment_socket_hang(
        self,
        failure_type,
        failure_payload_factory,
        stream_info,
        config,
        validate_precheck,
        validate_postcheck,
    ):
        """Test segment socket hang failures."""
        # Hangs take longer to detect
        window_seconds = 24

        # Apply failure
        failure_payload_factory(
            segment_failure_type=failure_type,
            segment_consecutive_failures=0,
            segment_failure_frequency=0,
            segment_failure_mode="requests",
            segment_consecutive_units="requests",
            segment_frequency_units="requests",
        )

        # Run test window with extended timeout
        counters = run_probe_window(
            stream_info['media_url'],
            config,
            window_seconds,
            verbose_label=f"segment_{failure_type}",
            timeout_override=window_seconds,
        )

        # Assertions
        assert counters.get("segment_timeout", 0) >= config.expect_timeouts, \
            f"Expected {config.expect_timeouts} timeouts, got {counters.get('segment_timeout', 0)}"

    @pytest.mark.socket
    @pytest.mark.segment
    @pytest.mark.parametrize("failure_type", SOCKET_DELAY_TYPES)
    def test_segment_socket_delay(
        self,
        failure_type,
        failure_payload_factory,
        stream_info,
        config,
        validate_precheck,
        validate_postcheck,
    ):
        """Test segment socket delay failures."""
        window_seconds = 14

        # Apply failure
        failure_payload_factory(
            segment_failure_type=failure_type,
            segment_consecutive_failures=0,
            segment_failure_frequency=0,
            segment_failure_mode="requests",
            segment_consecutive_units="requests",
            segment_frequency_units="requests",
        )

        # Run test window
        counters = run_probe_window(
            stream_info['media_url'],
            config,
            window_seconds,
            verbose_label=f"segment_{failure_type}",
            timeout_override=window_seconds,
        )

        # Assertions - delays can result in either timeout or http_error
        failures = counters.get("segment_timeout", 0) + counters.get("segment_http_error", 0)
        assert failures >= config.expect_timeouts, \
            f"Expected {config.expect_timeouts} failures (timeout or error), got {failures}"

    @pytest.mark.socket
    @pytest.mark.manifest
    @pytest.mark.parametrize("failure_type", SOCKET_RESET_TYPES)
    def test_manifest_socket_reset(
        self,
        failure_type,
        failure_payload_factory,
        stream_info,
        config,
        clean_session,
    ):
        """Test manifest socket reset failures."""
        # Apply failure
        failure_payload_factory(
            manifest_failure_type=failure_type,
            manifest_consecutive_failures=0,
            manifest_failure_frequency=0,
            manifest_failure_mode="requests",
            manifest_consecutive_units="requests",
            manifest_frequency_units="requests",
        )

        # Run test window
        counters = run_manifest_window(
            stream_info['media_url'],
            config,
            config.test_seconds,
            verbose_label=f"manifest_{failure_type}",
        )

        # Assertions
        assert counters.get("manifest_conn_reset", 0) >= config.expect_resets


class TestCorruption:
    """Test content corruption scenarios."""

    @pytest.mark.corruption
    @pytest.mark.segment
    @pytest.mark.smoke
    def test_segment_corrupted_zero_fill(
        self,
        failure_payload_factory,
        stream_info,
        config,
        validate_precheck,
        validate_postcheck,
    ):
        """Test segment corruption with zero-filled data."""
        # Apply failure
        failure_payload_factory(
            segment_failure_type="corrupted",
            segment_consecutive_failures=0,
            segment_failure_frequency=0,
            segment_failure_mode="requests",
            segment_consecutive_units="requests",
            segment_frequency_units="requests",
        )

        # Define corruption inspector
        def is_corrupted(data):
            return data and all(b == 0 for b in data)

        # Run test window
        counters = run_probe_window(
            stream_info['media_url'],
            config,
            4,  # Shorter window for corruption test
            verbose_label="segment_corrupted",
            segment_inspector=is_corrupted,
        )

        # Assertions
        assert counters.get("segment_corrupted", 0) >= config.expect_corrupted, \
            f"Expected {config.expect_corrupted} corrupted segments, got {counters.get('segment_corrupted', 0)}"


class TestTransportFailures:
    """Test transport-level failures (packet drop, reject)."""

    @pytest.mark.transport
    @pytest.mark.slow
    @pytest.mark.parametrize("failure_type", ["drop", "reject"])
    def test_transport_fault(
        self,
        failure_type,
        failure_payload_factory,
        stream_info,
        api_base,
        session_id,
        config,
        validate_precheck,
        validate_postcheck,
    ):
        """Test transport-level drop/reject faults."""
        # Apply failure
        failure_payload_factory(
            transport_failure_type=failure_type,
            transport_consecutive_failures=2,
            transport_failure_frequency=0,
            transport_failure_units="seconds",
        )

        # Wait for transport fault to activate
        snapshot = wait_for_transport_active(api_base, session_id, verbose=config.verbose)

        if not snapshot or not snapshot.get("transport_fault_active"):
            pytest.skip("Transport fault did not activate")

        # Run test window with short timeout
        counters = run_probe_window(
            stream_info['media_url'],
            config,
            8,
            verbose_label=f"transport_{failure_type}",
            timeout_override=3,
        )

        # Check drop/reject events
        drop_reject = (
            counters.get("segment_timeout", 0) +
            counters.get("segment_conn_reset", 0) +
            counters.get("segment_conn_refused", 0)
        )

        # Also check server-side packet counters
        from .helpers import fetch_session_snapshot
        final_snapshot = fetch_session_snapshot(api_base, session_id)
        fault_packets = 0
        if final_snapshot:
            fault_packets = (
                int(final_snapshot.get("transport_fault_drop_packets", 0)) +
                int(final_snapshot.get("transport_fault_reject_packets", 0))
            )

        assert drop_reject >= config.expect_drop_reject or fault_packets >= config.expect_drop_reject, \
            f"Expected {config.expect_drop_reject} drop/reject events " \
            f"(client: {drop_reject}, server packets: {fault_packets})"

    @pytest.mark.transport
    @pytest.mark.slow
    def test_bandwidth_throttle(
        self,
        stream_info,
        api_base,
        session_port,
        config,
        validate_precheck,
        validate_postcheck,
    ):
        """Test bandwidth throttling."""
        if not session_port:
            pytest.skip("Session port not available for throttling")

        from .helpers import apply_shaping

        # Apply throttling
        apply_shaping(api_base, session_port, config.throttle_mbps, verbose=config.verbose)

        # Run test window
        counters = run_probe_window(
            stream_info['media_url'],
            config,
            max(4, config.test_seconds),
            verbose_label="throttle",
        )

        # Check throttle events
        assert counters.get("throttle", 0) >= config.expect_throttle, \
            f"Expected {config.expect_throttle} throttle events, got {counters.get('throttle', 0)}"


class TestVariantScoping:
    """Test variant-specific failure scoping."""

    @pytest.mark.variant
    @pytest.mark.segment
    def test_segment_failure_variant_subset(
        self,
        failure_payload_factory,
        stream_info,
        config,
        validate_precheck,
        validate_postcheck,
    ):
        """Test segment failures scoped to specific variants."""
        variants = stream_info.get('variants', [])
        if len(variants) < 2:
            pytest.skip("Need at least 2 variants for scoping test")

        # Select first two variants to fail
        selected_variants = [v['name'] for v in variants[:2]]
        unselected_variants = [v['name'] for v in variants[2:]]

        # Apply failure to selected variants only
        failure_payload_factory(
            segment_failure_type="404",
            segment_consecutive_failures=1,
            segment_failure_frequency=0,
            segment_failure_mode="requests",
            segment_failure_urls=selected_variants,
        )

        # Test that selected variants fail and unselected succeed
        from .helpers import http_fetch

        # Test selected variant (should fail)
        selected_url = variants[0]['url']
        status, _, _, err = http_fetch(selected_url, timeout=config.timeout)
        assert status == 404 or err is not None, \
            f"Expected failure for selected variant, got status={status}"

        # Test unselected variant (should succeed)
        if unselected_variants:
            unselected_url = variants[2]['url'] if len(variants) > 2 else None
            if unselected_url:
                status, _, _, err = http_fetch(unselected_url, timeout=config.timeout)
                assert status == 200 and err is None, \
                    f"Expected success for unselected variant, got status={status}, err={err}"


class TestRecovery:
    """Test recovery and resilience scenarios."""

    @pytest.mark.regression
    @pytest.mark.segment
    def test_recovery_after_transient_failure(
        self,
        failure_payload_factory,
        stream_info,
        config,
        clean_session,
    ):
        """Test that stream recovers after transient failure."""
        # Apply brief failure
        failure_payload_factory(
            segment_failure_type="timeout",
            segment_consecutive_failures=1,
            segment_failure_frequency=10,  # 1 failure every 10 requests
            segment_failure_mode="requests",
        )

        # Run test window
        counters = run_probe_window(
            stream_info['media_url'],
            config,
            config.test_seconds,
            verbose_label="recovery_test",
        )

        # Should have both failures and successes
        assert counters.get("segment_timeout", 0) >= 1, "Expected at least 1 timeout"
        assert counters.get("segment_success", 0) >= 5, "Expected at least 5 successes showing recovery"

    @pytest.mark.regression
    def test_no_failures_with_clean_config(
        self,
        stream_info,
        config,
        clean_session,
    ):
        """Test that stream works properly with no failures configured."""
        # No failures applied (clean_session fixture resets everything)

        # Run test window
        counters = run_probe_window(
            stream_info['media_url'],
            config,
            config.test_seconds,
            verbose_label="baseline",
        )

        # Should have only successes
        assert counters.get("segment_success", 0) >= 5, "Expected successful streaming"
        assert counters.get("segment_http_error", 0) == 0, "Expected no HTTP errors"
        assert counters.get("segment_timeout", 0) == 0, "Expected no timeouts"
