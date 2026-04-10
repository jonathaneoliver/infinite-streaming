#!/usr/bin/env python3
import argparse
from collections import Counter, deque
from datetime import datetime, timezone
import json
import http.client
import os
import random
import re
import socket
import time
import uuid
import urllib.error
import urllib.parse
import urllib.request

UA = "hls-failure-probe/1.0"
LAST_FAILED_PATH = os.path.join(os.path.dirname(__file__), ".hls_failure_probe_last_failed")


def utc_now_iso():
    return datetime.now(timezone.utc).isoformat(timespec="milliseconds").replace("+00:00", "Z")


def log_line(message):
    print(f"{utc_now_iso()} {message}", flush=True)


def load_last_failed(path):
    try:
        with open(path, "r", encoding="utf-8") as handle:
            value = handle.read().strip()
        return value or None
    except FileNotFoundError:
        return None
    except OSError:
        return None


def save_last_failed(path, test_id):
    try:
        with open(path, "w", encoding="utf-8") as handle:
            handle.write(test_id)
    except OSError:
        pass


def display_kind(kind):
    return kind


def display_counter_name(key):
    return key


def _read_response(resp, max_bytes):
    data_parts = []
    bytes_read = 0
    read_err = None
    while True:
        to_read = 64 * 1024
        if max_bytes is not None:
            remaining = max_bytes - bytes_read
            if remaining <= 0:
                break
            to_read = min(to_read, remaining)
        try:
            chunk = resp.read(to_read)
        except http.client.IncompleteRead as exc:
            chunk = exc.partial
            read_err = "incomplete_read"
        except Exception as exc:
            chunk = b""
            read_err = f"read_error:{exc}"
        if chunk:
            data_parts.append(chunk)
            bytes_read += len(chunk)
        if not chunk or read_err:
            break
    return b"".join(data_parts), bytes_read, read_err


def http_fetch_with_info(url, timeout=20, verbose=False, max_bytes=None):
    info = {"headers": False, "bytes": 0, "status": None}
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
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            info["headers"] = True
            status = getattr(resp, "status", resp.getcode())
            info["status"] = status
            data, bytes_read, read_err = _read_response(resp, max_bytes)
            info["bytes"] = bytes_read
        dt = time.time() - t0
        err = None
        if read_err:
            err = read_err
        if verbose:
            print(
                f"{utc_now_iso()} FETCH status={status} dur_ms={dt * 1000:.1f} bytes={len(data)} url={url}",
                flush=True,
            )
        return status, data, dt, err, info
    except urllib.error.HTTPError as exc:
        dt = time.time() - t0
        info["headers"] = True
        info["status"] = exc.code
        data, bytes_read, read_err = _read_response(exc, max_bytes)
        info["bytes"] = bytes_read
        if verbose:
            print(
                f"{utc_now_iso()} FETCH status={exc.code} dur_ms={dt * 1000:.1f} bytes={len(data)} url={url} error=http_error",
                flush=True,
            )
        err = "http_error"
        if read_err:
            err = read_err
        return exc.code, data, dt, err, info
    except (socket.timeout, TimeoutError):
        dt = time.time() - t0
        if verbose:
            print(
                f"{utc_now_iso()} FETCH status=ERR dur_ms={dt * 1000:.1f} bytes=0 url={url} error=timeout",
                flush=True,
            )
        return None, b"", dt, "timeout", info
    except urllib.error.URLError as exc:
        dt = time.time() - t0
        reason = str(exc.reason)
        if verbose:
            print(
                f"{utc_now_iso()} FETCH status=ERR dur_ms={dt * 1000:.1f} bytes=0 url={url} error=url_error:{reason}",
                flush=True,
            )
        return None, b"", dt, f"url_error:{reason}", info
    except Exception as exc:
        dt = time.time() - t0
        if verbose:
            print(
                f"{utc_now_iso()} FETCH status=ERR dur_ms={dt * 1000:.1f} bytes=0 url={url} error=error:{exc}",
                flush=True,
            )
        return None, b"", dt, f"error:{exc}", info


def http_fetch(url, timeout=20, verbose=False, max_bytes=None):
    status, data, dt, err, _ = http_fetch_with_info(
        url, timeout=timeout, verbose=verbose, max_bytes=max_bytes
    )
    return status, data, dt, err


def http_get_text(url, timeout=10, verbose=False):
    status, data, dt, err = http_fetch(url, timeout=timeout, verbose=verbose)
    return status, data.decode("utf-8", errors="replace"), dt, err


def response_shape_expectation(failure_type):
    if "connect_" in failure_type:
        return {"headers": False, "min_bytes": 0, "max_bytes": 0}
    if "first_byte_" in failure_type:
        return {"headers": True, "min_bytes": 0, "max_bytes": 0}
    if "body_" in failure_type:
        return {"headers": True, "min_bytes": 1, "max_bytes": None}
    return None


def check_response_shape(info, failure_type):
    expected = response_shape_expectation(failure_type)
    if not expected:
        return True, None
    headers = bool(info.get("headers"))
    bytes_read = int(info.get("bytes", 0))
    if headers != expected["headers"]:
        return False, f"headers={headers} expected={expected['headers']}"
    if bytes_read < expected["min_bytes"]:
        return False, f"bytes={bytes_read} expected>={expected['min_bytes']}"
    max_bytes = expected["max_bytes"]
    if max_bytes is not None and bytes_read > max_bytes:
        return False, f"bytes={bytes_read} expected<={max_bytes}"
    return True, None


def inherit_parent_query(parent_url, child_url):
    parent = urllib.parse.urlsplit(parent_url)
    child = urllib.parse.urlsplit(child_url)
    if child.query or not parent.query:
        return child_url
    return urllib.parse.urlunsplit(
        (child.scheme, child.netloc, child.path, parent.query, child.fragment)
    )


def ensure_player_id(url, player_id):
    split = urllib.parse.urlsplit(url)
    query = urllib.parse.parse_qs(split.query, keep_blank_values=True)
    if "player_id" not in query:
        query["player_id"] = [player_id]
    new_query = urllib.parse.urlencode(query, doseq=True)
    return urllib.parse.urlunsplit(
        (split.scheme, split.netloc, split.path, new_query, split.fragment)
    )


def api_request_json(url, method="GET", payload=None, timeout=10, verbose=False):
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


def find_session_by_player_id(api_base, player_id, timeout=12, verbose=False):
    deadline = time.time() + timeout
    while time.time() < deadline:
        url = f"{api_base}/api/sessions"
        try:
            sessions = api_request_json(url, timeout=10, verbose=verbose)
        except Exception:
            sessions = None
        if isinstance(sessions, list):
            for session in sessions:
                if session.get("player_id") == player_id:
                    return session
        time.sleep(0.4)
    return None


def fetch_session_snapshot(api_base, session_id, verbose=False):
    url = f"{api_base}/api/session/{session_id}"
    try:
        return api_request_json(url, timeout=10, verbose=verbose)
    except Exception as exc:
        log_line(f"session snapshot fetch failed: {exc}")
        return None


