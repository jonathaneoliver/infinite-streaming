"""Helper functions for HLS failure injection tests."""
from collections import Counter, deque
from datetime import datetime, timezone
import json
import re
import socket
import time
import urllib.error
import urllib.parse
import urllib.request


UA = "hls-failure-probe-pytest/1.0"


class _NoRedirectHandler(urllib.request.HTTPRedirectHandler):
    """Redirect handler that blocks HTTP redirects when disabled."""

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None


def utc_now_iso():
    """Return current UTC time in ISO format."""
    return datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def encode_url_path(url):
    """Encode URL path component."""
    split = urllib.parse.urlsplit(url)
    safe_path = urllib.parse.quote(split.path, safe="/:%")
    return urllib.parse.urlunsplit(
        (split.scheme, split.netloc, safe_path, split.query, split.fragment)
    )


def http_fetch(url, timeout=20, verbose=False, follow_redirects=True):
    """
    Fetch URL and return (status, data, duration, error).

    Returns:
        Tuple of (status_code, bytes_data, duration_seconds, error_string)
        status_code is None if request failed
        error_string is None on success, otherwise describes failure
    """
    url = encode_url_path(url)
    req = urllib.request.Request(
        url,
        headers={
            "User-Agent": UA,
            "Cache-Control": "no-cache",
            "Pragma": "no-cache",
        },
    )
    t0 = time.time()
    try:
        opener = (
            urllib.request.build_opener()
            if follow_redirects
            else urllib.request.build_opener(_NoRedirectHandler())
        )
        with opener.open(req, timeout=timeout) as resp:
            data = resp.read()
            status = getattr(resp, "status", resp.getcode())
            final_url = resp.geturl()
        dt = time.time() - t0
        if verbose:
            redirect_note = ""
            if final_url and final_url != url:
                redirect_note = f" final_url={final_url}"
            print(
                f"{utc_now_iso()} FETCH status={status} dur_ms={dt * 1000:.1f} "
                f"bytes={len(data)} url={url}{redirect_note}",
                flush=True,
            )
        return status, data, dt, None
    except urllib.error.HTTPError as exc:
        dt = time.time() - t0
        try:
            data = exc.read()
        finally:
            try:
                exc.close()
            except Exception:
                pass
        if verbose:
            print(
                f"{utc_now_iso()} FETCH status={exc.code} dur_ms={dt * 1000:.1f} "
                f"bytes={len(data)} url={url} error=http_error",
                flush=True,
            )
        return exc.code, data, dt, "http_error"
    except (socket.timeout, TimeoutError):
        dt = time.time() - t0
        if verbose:
            print(
                f"{utc_now_iso()} FETCH status=ERR dur_ms={dt * 1000:.1f} "
                f"bytes=0 url={url} error=timeout",
                flush=True,
            )
        return None, b"", dt, "timeout"
    except urllib.error.URLError as exc:
        dt = time.time() - t0
        reason = str(exc.reason)
        if verbose:
            print(
                f"{utc_now_iso()} FETCH status=ERR dur_ms={dt * 1000:.1f} "
                f"bytes=0 url={url} error=url_error:{reason}",
                flush=True,
            )
        return None, b"", dt, f"url_error:{reason}"
    except Exception as exc:
        dt = time.time() - t0
        if verbose:
            print(
                f"{utc_now_iso()} FETCH status=ERR dur_ms={dt * 1000:.1f} "
                f"bytes=0 url={url} error=error:{exc}",
                flush=True,
            )
        return None, b"", dt, f"error:{exc}"


def http_get_text(url, timeout=10, verbose=False, follow_redirects=True):
    """Fetch URL and return (status, text, duration, error)."""
    status, data, dt, err = http_fetch(
        url,
        timeout=timeout,
        verbose=verbose,
        follow_redirects=follow_redirects,
    )
    return status, data.decode("utf-8", errors="replace"), dt, err


def api_request_json(url, method="GET", payload=None, timeout=10, verbose=False):
    """Make JSON API request and return parsed response."""
    body = None
    headers = {"User-Agent": UA}
    if payload is not None:
        body = json.dumps(payload).encode("utf-8")
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(url, data=body, method=method, headers=headers)
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        data = resp.read().decode("utf-8", errors="replace")
    if verbose:
        print(f"{utc_now_iso()} API {method} url={url} bytes={len(data)}", flush=True)
    return json.loads(data) if data else None


