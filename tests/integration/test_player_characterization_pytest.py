"""Host-side pytest runner for player ABR characterization.

This test ports the core control loop from dashboard/player-characterization.js
into an API-driven integration test that runs entirely on the host.
"""

from __future__ import annotations

from datetime import datetime, timezone
import json
import re
import statistics
import time
from typing import Any

import pytest

from .helpers import api_request_json, fetch_session_snapshot, http_get_text, utc_now_iso


def _to_number(value: Any, default: float | None = None) -> float | None:
    try:
        num = float(value)
    except (TypeError, ValueError):
        return default
    if num != num or num in (float("inf"), float("-inf")):
        return default
    return num


def _median(values: list[float]) -> float | None:
    if not values:
        return None
    return float(statistics.median(values))


def _mean(values: list[float]) -> float | None:
    if not values:
        return None
    return float(statistics.mean(values))


def _parse_iso_z(ts: Any) -> datetime | None:
    raw = str(ts or "").strip()
    if not raw:
        return None
    try:
        return datetime.fromisoformat(raw.replace("Z", "+00:00"))
    except ValueError:
        return None


def _parse_master_variants(master_url: str, timeout: int, verbose: bool) -> list[dict[str, Any]]:
    status, text, _, err = http_get_text(master_url, timeout=timeout, verbose=verbose)
    if status != 200:
        raise RuntimeError(f"Failed to load master playlist ({status}): {err}")

    lines = text.splitlines()
    variants: list[dict[str, Any]] = []
    for line in lines:
        item = line.strip()
        if not item.startswith("#EXT-X-STREAM-INF:"):
            continue
        attrs_raw = item.split(":", 1)[1] if ":" in item else ""
        attrs: dict[str, str] = {}
        for part in attrs_raw.split(","):
            if "=" not in part:
                continue
            key, value = part.split("=", 1)
            attrs[key.strip().upper()] = value.strip().strip('"')

        bandwidth = _to_number(attrs.get("BANDWIDTH"), None)
        avg_bandwidth = _to_number(attrs.get("AVERAGE-BANDWIDTH"), None)
        if not bandwidth or bandwidth <= 0:
            continue

        variants.append(
            {
                "bandwidth": int(bandwidth),
                "averageBandwidth": int(avg_bandwidth) if avg_bandwidth and avg_bandwidth > 0 else None,
                "resolution": attrs.get("RESOLUTION", ""),
            }
        )

    variants.sort(key=lambda item: int(item.get("bandwidth", 0)))
    return variants


def _variant_to_mbps(variant: dict[str, Any]) -> float | None:
    preferred = _to_number(variant.get("averageBandwidth"), None)
    fallback = _to_number(variant.get("bandwidth"), None)
    bps = preferred if preferred and preferred > 0 else fallback
    if not bps or bps <= 0:
        return None
    return round(bps / 1_000_000, 3)


def _unique_sorted_positive(values: list[float]) -> list[float]:
    out: list[float] = []
    seen: set[float] = set()
    for value in values:
        num = _to_number(value, None)
        if not num or num <= 0:
            continue
        rounded = round(num, 3)
        if rounded in seen:
            continue
        seen.add(rounded)
        out.append(rounded)
    return sorted(out)


def _build_variant_catalog(variants: list[dict[str, Any]]) -> list[dict[str, Any]]:
    catalog: list[dict[str, Any]] = []
    for idx, item in enumerate(variants, start=1):
        bw = _to_number(item.get("bandwidth"), None)
        avg = _to_number(item.get("averageBandwidth"), None)
        catalog.append(
            {
                "index": idx,
                "resolution": str(item.get("resolution") or ""),
                "bandwidth_mbps": round(float(bw) / 1_000_000, 3) if bw is not None and bw > 0 else None,
                "average_mbps": round(float(avg) / 1_000_000, 3) if avg is not None and avg > 0 else None,
                "selected_mbps": _variant_to_mbps(item),
            }
        )
    catalog.sort(key=lambda row: float(_to_number(row.get("bandwidth_mbps"), -1.0) or -1.0))
    for idx, row in enumerate(catalog, start=1):
        row["index"] = idx
    return catalog


def _build_variant_aware_schedule(
    ladder_mbps: list[float],
    hold_seconds: int,
    overhead_pct: float,
    max_steps: int,
) -> list[dict[str, Any]]:
    if not ladder_mbps:
        return []

    overhead_ratio = max(0.0, min(0.25, _to_number(overhead_pct, 0.1) / 100.0))
    wire_multiplier = 1.0 / max(0.01, (1.0 - overhead_ratio))

    # Smooth mode: per adjacent transition, probe offsets above the next variant.
    # Down offsets: 50, 20, 15, 10, 5, 0 (exact)
    # Up offsets:   0,  5, 10, 15, 20, 50
    wire_ladder = [float(value) * wire_multiplier for value in ladder_mbps]
    if not wire_ladder:
        return []

    down_offset_pcts = [50, 20, 15, 10, 5, 0]
    up_offset_pcts = [0, 5, 10, 15, 20, 50]

    steps: list[dict[str, Any]] = []

    def append_step(direction: str, base_index: int, offset_pct: int) -> None:
        if base_index < 0 or base_index >= len(wire_ladder):
            return
        base = float(wire_ladder[base_index])
        pct = int(offset_pct)
        target = base * (1.0 + (float(pct) / 100.0))
        source_detail = f"V{base_index + 1}+{pct}%"
        if pct == 0:
            source_detail = f"V{base_index + 1} exact"
        steps.append(
            {
                "target_mbps": round(target, 3),
                "direction": direction,
                "hold_seconds": int(max(1, hold_seconds)),
                "source": "offset",
                "source_detail": source_detail,
                "base_variant_index": base_index + 1,
                "offset_pct": pct,
            }
        )

    if len(wire_ladder) == 1:
        append_step("down", 0, 0)
        append_step("up", 0, 0)
    else:
        # Down starts with top-variant reverse block, then continues downward.
        for offset_pct in down_offset_pcts:
            append_step("down", len(wire_ladder) - 1, offset_pct)
        # Down: target the next lower variant each hop.
        for next_lower_idx in range(len(wire_ladder) - 2, -1, -1):
            for offset_pct in down_offset_pcts:
                append_step("down", next_lower_idx, offset_pct)
        # Up: start from V1 (reverse of down tail), then progress upward.
        for next_higher_idx in range(0, len(wire_ladder)):
            for offset_pct in up_offset_pcts:
                append_step("up", next_higher_idx, offset_pct)

    if max_steps and max_steps > 0:
        return steps[:max_steps]
    return steps


def _build_huge_steps_schedule(
    ladder_mbps: list[float],
    cycles: int,
) -> list[dict[str, Any]]:
    if len(ladder_mbps) < 2:
        return []

    bottom_variant = float(ladder_mbps[0])
    second_bottom_variant = float(ladder_mbps[1])
    top_variant = float(ladder_mbps[-1])

    # Huge-step mode intentionally uses aggressive caps to stress adaptation.
    bottom_target = round((bottom_variant + second_bottom_variant) / 2.0, 3)
    top_target = round(top_variant * 2.0, 3)
    cycles = max(1, int(cycles))

    steps: list[dict[str, Any]] = []
    for idx in range(cycles):
        steps.append(
            {
                "target_mbps": bottom_target,
                "direction": "step-down",
                "hold_seconds": 30,
                "step_kind": "step-down",
                "cycle_index": idx + 1,
                "target_variant_mbps": bottom_variant,
                "target_variant_label": "bottom",
            }
        )
        steps.append(
            {
                "target_mbps": top_target,
                "direction": "step-up",
                "hold_seconds": 30,
                "step_kind": "step-up",
                "cycle_index": idx + 1,
                "target_variant_mbps": top_variant,
                "target_variant_label": "top",
            }
        )
    return steps


def _throughput_mbps(snapshot: dict[str, Any]) -> float | None:
    for key in (
        "mbps_wire_active_6s",
        "mbps_wire_throughput",
        "mbps_wire_sustained_6s",
        "mbps_wire_sustained_1s",
        "measured_mbps",
    ):
        value = _to_number(snapshot.get(key), None)
        if value is not None:
            return value
    return None


def _wire_throughput_mbps(snapshot: dict[str, Any]) -> float | None:
    return _to_number(
        snapshot.get("mbps_wire_active_6s"),
        _to_number(
            snapshot.get("mbps_wire_throughput"),
            _to_number(
                snapshot.get("mbps_wire_sustained_6s"),
                _to_number(snapshot.get("mbps_wire_sustained_1s"), _to_number(snapshot.get("measured_mbps"), None)),
            ),
        ),
    )


def _timing_variant_mbps(snapshot: dict[str, Any]) -> float | None:
    # Timing signal should reflect when the player starts fetching a new rendition.
    return _to_number(
        snapshot.get("server_video_rendition_mbps"),
        _to_number(snapshot.get("player_metrics_video_bitrate_mbps"), None),
    )


