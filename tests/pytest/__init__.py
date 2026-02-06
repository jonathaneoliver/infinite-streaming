"""
Pytest-based HLS Failure Injection Test Suite.

This package provides comprehensive testing for HLS player resilience
under various failure conditions.
"""

__version__ = "1.0.0"
__author__ = "HLS Test Team"

# Make helpers available at package level
from .helpers import (
    http_fetch,
    http_get_text,
    api_request_json,
    ensure_player_id,
    parse_master_variants,
    parse_media_playlist,
    find_session_by_player_id,
    fetch_session_snapshot,
    apply_failure_settings,
    apply_shaping,
    base_failure_payload,
    run_probe_window,
    run_manifest_window,
    run_simple_window,
)

__all__ = [
    "http_fetch",
    "http_get_text",
    "api_request_json",
    "ensure_player_id",
    "parse_master_variants",
    "parse_media_playlist",
    "find_session_by_player_id",
    "fetch_session_snapshot",
    "apply_failure_settings",
    "apply_shaping",
    "base_failure_payload",
    "run_probe_window",
    "run_manifest_window",
    "run_simple_window",
]