def inherit_parent_query(parent_url, child_url):
    """Inherit query parameters from parent URL to child URL."""
    parent = urllib.parse.urlsplit(parent_url)
    child = urllib.parse.urlsplit(child_url)
    if child.query or not parent.query:
        return child_url
    return urllib.parse.urlunsplit(
        (child.scheme, child.netloc, child.path, parent.query, child.fragment)
    )


def ensure_player_id(url, player_id):
    """Ensure URL has player_id query parameter."""
    split = urllib.parse.urlsplit(url)
    query = urllib.parse.parse_qs(split.query, keep_blank_values=True)
    if "player_id" not in query:
        query["player_id"] = [player_id]
    new_query = urllib.parse.urlencode(query, doseq=True)
    return urllib.parse.urlunsplit(
        (split.scheme, split.netloc, split.path, new_query, split.fragment)
    )


def is_h264_master(master_text):
    """Check if master playlist contains H.264 variants."""
    for line in master_text.splitlines():
        if "CODECS" in line and ("avc1" in line or "avc3" in line):
            return True
    return False


def pick_best_variant(master_text, base_url):
    """Pick highest bandwidth variant from master playlist."""
    lines = [x.strip() for x in master_text.splitlines()]
    best_bw = -1
    best_uri = None
    for i, line in enumerate(lines):
        if line.startswith("#EXT-X-STREAM-INF"):
            m = re.search(r"BANDWIDTH=(\d+)", line)
            bw = int(m.group(1)) if m else 0
            j = i + 1
            while j < len(lines) and (not lines[j] or lines[j].startswith("#")):
                j += 1
            if j < len(lines):
                uri = urllib.parse.urljoin(base_url, lines[j])
                uri = inherit_parent_query(base_url, uri)
                if bw > best_bw:
                    best_bw, best_uri = bw, uri
    return best_uri, best_bw


def parse_master_variants(master_text, base_url, player_id):
    """Parse all variants from master playlist."""
    lines = [x.strip() for x in master_text.splitlines()]
    variants = []
    for i, line in enumerate(lines):
        if line.startswith("#EXT-X-STREAM-INF"):
            j = i + 1
            while j < len(lines) and (not lines[j] or lines[j].startswith("#")):
                j += 1
            if j < len(lines):
                uri = urllib.parse.urljoin(base_url, lines[j])
                uri = inherit_parent_query(base_url, uri)
                uri = ensure_player_id(uri, player_id)
                uri = encode_url_path(uri)
                path = urllib.parse.urlsplit(uri).path
                parts = [p for p in path.split("/") if p]
                name = parts[-2] if len(parts) >= 2 else parts[-1]
                if parts:
                    base = parts[-1]
                    if base.endswith(".m3u8"):
                        base = base[: -len(".m3u8")]
                    if "_" in base:
                        name = base.split("_")[-1]
                if name:
                    variants.append({"name": name, "url": uri})
    # Deduplicate
    seen = set()
    unique = []
    for item in variants:
        if item["name"] in seen:
            continue
        seen.add(item["name"])
        unique.append(item)
    return unique


def parse_media_playlist(text, base_url, player_id):
    """Parse media playlist and return (segments, target_duration, endlist)."""
    lines = [x.strip() for x in text.splitlines()]
    segs = []
    target = 6
    endlist = False
    for line in lines:
        if not line:
            continue
        if line.startswith("#EXT-X-TARGETDURATION:"):
            try:
                target = max(1, int(line.split(":", 1)[1]))
            except Exception:
                pass
        elif line.startswith("#EXT-X-ENDLIST"):
            endlist = True
        elif not line.startswith("#"):
            seg_url = urllib.parse.urljoin(base_url, line)
            seg_url = inherit_parent_query(base_url, seg_url)
            seg_url = ensure_player_id(seg_url, player_id)
            segs.append(encode_url_path(seg_url))
    return segs, target, endlist