def fetch_session_snapshot_with_fallback(api_base, session_id, player_id, verbose=False):
    snapshot = fetch_session_snapshot(api_base, session_id, verbose=verbose)
    if snapshot is not None or not player_id:
        return snapshot
    session = find_session_by_player_id(api_base, player_id, verbose=verbose)
    if not session:
        return None
    fallback_id = session.get("session_id")
    if not fallback_id:
        return None
    return fetch_session_snapshot(api_base, fallback_id, verbose=verbose)


def log_session_snapshot(api_base, session_id, verbose=False):
    snapshot = fetch_session_snapshot(api_base, session_id, verbose=verbose)
    if not snapshot:
        log_line("SESSION SNAPSHOT: empty")
        return
    pretty = json.dumps(snapshot, indent=2, sort_keys=True)
    log_line("SESSION SNAPSHOT BEGIN")
    for line in pretty.splitlines():
        log_line(f"SESSION {line}")
    log_line("SESSION SNAPSHOT END")


def wait_for_transport_active(api_base, session_id, timeout=6, verbose=False):
    deadline = time.time() + timeout
    while time.time() < deadline:
        snapshot = fetch_session_snapshot(api_base, session_id, verbose=verbose)
        if snapshot and snapshot.get("transport_fault_active"):
            return snapshot
        time.sleep(0.25)
    return fetch_session_snapshot(api_base, session_id, verbose=verbose)


def pick_best_variant(master_text, base_url):
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


def encode_url_path(url):
    split = urllib.parse.urlsplit(url)
    safe_path = urllib.parse.quote(split.path, safe="/:%")
    return urllib.parse.urlunsplit(
        (split.scheme, split.netloc, safe_path, split.query, split.fragment)
    )


def parse_media_playlist(text, base_url, player_id):
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


def parse_master_variants(master_text, base_url, player_id):
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
    seen = set()
    unique = []
    for item in variants:
        if item["name"] in seen:
            continue
        seen.add(item["name"])
        unique.append(item)
    return unique


def select_variant_groups(variants):
    names = [v["name"] for v in variants]
    if len(names) < 2:
        return names, []
    selected = names[:2]
    unselected = names[2:]
    return selected, unselected


def derive_segment_scope_key(segment_url, variant_name):
    if not segment_url:
        return None
    path = urllib.parse.urlsplit(segment_url).path
    parts = [p for p in path.split("/") if p]
    if not parts:
        return None
    name_lower = (variant_name or "").lower()
    if name_lower:
        for part in reversed(parts):
            if name_lower in part.lower():
                return part
    if len(parts) >= 2:
        return parts[-2]
    return parts[-1]


def build_segment_scope_keys(variants, args):
    keys = {}
    for variant in variants:
        seg_url = get_first_segment_url(variant["url"], args)
        key = derive_segment_scope_key(seg_url, variant.get("name"))
        if key:
            keys[variant["name"]] = key
    return keys


def get_first_segment_url(playlist_url, args):
    status, text, _, err = http_get_text(playlist_url, timeout=args.timeout, verbose=args.verbose)
    if status != 200 or err:
        return None
    segs, _, _ = parse_media_playlist(text, playlist_url, args.player_id)
    return segs[0] if segs else None


def probe_failure_shape(kind, url, args, failure_type):
    target_url = url
    if kind == "segment":
        seg_url = get_first_segment_url(url, args)
        if not seg_url:
            return False, "no segment url to probe"
        target_url = seg_url
    status, _, _, err, info = http_fetch_with_info(
        target_url, timeout=args.timeout, verbose=args.verbose, max_bytes=256 * 1024
    )
    ok, reason = check_response_shape(info, failure_type)
    if not ok:
        return False, f"shape_mismatch {reason} status={status} err={err}"
    return True, None


def is_failure_observed(status, err, data=None, expect_corrupted=False):
    if expect_corrupted and status == 200 and data:
        return all(b == 0 for b in data)
    if err or status is None:
        return True
    return status >= 400


def check_variant_scope(
    kind,
    variants,
    selected,
    unselected,
    args,
    expect_corrupted=False,
    window_seconds=6,
    require_all_failures=True,
    min_failures=1,
    pass_before_fail=False,
    require_recovery=False,
):
    if not variants:
        return True, None

    url_map = {v["name"]: v["url"] for v in variants}
    last_attempt = {}
    log_line(f"variant scope: kind={kind} fail={selected} pass={unselected}")

    def request_once(variant_name):
        url = url_map.get(variant_name)
        if not url:
            return None, None, None
        if kind == "manifest":
            last_attempt[variant_name] = {"manifest": url, "segment": None}
            status, _, _, err = http_get_text(url, timeout=args.timeout, verbose=args.verbose)
            return status, err, None
        seg_url = get_first_segment_url(url, args)
        if not seg_url:
            return None, None, None
        last_attempt[variant_name] = {"manifest": url, "segment": seg_url}
        status, data, _, err = http_fetch(seg_url, timeout=args.timeout, verbose=args.verbose)
        return status, err, data

    def wait_for_failure(variant_name):
        deadline = time.time() + window_seconds
        while time.time() < deadline:
            status, err, data = request_once(variant_name)
            if status is None and err is None:
                time.sleep(0.25)
                continue
            if is_failure_observed(status, err, data=data, expect_corrupted=expect_corrupted):
                return True
            time.sleep(0.25)
        return False

    def ensure_success(variant_name, attempts=2):
        for _ in range(attempts):
            status, err, data = request_once(variant_name)
            if status != 200 or err:
                return False
            if expect_corrupted and data and all(b == 0 for b in data):
                return False
        return True

    def wait_for_recovery(variant_name, timeout_s):
        deadline = time.time() + timeout_s
        while time.time() < deadline:
            if ensure_success(variant_name, attempts=1):
                return True
            time.sleep(0.25)
        return False

    passed = []
    passed_miss = []
    if pass_before_fail:
        for name in unselected:
            if ensure_success(name):
                passed.append(name)
            else:
                passed_miss.append(name)

    failed = []
    failed_miss = []
    for name in selected:
        if wait_for_failure(name):
            failed.append(name)
        else:
            failed_miss.append(name)

    if not pass_before_fail:
        for name in unselected:
            if ensure_success(name):
                passed.append(name)
            else:
                passed_miss.append(name)

    if require_recovery:
        recovery_window = max(2.0, min(6.0, window_seconds))
        for name in failed:
            if not wait_for_recovery(name, recovery_window):
                passed_miss.append(name)

    log_line(f"variant scope results: fail_ok={failed} fail_miss={failed_miss}")
    log_line(f"variant scope results: pass_ok={passed} pass_miss={passed_miss}")

    for name in failed_miss:
        attempt = last_attempt.get(name, {})
        log_line(
            "variant scope miss: "
            f"kind={kind} variant={name} manifest={attempt.get('manifest')} segment={attempt.get('segment')}"
        )
    for name in passed_miss:
        attempt = last_attempt.get(name, {})
        log_line(
            "variant scope unexpected fail: "
            f"kind={kind} variant={name} manifest={attempt.get('manifest')} segment={attempt.get('segment')}"
        )

    if require_all_failures and failed_miss:
        return False, f"selected variants did not fail: {failed_miss}"
    if not require_all_failures and len(failed) < min_failures:
        return False, f"expected at least {min_failures} selected variants to fail"
    if passed_miss:
        return False, f"unselected variants did not succeed: {passed_miss}"

    return True, None