def _validate_data_plane_rate_effect(
    api_base: str,
    session_id: str,
    target_mbps: float,
    sample_seconds: int = 15,
) -> dict[str, Any]:
    target = _to_number(target_mbps, None)
    if target is None or target <= 0.05:
        return {"checked": False, "reason": "unbounded_target"}
    if target > 120:
        return {"checked": False, "reason": "target_too_high_for_signal_check"}

    count = max(10, min(30, int(sample_seconds)))
    samples: list[float] = []
    for _ in range(count):
        latest = fetch_session_snapshot(api_base, session_id, verbose=False) or {}
        wire = _wire_throughput_mbps(latest)
        if wire is not None:
            samples.append(float(wire))
        time.sleep(1)

    if len(samples) < 3:
        return {
            "checked": False,
            "reason": "insufficient_wire_samples",
            "sample_count": len(samples),
        }

    median_wire = _median(samples)
    ratio = (median_wire / target) if (median_wire is not None and target > 0) else None
    min_wire = min(samples)
    max_wire = max(samples)
    delta_wire = max_wire - min_wire
    stagnant_threshold = max(0.5, target * 0.05)
    stagnant = delta_wire < stagnant_threshold
    suspicious = ratio is not None and ratio > 1.35
    return {
        "checked": True,
        "suspicious": suspicious,
        "stagnant": stagnant,
        "median_wire_mbps": median_wire,
        "ratio": ratio,
        "min_wire_mbps": min_wire,
        "max_wire_mbps": max_wire,
        "delta_wire_mbps": delta_wire,
        "sample_count": len(samples),
    }


def _patch_session_fields(api_base: str, session_id: str, set_values: dict[str, Any]) -> bool:
    fields = list(set_values.keys())
    if not fields:
        return True
    url = f"{api_base}/api/session/{session_id}"
    payload = {"set": set_values, "fields": fields}
    try:
        api_request_json(url, method="PATCH", payload=payload, timeout=10, verbose=False)
        return True
    except Exception:
        return False


def _apply_rate(api_base: str, session_id: str, target_mbps: float) -> None:
    ok = _patch_session_fields(
        api_base,
        session_id,
        {
            "nftables_bandwidth_mbps": round(target_mbps, 3),
            "nftables_delay_ms": 0,
            "nftables_packet_loss": 0,
            "nftables_pattern_enabled": False,
            "nftables_pattern_steps": [],
        },
    )
    if not ok:
        raise RuntimeError(f"Failed to apply shaping target {target_mbps:.3f} Mbps")


def _confirm_rate(api_base: str, session_id: str, target_mbps: float, timeout_seconds: int = 12) -> tuple[bool, float | None]:
    deadline = time.time() + max(2, timeout_seconds)
    observed: float | None = None
    tolerance = max(0.05, target_mbps * 0.02)
    while time.time() < deadline:
        snap = fetch_session_snapshot(api_base, session_id, verbose=False) or {}
        current = _to_number(snap.get("nftables_bandwidth_mbps"), None)
        if current is not None:
            observed = current
            if abs(current - target_mbps) <= tolerance:
                return True, observed
        time.sleep(0.5)
    return False, observed


def _build_transition_summary(run: dict[str, Any]) -> list[dict[str, Any]]:
    steps = run.get("steps", [])
    samples = run.get("samples", [])
    switch_events = run.get("switch_events", [])
    rows: list[dict[str, Any]] = []

    for step_index, step in enumerate(steps):
        step_samples = [
            sample
            for sample in samples
            if int(sample.get("step_index", -1)) == step_index
        ]

        start_ts = _parse_iso_z(step_samples[0].get("ts")) if step_samples else None
        start_variant = None
        restart_start = 0
        for sample in step_samples:
            value = _to_number(sample.get("timing_variant_mbps"), None)
            if value is not None:
                start_variant = float(value)
            restart_value = int(_to_number(sample.get("player_restarts"), 0) or 0)
            restart_start = restart_value
            if value is not None:
                break

        restart_end = restart_start
        if step_samples:
            restart_end = int(_to_number(step_samples[-1].get("player_restarts"), restart_start) or restart_start)
        restart_delta = max(0, restart_end - restart_start)

        first_event = None
        for event in switch_events:
            if int(event.get("step_index", -1)) != step_index:
                continue
            event_ts = _parse_iso_z(event.get("ts"))
            if event_ts is None:
                continue
            if start_ts is not None and event_ts < start_ts:
                continue
            first_event = event
            break

        to_variant = None
        latency_s = None
        stall_count_delta = None
        stall_time_delta_s = None
        if first_event is not None:
            to_variant = _to_number(first_event.get("to_variant_mbps"), None)
            event_ts = _parse_iso_z(first_event.get("ts"))
            if start_ts is not None and event_ts is not None:
                latency_s = round(max(0.0, (event_ts - start_ts).total_seconds()), 3)
            stall_count_delta = _to_number(first_event.get("stall_count_delta"), None)
            stall_time_delta_s = _to_number(first_event.get("stall_time_delta_s"), None)

        switch_dir = "none"
        if start_variant is not None and to_variant is not None:
            if float(to_variant) > float(start_variant):
                switch_dir = "up"
            elif float(to_variant) < float(start_variant):
                switch_dir = "down"

        rows.append(
            {
                "step": step_index + 1,
                "direction": step.get("direction"),
                "target_mbps": _to_number(step.get("target_mbps"), None),
                "target_variant_label": step.get("target_variant_label"),
                "target_variant_mbps": _to_number(step.get("target_variant_mbps"), None),
                "from_variant_mbps": start_variant,
                "to_variant_mbps": float(to_variant) if to_variant is not None else None,
                "time_to_variant_change_s": latency_s,
                "stall_count_delta": stall_count_delta,
                "stall_time_delta_s": stall_time_delta_s,
                "restart_delta": restart_delta,
                "switch_direction": switch_dir,
            }
        )

    return rows


def _render_transition_summary_table(rows: list[dict[str, Any]]) -> list[str]:
    header = "| Step | Limit Mbps | Direction | Target Variant | Target Variant Mbps | From Variant Mbps | To Variant Mbps | Time To Variant Change (s) | Stall Δ Count | Stall Δ Time (s) | Restarts Δ | Switch |"
    divider = "|---:|---:|:---|:---|---:|---:|---:|---:|---:|---:|---:|:---|"
    lines = [header, divider]

    def _fmt(value: Any) -> str:
        num = _to_number(value, None)
        return f"{num:.3f}" if num is not None else "—"

    for row in rows:
        target = row.get("target_mbps")
        from_variant = row.get("from_variant_mbps")
        to_variant = row.get("to_variant_mbps")
        latency = row.get("time_to_variant_change_s")
        lines.append(
            "| "
            f"{row.get('step', '—')} | "
            f"{_fmt(target)} | "
            f"{row.get('direction') or '—'} | "
            f"{row.get('target_variant_label') or '—'} | "
            f"{_fmt(row.get('target_variant_mbps'))} | "
            f"{_fmt(from_variant)} | "
            f"{_fmt(to_variant)} | "
            f"{_fmt(latency)} | "
            f"{_fmt(row.get('stall_count_delta'))} | "
            f"{_fmt(row.get('stall_time_delta_s'))} | "
            f"{int(_to_number(row.get('restart_delta'), 0) or 0)} | "
            f"{row.get('switch_direction') or 'none'} |"
        )
    return lines


def _render_plain_table(headers: list[str], rows: list[list[str]]) -> list[str]:
    widths = [len(header) for header in headers]
    for row in rows:
        for idx, cell in enumerate(row):
            if idx < len(widths):
                widths[idx] = max(widths[idx], len(str(cell)))

    def _line(cells: list[str]) -> str:
        return " | ".join(str(cell).ljust(widths[idx]) for idx, cell in enumerate(cells))

    sep = "-+-".join("-" * width for width in widths)
    out = [_line(headers), sep]
    for row in rows:
        out.append(_line(row))
    return out


def _render_transition_summary_plain(rows: list[dict[str, Any]]) -> list[str]:
    headers = [
        "Step",
        "Limit Mbps",
        "Direction",
        "Target Variant",
        "Target Variant Mbps",
        "From Variant",
        "To Variant",
        "Time To Change (s)",
        "Stall Δ Count",
        "Stall Δ Time (s)",
        "Restarts Δ",
        "Switch",
    ]
    data_rows: list[list[str]] = []
    for row in rows:
        data_rows.append(
            [
                str(int(_to_number(row.get("step"), 0) or 0)),
                _fmt3(row.get("target_mbps")),
                str(row.get("direction") or "—"),
                str(row.get("target_variant_label") or "—"),
                _fmt3(row.get("target_variant_mbps")),
                _fmt3(row.get("from_variant_mbps")),
                _fmt3(row.get("to_variant_mbps")),
                _fmt3(row.get("time_to_variant_change_s")),
                _fmt3(row.get("stall_count_delta")),
                _fmt3(row.get("stall_time_delta_s")),
                str(int(_to_number(row.get("restart_delta"), 0) or 0)),
                str(row.get("switch_direction") or "none"),
            ]
        )
    return _render_plain_table(headers, data_rows)