def find_session_by_player_id(api_base, player_id, timeout=12, verbose=False):
    """Find session by player_id with polling."""
    deadline = time.time() + timeout
    last_sessions = None
    while time.time() < deadline:
        url = f"{api_base}/api/sessions"
        try:
            sessions = api_request_json(url, timeout=10, verbose=verbose)
        except Exception:
            sessions = None
        last_sessions = sessions
        if isinstance(sessions, list):
            if verbose:
                raw_payload = json.dumps(sessions, default=str)
                if len(raw_payload) > 12000:
                    raw_payload = raw_payload[:12000] + " ...<truncated>"
                print(
                    f"{utc_now_iso()} SESSIONS raw_json={raw_payload}",
                    flush=True,
                )
                summary = []
                for item in sessions[:8]:
                    if not isinstance(item, dict):
                        continue
                    sid = item.get("session_id")
                    pid = item.get("player_id")
                    port = item.get("x_forwarded_port_external") or item.get("x_forwarded_port")
                    state = item.get("player_metrics_state")
                    summary.append(
                        f"sid={sid} pid={pid} port={port} state={state}"
                    )
                print(
                    f"{utc_now_iso()} SESSIONS poll expected_player_id={player_id} "
                    f"count={len(sessions)} sample=[{'; '.join(summary)}]",
                    flush=True,
                )
        elif verbose:
            payload_type = type(sessions).__name__
            payload_value = repr(sessions)
            if len(payload_value) > 2000:
                payload_value = payload_value[:2000] + " ...<truncated>"
            print(
                f"{utc_now_iso()} SESSIONS raw_nonlist type={payload_type} value={payload_value}",
                flush=True,
            )
        if isinstance(sessions, list):
            for session in sessions:
                if session.get("player_id") == player_id:
                    if verbose:
                        print(
                            f"{utc_now_iso()} SESSIONS match found session_id={session.get('session_id')} "
                            f"player_id={session.get('player_id')}",
                            flush=True,
                        )
                    return session
        time.sleep(0.4)
    if verbose:
        if isinstance(last_sessions, list):
            print(
                f"{utc_now_iso()} SESSIONS no match for player_id={player_id}; "
                f"last_count={len(last_sessions)}",
                flush=True,
            )
            for item in last_sessions[:20]:
                if not isinstance(item, dict):
                    continue
                sid = item.get("session_id")
                pid = item.get("player_id")
                port = item.get("x_forwarded_port_external") or item.get("x_forwarded_port")
                state = item.get("player_metrics_state")
                print(
                    f"{utc_now_iso()} SESSIONS item sid={sid} pid={pid} port={port} state={state}",
                    flush=True,
                )
        else:
            print(
                f"{utc_now_iso()} SESSIONS no match for player_id={player_id}; "
                f"last_payload_type={type(last_sessions).__name__}",
                flush=True,
            )
    return None


def find_session_by_player_id_sse(api_base, player_id, timeout=12, verbose=False):
    """Find session by player_id via SSE stream (/api/sessions/stream)."""
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
    deadline = time.time() + max(1, float(timeout))

    def _player_match(candidate):
        if candidate is None:
            return False
        cand = str(candidate)
        target = str(player_id)
        return cand == target or cand.startswith(target) or target.startswith(cand)

    buffer = []
    try:
        with urllib.request.urlopen(req, timeout=max(5, int(timeout) + 3)) as resp:
            if verbose:
                print(f"{utc_now_iso()} SSE connected url={url}", flush=True)
            while time.time() < deadline:
                raw = resp.readline()
                if not raw:
                    break
                line = raw.decode("utf-8", errors="replace").rstrip("\r\n")
                if line:
                    buffer.append(line)
                    continue

                event_name = "message"
                data_lines = []
                for item in buffer:
                    if item.startswith("event:"):
                        event_name = item.split(":", 1)[1].strip() or "message"
                    elif item.startswith("data:"):
                        data_lines.append(item[5:].lstrip())
                buffer = []

                if not data_lines:
                    continue
                payload_raw = "\n".join(data_lines)
                if verbose:
                    trimmed_payload = payload_raw
                    if len(trimmed_payload) > 12000:
                        trimmed_payload = trimmed_payload[:12000] + " ...<truncated>"
                    print(
                        f"{utc_now_iso()} SSE raw_event event={event_name} data={trimmed_payload}",
                        flush=True,
                    )
                try:
                    payload = json.loads(payload_raw)
                except json.JSONDecodeError:
                    if verbose:
                        trimmed = payload_raw[:300] + (" ...<truncated>" if len(payload_raw) > 300 else "")
                        print(f"{utc_now_iso()} SSE decode_error event={event_name} data={trimmed}", flush=True)
                    continue

                sessions = payload.get("sessions") if isinstance(payload, dict) else None
                if not isinstance(sessions, list):
                    continue

                if verbose:
                    preview = []
                    for s in sessions[:8]:
                        if not isinstance(s, dict):
                            continue
                        preview.append(
                            f"sid={s.get('session_id')} pid={s.get('player_id')} "
                            f"port={s.get('x_forwarded_port_external') or s.get('x_forwarded_port')}"
                        )
                    print(
                        f"{utc_now_iso()} SSE sessions event={event_name} count={len(sessions)} "
                        f"expected_player_id={player_id} sample=[{'; '.join(preview)}]",
                        flush=True,
                    )

                for session in sessions:
                    if isinstance(session, dict) and _player_match(session.get("player_id")):
                        if verbose:
                            print(
                                f"{utc_now_iso()} SSE match found session_id={session.get('session_id')} "
                                f"player_id={session.get('player_id')}",
                                flush=True,
                            )
                        return session
    except Exception as exc:
        if verbose:
            print(f"{utc_now_iso()} SSE subscribe failed url={url} error={exc}", flush=True)
    return None


