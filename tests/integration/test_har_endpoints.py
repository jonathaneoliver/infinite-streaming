"""
Smoke tests for the HAR snapshot endpoints (issue #272).

These tests don't require a live playback session — they exercise the
empty / not-found / path-traversal paths against a running go-proxy.

Usage:
    cd tests/integration
    pytest test_har_endpoints.py -m smoke -v \
        --host localhost --scheme http --api-port 30081

    # Or against test-dev:
    pytest test_har_endpoints.py -m smoke -v \
        --host jonathanoliver-ubuntu.local --api-port 21081
"""
import json
import urllib.error
import urllib.request

import pytest


def _get_status(url):
    """GET url, return (status_code, decoded_body). Always closes the
    underlying response — Python 3.14 emits a ResourceWarning if the
    HTTPError's file-like body isn't drained + closed explicitly."""
    try:
        with urllib.request.urlopen(url, timeout=10) as resp:
            return resp.getcode(), resp.read().decode("utf-8", errors="replace")
    except urllib.error.HTTPError as exc:
        try:
            return exc.code, exc.read().decode("utf-8", errors="replace")
        finally:
            exc.close()


def _post_json(url, payload):
    body = json.dumps(payload).encode("utf-8") if payload is not None else None
    headers = {"Content-Type": "application/json"}
    req = urllib.request.Request(url, data=body, method="POST", headers=headers)
    try:
        with urllib.request.urlopen(req, timeout=10) as resp:
            return resp.getcode(), resp.read().decode("utf-8", errors="replace")
    except urllib.error.HTTPError as exc:
        try:
            return exc.code, exc.read().decode("utf-8", errors="replace")
        finally:
            exc.close()


@pytest.mark.smoke
@pytest.mark.har
class TestHARListing:
    """GET /api/incidents — listing endpoint."""

    def test_list_returns_envelope(self, api_base):
        status, body = _get_status(f"{api_base}/api/incidents")
        assert status == 200
        data = json.loads(body)
        assert "count" in data
        assert "incidents" in data
        assert isinstance(data["incidents"], list)
        assert isinstance(data["count"], int)
        assert data["count"] == len(data["incidents"])

    def test_list_player_filter_accepts(self, api_base):
        # Even with an unknown player_id the endpoint should 200 with an
        # empty list, not error.
        status, body = _get_status(
            f"{api_base}/api/incidents?player_id=nosuchplayer"
        )
        assert status == 200
        data = json.loads(body)
        assert data["count"] == 0
        assert data["incidents"] == []


@pytest.mark.smoke
@pytest.mark.har
class TestHARTimelineMissing:
    """GET /api/sessions/{player_id}/timeline.har — 404 for unknown player."""

    def test_unknown_player_returns_404(self, api_base):
        status, body = _get_status(
            f"{api_base}/api/sessions/nosuchplayer-xyz/timeline.har"
        )
        assert status == 404
        # Should be JSON with an error message.
        data = json.loads(body)
        assert "error" in data


@pytest.mark.smoke
@pytest.mark.har
class TestHARSnapshotMissing:
    """POST /api/session/{id}/har/snapshot — 404 for unknown session."""

    def test_unknown_session_returns_404(self, api_base):
        status, body = _post_json(
            f"{api_base}/api/session/nosuch-session/har/snapshot",
            {"reason": "manual", "source": "rest"},
        )
        assert status == 404
        data = json.loads(body)
        assert "error" in data

    def test_empty_body_accepted(self, api_base):
        # No body should still validate (defaults applied), then 404 since
        # the session doesn't exist. We're verifying body parsing doesn't
        # crash.
        status, _body = _post_json(
            f"{api_base}/api/session/nosuch-session/har/snapshot", None
        )
        assert status == 404

    def test_invalid_json_returns_400(self, api_base):
        # Send an explicitly bad JSON body — must reject before the
        # session lookup.
        req = urllib.request.Request(
            f"{api_base}/api/session/nosuch-session/har/snapshot",
            data=b"{not json",
            method="POST",
            headers={"Content-Type": "application/json"},
        )
        try:
            with urllib.request.urlopen(req, timeout=10) as resp:
                status = resp.getcode()
        except urllib.error.HTTPError as exc:
            status = exc.code
            exc.close()
        assert status == 400


@pytest.mark.smoke
@pytest.mark.har
class TestIncidentDownloadSafety:
    """GET /api/incidents/{path} — path-traversal must be rejected."""

    @pytest.mark.parametrize(
        "bad_path",
        [
            "../etc/passwd",
            "..%2Fetc%2Fpasswd",
            "foo/../../etc/passwd",
        ],
    )
    def test_path_traversal_blocked(self, api_base, bad_path):
        status, body = _get_status(f"{api_base}/api/incidents/{bad_path}")
        # Either 400 (rejected by the explicit `..` check) or 404 (mux
        # didn't route it). Both are safe; we just need to confirm the
        # request doesn't reach a real file outside the incidents dir.
        assert status in (400, 404)
        # Body should never contain a 200-style filesystem leak.
        assert "root:" not in body
        assert "/etc/passwd" not in body or status >= 400

    def test_unknown_file_returns_404(self, api_base):
        status, _body = _get_status(
            f"{api_base}/api/incidents/2026-04-28/nosuchfile.har"
        )
        assert status == 404