def _build_smooth_switch_summary(run: dict[str, Any]) -> dict[str, Any]:
    events = run.get("switch_events", []) if isinstance(run, dict) else []
    detailed_rows: list[dict[str, Any]] = []
    grouped: dict[str, dict[str, Any]] = {}

    for event in events:
        from_variant = _to_number(event.get("from_variant_mbps"), None)
        to_variant = _to_number(event.get("to_variant_mbps"), None)
        limit = _to_number(event.get("target_mbps"), None)
        if from_variant is None or to_variant is None or limit is None:
            continue
        if abs(float(to_variant) - float(from_variant)) < 0.05:
            continue

        direction = "up" if float(to_variant) > float(from_variant) else "down"
        row = {
            "cycle_index": int(_to_number(event.get("cycle_index"), 0) or 0),
            "step_index": int(_to_number(event.get("step_index"), -1) or -1),
            "direction": direction,
            "from_variant_mbps": round(float(from_variant), 3),
            "to_variant_mbps": round(float(to_variant), 3),
            "limit_mbps": round(float(limit), 3),
            "throughput_mbps": _to_number(event.get("throughput_mbps"), None),
            "time_from_limit_change_s": _to_number(event.get("time_from_limit_change_s"), None),
            "stall_count_delta": _to_number(event.get("stall_count_delta"), None),
            "stall_time_delta_s": _to_number(event.get("stall_time_delta_s"), None),
        }
        detailed_rows.append(row)

        key = f"{direction}:{row['from_variant_mbps']:.3f}->{row['to_variant_mbps']:.3f}"
        bucket = grouped.setdefault(
            key,
            {
                "direction": direction,
                "from_variant_mbps": row["from_variant_mbps"],
                "to_variant_mbps": row["to_variant_mbps"],
                "limits": [],
            },
        )
        bucket["limits"].append(float(row["limit_mbps"]))

    detailed_rows.sort(key=lambda item: (item.get("step_index", -1), item.get("cycle_index", 0)))

    aggregate_rows: list[dict[str, Any]] = []
    for bucket in grouped.values():
        limits = [float(x) for x in bucket.get("limits", []) if x is not None]
        if not limits:
            continue
        aggregate_rows.append(
            {
                "direction": bucket.get("direction"),
                "from_variant_mbps": bucket.get("from_variant_mbps"),
                "to_variant_mbps": bucket.get("to_variant_mbps"),
                "switch_count": len(limits),
                "limit_min_mbps": round(min(limits), 3),
                "limit_median_mbps": round(_median(limits), 3) if _median(limits) is not None else None,
                "limit_max_mbps": round(max(limits), 3),
            }
        )

    aggregate_rows.sort(key=lambda item: (str(item.get("direction")), _to_number(item.get("from_variant_mbps"), 0) or 0))
    return {
        "events": detailed_rows,
        "aggregate": aggregate_rows,
    }


def _render_smooth_switch_events_table(rows: list[dict[str, Any]]) -> list[str]:
    lines = ["| Step | Cycle | Direction | From Variant | To Variant | Applied Limit (Mbps) | Throughput (Mbps) | Time From Limit Change (s) | Stall Δ Count | Stall Δ Time (s) |",
             "|---:|---:|:---:|---:|---:|---:|---:|---:|---:|---:|"]
    for row in rows:
        lines.append(
            "| "
            f"{int(_to_number(row.get('step_index'), -1) or -1) + 1} | "
            f"{int(_to_number(row.get('cycle_index'), 0) or 0)} | "
            f"{row.get('direction') or '—'} | "
            f"{_fmt3(row.get('from_variant_mbps'))} | "
            f"{_fmt3(row.get('to_variant_mbps'))} | "
            f"{_fmt3(row.get('limit_mbps'))} | "
            f"{_fmt3(row.get('throughput_mbps'))} | "
            f"{_fmt3(row.get('time_from_limit_change_s'))} | "
            f"{_fmt3(row.get('stall_count_delta'))} | "
            f"{_fmt3(row.get('stall_time_delta_s'))} |"
        )
    return lines


def _render_smooth_switch_aggregate_table(rows: list[dict[str, Any]]) -> list[str]:
    lines = ["| Direction | From Variant | To Variant | Switch Count | Limit Min (Mbps) | Limit Median (Mbps) | Limit Max (Mbps) |",
             "|:---:|---:|---:|---:|---:|---:|---:|"]
    for row in rows:
        lines.append(
            "| "
            f"{row.get('direction') or '—'} | "
            f"{_fmt3(row.get('from_variant_mbps'))} | "
            f"{_fmt3(row.get('to_variant_mbps'))} | "
            f"{int(_to_number(row.get('switch_count'), 0) or 0)} | "
            f"{_fmt3(row.get('limit_min_mbps'))} | "
            f"{_fmt3(row.get('limit_median_mbps'))} | "
            f"{_fmt3(row.get('limit_max_mbps'))} |"
        )
    return lines


def _render_smooth_switch_aggregate_plain(rows: list[dict[str, Any]]) -> list[str]:
    headers = [
        "Direction",
        "From",
        "To",
        "Count",
        "Limit Min",
        "Limit Median",
        "Limit Max",
    ]
    data_rows: list[list[str]] = []
    for row in rows:
        data_rows.append(
            [
                str(row.get("direction") or "—"),
                _fmt3(row.get("from_variant_mbps")),
                _fmt3(row.get("to_variant_mbps")),
                str(int(_to_number(row.get("switch_count"), 0) or 0)),
                _fmt3(row.get("limit_min_mbps")),
                _fmt3(row.get("limit_median_mbps")),
                _fmt3(row.get("limit_max_mbps")),
            ]
        )
    return _render_plain_table(headers, data_rows)


def _percentile(values: list[float], pct: float) -> float | None:
    if not values:
        return None
    ordered = sorted(values)
    if len(ordered) == 1:
        return ordered[0]
    rank = max(0.0, min(1.0, pct / 100.0)) * (len(ordered) - 1)
    low = int(rank)
    high = min(len(ordered) - 1, low + 1)
    frac = rank - low
    return ordered[low] + ((ordered[high] - ordered[low]) * frac)


def _fmt3(value: Any) -> str:
    num = _to_number(value, None)
    return f"{num:.3f}" if num is not None else "—"


def _render_huge_cycle_table(title: str, rows: list[dict[str, Any]]) -> list[str]:
    lines = [f"### {title}", ""]
    lines.append("| Cycle | Target Variant Mbps | Reached | Time To Target (s) | Frames Delta | Avg Buffer (s) | Avg Throughput (Mbps) | Avg Rendition (Mbps) |")
    lines.append("|---:|---:|:---:|---:|---:|---:|---:|---:|")
    for row in rows:
        lines.append(
            "| "
            f"{int(_to_number(row.get('cycle_index'), 0) or 0)} | "
            f"{_fmt3(row.get('target_variant_mbps'))} | "
            f"{'yes' if row.get('target_reached') else 'no'} | "
            f"{_fmt3(row.get('time_to_target_s'))} | "
            f"{_fmt3(row.get('frames_presented_delta'))} | "
            f"{_fmt3(row.get('avg_buffer_depth_s'))} | "
            f"{_fmt3(row.get('avg_throughput_mbps'))} | "
            f"{_fmt3(row.get('avg_variant_mbps'))} |"
        )

    timing = [_to_number(item.get("time_to_target_s"), None) for item in rows if item.get("target_reached")]
    timing = [float(item) for item in timing if item is not None]
    if timing:
        lines.extend(
            [
                "",
                f"- Timing median (s): {_fmt3(_median(timing))}",
                f"- Timing p95 (s): {_fmt3(_percentile(timing, 95))}",
                f"- Timing min/max (s): {_fmt3(min(timing))} / {_fmt3(max(timing))}",
            ]
        )
    else:
        lines.extend(["", "- No successful target reaches recorded."])
    return lines


def _render_huge_switch_table(title: str, rows: list[dict[str, Any]]) -> list[str]:
    lines = [f"### {title}", ""]
    lines.append("| Cycle | From Variant | To Variant | Time From Limit Change (s) | Frames Delta | Avg Buffer (s) | Avg Throughput (Mbps) | Stall Δ Count | Stall Δ Time (s) |")
    lines.append("|---:|---:|---:|---:|---:|---:|---:|---:|---:|")
    for row in rows:
        lines.append(
            "| "
            f"{int(_to_number(row.get('cycle_index'), 0) or 0)} | "
            f"{_fmt3(row.get('from_variant_mbps'))} | "
            f"{_fmt3(row.get('to_variant_mbps'))} | "
            f"{_fmt3(row.get('time_from_limit_change_s'))} | "
            f"{_fmt3(row.get('frames_presented_delta'))} | "
            f"{_fmt3(row.get('avg_buffer_depth_s'))} | "
            f"{_fmt3(row.get('avg_throughput_mbps'))} | "
            f"{_fmt3(row.get('stall_count_delta'))} | "
            f"{_fmt3(row.get('stall_time_delta_s'))} |"
        )

    timing = [_to_number(item.get("time_from_limit_change_s"), None) for item in rows]
    timing = [float(item) for item in timing if item is not None]
    if timing:
        lines.extend(
            [
                "",
                f"- Timing median (s): {_fmt3(_median(timing))}",
                f"- Timing p95 (s): {_fmt3(_percentile(timing, 95))}",
                f"- Timing min/max (s): {_fmt3(min(timing))} / {_fmt3(max(timing))}",
            ]
        )
    else:
        lines.extend(["", "- No rendition-change events recorded."])
    return lines