def is_h264_master(master_text):
    for line in master_text.splitlines():
        if "CODECS" in line and ("avc1" in line or "avc3" in line):
            return True
    return False


def build_base_urls(args):
    if args.api_base:
        api_base = args.api_base.rstrip("/")
    else:
        api_base = f"{args.scheme}://{args.host}:{args.api_port}"
    if args.hls_base:
        hls_base = args.hls_base.rstrip("/")
    else:
        hls_base = f"{args.scheme}://{args.host}:{args.hls_port}"
    return api_base, hls_base


def select_auto_url(args):
    api_base, hls_base = build_base_urls(args)
    content_url = f"{api_base}/api/content"
    if args.verbose:
        print(f"{utc_now_iso()} AUTO content_url={content_url}", flush=True)
    status, text, _, err = http_get_text(content_url, timeout=10, verbose=args.verbose)
    if status != 200:
        detail = err or f"status={status}"
        raise SystemExit(f"Failed to fetch content list: {content_url} ({detail})")

    try:
        items = json.loads(text)
    except json.JSONDecodeError as exc:
        raise SystemExit(f"Invalid JSON from {content_url}: {exc}")

    if not isinstance(items, list):
        raise SystemExit(f"Unexpected content list from {content_url}")

    candidates = [x for x in items if x.get("has_hls")]
    candidates.sort(key=lambda x: x.get("name", ""))
    if not candidates:
        raise SystemExit("No HLS content returned from /api/content")

    for item in candidates:
        name = item.get("name")
        if not name:
            continue
        safe_name = urllib.parse.quote(name, safe="")
        master_url = f"{hls_base}/go-live/{safe_name}/master_6s.m3u8"
        master_url = ensure_player_id(master_url, args.player_id)
        if args.verbose:
            print(f"{utc_now_iso()} AUTO probe={master_url}", flush=True)
        status, master_text, _, _ = http_get_text(master_url, timeout=10, verbose=args.verbose)
        if status != 200:
            continue
        if not is_h264_master(master_text):
            continue
        return master_url, name, bool(item.get("has_dash"))

    raise SystemExit("No 6s HLS H264 content found via /api/content")


def classify_error(err):
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


def validate_expectations(counters, args):
    missing = []
    if counters["http_error"] < args.expect_http:
        missing.append(
            f"http errors {counters['http_error']} < expected {args.expect_http}"
        )
    if counters["timeout"] < args.expect_timeouts:
        missing.append(
            f"timeouts {counters['timeout']} < expected {args.expect_timeouts}"
        )
    if counters["conn_reset"] < args.expect_resets:
        missing.append(
            f"connection resets {counters['conn_reset']} < expected {args.expect_resets}"
        )
    if counters["throttle"] < args.expect_throttle:
        missing.append(
            f"throttle events {counters['throttle']} < expected {args.expect_throttle}"
        )
    drop_reject = counters["timeout"] + counters["conn_reset"] + counters["conn_refused"]
    if drop_reject < args.expect_drop_reject:
        missing.append(
            f"drop/reject events {drop_reject} < expected {args.expect_drop_reject}"
        )
    return missing


def record_failure(counters, kind, status, err):
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
    if status == 200 and not err:
        counters[f"{kind}_success"] += 1


def summarize_failure_counts(counters):
    keys = [
        "master_manifest_http_error",
        "master_manifest_timeout",
        "master_manifest_conn_reset",
        "master_manifest_conn_refused",
        "manifest_http_error",
        "manifest_timeout",
        "manifest_conn_reset",
        "manifest_conn_refused",
        "segment_http_error",
        "segment_timeout",
        "segment_conn_reset",
        "segment_conn_refused",
        "segment_corrupted",
        "throttle",
    ]
    parts = [f"{display_counter_name(key)}={counters.get(key, 0)}" for key in keys]
    return ", ".join(parts)


def run_probe_window(
    url,
    args,
    duration_s,
    verbose_label="PROBE",
    timeout_override=None,
    segment_inspector=None,
    stop_on_failure=False,
    max_bytes=None,
):
    bytes_total = 0
    dl_time_total = 0.0
    fetch_count = 0
    manifest_refresh_count = 0
    t_wall0 = time.time()
    recent_fetches = deque()
    counters = Counter()

    current_segments = []
    current_target = 6
    next_manifest_refresh = 0.0
    rr_index = 0

    while True:
        if time.time() - t_wall0 > duration_s:
            break

        now = time.time()
        if now >= next_manifest_refresh or not current_segments:
            timeout = timeout_override or args.timeout
            status, media_text, _, err = http_get_text(url, timeout=timeout, verbose=args.verbose)
            if status != 200:
                record_failure(counters, "manifest", status, err)
                print(
                    f"{utc_now_iso()} {verbose_label} manifest status={status} error={err}",
                    flush=True,
                )
                if stop_on_failure:
                    return counters
                time.sleep(0.25)
                continue
            record_success(counters, "manifest", status, err)
            segs, target, _ = parse_media_playlist(media_text, url, args.player_id)
            if segs:
                current_segments = segs
                current_target = target
                rr_index = rr_index % len(current_segments)
            manifest_refresh_count += 1
            next_manifest_refresh = now + max(0.5, current_target / 2)
            if args.verbose:
                print(
                    f"{utc_now_iso()} {verbose_label} manifest refresh={manifest_refresh_count} target_s={current_target} segments={len(current_segments)}",
                    flush=True,
                )

        if not current_segments:
            time.sleep(0.25)
            continue

        seg_url = current_segments[rr_index]
        rr_index = (rr_index + 1) % len(current_segments)

        timeout = timeout_override or args.timeout
        status, data, dt, err = http_fetch(seg_url, timeout=timeout, verbose=args.verbose, max_bytes=max_bytes)
        fetch_count += 1
        record_failure(counters, "segment", status, err)
        if stop_on_failure and (err or (status is not None and status >= 400)):
            return counters
        record_success(counters, "segment", status, err)
        if segment_inspector and status == 200 and data:
            if segment_inspector(data):
                counters["segment_corrupted"] += 1
                if stop_on_failure:
                    return counters
        bytes_total += len(data)
        dl_time_total += max(dt, 1e-6)
        recent_fetches.append((time.time(), len(data)))

        cutoff = time.time() - 1.0
        while recent_fetches and recent_fetches[0][0] < cutoff:
            recent_fetches.popleft()
        rolling_1s_bytes = sum(item[1] for item in recent_fetches)
        rolling_1s_mbps = (rolling_1s_bytes * 8) / 1e6

        if rolling_1s_mbps < args.throttle_mbps:
            counters["throttle"] += 1

        if args.verbose:
            wall_elapsed = max(time.time() - t_wall0, 1e-6)
            xfer_mbps = (bytes_total * 8) / dl_time_total / 1e6 if dl_time_total > 0 else 0
            wall_mbps = (bytes_total * 8) / wall_elapsed / 1e6
            status_label = status if status is not None else "ERR"
            print(
                f"{verbose_label} fetches={fetch_count:5d} status={status_label} roll1s={rolling_1s_mbps:7.3f} Mbps xfer={xfer_mbps:7.3f} Mbps wall={wall_mbps:7.3f} Mbps",
                flush=True,
            )

    return counters