def fetch_session_snapshot(api_base, session_id, verbose=False):
    """Fetch session snapshot."""
    url = f"{api_base}/api/session/{session_id}"
    try:
        return api_request_json(url, timeout=10, verbose=verbose)
    except Exception as exc:
        if verbose:
            print(f"{utc_now_iso()} Session snapshot fetch failed: {exc}", flush=True)
        return None


def wait_for_transport_active(api_base, session_id, timeout=6, verbose=False):
    """Wait for transport fault to become active."""
    deadline = time.time() + timeout
    while time.time() < deadline:
        snapshot = fetch_session_snapshot(api_base, session_id, verbose=verbose)
        if snapshot and snapshot.get("transport_fault_active"):
            return snapshot
        time.sleep(0.25)
    return fetch_session_snapshot(api_base, session_id, verbose=verbose)


def apply_failure_settings(api_base, session_id, payload, verbose=False):
    """Apply failure settings to session."""
    url = f"{api_base}/api/failure-settings/{session_id}"
    return api_request_json(url, method="POST", payload=payload, timeout=10, verbose=verbose)


def apply_shaping(api_base, port, rate_mbps, delay_ms=0, loss_pct=0.0, verbose=False):
    """Apply network shaping."""
    url = f"{api_base}/api/nftables/shape/{port}"
    payload = {"rate_mbps": rate_mbps, "delay_ms": delay_ms, "loss_pct": loss_pct}
    return api_request_json(url, method="POST", payload=payload, timeout=10, verbose=verbose)


def base_failure_payload():
    """Return baseline failure payload with all failures disabled."""
    return {
        "segment_failure_type": "none",
        "segment_failure_frequency": 0,
        "segment_consecutive_failures": 0,
        "segment_failure_units": "requests",
        "segment_consecutive_units": "requests",
        "segment_frequency_units": "requests",
        "segment_failure_mode": "requests",
        "segment_failure_urls": ["All"],
        "manifest_failure_type": "none",
        "manifest_failure_frequency": 0,
        "manifest_consecutive_failures": 0,
        "manifest_failure_units": "requests",
        "manifest_consecutive_units": "requests",
        "manifest_frequency_units": "requests",
        "manifest_failure_mode": "requests",
        "manifest_failure_urls": ["All"],
        "master_manifest_failure_type": "none",
        "master_manifest_failure_frequency": 0,
        "master_manifest_consecutive_failures": 0,
        "master_manifest_failure_units": "requests",
        "master_manifest_consecutive_units": "requests",
        "master_manifest_frequency_units": "requests",
        "master_manifest_failure_mode": "requests",
        "transport_failure_type": "none",
        "transport_consecutive_failures": 0,
        "transport_failure_frequency": 0,
        "transport_failure_units": "seconds",
        "transport_consecutive_units": "seconds",
        "transport_frequency_units": "seconds",
        "transport_failure_mode": "failures_per_seconds",
    }


def classify_error(err):
    """Classify error string into category."""
    if not err:
        return None
    if err == "timeout":
        return "timeout"
    if err.startswith("url_error:"):
        reason = err.split(":", 1)[1].lower()
        if "reset" in reason:
            return "conn_reset"
        if "refused" in reason:
            return "conn_refused"
        if "timed out" in reason or "timeout" in reason:
            return "timeout"
        return "url_error"
    if err.startswith("error:"):
        return "error"
    if err == "http_error":
        return "http_error"
    return "error"


def record_failure(counters, kind, status, err):
    """Record failure in counters."""
    if status is not None and status >= 400:
        counters[f"{kind}_http_error"] += 1
        counters["http_error"] += 1
    if status is not None:
        counters[f"{kind}_status_{status}"] += 1
    label = classify_error(err)
    if label:
        counters[f"{kind}_{label}"] += 1
        counters[label] += 1