def _render_huge_cycle_plain(title: str, rows: list[dict[str, Any]]) -> list[str]:
    headers = [
        "Cycle",
        "Target Variant",
        "Reached",
        "Time To Target (s)",
        "Frames Delta",
        "Avg Buffer (s)",
        "Avg Throughput",
        "Avg Rendition",
    ]
    data_rows: list[list[str]] = []
    for row in rows:
        data_rows.append(
            [
                str(int(_to_number(row.get("cycle_index"), 0) or 0)),
                _fmt3(row.get("target_variant_mbps")),
                "yes" if row.get("target_reached") else "no",
                _fmt3(row.get("time_to_target_s")),
                _fmt3(row.get("frames_presented_delta")),
                _fmt3(row.get("avg_buffer_depth_s")),
                _fmt3(row.get("avg_throughput_mbps")),
                _fmt3(row.get("avg_variant_mbps")),
            ]
        )
    return [title] + _render_plain_table(headers, data_rows)


def _render_huge_switch_plain(title: str, rows: list[dict[str, Any]]) -> list[str]:
    headers = [
        "Cycle",
        "From",
        "To",
        "Time From Limit (s)",
        "Frames Delta",
        "Avg Buffer (s)",
        "Avg Throughput",
        "Stall Δ Count",
        "Stall Δ Time (s)",
    ]
    data_rows: list[list[str]] = []
    for row in rows:
        data_rows.append(
            [
                str(int(_to_number(row.get("cycle_index"), 0) or 0)),
                _fmt3(row.get("from_variant_mbps")),
                _fmt3(row.get("to_variant_mbps")),
                _fmt3(row.get("time_from_limit_change_s")),
                _fmt3(row.get("frames_presented_delta")),
                _fmt3(row.get("avg_buffer_depth_s")),
                _fmt3(row.get("avg_throughput_mbps")),
                _fmt3(row.get("stall_count_delta")),
                _fmt3(row.get("stall_time_delta_s")),
            ]
        )
    return [title] + _render_plain_table(headers, data_rows)


def _write_reports(run: dict[str, Any], output_prefix: str) -> tuple[str, str]:
    json_path = f"{output_prefix}.json"
    md_path = f"{output_prefix}.md"

    with open(json_path, "w", encoding="utf-8") as fh:
        json.dump(run, fh, indent=2)

    summary = run.get("summary", {})
    lines = [
        "# Player Characterization Report (Pytest Host Runner)",
        "",
        f"- Run #: {run.get('run_number')}",
        f"- Run name: {run.get('run_name')}",
        f"- Generated: {datetime.now(timezone.utc).isoformat(timespec='seconds')}",
        f"- Session: {run.get('session_id')}",
        f"- Steps: {summary.get('step_count', 0)}",
        f"- Samples: {summary.get('sample_count', 0)}",
        f"- Switches: {summary.get('switch_count', 0)}",
        f"- Downswitches: {summary.get('downswitch_count', 0)}",
        f"- Upswitches: {summary.get('upswitch_count', 0)}",
        f"- Loop completions: {summary.get('loop_completion_count', 0)}/{summary.get('repeat_count', run.get('repeat_count', 0))}",
        f"- Stall count delta: {summary.get('stall_count_delta', 0)}",
        f"- Stall time delta (s): {summary.get('stall_time_delta_s', 0)}",
        f"- Median throughput (Mbps): {summary.get('throughput_median_mbps', '—')}",
        "",
        "## Step Targets",
        "",
    ]
    for index, step in enumerate(run.get("steps", []), start=1):
        source_detail = str(step.get("source_detail") or "").strip()
        source_suffix = f", src={source_detail}" if source_detail else ""
        lines.append(
            f"- Step {index:02d}: {step.get('direction')} @ {step.get('target_mbps'):.3f} Mbps, hold={step.get('hold_seconds')}s{source_suffix}"
        )

    transition_rows = run.get("transition_summary", [])
    lines.extend(["", "## Limit-Change To Variant-Change Summary", ""])
    if transition_rows:
        lines.extend(_render_transition_summary_table(transition_rows))
    else:
        lines.append("No transition summary rows were generated.")

    huge_summary = run.get("huge_cycle_summary") if isinstance(run, dict) else None
    if isinstance(huge_summary, dict):
        down_rows = huge_summary.get("down") if isinstance(huge_summary.get("down"), list) else []
        up_rows = huge_summary.get("up") if isinstance(huge_summary.get("up"), list) else []
        down_switch_rows = huge_summary.get("down_switches") if isinstance(huge_summary.get("down_switches"), list) else []
        up_switch_rows = huge_summary.get("up_switches") if isinstance(huge_summary.get("up_switches"), list) else []
        if down_rows or up_rows:
            lines.extend(["", "## Steps Cycle Summary", ""])
            if down_rows:
                lines.extend(_render_huge_cycle_table("Step Down (Top -> Bottom)", down_rows))
                lines.append("")
            if up_rows:
                lines.extend(_render_huge_cycle_table("Step Up (Bottom -> Top)", up_rows))
        if down_switch_rows or up_switch_rows:
            lines.extend(["", "## Steps Rendition-Change Summary", ""])
            if down_switch_rows:
                lines.extend(_render_huge_switch_table("Rendition Changes During Down Steps", down_switch_rows))
                lines.append("")
            if up_switch_rows:
                lines.extend(_render_huge_switch_table("Rendition Changes During Up Steps", up_switch_rows))

    smooth_summary = run.get("smooth_switch_summary") if isinstance(run, dict) else None
    if isinstance(smooth_summary, dict):
        event_rows = smooth_summary.get("events") if isinstance(smooth_summary.get("events"), list) else []
        aggregate_rows = smooth_summary.get("aggregate") if isinstance(smooth_summary.get("aggregate"), list) else []
        if event_rows:
            lines.extend(["", "## Smooth Switch Threshold Events", ""])
            lines.extend(_render_smooth_switch_events_table(event_rows))
        if aggregate_rows:
            lines.extend(["", "## Smooth Switch Threshold Summary", ""])
            lines.extend(_render_smooth_switch_aggregate_table(aggregate_rows))

    lines.extend(["", "## Artifacts", "", f"- JSON: {json_path}", f"- Markdown: {md_path}"])

    with open(md_path, "w", encoding="utf-8") as fh:
        fh.write("\n".join(lines) + "\n")

    return json_path, md_path


def _slugify(value: str) -> str:
    cleaned = re.sub(r"[^a-zA-Z0-9]+", "-", str(value or "").strip()).strip("-")
    cleaned = re.sub(r"-+", "-", cleaned)
    return (cleaned or "run").lower()[:80]