def run_manifest_window(url, args, duration_s, verbose_label="MANIFEST", timeout_override=None, stop_on_failure=False):
    counters = Counter()
    t_wall0 = time.time()
    while True:
        if time.time() - t_wall0 > duration_s:
            break
        timeout = timeout_override or args.timeout
        status, _, _, err = http_get_text(url, timeout=timeout, verbose=args.verbose)
        record_failure(counters, "manifest", status, err)
        if stop_on_failure and (err or (status is not None and status >= 400)):
            return counters
        record_success(counters, "manifest", status, err)
        if args.verbose:
            status_label = status if status is not None else "ERR"
            print(
                f"{utc_now_iso()} {verbose_label} status={status_label} error={err}",
                flush=True,
            )
        time.sleep(0.25)
    return counters


def run_simple_window(kind, url, args, duration_s, verbose_label="MASTER", timeout_override=None, stop_on_failure=False):
    counters = Counter()
    t_wall0 = time.time()
    while True:
        if time.time() - t_wall0 > duration_s:
            break
        timeout = timeout_override or args.timeout
        status, _, _, err = http_get_text(url, timeout=timeout, verbose=args.verbose)
        record_failure(counters, kind, status, err)
        if stop_on_failure and (err or (status is not None and status >= 400)):
            return counters
        record_success(counters, kind, status, err)
        if args.verbose:
            status_label = status if status is not None else "ERR"
            print(
                f"{utc_now_iso()} {verbose_label} status={status_label} error={err}",
                flush=True,
            )
        time.sleep(0.25)
    return counters


def apply_failure_settings(api_base, session_id, payload, verbose=False):
    url = f"{api_base}/api/failure-settings/{session_id}"
    return api_request_json(url, method="POST", payload=payload, timeout=10, verbose=verbose)


def apply_shaping(api_base, port, rate_mbps, delay_ms=0, loss_pct=0.0, verbose=False):
    url = f"{api_base}/api/nftables/shape/{port}"
    payload = {"rate_mbps": rate_mbps, "delay_ms": delay_ms, "loss_pct": loss_pct}
    return api_request_json(url, method="POST", payload=payload, timeout=10, verbose=verbose)


def base_failure_payload():
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