def record_success(counters, kind, status, err):
    """Record success in counters."""
    if status == 200 and not err:
        counters[f"{kind}_success"] += 1


def run_probe_window(
    url,
    config,
    duration_s,
    verbose_label="PROBE",
    timeout_override=None,
    segment_inspector=None,
    stop_on_failure=False,
):
    """
    Run probe window fetching segments for specified duration.

    Returns Counter with failure/success counts.
    """
    counters = Counter()
    t_wall0 = time.time()
    recent_fetches = deque()

    current_segments = []
    current_target = 6
    next_manifest_refresh = 0.0
    rr_index = 0

    while True:
        if time.time() - t_wall0 > duration_s:
            break

        now = time.time()
        if now >= next_manifest_refresh or not current_segments:
            timeout = timeout_override or config.timeout
            status, media_text, _, err = http_get_text(url, timeout=timeout, verbose=config.verbose)
            if status != 200:
                record_failure(counters, "manifest", status, err)
                if config.verbose:
                    print(
                        f"{utc_now_iso()} {verbose_label} manifest status={status} error={err}",
                        flush=True,
                    )
                if stop_on_failure:
                    return counters
                time.sleep(0.25)
                continue
            record_success(counters, "manifest", status, err)
            segs, target, _ = parse_media_playlist(media_text, url, config.player_id if hasattr(config, 'player_id') else "test")
            if segs:
                current_segments = segs
                current_target = target
                rr_index = rr_index % len(current_segments)
            next_manifest_refresh = now + max(0.5, current_target / 2)

        if not current_segments:
            time.sleep(0.25)
            continue

        seg_url = current_segments[rr_index]
        rr_index = (rr_index + 1) % len(current_segments)

        timeout = timeout_override or config.timeout
        status, data, dt, err = http_fetch(seg_url, timeout=timeout, verbose=config.verbose)
        record_failure(counters, "segment", status, err)
        if stop_on_failure and (err or (status is not None and status >= 400)):
            return counters
        record_success(counters, "segment", status, err)
        if segment_inspector and status == 200 and data:
            if segment_inspector(data):
                counters["segment_corrupted"] += 1
                if stop_on_failure:
                    return counters

        recent_fetches.append((time.time(), len(data)))
        cutoff = time.time() - 1.0
        while recent_fetches and recent_fetches[0][0] < cutoff:
            recent_fetches.popleft()
        rolling_1s_bytes = sum(item[1] for item in recent_fetches)
        rolling_1s_mbps = (rolling_1s_bytes * 8) / 1e6

        if rolling_1s_mbps < config.throttle_mbps:
            counters["throttle"] += 1

    return counters


def run_manifest_window(url, config, duration_s, verbose_label="MANIFEST", timeout_override=None, stop_on_failure=False):
    """Run manifest polling window."""
    counters = Counter()
    t_wall0 = time.time()
    while True:
        if time.time() - t_wall0 > duration_s:
            break
        timeout = timeout_override or config.timeout
        status, _, _, err = http_get_text(url, timeout=timeout, verbose=config.verbose)
        record_failure(counters, "manifest", status, err)
        if stop_on_failure and (err or (status is not None and status >= 400)):
            return counters
        record_success(counters, "manifest", status, err)
        if config.verbose:
            status_label = status if status is not None else "ERR"
            print(
                f"{utc_now_iso()} {verbose_label} status={status_label} error={err}",
                flush=True,
            )
        time.sleep(0.25)
    return counters


def run_simple_window(kind, url, config, duration_s, verbose_label="MASTER", timeout_override=None, stop_on_failure=False):
    """Run simple request window for master manifests."""
    counters = Counter()
    t_wall0 = time.time()
    while True:
        if time.time() - t_wall0 > duration_s:
            break
        timeout = timeout_override or config.timeout
        status, _, _, err = http_get_text(url, timeout=timeout, verbose=config.verbose)
        record_failure(counters, kind, status, err)
        if stop_on_failure and (err or (status is not None and status >= 400)):
            return counters
        record_success(counters, kind, status, err)
        if config.verbose:
            status_label = status if status is not None else "ERR"
            print(
                f"{utc_now_iso()} {verbose_label} status={status_label} error={err}",
                flush=True,
            )
        time.sleep(0.25)
    return counters