@pytest.mark.integration
@pytest.mark.slow
@pytest.mark.abrchar
def test_player_characterization_host_runner(
    request,
    api_base,
    stream_info,
    session_id,
    config,
    clean_session,
    tmp_path,
):
    """Run host-side ABR characterization against an active playback session."""

    run_number = int(request.config.cache.get("abrchar/run_counter", 0)) + 1
    request.config.cache.set("abrchar/run_counter", run_number)
    content_name = stream_info.get("content_name") or "stream"
    started_stamp = datetime.now(timezone.utc).strftime("%Y-%m-%d %H:%M:%SZ")
    configured_name = str(getattr(config, "abrchar_run_name", "") or "").strip()
    run_name = configured_name or f"Run #{run_number} - {content_name} - {started_stamp}"

    if config.verbose:
        print(
            f"{utc_now_iso()} ABRCHAR start run_number={run_number} run_name={run_name} "
            f"session_id={session_id} player_url={stream_info.get('master_url') or stream_info.get('media_url')}",
            flush=True,
        )

    initial = fetch_session_snapshot(api_base, session_id, verbose=config.verbose) or {}
    session_variants = initial.get("manifest_variants") if isinstance(initial, dict) else None

    variants: list[dict[str, Any]] = []
    if isinstance(session_variants, list):
        variants = [item for item in session_variants if isinstance(item, dict) and _to_number(item.get("bandwidth"), None)]

    if not variants:
        master_url = stream_info.get("master_url")
        if not master_url:
            pytest.fail("No master manifest URL available for characterization")
        variants = _parse_master_variants(master_url, timeout=config.timeout, verbose=config.verbose)

    ladder_mbps = _unique_sorted_positive([_variant_to_mbps(item) for item in variants if _variant_to_mbps(item) is not None])
    if not ladder_mbps:
        pytest.fail("Unable to build bitrate ladder for characterization")
    variant_catalog = _build_variant_catalog(variants)

    net_overhead_pct = (
        float(config.net_overhead_pct)
        if getattr(config, "net_overhead_pct", None) is not None
        else float(config.abrchar_overhead_pct)
    )
    configured_smooth_step_seconds = getattr(config, "abrchar_smooth_step_seconds", None)
    smooth_step_seconds = max(
        1,
        int(configured_smooth_step_seconds)
        if configured_smooth_step_seconds is not None
        else int(config.abrchar_hold_seconds),
    )
    step_gap_seconds = max(0.0, float(getattr(config, "abrchar_step_gap_seconds", 0.0) or 0.0))
    test_mode = str(getattr(config, "abrchar_test_mode", "smooth"))
    repeat_count = max(1, int(getattr(config, "abrchar_repeat_count", 10)))

    if test_mode == "steps":
        schedule = _build_huge_steps_schedule(
            ladder_mbps=ladder_mbps,
            cycles=repeat_count,
        )
        expanded_steps = schedule
    else:
        schedule = _build_variant_aware_schedule(
            ladder_mbps=ladder_mbps,
            hold_seconds=smooth_step_seconds,
            overhead_pct=net_overhead_pct,
            max_steps=config.abrchar_max_steps,
        )
        expanded_steps = []
        for cycle_index in range(repeat_count):
            for template_index, template_step in enumerate(schedule):
                step_copy = dict(template_step)
                step_copy["cycle_index"] = cycle_index + 1
                step_copy["template_step_index"] = template_index
                expanded_steps.append(step_copy)

    if not schedule:
        pytest.fail("Characterization schedule is empty")

    if not expanded_steps:
        pytest.fail("Characterization expanded schedule is empty")

    if config.verbose:
        print(
            f"{utc_now_iso()} ABRCHAR schedule_ready template_steps={len(schedule)} "
            f"repeat_count={repeat_count} total_steps={len(expanded_steps)} "
            f"mode={test_mode} ladder_mbps={ladder_mbps} "
            f"net_overhead_pct={net_overhead_pct} test_mode={test_mode} "
            f"smooth_step_s={smooth_step_seconds if test_mode != 'steps' else '-'} step_gap_s={step_gap_seconds if test_mode != 'steps' else '-'}",
            flush=True,
        )
        if variant_catalog:
            print(f"{utc_now_iso()} ABRCHAR variants_used", flush=True)
            for row in variant_catalog:
                print(
                    f"  V{row.get('index')}: res={row.get('resolution') or '-'} "
                    f"bw={row.get('bandwidth_mbps')} avg={row.get('average_mbps')} selected={row.get('selected_mbps')}",
                    flush=True,
                )
        if test_mode != "steps":
            print(f"{utc_now_iso()} ABRCHAR smooth_limit_template", flush=True)
            for idx, step in enumerate(schedule, start=1):
                print(
                    f"  T{idx:02d}: {step.get('direction')} {float(step.get('target_mbps', 0)):.3f} Mbps "
                    f"source={step.get('source_detail') or step.get('source')}",
                    flush=True,
                )

    run: dict[str, Any] = {
        "run_number": run_number,
        "run_name": run_name,
        "session_id": session_id,
        "started_at": utc_now_iso(),
        "test_mode": test_mode,
        "repeat_count": repeat_count,
        "ladder_mbps": ladder_mbps,
        "variant_catalog": variant_catalog,
        "net_overhead_pct": net_overhead_pct,
        "schedule_template": schedule,
        "steps": expanded_steps,
        "samples": [],
        "switch_events": [],
        "loop_completion_events": [],
        "step_transition_summary": [],
        "recovery_events": [],
        "warnings": [],
        "summary": {},
    }

    huge_mode = test_mode == "steps"
    last_observed_player_restarts = int(_to_number(initial.get("player_restarts"), 0) or 0)
    last_variant = _timing_variant_mbps(initial)
    top_variant_mbps = float(ladder_mbps[-1])
    top_variant_tolerance = max(0.05, top_variant_mbps * 0.02)
    loop_completion_target_s = 30.0
    loop_state_by_cycle: dict[int, dict[str, Any]] = {}

    def _observe_loop_completion(sample: dict[str, Any], step: dict[str, Any]) -> None:
        cycle_index = int(_to_number(step.get("cycle_index"), 1) or 1)
        state = loop_state_by_cycle.setdefault(
            cycle_index,
            {
                "completed": False,
                "top_since_ts": None,
                "last_ts": None,
                "last_stall_count": None,
                "last_stall_time_s": None,
            },
        )

        ts = _parse_iso_z(sample.get("ts"))
        variant = _to_number(sample.get("variant_mbps"), None)
        stall_count = _to_number(sample.get("stall_count"), None)
        stall_time_s = _to_number(sample.get("stall_time_s"), None)

        if state.get("completed"):
            state["last_ts"] = ts
            state["last_stall_count"] = stall_count
            state["last_stall_time_s"] = stall_time_s
            return

        stall_increased = False
        if stall_count is not None and state.get("last_stall_count") is not None:
            stall_increased = stall_increased or stall_count > float(state["last_stall_count"])
        if stall_time_s is not None and state.get("last_stall_time_s") is not None:
            stall_increased = stall_increased or stall_time_s > float(state["last_stall_time_s"]) + 0.01

        at_top_variant = (
            variant is not None
            and abs(float(variant) - top_variant_mbps) <= top_variant_tolerance
        )

        if not at_top_variant or stall_increased:
            state["top_since_ts"] = None
        elif state.get("top_since_ts") is None and ts is not None:
            state["top_since_ts"] = ts

        dwell_seconds = None
        if ts is not None and state.get("top_since_ts") is not None:
            dwell_seconds = max(0.0, (ts - state["top_since_ts"]).total_seconds())

        if dwell_seconds is not None and dwell_seconds >= loop_completion_target_s:
            state["completed"] = True
            event = {
                "ts": sample.get("ts"),
                "cycle_index": cycle_index,
                "step_index": sample.get("step_index"),
                "top_variant_mbps": round(top_variant_mbps, 3),
                "observed_variant_mbps": round(float(variant), 3) if variant is not None else None,
                "top_play_seconds": round(dwell_seconds, 3),
                "criterion": "top_variant_stable_30s",
            }
            run["loop_completion_events"].append(event)
            print(
                f"{utc_now_iso()} ABRCHAR loop_complete cycle={cycle_index}/{repeat_count} "
                f"top_variant_mbps={top_variant_mbps:.3f} dwell_s={dwell_seconds:.1f}",
                flush=True,
            )

        state["last_ts"] = ts
        state["last_stall_count"] = stall_count
        state["last_stall_time_s"] = stall_time_s

    def _avg_from_samples(samples: list[dict[str, Any]], key: str) -> float | None:
        values = [_to_number(item.get(key), None) for item in samples]
        clean = [float(value) for value in values if value is not None]
        return round(_mean(clean), 3) if clean else None

    def _sample_from_snapshot(step_index: int, step: dict[str, Any], target: float, snap: dict[str, Any]) -> dict[str, Any]:
        player_variant = _to_number(snap.get("player_metrics_video_bitrate_mbps"), None)
        server_variant = _to_number(snap.get("server_video_rendition_mbps"), None)
        timing_variant = server_variant if server_variant is not None else player_variant
        return {
            "ts": utc_now_iso(),
            "step_index": step_index,
            "cycle_index": step.get("cycle_index"),
            "step_kind": step.get("step_kind"),
            "target_mbps": target,
            "direction": step.get("direction"),
            "throughput_mbps": _throughput_mbps(snap),
            "variant_mbps": player_variant,
            "server_variant_mbps": server_variant,
            "timing_variant_mbps": timing_variant,
            "network_bitrate_mbps": _to_number(snap.get("player_metrics_network_bitrate_mbps"), None),
            "buffer_depth_s": _to_number(snap.get("player_metrics_buffer_depth_s"), None),
            "frames_displayed": _to_number(snap.get("player_metrics_frames_displayed"), None),
            "stall_count": _to_number(snap.get("player_metrics_stall_count"), None),
            "stall_time_s": _to_number(snap.get("player_metrics_stall_time_s"), None),
            "player_restarts": int(_to_number(snap.get("player_restarts"), 0) or 0),
        }

    def _observe_player_restarts(sample: dict[str, Any], step_index: int) -> None:
        nonlocal last_observed_player_restarts
        current = int(_to_number(sample.get("player_restarts"), 0) or 0)
        if current <= last_observed_player_restarts:
            return
        delta = current - last_observed_player_restarts
        event = {
            "ts": sample.get("ts"),
            "type": "player_restart_observed",
            "step_index": step_index,
            "cycle_index": sample.get("cycle_index"),
            "player_restarts": current,
            "delta": delta,
        }
        run["recovery_events"].append(event)
        print(
            f"{utc_now_iso()} ABRCHAR recovery player_restart_observed "
            f"step_index={step_index + 1} delta={delta} total={current}",
            flush=True,
        )
        last_observed_player_restarts = current

    try:
        for step_index, step in enumerate(run["steps"]):
            target = float(step["target_mbps"])
            if config.verbose:
                print(
                    f"{utc_now_iso()} ABRCHAR step_begin index={step_index + 1}/{len(run['steps'])} "
                    f"cycle={step.get('cycle_index', 1)}/{repeat_count} "
                    f"direction={step.get('direction')} step_kind={step.get('step_kind', 'smooth')} "
                    f"target_mbps={target:.3f}",
                    flush=True,
                )

            if huge_mode:
                target_variant = _to_number(step.get("target_variant_mbps"), None)
                if target_variant is None:
                    run["warnings"].append(
                        {
                            "type": "huge_step_missing_target_variant",
                            "step_index": step_index,
                            "step": step,
                        }
                    )
                    continue

                target_variant = float(target_variant)
                target_tolerance = max(0.05, target_variant * 0.02)
                step_started_mono = time.time()
                step_samples: list[dict[str, Any]] = []
                step_start_frames: float | None = None
                step_start_stall_count: float | None = None
                step_start_stall_time_s: float | None = None

                _apply_rate(api_base, session_id, target)
                ok, observed = _confirm_rate(api_base, session_id, target)
                if not ok:
                    run["warnings"].append(
                        {
                            "type": "shape_confirm_failed",
                            "step_index": step_index,
                            "target_mbps": target,
                            "observed_mbps": observed,
                        }
                    )

                wait_deadline = time.time() + 240.0
                target_reached = False
                time_to_target_s: float | None = None
                while time.time() < wait_deadline:
                    snap = fetch_session_snapshot(api_base, session_id, verbose=False) or {}
                    sample = _sample_from_snapshot(step_index, step, target, snap)
                    run["samples"].append(sample)
                    _observe_player_restarts(sample, step_index)
                    step_samples.append(sample)

                    if step_start_frames is None and sample.get("frames_displayed") is not None:
                        step_start_frames = float(sample["frames_displayed"])
                    if step_start_stall_count is None and sample.get("stall_count") is not None:
                        step_start_stall_count = float(sample["stall_count"])
                    if step_start_stall_time_s is None and sample.get("stall_time_s") is not None:
                        step_start_stall_time_s = float(sample["stall_time_s"])

                    variant = _to_number(sample.get("timing_variant_mbps"), None)
                    if variant is not None and last_variant is not None and abs(float(variant) - float(last_variant)) >= 0.05:
                        elapsed = round(max(0.0, time.time() - step_started_mono), 3)
                        frames_delta = None
                        current_frames = _to_number(sample.get("frames_displayed"), None)
                        if current_frames is not None and step_start_frames is not None:
                            frames_delta = round(max(0.0, float(current_frames) - float(step_start_frames)), 3)
                        current_stall_count = _to_number(sample.get("stall_count"), None)
                        current_stall_time_s = _to_number(sample.get("stall_time_s"), None)
                        stall_count_delta = None
                        stall_time_delta_s = None
                        if current_stall_count is not None and step_start_stall_count is not None:
                            stall_count_delta = round(max(0.0, float(current_stall_count) - float(step_start_stall_count)), 3)
                        if current_stall_time_s is not None and step_start_stall_time_s is not None:
                            stall_time_delta_s = round(max(0.0, float(current_stall_time_s) - float(step_start_stall_time_s)), 3)
                        event = {
                            "ts": sample["ts"],
                            "step_index": step_index,
                            "cycle_index": step.get("cycle_index"),
                            "step_kind": step.get("step_kind"),
                            "from_variant_mbps": float(last_variant),
                            "to_variant_mbps": float(variant),
                            "target_mbps": target,
                            "target_variant_mbps": target_variant,
                            "time_from_limit_change_s": elapsed,
                            "frames_presented_delta": frames_delta,
                            "stall_count_delta": stall_count_delta,
                            "stall_time_delta_s": stall_time_delta_s,
                            "avg_buffer_depth_s": _avg_from_samples(step_samples, "buffer_depth_s"),
                            "avg_throughput_mbps": _avg_from_samples(step_samples, "throughput_mbps"),
                            "avg_variant_mbps": _avg_from_samples(step_samples, "variant_mbps"),
                            "variant_source": "server_rendition" if sample.get("server_variant_mbps") is not None else "player_variant",
                        }
                        run["switch_events"].append(event)
                        print(
                            f"{utc_now_iso()} ABRCHAR rendition_change cycle={step.get('cycle_index')} "
                            f"step_kind={step.get('step_kind')} from={float(last_variant):.3f} to={float(variant):.3f} "
                            f"time_s={elapsed:.3f} frames_presented={event.get('frames_presented_delta')} "
                            f"avg_buffer_s={event.get('avg_buffer_depth_s')} avg_throughput_mbps={event.get('avg_throughput_mbps')}",
                            flush=True,
                        )

                    if variant is not None:
                        last_variant = float(variant)
                        if abs(float(variant) - target_variant) <= target_tolerance:
                            target_reached = True
                            time_to_target_s = round(max(0.0, time.time() - step_started_mono), 3)
                            break

                    time.sleep(1)

                if not target_reached:
                    run["warnings"].append(
                        {
                            "type": "target_rendition_timeout",
                            "step_index": step_index,
                            "cycle_index": step.get("cycle_index"),
                            "step_kind": step.get("step_kind"),
                            "target_variant_mbps": target_variant,
                            "timeout_seconds": 240,
                        }
                    )

                hold_target_seconds = 0
                for _ in range(30):
                    snap = fetch_session_snapshot(api_base, session_id, verbose=False) or {}
                    sample = _sample_from_snapshot(step_index, step, target, snap)
                    run["samples"].append(sample)
                    _observe_player_restarts(sample, step_index)
                    step_samples.append(sample)

                    if step_start_frames is None and sample.get("frames_displayed") is not None:
                        step_start_frames = float(sample["frames_displayed"])
                    if step_start_stall_count is None and sample.get("stall_count") is not None:
                        step_start_stall_count = float(sample["stall_count"])
                    if step_start_stall_time_s is None and sample.get("stall_time_s") is not None:
                        step_start_stall_time_s = float(sample["stall_time_s"])

                    variant = _to_number(sample.get("timing_variant_mbps"), None)
                    if variant is not None and abs(float(variant) - target_variant) <= target_tolerance:
                        hold_target_seconds += 1

                    if variant is not None and last_variant is not None and abs(float(variant) - float(last_variant)) >= 0.05:
                        elapsed = round(max(0.0, time.time() - step_started_mono), 3)
                        frames_delta = None
                        current_frames = _to_number(sample.get("frames_displayed"), None)
                        if current_frames is not None and step_start_frames is not None:
                            frames_delta = round(max(0.0, float(current_frames) - float(step_start_frames)), 3)
                        current_stall_count = _to_number(sample.get("stall_count"), None)
                        current_stall_time_s = _to_number(sample.get("stall_time_s"), None)
                        stall_count_delta = None
                        stall_time_delta_s = None
                        if current_stall_count is not None and step_start_stall_count is not None:
                            stall_count_delta = round(max(0.0, float(current_stall_count) - float(step_start_stall_count)), 3)
                        if current_stall_time_s is not None and step_start_stall_time_s is not None:
                            stall_time_delta_s = round(max(0.0, float(current_stall_time_s) - float(step_start_stall_time_s)), 3)
                        event = {
                            "ts": sample["ts"],
                            "step_index": step_index,
                            "cycle_index": step.get("cycle_index"),
                            "step_kind": step.get("step_kind"),
                            "from_variant_mbps": float(last_variant),
                            "to_variant_mbps": float(variant),
                            "target_mbps": target,
                            "target_variant_mbps": target_variant,
                            "time_from_limit_change_s": elapsed,
                            "frames_presented_delta": frames_delta,
                            "stall_count_delta": stall_count_delta,
                            "stall_time_delta_s": stall_time_delta_s,
                            "avg_buffer_depth_s": _avg_from_samples(step_samples, "buffer_depth_s"),
                            "avg_throughput_mbps": _avg_from_samples(step_samples, "throughput_mbps"),
                            "avg_variant_mbps": _avg_from_samples(step_samples, "variant_mbps"),
                            "variant_source": "server_rendition" if sample.get("server_variant_mbps") is not None else "player_variant",
                        }
                        run["switch_events"].append(event)
                        print(
                            f"{utc_now_iso()} ABRCHAR rendition_change cycle={step.get('cycle_index')} "
                            f"step_kind={step.get('step_kind')} from={float(last_variant):.3f} to={float(variant):.3f} "
                            f"time_s={elapsed:.3f} frames_presented={event.get('frames_presented_delta')} "
                            f"avg_buffer_s={event.get('avg_buffer_depth_s')} avg_throughput_mbps={event.get('avg_throughput_mbps')}",
                            flush=True,
                        )

                    if variant is not None:
                        last_variant = float(variant)

                    time.sleep(1)

                end_frames = _to_number(step_samples[-1].get("frames_displayed"), None) if step_samples else None
                frames_delta_total = None
                if end_frames is not None and step_start_frames is not None:
                    frames_delta_total = round(max(0.0, float(end_frames) - float(step_start_frames)), 3)

                step_result = {
                    "cycle_index": step.get("cycle_index"),
                    "step_index": step_index,
                    "step_kind": step.get("step_kind"),
                    "direction": step.get("direction"),
                    "target_mbps": target,
                    "target_variant_mbps": target_variant,
                    "target_reached": target_reached,
                    "time_to_target_s": time_to_target_s,
                    "hold_target_seconds": hold_target_seconds,
                    "frames_presented_delta": frames_delta_total,
                    "avg_buffer_depth_s": _avg_from_samples(step_samples, "buffer_depth_s"),
                    "avg_throughput_mbps": _avg_from_samples(step_samples, "throughput_mbps"),
                    "avg_variant_mbps": _avg_from_samples(step_samples, "variant_mbps"),
                    "avg_network_bitrate_mbps": _avg_from_samples(step_samples, "network_bitrate_mbps"),
                }
                run["step_transition_summary"].append(step_result)
                print(
                    f"{utc_now_iso()} ABRCHAR step_summary cycle={step.get('cycle_index')} "
                    f"step_kind={step.get('step_kind')} target_reached={target_reached} "
                    f"time_to_target_s={time_to_target_s} hold_target_s={hold_target_seconds} "
                    f"avg_buffer_s={step_result.get('avg_buffer_depth_s')} avg_throughput_mbps={step_result.get('avg_throughput_mbps')}",
                    flush=True,
                )

                if (
                    str(step.get("step_kind")) == "step-up"
                    and target_reached
                    and hold_target_seconds >= 30
                ):
                    run["loop_completion_events"].append(
                        {
                            "ts": utc_now_iso(),
                            "cycle_index": step.get("cycle_index"),
                            "step_index": step_index,
                            "top_variant_mbps": round(target_variant, 3),
                            "top_play_seconds": hold_target_seconds,
                            "criterion": "top_variant_stable_30s",
                        }
                    )
                    print(
                        f"{utc_now_iso()} ABRCHAR loop_complete cycle={step.get('cycle_index')}/{repeat_count} "
                        f"top_variant_mbps={target_variant:.3f} dwell_s={hold_target_seconds}",
                        flush=True,
                    )
                if (not huge_mode) and step_gap_seconds > 0 and step_index < (len(run["steps"]) - 1):
                    if config.verbose:
                        print(
                            f"{utc_now_iso()} ABRCHAR inter_step_wait step_index={step_index + 1} "
                            f"wait_s={step_gap_seconds:.3f}",
                            flush=True,
                        )
                    time.sleep(step_gap_seconds)
                continue

            _apply_rate(api_base, session_id, target)
            if config.verbose:
                print(
                    f"{utc_now_iso()} ABRCHAR apply_rate_sent session_id={session_id} "
                    f"nftables_bandwidth_mbps={target:.3f}",
                    flush=True,
                )
            ok, observed = _confirm_rate(api_base, session_id, target)
            if config.verbose:
                print(
                    f"{utc_now_iso()} ABRCHAR apply_rate_confirmed ok={ok} "
                    f"target_mbps={target:.3f} observed_mbps={observed}",
                    flush=True,
                )
            if not ok:
                run["warnings"].append(
                    {
                        "type": "shape_confirm_failed",
                        "step_index": step_index,
                        "target_mbps": target,
                        "observed_mbps": observed,
                    }
                )

            data_plane_validation = _validate_data_plane_rate_effect(
                api_base,
                session_id,
                target,
                sample_seconds=15,
            )
            if config.verbose:
                print(
                    f"{utc_now_iso()} ABRCHAR dataplane_check target_mbps={target:.3f} "
                    f"result={data_plane_validation}",
                    flush=True,
                )
            if data_plane_validation.get("checked") and data_plane_validation.get("suspicious"):
                run["warnings"].append(
                    {
                        "type": "dataplane_suspicious",
                        "step_index": step_index,
                        "target_mbps": target,
                        "details": data_plane_validation,
                    }
                )
            if data_plane_validation.get("checked") and data_plane_validation.get("stagnant"):
                run["warnings"].append(
                    {
                        "type": "dataplane_stagnant",
                        "step_index": step_index,
                        "target_mbps": target,
                        "details": data_plane_validation,
                    }
                )

            settle_deadline = time.time() + max(5, int(config.abrchar_settle_timeout))
            settle_needed = max(2, int(round(max(0.05, float(config.abrchar_settle_tolerance)) * 10)))
            settle_hits = 0
            settled = False
            last_settle_log_at = 0.0
            step_start_stall_count: float | None = None
            step_start_stall_time_s: float | None = None

            while time.time() < settle_deadline:
                snap = fetch_session_snapshot(api_base, session_id, verbose=False) or {}
                throughput = _throughput_mbps(snap)
                variant = _timing_variant_mbps(snap)
                stall_count = _to_number(snap.get("player_metrics_stall_count"), None)
                stall_time = _to_number(snap.get("player_metrics_stall_time_s"), None)
                sample = _sample_from_snapshot(step_index, step, target, snap)
                run["samples"].append(sample)
                _observe_player_restarts(sample, step_index)
                _observe_loop_completion(sample, step)
                if step_start_stall_count is None and sample.get("stall_count") is not None:
                    step_start_stall_count = float(sample["stall_count"])
                if step_start_stall_time_s is None and sample.get("stall_time_s") is not None:
                    step_start_stall_time_s = float(sample["stall_time_s"])

                if variant is not None and last_variant is not None and abs(variant - last_variant) >= 0.05:
                    current_stall_count = _to_number(sample.get("stall_count"), None)
                    current_stall_time_s = _to_number(sample.get("stall_time_s"), None)
                    stall_count_delta = None
                    stall_time_delta_s = None
                    if current_stall_count is not None and step_start_stall_count is not None:
                        stall_count_delta = round(max(0.0, float(current_stall_count) - float(step_start_stall_count)), 3)
                    if current_stall_time_s is not None and step_start_stall_time_s is not None:
                        stall_time_delta_s = round(max(0.0, float(current_stall_time_s) - float(step_start_stall_time_s)), 3)
                    run["switch_events"].append(
                        {
                            "ts": sample["ts"],
                            "step_index": step_index,
                            "from_variant_mbps": last_variant,
                            "to_variant_mbps": variant,
                            "target_mbps": target,
                            "throughput_mbps": throughput,
                            "stall_count_delta": stall_count_delta,
                            "stall_time_delta_s": stall_time_delta_s,
                        }
                    )
                if variant is not None:
                    last_variant = variant

                if throughput is not None and target > 0:
                    tolerance = max(0.05, target * float(config.abrchar_settle_tolerance))
                    if abs(throughput - target) <= tolerance:
                        settle_hits += 1
                    else:
                        settle_hits = 0
                    if settle_hits >= settle_needed:
                        settled = True
                        break

                now = time.time()
                if config.verbose and (now - last_settle_log_at) >= 5.0:
                    print(
                        f"{utc_now_iso()} ABRCHAR settle_wait step_index={step_index + 1} "
                        f"target_mbps={target:.3f} throughput_mbps={throughput} variant_mbps={variant} "
                        f"buffer_depth_s={sample.get('buffer_depth_s')} stall_count={stall_count}",
                        flush=True,
                    )
                    last_settle_log_at = now

                time.sleep(1)

            if not settled:
                run["warnings"].append(
                    {
                        "type": "settle_timeout",
                        "step_index": step_index,
                        "target_mbps": target,
                    }
                )
            elif config.verbose:
                print(
                    f"{utc_now_iso()} ABRCHAR settled step_index={step_index + 1} target_mbps={target:.3f}",
                    flush=True,
                )

            hold_seconds = max(1, int(step.get("hold_seconds", smooth_step_seconds)))
            last_hold_log_at = 0.0
            for _ in range(hold_seconds):
                snap = fetch_session_snapshot(api_base, session_id, verbose=False) or {}
                throughput = _throughput_mbps(snap)
                variant = _timing_variant_mbps(snap)
                stall_count = _to_number(snap.get("player_metrics_stall_count"), None)
                stall_time = _to_number(snap.get("player_metrics_stall_time_s"), None)
                sample = _sample_from_snapshot(step_index, step, target, snap)
                run["samples"].append(sample)
                _observe_player_restarts(sample, step_index)
                _observe_loop_completion(sample, step)
                if variant is not None:
                    last_variant = variant
                now = time.time()
                if config.verbose and (now - last_hold_log_at) >= 5.0:
                    print(
                        f"{utc_now_iso()} ABRCHAR hold_monitor step_index={step_index + 1} "
                        f"target_mbps={target:.3f} throughput_mbps={throughput} variant_mbps={variant} "
                        f"stall_count={stall_count} stall_time_s={stall_time}",
                        flush=True,
                    )
                    last_hold_log_at = now
                time.sleep(1)

            if (not huge_mode) and step_gap_seconds > 0 and step_index < (len(run["steps"]) - 1):
                if config.verbose:
                    print(
                        f"{utc_now_iso()} ABRCHAR inter_step_wait step_index={step_index + 1} "
                        f"wait_s={step_gap_seconds:.3f}",
                        flush=True,
                    )
                time.sleep(step_gap_seconds)

    finally:
        if config.verbose:
            print(
                f"{utc_now_iso()} ABRCHAR restore_rate_sent session_id={session_id} "
                f"nftables_bandwidth_mbps={max(10.0, float(config.restore_mbps)):.3f}",
                flush=True,
            )
        _apply_rate(api_base, session_id, max(10.0, float(config.restore_mbps)))

    throughput_values = [
        float(sample["throughput_mbps"])
        for sample in run["samples"]
        if sample.get("throughput_mbps") is not None
    ]
    stall_counts = [
        int(sample["stall_count"])
        for sample in run["samples"]
        if sample.get("stall_count") is not None
    ]
    stall_times = [
        float(sample["stall_time_s"])
        for sample in run["samples"]
        if sample.get("stall_time_s") is not None
    ]
    downswitch_count = sum(
        1
        for event in run["switch_events"]
        if _to_number(event.get("to_variant_mbps"), None) is not None
        and _to_number(event.get("from_variant_mbps"), None) is not None
        and float(event["to_variant_mbps"]) < float(event["from_variant_mbps"])
    )
    upswitch_count = sum(
        1
        for event in run["switch_events"]
        if _to_number(event.get("to_variant_mbps"), None) is not None
        and _to_number(event.get("from_variant_mbps"), None) is not None
        and float(event["to_variant_mbps"]) > float(event["from_variant_mbps"])
    )

    run["summary"] = {
        "step_count": len(run["steps"]),
        "template_step_count": len(schedule),
        "repeat_count": repeat_count,
        "sample_count": len(run["samples"]),
        "switch_count": len(run["switch_events"]),
        "downswitch_count": downswitch_count,
        "upswitch_count": upswitch_count,
        "recovery_event_count": len(run.get("recovery_events", [])),
        "stall_count_delta": (max(stall_counts) - min(stall_counts)) if stall_counts else 0,
        "stall_time_delta_s": round((max(stall_times) - min(stall_times)), 3) if stall_times else 0,
        "throughput_median_mbps": round(_median(throughput_values), 3) if throughput_values else None,
    }
    run["transition_summary"] = _build_transition_summary(run)

    transition_latencies = [
        float(row["time_to_variant_change_s"])
        for row in run["transition_summary"]
        if row.get("time_to_variant_change_s") is not None
    ]
    run["summary"]["transition_rows"] = len(run["transition_summary"])
    run["summary"]["transition_observed_count"] = len(transition_latencies)
    run["summary"]["transition_latency_median_s"] = (
        round(_median(transition_latencies), 3) if transition_latencies else None
    )
    run["summary"]["loop_completion_count"] = len(run.get("loop_completion_events", []))
    if not huge_mode:
        run["smooth_switch_summary"] = _build_smooth_switch_summary(run)

    if huge_mode:
        down_rows = [
            row
            for row in run.get("step_transition_summary", [])
            if str(row.get("step_kind")) == "step-down"
        ]
        up_rows = [
            row
            for row in run.get("step_transition_summary", [])
            if str(row.get("step_kind")) == "step-up"
        ]
        down_switch_rows = [
            row
            for row in run.get("switch_events", [])
            if str(row.get("step_kind")) == "step-down"
        ]
        up_switch_rows = [
            row
            for row in run.get("switch_events", [])
            if str(row.get("step_kind")) == "step-up"
        ]
        run["huge_cycle_summary"] = {
            "down": sorted(down_rows, key=lambda item: int(_to_number(item.get("cycle_index"), 0) or 0)),
            "up": sorted(up_rows, key=lambda item: int(_to_number(item.get("cycle_index"), 0) or 0)),
            "down_switches": sorted(down_switch_rows, key=lambda item: int(_to_number(item.get("cycle_index"), 0) or 0)),
            "up_switches": sorted(up_switch_rows, key=lambda item: int(_to_number(item.get("cycle_index"), 0) or 0)),
        }

        down_timings = [
            float(_to_number(item.get("time_to_target_s"), 0.0) or 0.0)
            for item in down_rows
            if item.get("target_reached")
        ]
        up_timings = [
            float(_to_number(item.get("time_to_target_s"), 0.0) or 0.0)
            for item in up_rows
            if item.get("target_reached")
        ]
        run["summary"]["huge_down_reached_count"] = len(down_timings)
        run["summary"]["huge_up_reached_count"] = len(up_timings)
        run["summary"]["huge_down_timing_median_s"] = round(_median(down_timings), 3) if down_timings else None
        run["summary"]["huge_up_timing_median_s"] = round(_median(up_timings), 3) if up_timings else None
        run["summary"]["huge_down_timing_p95_s"] = round(_percentile(down_timings, 95), 3) if down_timings else None
        run["summary"]["huge_up_timing_p95_s"] = round(_percentile(up_timings, 95), 3) if up_timings else None

    completed_cycles = {
        int(_to_number(item.get("cycle_index"), 0) or 0)
        for item in run.get("loop_completion_events", [])
        if int(_to_number(item.get("cycle_index"), 0) or 0) > 0
    }
    missing_cycles = [cycle for cycle in range(1, repeat_count + 1) if cycle not in completed_cycles]
    if missing_cycles:
        run["warnings"].append(
            {
                "type": "loop_completion_missing",
                "missing_cycles": missing_cycles,
                "criterion": "top_variant_stable_30s",
            }
        )
        print(
            f"{utc_now_iso()} ABRCHAR loop_incomplete missing_cycles={missing_cycles} "
            "criterion=top_variant_stable_30s",
            flush=True,
        )

    artifact_prefix = str(
        tmp_path / f"abrchar_run{run_number:04d}_{_slugify(run_name)}_{session_id}_{int(time.time())}"
    )
    json_path, md_path = _write_reports(run, artifact_prefix)

    print(f"{utc_now_iso()} ABRCHAR transition_summary", flush=True)
    for table_line in _render_transition_summary_plain(run.get("transition_summary", [])):
        print(table_line, flush=True)

    huge_summary = run.get("huge_cycle_summary") if isinstance(run, dict) else None
    if isinstance(huge_summary, dict):
        down_rows = huge_summary.get("down") if isinstance(huge_summary.get("down"), list) else []
        up_rows = huge_summary.get("up") if isinstance(huge_summary.get("up"), list) else []
        down_switch_rows = huge_summary.get("down_switches") if isinstance(huge_summary.get("down_switches"), list) else []
        up_switch_rows = huge_summary.get("up_switches") if isinstance(huge_summary.get("up_switches"), list) else []
        if down_rows:
            print(f"{utc_now_iso()} ABRCHAR steps_down_summary", flush=True)
            for table_line in _render_huge_cycle_plain("Step Down (Top -> Bottom)", down_rows):
                print(table_line, flush=True)
        if up_rows:
            print(f"{utc_now_iso()} ABRCHAR steps_up_summary", flush=True)
            for table_line in _render_huge_cycle_plain("Step Up (Bottom -> Top)", up_rows):
                print(table_line, flush=True)
        if down_switch_rows:
            print(f"{utc_now_iso()} ABRCHAR steps_down_switches", flush=True)
            for table_line in _render_huge_switch_plain("Rendition Changes During Down Steps", down_switch_rows):
                print(table_line, flush=True)
        if up_switch_rows:
            print(f"{utc_now_iso()} ABRCHAR steps_up_switches", flush=True)
            for table_line in _render_huge_switch_plain("Rendition Changes During Up Steps", up_switch_rows):
                print(table_line, flush=True)

    smooth_summary = run.get("smooth_switch_summary") if isinstance(run, dict) else None
    if isinstance(smooth_summary, dict):
        aggregate_rows = smooth_summary.get("aggregate") if isinstance(smooth_summary.get("aggregate"), list) else []
        if aggregate_rows:
            print(f"{utc_now_iso()} ABRCHAR smooth_switch_threshold_summary", flush=True)
            for table_line in _render_smooth_switch_aggregate_plain(aggregate_rows):
                print(table_line, flush=True)

    if config.verbose:
        print(f"{utc_now_iso()} ABRCHAR JSON report: {json_path}", flush=True)
        print(f"{utc_now_iso()} ABRCHAR Markdown report: {md_path}", flush=True)

    assert run["samples"], "Characterization collected no samples"
    assert run["summary"]["step_count"] > 0