def run_failure_tests(url, api_base, session, args, manifest_url=None, master_url=None, variants=None):
    session_id = session.get("session_id")
    session_port = session.get("x_forwarded_port")
    if not session_id:
        raise SystemExit("Session ID missing from /api/sessions")

    # Reset to a clean baseline to avoid inheriting state from prior runs.
    apply_failure_settings(api_base, session_id, base_failure_payload(), verbose=args.verbose)
    if session_port:
        apply_shaping(api_base, session_port, args.restore_mbps, verbose=args.verbose)

    continue_path = args.continue_file or LAST_FAILED_PATH
    resume_id = None
    if args.continue_from:
        resume_id = args.continue_from
        log_line(f"RESUME from provided test id: {resume_id}")
    elif args.continue_tests:
        resume_id = load_last_failed(continue_path)
        if resume_id:
            log_line(f"RESUME from failed test id: {resume_id}")
        else:
            log_line(f"RESUME requested but no saved id found at {continue_path}")
    resume_pending = bool(resume_id)

    def record_test_failure(test_id, reason):
        log_line(f"TEST FAILED {test_id}: {reason}")
        save_last_failed(continue_path, test_id)
        log_session_snapshot(api_base, session_id, verbose=args.verbose)

    def log_test_result(test_id, status, reason=None):
        message = f"RESULT {test_id}: {status}"
        if reason:
            message = f"{message} ({reason})"
        log_line(message)

    http_failures = ["404", "403", "500", "timeout", "connection_refused", "dns_failure", "rate_limiting"]
    socket_resets = ["request_connect_reset", "request_first_byte_reset", "request_body_reset"]
    socket_hangs = ["request_connect_hang", "request_first_byte_hang", "request_body_hang"]
    socket_delays = ["request_connect_delayed", "request_first_byte_delayed", "request_body_delayed"]

    modes = [
        {
            "label": "requests",
            "mode": "requests",
            "consecutive_units": "requests",
            "frequency_units": "requests",
        },
        {
            "label": "seconds",
            "mode": "seconds",
            "consecutive_units": "seconds",
            "frequency_units": "seconds",
        },
        {
            "label": "failures_per_seconds",
            "mode": "failures_per_seconds",
            "consecutive_units": "requests",
            "frequency_units": "seconds",
        },
    ]
    instant_schedule = {"label": "c0_f0", "consecutive": 0, "frequency": 0, "manual": True}
    expanded_schedules = [
        instant_schedule,
        {"label": "c1_f0", "consecutive": 1, "frequency": 0, "manual": False},
        {"label": "c1_f10", "consecutive": 1, "frequency": 10, "manual": False},
    ]

    def make_http_test(kind, failure_type, schedule, mode, variant_scope=None):
        expected_status = {
            "404": 404,
            "403": 403,
            "500": 500,
            "timeout": 504,
            "connection_refused": 503,
            "dns_failure": 502,
            "rate_limiting": 429,
        }.get(failure_type)
        return {
            "name": f"{kind}_{failure_type}_{mode['label']}_{schedule['label']}{'_' + variant_scope if variant_scope else ''}",
            "desc": f"{display_kind(kind).replace('_', ' ')} failure {failure_type} ({mode['label']} {schedule['label']})",
            "payload": {
                f"{kind}_failure_type": failure_type,
                f"{kind}_consecutive_failures": schedule["consecutive"],
                f"{kind}_failure_frequency": schedule["frequency"],
                f"{kind}_failure_units": mode["consecutive_units"],
                f"{kind}_consecutive_units": mode["consecutive_units"],
                f"{kind}_frequency_units": mode["frequency_units"],
                f"{kind}_failure_mode": mode["mode"],
            },
            "expect": {f"{kind}_http_error": args.expect_http},
            "expected_status": expected_status,
            "expected_duration": 2,
            "kind": kind,
            "mode": mode,
            "schedule": schedule,
            "variant_scope": variant_scope,
        }

    def make_socket_test(kind, failure_type, expected_key, expected_count, schedule, mode, variant_scope=None):
        is_delay = "delayed" in failure_type
        is_hang = "hang" in failure_type
        window = 24 if is_hang else 14 if is_delay else args.test_seconds
        shape_failure_type = None
        if response_shape_expectation(failure_type):
            shape_failure_type = failure_type
        return {
            "name": f"{kind}_{failure_type}_{mode['label']}_{schedule['label']}{'_' + variant_scope if variant_scope else ''}",
            "desc": f"{display_kind(kind).replace('_', ' ')} socket failure {failure_type} ({mode['label']} {schedule['label']})",
            "payload": {
                f"{kind}_failure_type": failure_type,
                f"{kind}_consecutive_failures": schedule["consecutive"],
                f"{kind}_failure_frequency": schedule["frequency"],
                f"{kind}_failure_units": mode["consecutive_units"],
                f"{kind}_consecutive_units": mode["consecutive_units"],
                f"{kind}_frequency_units": mode["frequency_units"],
                f"{kind}_failure_mode": mode["mode"],
            },
            "expect_any_groups": [[expected_key, f"{kind}_http_error"]],
            "expect_any_count": expected_count,
            "window_seconds": window,
            "timeout_override": window,
            "expected_duration": window,
            "kind": kind,
            "mode": mode,
            "schedule": schedule,
            "variant_scope": variant_scope,
            "fault_counter": f"fault_count_{failure_type}",
            "allow_precheck_fail": is_delay or is_hang,
            "shape_failure_type": shape_failure_type,
        }

    tests = []
    variant_scopes = []
    if variants:
        variant_scopes = ["all", "subset"]
    for kind in ["manifest", "segment"]:
        base_mode = modes[0]
        for failure_type in http_failures:
            tests.append(make_http_test(kind, failure_type, instant_schedule, base_mode, variant_scope=None))
        for failure_type in socket_resets:
            tests.append(
                make_socket_test(
                    kind,
                    failure_type,
                    f"{kind}_conn_reset",
                    args.expect_resets,
                    instant_schedule,
                    base_mode,
                    variant_scope=None,
                )
            )
        for failure_type in socket_hangs:
            tests.append(
                make_socket_test(
                    kind,
                    failure_type,
                    f"{kind}_timeout",
                    args.expect_timeouts,
                    instant_schedule,
                    base_mode,
                    variant_scope=None,
                )
            )
        for failure_type in socket_delays:
            tests.append(
                make_socket_test(
                    kind,
                    failure_type,
                    f"{kind}_timeout",
                    args.expect_timeouts,
                    instant_schedule,
                    base_mode,
                    variant_scope=None,
                )
            )
        if kind == "segment":
            tests.append(
                {
                    "name": "segment_corrupted_c0_f0",
                    "desc": "segment corrupted zero-fill (instant)",
                    "payload": {
                        "segment_failure_type": "corrupted",
                        "segment_consecutive_failures": instant_schedule["consecutive"],
                        "segment_failure_frequency": instant_schedule["frequency"],
                        "segment_failure_units": base_mode["consecutive_units"],
                        "segment_consecutive_units": base_mode["consecutive_units"],
                        "segment_frequency_units": base_mode["frequency_units"],
                        "segment_failure_mode": base_mode["mode"],
                    },
                    "expect": {"segment_corrupted": args.expect_corrupted},
                    "inspect_corrupted": True,
                    "expected_duration": 4,
                    "kind": "segment",
                    "mode": base_mode,
                    "schedule": instant_schedule,
                    "variant_scope": None,
                }
            )

    if master_url:
        for failure_type in http_failures:
            tests.append(
                make_http_test("master_manifest", failure_type, instant_schedule, base_mode, variant_scope=None)
            )
        for failure_type in socket_resets:
            tests.append(
                make_socket_test(
                    "master_manifest",
                    failure_type,
                    "master_manifest_conn_reset",
                    args.expect_resets,
                    instant_schedule,
                    base_mode,
                    variant_scope=None,
                )
            )
        for failure_type in socket_hangs:
            tests.append(
                make_socket_test(
                    "master_manifest",
                    failure_type,
                    "master_manifest_timeout",
                    args.expect_timeouts,
                    instant_schedule,
                    base_mode,
                    variant_scope=None,
                )
            )
        for failure_type in socket_delays:
            tests.append(
                make_socket_test(
                    "master_manifest",
                    failure_type,
                    "master_manifest_timeout",
                    args.expect_timeouts,
                    instant_schedule,
                    base_mode,
                    variant_scope=None,
                )
            )

        for mode in modes:
            for schedule in expanded_schedules:
                for scope in variant_scopes:
                    if kind == "manifest" and scope:
                        continue
                    tests.append(make_http_test(kind, "404", schedule, mode, variant_scope=scope))

    if session_port:
        tests.extend(
            [
                {
                    "name": "transport_drop",
                    "desc": "transport drop (blackhole)",
                    "payload": {
                        "transport_failure_type": "drop",
                        "transport_consecutive_failures": 2,
                        "transport_failure_frequency": 0,
                        "transport_failure_units": "seconds",
                    },
                    "expect_drop_reject": args.expect_drop_reject,
                    "expected_duration": 6,
                    "window_seconds": 8,
                    "timeout_override": 3,
                    "wait_transport_active": True,
                    "kind": "segment",
                },
                {
                    "name": "transport_reject",
                    "desc": "transport reject (RST)",
                    "payload": {
                        "transport_failure_type": "reject",
                        "transport_consecutive_failures": 2,
                        "transport_failure_frequency": 0,
                        "transport_failure_units": "seconds",
                    },
                    "expect_drop_reject": args.expect_drop_reject,
                    "expected_duration": 6,
                    "window_seconds": 8,
                    "timeout_override": 3,
                    "wait_transport_active": True,
                    "kind": "segment",
                },
                {
                    "name": "throttle",
                    "desc": f"bandwidth throttle to {args.throttle_mbps} Mbps",
                    "shaping": True,
                    "expect": {"throttle": args.expect_throttle},
                    "expected_duration": max(4, args.test_seconds),
                    "kind": "segment",
                    "max_bytes": 256 * 1024,
                },
            ]
        )

    selected_variants, unselected_variants = select_variant_groups(variants or [])
    segment_scope_keys = build_segment_scope_keys(variants or [], args)
    segment_scope_supported = False
    if segment_scope_keys:
        selected_keys = {segment_scope_keys.get(name) for name in selected_variants if segment_scope_keys.get(name)}
        unselected_keys = {
            segment_scope_keys.get(name) for name in unselected_variants if segment_scope_keys.get(name)
        }
        if selected_keys and not (selected_keys & unselected_keys):
            segment_scope_supported = True

    iterations = max(1, args.iterations)
    try:
        for iteration in range(1, iterations + 1):
            ordered_tests = list(tests)
            if args.shuffle_tests:
                random.shuffle(ordered_tests)
            else:
                ordered_tests.sort(key=lambda item: item.get("expected_duration", args.test_seconds))
            log_line(f"=== Iteration {iteration}/{iterations} ===")

            if manifest_url:
                http_get_text(manifest_url, timeout=args.timeout, verbose=args.verbose)

            for test in ordered_tests:
                test_id = f"iter{iteration}:{test['name']}"
                if resume_pending:
                    if test_id != resume_id:
                        continue
                    resume_pending = False
                if test.get("variant_scope") and not variants:
                    log_test_result(test_id, "SKIP", "no_variants")
                    continue
                log_line(f"TEST {test_id}: {test['desc']}")
                if session_port:
                    apply_shaping(api_base, session_port, args.restore_mbps, verbose=args.verbose)
                apply_failure_settings(api_base, session_id, base_failure_payload(), verbose=args.verbose)

                warmup_seconds = min(4, args.test_seconds)
                if test.get("kind") == "manifest" and manifest_url:
                    pre_counters = run_manifest_window(
                        manifest_url,
                        args,
                        warmup_seconds,
                        verbose_label="precheck_manifest",
                        timeout_override=min(args.timeout, max(2, warmup_seconds - 1)),
                    )
                    if pre_counters.get("manifest_success", 0) < 1:
                        if args.stop_on_failure:
                            record_test_failure(test_id, "precheck_manifest_no_success")
                            log_test_result(test_id, "FAIL", "precheck_manifest_no_success")
                            raise SystemExit("No successful manifest responses before failures")
                        record_test_failure(test_id, "precheck_manifest_no_success")
                        log_test_result(test_id, "FAIL", "precheck_manifest_no_success")
                        log_line("precheck FAIL: no successful manifest responses")
                        continue
                else:
                    pre_counters = run_probe_window(
                        url,
                        args,
                        warmup_seconds,
                        verbose_label="precheck_stream",
                        timeout_override=min(args.timeout, max(2, warmup_seconds - 1)),
                    )
                    if pre_counters.get("segment_success", 0) < 1:
                        if test.get("allow_precheck_fail"):
                            log_line("precheck WARN: no successful segment responses; proceeding")
                        else:
                            if args.stop_on_failure:
                                record_test_failure(test_id, "precheck_segment_no_success")
                                log_test_result(test_id, "FAIL", "precheck_segment_no_success")
                                raise SystemExit("No successful segment responses before failures")
                            record_test_failure(test_id, "precheck_segment_no_success")
                            log_test_result(test_id, "FAIL", "precheck_segment_no_success")
                            log_line("precheck FAIL: no successful segment responses")
                            continue

                payload = base_failure_payload()
                payload.update(test.get("payload", {}))

                scope = test.get("variant_scope")
                kind = test.get("kind")
                selected_scope = selected_variants
                unselected_scope = unselected_variants
                probe_url = url
                skip_global_expectations = False
                if scope and kind in ("manifest", "segment"):
                    if scope == "all":
                        selected_scope = [v["name"] for v in (variants or [])]
                        unselected_scope = []
                        payload[f"{kind}_failure_urls"] = ["All"]
                    elif scope == "subset":
                        if kind == "segment" and not segment_scope_supported:
                            log_test_result(test_id, "SKIP", "segment_scope_unsupported")
                            continue
                        if not selected_variants:
                            log_line("variant scope skipped: no variants available")
                            continue
                        if kind == "segment" and segment_scope_keys:
                            scoped_keys = [
                                segment_scope_keys.get(name)
                                for name in selected_variants
                                if segment_scope_keys.get(name)
                            ]
                            if not scoped_keys:
                                log_test_result(test_id, "SKIP", "segment_scope_unresolved")
                                continue
                            payload[f"{kind}_failure_urls"] = scoped_keys
                        else:
                            payload[f"{kind}_failure_urls"] = selected_variants
                    if variants and selected_scope:
                        url_map = {v["name"]: v["url"] for v in variants}
                        candidate = url_map.get(selected_scope[0])
                        if candidate:
                            probe_url = candidate
                    skip_global_expectations = True

                apply_failure_settings(api_base, session_id, payload, verbose=args.verbose)

                shape_failure_type = test.get("shape_failure_type")
                if shape_failure_type:
                    shape_url = probe_url
                    if kind == "manifest" and manifest_url:
                        shape_url = manifest_url
                    elif kind == "master_manifest" and master_url:
                        shape_url = master_url
                    ok, err = probe_failure_shape(kind, shape_url, args, shape_failure_type)
                    if not ok:
                        log_line(f"response shape FAIL: {err}")
                        if args.stop_on_failure:
                            record_test_failure(test_id, "response_shape_fail")
                            log_test_result(test_id, "FAIL", "response_shape_fail")
                            raise SystemExit(2)
                        record_test_failure(test_id, "response_shape_fail")
                        log_test_result(test_id, "FAIL", "response_shape_fail")
                        continue

                skip_transport_expectations = False
                if test.get("wait_transport_active"):
                    snapshot = wait_for_transport_active(api_base, session_id, verbose=args.verbose)
                    if not snapshot or not snapshot.get("transport_fault_active"):
                        log_line("transport fault not active; skipping drop/reject expectations")
                        skip_transport_expectations = True

                if test.get("shaping"):
                    apply_shaping(api_base, session_port, args.throttle_mbps, verbose=args.verbose)

                expected_parts = []
                for key, value in test.get("expect", {}).items():
                    expected_parts.append(f"{display_counter_name(key)}>={value}")
                if test.get("expect_any_groups"):
                    groups = test["expect_any_groups"]
                    expected_parts.append(
                        "sum(" + ", ".join("+".join(display_counter_name(name) for name in group) for group in groups)
                        + f")>={test.get('expect_any_count', 0)}"
                    )
                expected_status = test.get("expected_status")
                if expected_status is not None:
                    expected_parts.append(f"{display_kind(test['kind'])}_status_{expected_status}>=1")
                if test.get("expect_drop_reject") is not None:
                    expected_parts.append(f"drop/reject>={test['expect_drop_reject']}")
                log_line(f"expected: {', '.join(expected_parts) if expected_parts else 'n/a'}")

                window_seconds = test.get("window_seconds", args.test_seconds)
                timeout_override = test.get("timeout_override")
                if timeout_override is None:
                    timeout_override = min(args.timeout, max(2, window_seconds - 1))
                inspect_corrupted = test.get("inspect_corrupted")
                stop_on_failure = bool(test.get("schedule", {}).get("manual"))

                if scope and kind in ("manifest", "segment"):
                    schedule = test.get("schedule", {})
                    require_all_failures = True
                    min_failures = 1
                    pass_before_fail = False
                    require_recovery = False
                    if schedule.get("consecutive", 0) == 1 and schedule.get("frequency", 0) == 0:
                        require_all_failures = False
                        pass_before_fail = True
                        require_recovery = True
                    ok, err = check_variant_scope(
                        kind,
                        variants or [],
                        selected_scope,
                        unselected_scope,
                        args,
                        expect_corrupted=bool(inspect_corrupted),
                        window_seconds=window_seconds,
                        require_all_failures=require_all_failures,
                        min_failures=min_failures,
                        pass_before_fail=pass_before_fail,
                        require_recovery=require_recovery,
                    )
                    if not ok:
                        log_line(f"variant scope FAIL: {err}")
                        if args.stop_on_failure:
                            record_test_failure(test_id, "variant_scope_fail")
                            log_test_result(test_id, "FAIL", "variant_scope_fail")
                            raise SystemExit(2)
                        record_test_failure(test_id, "variant_scope_fail")
                        log_test_result(test_id, "FAIL", "variant_scope_fail")
                        continue

                if test.get("kind") == "manifest" and manifest_url:
                    counters = run_manifest_window(
                        manifest_url,
                        args,
                        window_seconds,
                        verbose_label=test["name"],
                        timeout_override=timeout_override,
                        stop_on_failure=stop_on_failure,
                    )
                elif test.get("kind") == "master_manifest" and master_url:
                    counters = run_simple_window(
                        "master_manifest",
                        master_url,
                        args,
                        window_seconds,
                        verbose_label=test["name"],
                        timeout_override=timeout_override,
                        stop_on_failure=stop_on_failure,
                    )
                else:
                    counters = run_probe_window(
                        probe_url,
                        args,
                        window_seconds,
                        verbose_label=test["name"],
                        timeout_override=timeout_override,
                        segment_inspector=(
                            (lambda data: data and all(b == 0 for b in data))
                            if inspect_corrupted
                            else None
                        ),
                        stop_on_failure=stop_on_failure,
                        max_bytes=test.get("max_bytes"),
                    )

                observed_parts = summarize_failure_counts(counters)
                log_line(f"observed: {observed_parts}")

                missing = []
                if not skip_global_expectations:
                    for key, value in test.get("expect", {}).items():
                        if counters.get(key, 0) < value:
                            if key == "throttle":
                                snapshot = fetch_session_snapshot_with_fallback(
                                    api_base, session_id, args.player_id, verbose=args.verbose
                                )
                                shaped = None
                                if snapshot:
                                    shaped = snapshot.get("nftables_bandwidth_mbps")
                                if shaped is not None and float(shaped) <= args.throttle_mbps + 0.1:
                                    log_line("throttle counter stayed at zero; shaping is active")
                                    continue
                            missing.append(f"{display_counter_name(key)} {counters.get(key, 0)} < {value}")
                    if expected_status is not None:
                        status_key = f"{test['kind']}_status_{expected_status}"
                        if counters.get(status_key, 0) < 1:
                            missing.append(
                                f"{display_kind(test['kind'])}_status_{expected_status} {counters.get(status_key, 0)} < 1"
                            )
                    if test.get("schedule", {}).get("frequency", 0) > 0:
                        success_key = f"{kind}_success"
                        if counters.get(success_key, 0) < 1:
                            missing.append(f"{display_counter_name(success_key)} {counters.get(success_key, 0)} < 1")
                    if test.get("expect_any_groups"):
                        required = test.get("expect_any_count", 0)
                        for group in test["expect_any_groups"]:
                            observed = sum(counters.get(key, 0) for key in group)
                            if observed < required:
                                snapshot = fetch_session_snapshot_with_fallback(
                                    api_base, session_id, args.player_id, verbose=args.verbose
                                )
                                fault_key = test.get("fault_counter")
                                fault_count = 0
                                if snapshot and fault_key:
                                    fault_count = int(snapshot.get(fault_key, 0))
                                if fault_count < required:
                                    missing.append(
                                        f"any({', '.join(display_counter_name(name) for name in group)}) {observed} < {required}"
                                    )
                    if test.get("expect_drop_reject") is not None and not skip_transport_expectations:
                        drop_reject = (
                            counters.get("manifest_timeout", 0)
                            + counters.get("manifest_conn_reset", 0)
                            + counters.get("manifest_conn_refused", 0)
                            + counters.get("segment_timeout", 0)
                            + counters.get("segment_conn_reset", 0)
                            + counters.get("segment_conn_refused", 0)
                        )
                        snapshot = fetch_session_snapshot_with_fallback(
                            api_base, session_id, args.player_id, verbose=args.verbose
                        )
                        fault_packets = 0
                        if snapshot:
                            fault_packets = int(snapshot.get("transport_fault_drop_packets", 0)) + int(
                                snapshot.get("transport_fault_reject_packets", 0)
                            )
                        if drop_reject < test["expect_drop_reject"] and fault_packets < test["expect_drop_reject"]:
                            if test.get("wait_transport_active") and drop_reject == 0 and fault_packets == 0:
                                log_line("transport fault counters stayed at zero; skipping drop/reject expectations")
                            else:
                                missing.append(
                                    f"drop/reject {drop_reject} < {test['expect_drop_reject']} and fault_packets {fault_packets} < {test['expect_drop_reject']}"
                                )

                if missing:
                    log_line("result: FAIL")
                    for item in missing:
                        log_line(f"- {item}")
                    record_test_failure(test_id, "expectations_missing")
                    log_test_result(test_id, "FAIL", "expectations_missing")
                    if args.stop_on_failure:
                        raise SystemExit(2)
                    continue

                log_line("result: PASS")
                log_test_result(test_id, "PASS")

                if test.get("shaping"):
                    apply_shaping(api_base, session_port, args.restore_mbps, verbose=args.verbose)

                apply_failure_settings(api_base, session_id, base_failure_payload(), verbose=args.verbose)
                if test.get("kind") == "manifest" and manifest_url:
                    post_counters = run_manifest_window(
                        manifest_url,
                        args,
                        warmup_seconds,
                        verbose_label="postcheck_manifest",
                        timeout_override=min(args.timeout, max(2, warmup_seconds - 1)),
                    )
                    if post_counters.get("manifest_success", 0) < 1:
                        if args.stop_on_failure:
                            record_test_failure(test_id, "postcheck_manifest_no_success")
                            raise SystemExit("Manifest did not recover after failures")
                        record_test_failure(test_id, "postcheck_manifest_no_success")
                        log_line("postcheck FAIL: manifest did not recover")
                        continue
                    if (
                        post_counters.get("manifest_http_error", 0)
                        + post_counters.get("manifest_timeout", 0)
                        + post_counters.get("manifest_conn_reset", 0)
                        + post_counters.get("manifest_conn_refused", 0)
                    ) > 0:
                        if args.stop_on_failure:
                            record_test_failure(test_id, "postcheck_manifest_failures_persisted")
                            raise SystemExit("Manifest failures persisted after reset")
                        record_test_failure(test_id, "postcheck_manifest_failures_persisted")
                        log_line("postcheck FAIL: manifest failures persisted")
                        continue
                elif test.get("kind") == "master_manifest" and master_url:
                    post_counters = run_simple_window(
                        "master_manifest",
                        master_url,
                        args,
                        warmup_seconds,
                        verbose_label="postcheck_master",
                        timeout_override=min(args.timeout, max(2, warmup_seconds - 1)),
                    )
                    if post_counters.get("master_manifest_success", 0) < 1:
                        if args.stop_on_failure:
                            record_test_failure(test_id, "postcheck_master_manifest_no_success")
                            raise SystemExit("Master manifest did not recover after failures")
                        record_test_failure(test_id, "postcheck_master_manifest_no_success")
                        log_line("postcheck FAIL: master manifest did not recover")
                        continue
                    if (
                        post_counters.get("master_manifest_http_error", 0)
                        + post_counters.get("master_manifest_timeout", 0)
                        + post_counters.get("master_manifest_conn_reset", 0)
                        + post_counters.get("master_manifest_conn_refused", 0)
                    ) > 0:
                        if args.stop_on_failure:
                            record_test_failure(test_id, "postcheck_master_manifest_failures_persisted")
                            raise SystemExit("Master manifest failures persisted after reset")
                        record_test_failure(test_id, "postcheck_master_manifest_failures_persisted")
                        log_line("postcheck FAIL: master manifest failures persisted")
                        continue
                else:
                    post_counters = run_probe_window(
                        probe_url,
                        args,
                        warmup_seconds,
                        verbose_label="postcheck_stream",
                        timeout_override=min(args.timeout, max(2, warmup_seconds - 1)),
                        max_bytes=test.get("max_bytes"),
                    )
                    if post_counters.get("segment_success", 0) < 1:
                        if args.stop_on_failure:
                            record_test_failure(test_id, "postcheck_segment_no_success")
                            raise SystemExit("Segment did not recover after failures")
                        record_test_failure(test_id, "postcheck_segment_no_success")
                        log_line("postcheck FAIL: segment did not recover")
                        continue
                    if (
                        post_counters.get("segment_http_error", 0)
                        + post_counters.get("segment_timeout", 0)
                        + post_counters.get("segment_conn_reset", 0)
                        + post_counters.get("segment_conn_refused", 0)
                    ) > 0:
                        if args.stop_on_failure:
                            record_test_failure(test_id, "postcheck_segment_failures_persisted")
                            raise SystemExit("Segment failures persisted after reset")
                        record_test_failure(test_id, "postcheck_segment_failures_persisted")
                        log_line("postcheck FAIL: segment failures persisted")
                        continue

    finally:
        if session_port:
            apply_shaping(api_base, session_port, args.restore_mbps, verbose=args.verbose)
        apply_failure_settings(api_base, session_id, base_failure_payload(), verbose=args.verbose)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("url", nargs="?", help="master or media m3u8 URL")
    ap.add_argument("--host", default="localhost", help="server host (default: localhost)")
    ap.add_argument("--scheme", default="http", help="http or https")
    ap.add_argument("--api-port", type=int, default=30000, help="API/UI port")
    ap.add_argument("--hls-port", type=int, default=30081, help="HLS port")
    ap.add_argument("--api-base", help="override API base URL")
    ap.add_argument("--hls-base", help="override HLS base URL")
    ap.add_argument("--seconds", type=int, default=90, help="max run time")
    ap.add_argument("--test-seconds", type=int, default=12, help="per-test duration")
    ap.add_argument("--timeout", type=int, default=20, help="request timeout seconds")
    ap.add_argument("--verbose", action="store_true", help="enable verbose logging")
    ap.add_argument("--player-id", help="player_id query param")
    ap.add_argument("--mode", choices=["tests", "probe"], default="tests")
    ap.add_argument("--restore-mbps", type=float, default=1000.0, help="rate to restore after throttle test")
    ap.add_argument("--expect-http", type=int, default=1, help="min HTTP 4xx/5xx count")
    ap.add_argument("--expect-timeouts", type=int, default=1, help="min timeout count")
    ap.add_argument("--expect-resets", type=int, default=1, help="min connection reset count")
    ap.add_argument("--expect-drop-reject", type=int, default=1, help="min drop/reject count")
    ap.add_argument("--expect-throttle", type=int, default=1, help="min throttle count")
    ap.add_argument("--expect-corrupted", type=int, default=1, help="min corrupted segment count")
    ap.add_argument("--shuffle-tests", action="store_true", help="randomize test order")
    ap.add_argument("--iterations", type=int, default=1, help="repeat all tests N times")
    ap.add_argument("--stop-on-failure", action="store_true", help="stop after first test failure")
    ap.add_argument(
        "--continue",
        dest="continue_tests",
        action="store_true",
        help="resume from the last failed test id",
    )
    ap.add_argument(
        "--continue-from",
        help="resume from a specific test id (overrides --continue)",
    )
    ap.add_argument(
        "--continue-file",
        help="path to read/write the last failed test id",
    )
    ap.add_argument("--throttle-mbps", type=float, default=1.0, help="Mbps threshold")
    args = ap.parse_args()

    if not args.player_id:
        args.player_id = str(uuid.uuid4())

    url = args.url
    content_name = None
    has_dash = False
    master_url = None
    if not url:
        url, content_name, has_dash = select_auto_url(args)
        log_line(f"Auto-selected URL: {url}")
        master_url = url
    else:
        url = ensure_player_id(url, args.player_id)
        if "master" in urllib.parse.urlsplit(url).path:
            master_url = url

    if args.verbose:
        log_line(f"START url={url}")
    status, text, _, err = http_get_text(url, timeout=args.timeout, verbose=args.verbose)
    if status != 200:
        detail = err or f"status={status}"
        raise SystemExit(f"Failed to fetch manifest: {url} ({detail})")

    variants = []
    if "#EXT-X-STREAM-INF" in text:
        variants = parse_master_variants(text, url, args.player_id)
        vurl, bw = pick_best_variant(text, url)
        if not vurl:
            raise SystemExit("Could not pick variant from master manifest")
        log_line(f"Using top variant: {vurl} (BANDWIDTH={bw/1e6:.3f} Mbps)")
        url = vurl

    api_base, _ = build_base_urls(args)
    manifest_url = None
    if content_name and has_dash:
        safe_name = urllib.parse.quote(content_name, safe="")
        manifest_url = f"{args.scheme}://{args.host}:{args.hls_port}/go-live/{safe_name}/manifest_6s.mpd"
        manifest_url = ensure_player_id(manifest_url, args.player_id)
    warmup_url = ensure_player_id(url, args.player_id)
    http_get_text(warmup_url, timeout=args.timeout, verbose=args.verbose)
    session = find_session_by_player_id(api_base, args.player_id, verbose=args.verbose)
    if not session:
        raise SystemExit("Session not found via /api/sessions")

    if args.mode == "tests":
        run_failure_tests(
            warmup_url,
            api_base,
            session,
            args,
            manifest_url=manifest_url,
            master_url=master_url,
            variants=variants,
        )
        return

    counters = run_probe_window(warmup_url, args, args.seconds, verbose_label="probe")
    log_line("Final:")
    log_line(f"Counters: {dict(counters)}")
    missing = validate_expectations(counters, args)
    if missing:
        print("\nExpectation failures:")
        for item in missing:
            print(f"  - {item}")
        raise SystemExit(2)


if __name__ == "__main__":
    main()
