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
import uuid
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


def _build_transient_shock_schedule(
    ladder_mbps: list[float],
    overhead_pct: float,
    min_mbps: float | None,
    max_mbps: float | None,
) -> tuple[list[dict[str, Any]], dict[str, Any] | None]:
    if not ladder_mbps:
        return [], None

    overhead_ratio = max(0.0, min(0.25, _to_number(overhead_pct, 10.0) / 100.0))
    wire_multiplier = 1.0 / max(0.01, (1.0 - overhead_ratio))
    floor = max(0.1, float(min_mbps) if min_mbps is not None else (ladder_mbps[0] * wire_multiplier))
    ceiling_default = ladder_mbps[-1] * wire_multiplier * 2.0
    ceiling = max(floor, float(max_mbps) if max_mbps is not None and max_mbps > 0 else ceiling_default)

    top_wire = ladder_mbps[-1] * wire_multiplier
    baseline_target = max(floor, min(ceiling, top_wire * 1.15))
    severities = [
        {"key": "small", "drop_pct": 30},
        {"key": "medium", "drop_pct": 55},
        {"key": "severe", "drop_pct": 80},
    ]

    steps: list[dict[str, Any]] = []
    for order, severity in enumerate(severities, start=1):
        drop_pct = float(severity["drop_pct"])
        shock_target = max(floor, min(ceiling, baseline_target * (1.0 - (drop_pct / 100.0))))
        steps.append(
            {
                "target_mbps": round(baseline_target, 3),
                "direction": "up",
                "hold_seconds": 20,
                "mode": "transient-shock",
                "step_kind": "transient-precondition",
                "shock_severity": severity["key"],
                "shock_drop_pct": drop_pct,
                "shock_order": order,
            }
        )
        steps.append(
            {
                "target_mbps": round(shock_target, 3),
                "direction": "down",
                "hold_seconds": 8,
                "mode": "transient-shock",
                "step_kind": "transient-shock",
                "shock_severity": severity["key"],
                "shock_drop_pct": drop_pct,
                "shock_order": order,
                "skip_settle": True,
                "force_hold_without_settle": True,
            }
        )
        steps.append(
            {
                "target_mbps": round(baseline_target, 3),
                "direction": "up",
                "hold_seconds": 20,
                "mode": "transient-shock",
                "step_kind": "transient-recovery",
                "shock_severity": severity["key"],
                "shock_drop_pct": drop_pct,
                "shock_order": order,
            }
        )

    return steps, {
        "baseline_target_mbps": round(baseline_target, 3),
        "severities": severities,
    }


def _compute_stall_deltas(samples: list[dict[str, Any]]) -> tuple[float | None, float | None]:
    stall_counts = [
        _to_number(sample.get("stall_count"), None)
        for sample in samples
        if _to_number(sample.get("stall_count"), None) is not None
    ]
    stall_times = [
        _to_number(sample.get("stall_time_s"), None)
        for sample in samples
        if _to_number(sample.get("stall_time_s"), None) is not None
    ]
    stall_count_delta = (max(stall_counts) - min(stall_counts)) if stall_counts else None
    stall_time_delta = (max(stall_times) - min(stall_times)) if stall_times else None
    return stall_count_delta, stall_time_delta


def _time_to_buffer_full_seconds(samples: list[dict[str, Any]]) -> tuple[float | None, float | None]:
    """Estimate when buffer becomes effectively full using running-max envelope.

    This intentionally ignores stepped/sawtooth buffer increments by using a
    monotonic envelope and finding the earliest point after which no meaningful
    additional growth occurs.
    """
    points: list[tuple[float, float]] = []
    t0: datetime | None = None
    for sample in samples:
        value = _to_number(sample.get("buffer_depth_s"), None)
        if value is None:
            continue
        ts = _parse_iso_z(sample.get("ts"))
        if ts is None:
            continue
        if t0 is None:
            t0 = ts
        elapsed = max(0.0, (ts - t0).total_seconds())
        points.append((elapsed, float(value)))

    if not points:
        return None, None

    running_max: list[float] = []
    current_max = float("-inf")
    for _, value in points:
        current_max = max(current_max, value)
        running_max.append(current_max)

    final_peak = running_max[-1]
    if final_peak <= 0:
        return None, None

    tolerance = max(0.25, final_peak * 0.03)
    target_floor = final_peak - tolerance

    for idx, (elapsed, _) in enumerate(points):
        if running_max[idx] < target_floor:
            continue
        if idx < len(running_max) - 1:
            future_peak = max(running_max[idx + 1 :])
            if future_peak > running_max[idx] + tolerance:
                continue
        return round(elapsed, 3), round(final_peak, 3)

    return round(points[-1][0], 3), round(final_peak, 3)


def _build_transient_shock_summary(run: dict[str, Any]) -> dict[str, Any] | None:
    severities = ["small", "medium", "severe"]
    summaries: list[dict[str, Any]] = []

    for severity in severities:
        shock_steps = [
            (index, step)
            for index, step in enumerate(run.get("steps", []))
            if str(step.get("mode")) == "transient-shock"
            and str(step.get("shock_severity")) == severity
            and str(step.get("step_kind")) == "transient-shock"
        ]

        if not shock_steps:
            summaries.append(
                {
                    "severity": severity,
                    "drop_pct": None,
                    "downswitch_count": 0,
                    "downswitch_latency_median_s": None,
                    "recovery_upshift_latency_median_s": None,
                    "stall_count_delta_total": 0,
                    "stall_time_delta_s_total": 0,
                    "unexpected_downswitch_during_recovery": 0,
                }
            )
            continue

        down_latencies: list[float] = []
        recovery_latencies: list[float] = []
        downswitch_count = 0
        unexpected_recovery_downswitch_count = 0
        stall_count_total = 0.0
        stall_time_total = 0.0
        representative_drop = None

        for shock_step_index, shock_step in shock_steps:
            shock_switches = [
                event
                for event in run.get("switch_events", [])
                if int(_to_number(event.get("step_index"), -1) or -1) == shock_step_index
            ]
            down_switches = [
                event
                for event in shock_switches
                if _to_number(event.get("to_variant_mbps"), None) is not None
                and _to_number(event.get("from_variant_mbps"), None) is not None
                and float(event["to_variant_mbps"]) < float(event["from_variant_mbps"])
            ]
            if down_switches:
                first_latency = _to_number(down_switches[0].get("time_from_limit_change_s"), None)
                if first_latency is not None:
                    down_latencies.append(float(first_latency))
            downswitch_count += len(down_switches)
            representative_drop = representative_drop if representative_drop is not None else _to_number(shock_step.get("shock_drop_pct"), None)

            shock_samples = [
                sample
                for sample in run.get("samples", [])
                if int(_to_number(sample.get("step_index"), -1) or -1) == shock_step_index
            ]
            stall_count_delta, stall_time_delta = _compute_stall_deltas(shock_samples)
            stall_count_total += float(_to_number(stall_count_delta, 0) or 0)
            stall_time_total += float(_to_number(stall_time_delta, 0) or 0)

            recovery_step_index = None
            for candidate_index, candidate_step in enumerate(run.get("steps", [])):
                if candidate_index <= shock_step_index:
                    continue
                if str(candidate_step.get("mode")) != "transient-shock":
                    continue
                if str(candidate_step.get("step_kind")) != "transient-recovery":
                    continue
                if str(candidate_step.get("shock_severity")) != severity:
                    continue
                if int(_to_number(candidate_step.get("shock_order"), 0) or 0) != int(_to_number(shock_step.get("shock_order"), 0) or 0):
                    continue
                recovery_step_index = candidate_index
                break

            if recovery_step_index is not None:
                recovery_switches = [
                    event
                    for event in run.get("switch_events", [])
                    if int(_to_number(event.get("step_index"), -1) or -1) == recovery_step_index
                ]
                recovery_up = [
                    event
                    for event in recovery_switches
                    if _to_number(event.get("to_variant_mbps"), None) is not None
                    and _to_number(event.get("from_variant_mbps"), None) is not None
                    and float(event["to_variant_mbps"]) > float(event["from_variant_mbps"])
                ]
                recovery_down = [
                    event
                    for event in recovery_switches
                    if _to_number(event.get("to_variant_mbps"), None) is not None
                    and _to_number(event.get("from_variant_mbps"), None) is not None
                    and float(event["to_variant_mbps"]) < float(event["from_variant_mbps"])
                ]
                if recovery_up:
                    first_recovery_latency = _to_number(recovery_up[0].get("time_from_limit_change_s"), None)
                    if first_recovery_latency is not None:
                        recovery_latencies.append(float(first_recovery_latency))
                unexpected_recovery_downswitch_count += len(recovery_down)

                recovery_samples = [
                    sample
                    for sample in run.get("samples", [])
                    if int(_to_number(sample.get("step_index"), -1) or -1) == recovery_step_index
                ]
                recovery_stall_count_delta, recovery_stall_time_delta = _compute_stall_deltas(recovery_samples)
                stall_count_total += float(_to_number(recovery_stall_count_delta, 0) or 0)
                stall_time_total += float(_to_number(recovery_stall_time_delta, 0) or 0)

        summaries.append(
            {
                "severity": severity,
                "drop_pct": representative_drop,
                "downswitch_count": downswitch_count,
                "downswitch_latency_median_s": _median(down_latencies),
                "recovery_upshift_latency_median_s": _median(recovery_latencies),
                "stall_count_delta_total": stall_count_total,
                "stall_time_delta_s_total": round(stall_time_total, 2),
                "unexpected_downswitch_during_recovery": unexpected_recovery_downswitch_count,
            }
        )

    if not any((entry.get("downswitch_count", 0) > 0) or (entry.get("recovery_upshift_latency_median_s") is not None) for entry in summaries):
        return None
    return {"severities": summaries}


def _build_startup_caps_schedule(
    ladder_mbps: list[float],
    overhead_pct: float,
    min_mbps: float | None,
    max_mbps: float | None,
    repeat_count: int,
) -> tuple[list[dict[str, Any]], dict[str, Any] | None]:
    if not ladder_mbps:
        return [], None

    overhead_ratio = max(0.0, min(0.25, _to_number(overhead_pct, 10.0) / 100.0))
    wire_multiplier = 1.0 / max(0.01, (1.0 - overhead_ratio))
    floor = max(0.1, float(min_mbps) if min_mbps is not None else (ladder_mbps[0] * wire_multiplier))
    ceiling_default = ladder_mbps[-1] * wire_multiplier * 2.0
    ceiling = max(floor, float(max_mbps) if max_mbps is not None and max_mbps > 0 else ceiling_default)

    mid_index = max(0, (len(ladder_mbps) - 1) // 2)
    high_target_index = max(0, len(ladder_mbps) - 2)
    scenario_indices: list[tuple[str, int]] = [
        ("low", 0),
        ("mid", mid_index),
        ("high", high_target_index),
    ]
    seen_labels: set[tuple[str, int]] = set()
    deduped_indices: list[tuple[str, int]] = []
    for label, index in scenario_indices:
        key = (label, index)
        if key in seen_labels:
            continue
        seen_labels.add(key)
        deduped_indices.append((label, index))

    caps: list[dict[str, Any]] = []
    for label, target_index in deduped_indices:
        next_index = min(len(ladder_mbps) - 1, target_index + 1)
        target_variant = float(ladder_mbps[target_index])
        next_variant = float(ladder_mbps[next_index])
        midpoint_media = ((target_variant + next_variant) / 2.0) if next_index > target_index else target_variant
        cap_wire = max(floor, min(ceiling, midpoint_media * wire_multiplier))
        caps.append(
            {
                "cap_label": label,
                "cap_target_mbps": cap_wire,
                "target_variant_mbps": round(target_variant, 3),
                "next_variant_mbps": round(next_variant, 3),
                "cap_midpoint_media_mbps": round(midpoint_media, 3),
            }
        )

    repeats = max(1, int(repeat_count))
    steps: list[dict[str, Any]] = []
    scenario_counter = 0
    for cycle_index in range(repeats):
        for cap in caps:
            scenario_counter += 1
            steps.append(
                {
                    "target_mbps": round(cap["cap_target_mbps"], 3),
                    "hold_seconds": 45,
                    "direction": "up",
                    "mode": "startup-caps",
                    "step_kind": "startup-cap",
                    "startup_cap_label": cap["cap_label"],
                    "startup_scenario_index": scenario_counter,
                    "cycle_index": cycle_index + 1,
                    "target_variant_mbps": cap["target_variant_mbps"],
                    "next_variant_mbps": cap["next_variant_mbps"],
                    "cap_midpoint_media_mbps": cap["cap_midpoint_media_mbps"],
                    "restart_playback_before_step": True,
                    "skip_settle": True,
                    "force_hold_without_settle": True,
                }
            )

    return steps, {"caps": caps, "repeat_count": repeats}


def _build_downshift_severity_schedule(
    ladder_mbps: list[float],
    overhead_pct: float,
    min_mbps: float | None,
    max_mbps: float | None,
    repeat_count: int,
) -> tuple[list[dict[str, Any]], dict[str, Any] | None]:
    if not ladder_mbps:
        return [], None

    overhead_ratio = max(0.0, min(0.25, _to_number(overhead_pct, 10.0) / 100.0))
    wire_multiplier = 1.0 / max(0.01, (1.0 - overhead_ratio))
    floor = max(0.1, float(min_mbps) if min_mbps is not None else (ladder_mbps[0] * wire_multiplier))
    ceiling_default = ladder_mbps[-1] * wire_multiplier * 2.0
    ceiling = max(floor, float(max_mbps) if max_mbps is not None and max_mbps > 0 else ceiling_default)
    top_wire = ladder_mbps[-1] * wire_multiplier
    high_target = max(floor, min(ceiling, top_wire * 1.12))

    severity_buckets = [
        {"key": "small", "target_drop_pct": 30},
        {"key": "medium", "target_drop_pct": 55},
        {"key": "severe", "target_drop_pct": 80},
    ]

    repeats = max(1, int(repeat_count))
    steps: list[dict[str, Any]] = []
    for cycle_index in range(repeats):
        for order, bucket in enumerate(severity_buckets, start=1):
            drop_pct = float(bucket["target_drop_pct"])
            target = max(floor, min(ceiling, high_target * (1.0 - (drop_pct / 100.0))))
            steps.append(
                {
                    "target_mbps": round(high_target, 3),
                    "hold_seconds": 15,
                    "direction": "up",
                    "mode": "downshift-severity",
                    "step_kind": "severity-precondition",
                    "severity_bucket": bucket["key"],
                    "severity_drop_pct": drop_pct,
                    "severity_order": order + (cycle_index * len(severity_buckets)),
                    "cycle_index": cycle_index + 1,
                }
            )
            steps.append(
                {
                    "target_mbps": round(target, 3),
                    "hold_seconds": 25,
                    "direction": "down",
                    "mode": "downshift-severity",
                    "step_kind": "severity-drop",
                    "severity_bucket": bucket["key"],
                    "severity_drop_pct": drop_pct,
                    "severity_order": order + (cycle_index * len(severity_buckets)),
                    "cycle_index": cycle_index + 1,
                }
            )

    return steps, {
        "severity_buckets": severity_buckets,
        "high_target_mbps": round(high_target, 3),
        "repeat_count": repeats,
    }


def _build_hysteresis_gap_schedule(
    ladder_mbps: list[float],
    overhead_pct: float,
    min_mbps: float | None,
    max_mbps: float | None,
    repeat_count: int,
) -> tuple[list[dict[str, Any]], dict[str, Any] | None]:
    if len(ladder_mbps) < 2:
        return [], None

    overhead_ratio = max(0.0, min(0.25, _to_number(overhead_pct, 10.0) / 100.0))
    wire_multiplier = 1.0 / max(0.01, (1.0 - overhead_ratio))
    floor = max(0.1, float(min_mbps) if min_mbps is not None else (ladder_mbps[0] * wire_multiplier))
    ceiling_default = ladder_mbps[-1] * wire_multiplier * 2.0
    ceiling = max(floor, float(max_mbps) if max_mbps is not None and max_mbps > 0 else ceiling_default)

    repeats = max(1, int(repeat_count))
    steps: list[dict[str, Any]] = []
    for cycle_index in range(repeats):
        for low_index in range(0, len(ladder_mbps) - 1):
            high_index = low_index + 1
            down_probe_target = max(floor, min(ceiling, ladder_mbps[low_index] * wire_multiplier * 1.02))
            up_probe_target = max(floor, min(ceiling, ladder_mbps[high_index] * wire_multiplier * 0.98))
            rung_label = f"V{low_index + 1}<->V{high_index + 1}"
            steps.append(
                {
                    "target_mbps": round(down_probe_target, 3),
                    "hold_seconds": 18,
                    "direction": "down",
                    "mode": "hysteresis-gap",
                    "step_kind": "hysteresis-down-probe",
                    "rung_low_index": low_index,
                    "rung_high_index": high_index,
                    "rung_label": rung_label,
                    "cycle_index": cycle_index + 1,
                }
            )
            steps.append(
                {
                    "target_mbps": round(up_probe_target, 3),
                    "hold_seconds": 18,
                    "direction": "up",
                    "mode": "hysteresis-gap",
                    "step_kind": "hysteresis-up-probe",
                    "rung_low_index": low_index,
                    "rung_high_index": high_index,
                    "rung_label": rung_label,
                    "cycle_index": cycle_index + 1,
                }
            )

    return steps, {"pair_count": len(ladder_mbps) - 1, "repeat_count": repeats}


def _build_emergency_downshift_schedule(
    ladder_mbps: list[float],
    overhead_pct: float,
    min_mbps: float | None,
    max_mbps: float | None,
    repeat_count: int,
) -> tuple[list[dict[str, Any]], dict[str, Any] | None]:
    if len(ladder_mbps) < 2:
        return [], None

    overhead_ratio = max(0.0, min(0.25, _to_number(overhead_pct, 10.0) / 100.0))
    wire_multiplier = 1.0 / max(0.01, (1.0 - overhead_ratio))
    floor = max(0.1, float(min_mbps) if min_mbps is not None else (ladder_mbps[0] * wire_multiplier))
    ceiling_default = ladder_mbps[-1] * wire_multiplier * 6.0
    ceiling = max(floor, float(max_mbps) if max_mbps is not None and max_mbps > 0 else ceiling_default)

    top_variant = ladder_mbps[-1]
    second_bottom = ladder_mbps[1]
    bottom = ladder_mbps[0]
    high_target = max(floor, min(ceiling, top_variant * wire_multiplier * 3.0))
    low_midpoint = (bottom + second_bottom) / 2.0
    low_target = max(floor, min(ceiling, low_midpoint * wire_multiplier * 1.05))

    cycles = max(1, int(repeat_count))
    steps: list[dict[str, Any]] = []
    for cycle_index in range(1, cycles + 1):
        steps.append(
            {
                "target_mbps": round(high_target, 3),
                "hold_seconds": 30,
                "direction": "up",
                "mode": "emergency-downshift",
                "step_kind": "emergency-high",
                "cycle_index": cycle_index,
            }
        )
        steps.append(
            {
                "target_mbps": round(low_target, 3),
                "hold_seconds": 30,
                "direction": "down",
                "mode": "emergency-downshift",
                "step_kind": "emergency-low",
                "cycle_index": cycle_index,
            }
        )

    return steps, {
        "cycle_count": cycles,
        "high_target_mbps": round(high_target, 3),
        "low_target_mbps": round(low_target, 3),
    }


def _pick_sparse_ladder(ladder_mbps: list[float], sparse_count: int) -> list[float]:
    if not ladder_mbps:
        return []
    count = max(1, int(sparse_count))
    if count >= len(ladder_mbps):
        return [float(v) for v in ladder_mbps]
    if count == 1:
        return [float(ladder_mbps[0])]
    out: list[float] = []
    for i in range(count):
        pos = i * (len(ladder_mbps) - 1) / (count - 1)
        idx = int(round(pos))
        out.append(float(ladder_mbps[idx]))
    return _unique_sorted_positive(out)


def _build_throughput_accuracy_schedule(
    ladder_mbps: list[float],
    overhead_pct: float,
    min_mbps: float | None,
    max_mbps: float | None,
    repeat_count: int,
    max_limit_mbps: float,
    sparse_variants: int,
) -> tuple[list[dict[str, Any]], dict[str, Any] | None]:
    if not ladder_mbps:
        return [], None

    overhead_ratio = max(0.0, min(0.25, _to_number(overhead_pct, 10.0) / 100.0))
    wire_multiplier = 1.0 / max(0.01, (1.0 - overhead_ratio))
    floor = max(0.1, float(min_mbps) if min_mbps is not None else (ladder_mbps[0] * wire_multiplier))
    configured_cap = _to_number(max_limit_mbps, 100.0)
    ceiling_default = ladder_mbps[-1] * wire_multiplier * 4.0
    ceiling = max(floor, configured_cap if configured_cap is not None and configured_cap > 0 else ceiling_default)
    if max_mbps is not None and max_mbps > 0:
        ceiling = min(ceiling, float(max_mbps))

    sparse_ladder = _pick_sparse_ladder(ladder_mbps, sparse_variants)

    # Wide sweep up to max limit for sensitivity/accuracy characterization.
    raw_targets = [0.5, 1, 2, 3, 5, 8, 10, 15, 20, 30, 40, 60, 80, 100]
    targets = _unique_sorted_positive([
        max(floor, min(ceiling, float(v)))
        for v in raw_targets
        if float(v) <= ceiling + 1e-9
    ])
    if not targets:
        targets = [round(floor, 3)]

    repeats = max(1, int(repeat_count))
    steps: list[dict[str, Any]] = []
    for cycle_index in range(repeats):
        for idx, target in enumerate(targets):
            prev = targets[idx - 1] if idx > 0 else targets[0]
            direction = "up" if target >= prev else "down"
            steps.append(
                {
                    "target_mbps": round(float(target), 3),
                    "hold_seconds": 20,
                    "direction": direction,
                    "mode": "throughput-accuracy",
                    "step_kind": "throughput-accuracy-sweep",
                    "cycle_index": cycle_index + 1,
                    "sparse_ladder_mbps": sparse_ladder,
                }
            )

    return steps, {
        "sparse_ladder_mbps": sparse_ladder,
        "target_limits_mbps": targets,
        "repeat_count": repeats,
        "max_limit_mbps": round(float(ceiling), 3),
    }


def _build_throughput_calcs_schedule(
    ladder_mbps: list[float],
    overhead_pct: float,
    min_mbps: float | None,
    max_mbps: float | None,
    repeat_count: int,
    max_limit_mbps: float,
    sparse_variants: int,
) -> tuple[list[dict[str, Any]], dict[str, Any] | None]:
    if not ladder_mbps:
        return [], None

    overhead_ratio = max(0.0, min(0.25, _to_number(overhead_pct, 10.0) / 100.0))
    wire_multiplier = 1.0 / max(0.01, (1.0 - overhead_ratio))
    floor = max(0.1, float(min_mbps) if min_mbps is not None else (ladder_mbps[0] * wire_multiplier))
    configured_cap = _to_number(max_limit_mbps, 100.0)
    ceiling_default = ladder_mbps[-1] * wire_multiplier * 4.0
    ceiling = max(floor, configured_cap if configured_cap is not None and configured_cap > 0 else ceiling_default)
    if max_mbps is not None and max_mbps > 0:
        ceiling = min(ceiling, float(max_mbps))

    sparse_ladder = _pick_sparse_ladder(ladder_mbps, sparse_variants)

    # Accuracy sweep: 20% above each ladder rung + fixed 10Mbps jumps up to cap.
    per_variant_targets = [float(v) * 1.2 for v in ladder_mbps]
    fixed_targets = [float(v) for v in range(10, int(min(100.0, ceiling)) + 1, 10)]
    targets = _unique_sorted_positive([
        max(floor, min(ceiling, value))
        for value in (per_variant_targets + fixed_targets)
    ])
    if not targets:
        targets = [round(floor, 3)]

    # Response-time jumps for up/down throughput estimate adaptation.
    top_jump = max(10.0, min(ceiling, 100.0))
    low_jump = max(floor, min(ceiling, 10.0))
    mid_jump = max(floor, min(ceiling, 50.0))
    jump_sequence = [top_jump, low_jump, mid_jump, low_jump, top_jump]

    def make_phase_steps(phase: str, cycle_index: int) -> list[dict[str, Any]]:
        phase_steps: list[dict[str, Any]] = []
        prev_target = targets[0]
        for target in targets:
            direction = "up" if target >= prev_target else "down"
            phase_steps.append(
                {
                    "target_mbps": round(float(target), 3),
                    "hold_seconds": 16,
                    "direction": direction,
                    "mode": "throughput-calcs",
                    "step_kind": "throughput-calcs-accuracy",
                    "phase": phase,
                    "cycle_index": cycle_index,
                }
            )
            prev_target = target
        prev_jump = jump_sequence[0]
        for target in jump_sequence:
            direction = "up" if target >= prev_jump else "down"
            phase_steps.append(
                {
                    "target_mbps": round(float(target), 3),
                    "hold_seconds": 20,
                    "direction": direction,
                    "mode": "throughput-calcs",
                    "step_kind": "throughput-calcs-jump",
                    "phase": phase,
                    "cycle_index": cycle_index,
                    "skip_settle": True,
                    "force_hold_without_settle": True,
                }
            )
            prev_jump = target
        return phase_steps

    repeats = max(1, int(repeat_count))
    steps: list[dict[str, Any]] = []
    for cycle_index in range(1, repeats + 1):
        steps.extend(make_phase_steps("all_variants", cycle_index))
        steps.extend(make_phase_steps("sparse_variants", cycle_index))

    return steps, {
        "target_limits_mbps": targets,
        "jump_sequence_mbps": jump_sequence,
        "repeat_count": repeats,
        "max_limit_mbps": round(float(ceiling), 3),
        "sparse_variant_count": int(max(1, sparse_variants)),
        "sparse_ladder_mbps": sparse_ladder,
        "phases": ["all_variants", "sparse_variants"],
    }


def _build_throughput_accuracy_summary(run: dict[str, Any]) -> dict[str, Any] | None:
    steps = run.get("steps", []) if isinstance(run, dict) else []
    samples = run.get("samples", []) if isinstance(run, dict) else []
    if not isinstance(steps, list) or not isinstance(samples, list) or not steps:
        return None

    rows: list[dict[str, Any]] = []
    for step_index, step in enumerate(steps):
        if str(step.get("mode")) != "throughput-accuracy":
            continue
        step_samples = [
            sample
            for sample in samples
            if int(_to_number(sample.get("step_index"), -1) or -1) == step_index
        ]
        if not step_samples:
            continue
        target = _to_number(step.get("target_mbps"), None)
        if target is None or target <= 0:
            continue

        player_vals = [
            float(v)
            for v in (_to_number(sample.get("network_bitrate_mbps"), None) for sample in step_samples)
            if v is not None and v >= 0
        ]
        server_vals = [
            float(v)
            for v in (_to_number(sample.get("throughput_mbps"), None) for sample in step_samples)
            if v is not None and v >= 0
        ]
        variant_vals = [
            float(v)
            for v in (_to_number(sample.get("timing_variant_mbps"), None) for sample in step_samples)
            if v is not None and v > 0
        ]

        player_med = _median(player_vals) if player_vals else None
        server_med = _median(server_vals) if server_vals else None
        variant_med = _median(variant_vals) if variant_vals else None

        player_vs_limit_pct = abs(player_med - target) / target * 100.0 if player_med is not None else None
        player_vs_server_pct = abs(player_med - server_med) / server_med * 100.0 if player_med is not None and server_med not in (None, 0) else None
        variant_vs_limit_pct = abs(variant_med - target) / target * 100.0 if variant_med is not None else None
        headroom_ratio = target / variant_med if variant_med not in (None, 0) else None

        rows.append(
            {
                "step_index": step_index,
                "cycle_index": int(_to_number(step.get("cycle_index"), 0) or 0),
                "phase": str(step.get("phase") or "default"),
                "target_mbps": float(target),
                "player_median_mbps": player_med,
                "server_median_mbps": server_med,
                "variant_median_mbps": variant_med,
                "player_vs_limit_pct": player_vs_limit_pct,
                "player_vs_server_pct": player_vs_server_pct,
                "variant_vs_limit_pct": variant_vs_limit_pct,
                "headroom_ratio": headroom_ratio,
            }
        )

    if not rows:
        return None

    def bucket_for(headroom_ratio: float | None) -> str:
        if headroom_ratio is None:
            return "unknown"
        if headroom_ratio <= 1.5:
            return "<=1.5x"
        if headroom_ratio <= 3.0:
            return "1.5x-3x"
        if headroom_ratio <= 10.0:
            return "3x-10x"
        return ">10x"

    bucketed: dict[str, list[dict[str, Any]]] = {}
    for row in rows:
        key = bucket_for(_to_number(row.get("headroom_ratio"), None))
        bucketed.setdefault(key, []).append(row)

    phase_rows: dict[str, list[dict[str, Any]]] = {}
    for row in rows:
        phase_rows.setdefault(str(row.get("phase") or "default"), []).append(row)

    bucket_summary: list[dict[str, Any]] = []
    for key in ("<=1.5x", "1.5x-3x", "3x-10x", ">10x", "unknown"):
        items = bucketed.get(key, [])
        if not items:
            continue
        pvl = [float(v) for v in (_to_number(item.get("player_vs_limit_pct"), None) for item in items) if v is not None]
        pvs = [float(v) for v in (_to_number(item.get("player_vs_server_pct"), None) for item in items) if v is not None]
        vvl = [float(v) for v in (_to_number(item.get("variant_vs_limit_pct"), None) for item in items) if v is not None]
        bucket_summary.append(
            {
                "headroom_bucket": key,
                "steps": len(items),
                "player_vs_limit_mape_pct": _mean(pvl),
                "player_vs_server_mape_pct": _mean(pvs),
                "variant_vs_limit_mape_pct": _mean(vvl),
            }
        )

    phase_summary: list[dict[str, Any]] = []
    for phase, items in sorted(phase_rows.items(), key=lambda item: item[0]):
        pvl = [float(v) for v in (_to_number(item.get("player_vs_limit_pct"), None) for item in items) if v is not None]
        pvs = [float(v) for v in (_to_number(item.get("player_vs_server_pct"), None) for item in items) if v is not None]
        vvl = [float(v) for v in (_to_number(item.get("variant_vs_limit_pct"), None) for item in items) if v is not None]
        phase_summary.append(
            {
                "phase": phase,
                "steps": len(items),
                "player_vs_limit_mape_pct": _mean(pvl),
                "player_vs_server_mape_pct": _mean(pvs),
                "variant_vs_limit_mape_pct": _mean(vvl),
            }
        )

    return {
        "rows": rows,
        "headroom_buckets": bucket_summary,
        "phase_summary": phase_summary,
    }


def _build_startup_caps_summary(run: dict[str, Any]) -> dict[str, Any] | None:
    scenarios: list[dict[str, Any]] = []
    for index, step in enumerate(run.get("steps", [])):
        if str(step.get("mode")) != "startup-caps" or str(step.get("step_kind")) != "startup-cap":
            continue
        step_samples = [sample for sample in run.get("samples", []) if int(_to_number(sample.get("step_index"), -1) or -1) == index]
        startup_latency = None
        first_variant = None
        up_switches = [
            event
            for event in run.get("switch_events", [])
            if int(_to_number(event.get("step_index"), -1) or -1) == index
            and _to_number(event.get("to_variant_mbps"), None) is not None
            and _to_number(event.get("from_variant_mbps"), None) is not None
            and float(event["to_variant_mbps"]) > float(event["from_variant_mbps"])
        ]
        if up_switches:
            startup_latency = _to_number(up_switches[0].get("time_from_limit_change_s"), None)
            first_variant = _to_number(up_switches[0].get("to_variant_mbps"), None)
        for sample in step_samples:
            variant = _to_number(sample.get("timing_variant_mbps"), None)
            if variant is not None:
                if first_variant is None:
                    first_variant = variant
                break
        buffer_values = [_to_number(s.get("buffer_depth_s"), None) for s in step_samples]
        buffer_values = [v for v in buffer_values if v is not None]
        buffer_full_time_s, buffer_full_depth_s = _time_to_buffer_full_seconds(step_samples)
        video_start_values = [_to_number(s.get("video_start_time_s"), None) for s in step_samples]
        video_start_values = [v for v in video_start_values if v is not None]
        video_start_time_s = video_start_values[0] if video_start_values else None
        cold_start_event = next(
            (
                event
                for event in run.get("cold_start_events", [])
                if int(_to_number(event.get("step_index"), -1) or -1) == index
            ),
            None,
        )
        if video_start_time_s is None and isinstance(cold_start_event, dict):
            video_start_time_s = _to_number(cold_start_event.get("video_start_time_s"), None)

        initial_variant = next((
            _to_number(s.get("timing_variant_mbps"), None)
            for s in step_samples
            if _to_number(s.get("timing_variant_mbps"), None) is not None
        ), None)
        step_switches = [
            event
            for event in run.get("switch_events", [])
            if int(_to_number(event.get("step_index"), -1) or -1) == index
            and _to_number(event.get("to_variant_mbps"), None) is not None
            and _to_number(event.get("from_variant_mbps"), None) is not None
            and float(event["to_variant_mbps"]) > float(event["from_variant_mbps"])
        ]
        variant_path: list[str] = []
        if initial_variant is not None:
            variant_path.append(f"{float(initial_variant):.3f}")
        for event in step_switches:
            to_variant = _to_number(event.get("to_variant_mbps"), None)
            if to_variant is None:
                continue
            token = f"{float(to_variant):.3f}"
            if not variant_path or variant_path[-1] != token:
                variant_path.append(token)

        target_variant = _to_number(step.get("target_variant_mbps"), None)
        target_reached = False
        reached_target_latency_s = None
        if target_variant is not None:
            tolerance = max(0.05, float(target_variant) * 0.02)
            if initial_variant is not None and float(initial_variant) >= float(target_variant) - tolerance:
                target_reached = True
                reached_target_latency_s = 0.0
            else:
                for event in step_switches:
                    to_variant = _to_number(event.get("to_variant_mbps"), None)
                    if to_variant is None:
                        continue
                    if float(to_variant) >= float(target_variant) - tolerance:
                        target_reached = True
                        reached_target_latency_s = _to_number(event.get("time_from_limit_change_s"), None)
                        break

        stall_count_delta, stall_time_delta = _compute_stall_deltas(step_samples)
        scenarios.append(
            {
                "scenario": int(_to_number(step.get("startup_scenario_index"), len(scenarios) + 1) or (len(scenarios) + 1)),
                "cap_label": step.get("startup_cap_label"),
                "cap_target_mbps": _to_number(step.get("target_mbps"), None),
                "target_variant_mbps": target_variant,
                "next_variant_mbps": _to_number(step.get("next_variant_mbps"), None),
                "cap_midpoint_media_mbps": _to_number(step.get("cap_midpoint_media_mbps"), None),
                "startup_latency_s": startup_latency,
                "video_start_time_s": video_start_time_s,
                "cold_start_confirmed": bool(cold_start_event.get("confirmed")) if isinstance(cold_start_event, dict) else None,
                "buffer_full_time_s": buffer_full_time_s,
                "buffer_full_depth_s": buffer_full_depth_s,
                "first_rendition_mbps": first_variant,
                "variant_path": " -> ".join(variant_path) if variant_path else "",
                "target_reached": target_reached,
                "reached_target_latency_s": reached_target_latency_s,
                "minimum_buffer_s": min(buffer_values) if buffer_values else None,
                "stall_count_delta": stall_count_delta,
                "stall_time_delta_s": stall_time_delta,
            }
        )

    if not scenarios:
        return None
    latencies = [_to_number(item.get("startup_latency_s"), None) for item in scenarios]
    latencies = [v for v in latencies if v is not None]
    return {
        "scenarios": scenarios,
        "aggregate": {
            "startup_latency_median_s": _median([float(v) for v in latencies]) if latencies else None,
            "stall_count_delta_total": sum(float(_to_number(item.get("stall_count_delta"), 0) or 0) for item in scenarios),
            "stall_time_delta_s_total": round(sum(float(_to_number(item.get("stall_time_delta_s"), 0) or 0) for item in scenarios), 3),
        },
    }


def _build_downshift_severity_summary(run: dict[str, Any]) -> dict[str, Any] | None:
    buckets = [
        {"key": "small", "min": 20, "max": 40},
        {"key": "medium", "min": 40, "max": 70},
        {"key": "severe", "min": 70, "max": 100},
    ]
    rows: list[dict[str, Any]] = []
    for bucket in buckets:
        latencies: list[float] = []
        sample_count = 0
        for index, step in enumerate(run.get("steps", [])):
            if str(step.get("mode")) != "downshift-severity" or str(step.get("step_kind")) != "severity-drop":
                continue
            drop_pct = _to_number(step.get("severity_drop_pct"), None)
            if drop_pct is None or drop_pct < bucket["min"] or drop_pct >= bucket["max"]:
                continue
            sample_count += 1
            down_switches = [
                event
                for event in run.get("switch_events", [])
                if int(_to_number(event.get("step_index"), -1) or -1) == index
                and _to_number(event.get("to_variant_mbps"), None) is not None
                and _to_number(event.get("from_variant_mbps"), None) is not None
                and float(event["to_variant_mbps"]) < float(event["from_variant_mbps"])
            ]
            if down_switches:
                first_latency = _to_number(down_switches[0].get("time_from_limit_change_s"), None)
                if first_latency is not None:
                    latencies.append(float(first_latency))

        rows.append(
            {
                "severity": bucket["key"],
                "drop_pct_range": f"{bucket['min']}-{bucket['max']}",
                "sample_count": sample_count,
                "min_latency_s": min(latencies) if latencies else None,
                "median_latency_s": _median(latencies) if latencies else None,
                "p95_latency_s": _percentile(latencies, 95) if latencies else None,
                "max_latency_s": max(latencies) if latencies else None,
            }
        )

    if not rows:
        return None
    return {"buckets": rows}


def _build_hysteresis_gap_summary(run: dict[str, Any]) -> dict[str, Any] | None:
    pair_keys: set[tuple[int, int]] = set()
    for step in run.get("steps", []):
        if str(step.get("mode")) != "hysteresis-gap":
            continue
        low_idx = int(_to_number(step.get("rung_low_index"), -1) or -1)
        high_idx = int(_to_number(step.get("rung_high_index"), -1) or -1)
        if low_idx >= 0 and high_idx >= 0:
            pair_keys.add((low_idx, high_idx))

    if not pair_keys:
        return None

    pairs: list[dict[str, Any]] = []
    for low_idx, high_idx in sorted(pair_keys):
        down_steps = [
            (index, step)
            for index, step in enumerate(run.get("steps", []))
            if str(step.get("mode")) == "hysteresis-gap"
            and str(step.get("step_kind")) == "hysteresis-down-probe"
            and int(_to_number(step.get("rung_low_index"), -1) or -1) == low_idx
            and int(_to_number(step.get("rung_high_index"), -1) or -1) == high_idx
        ]
        up_steps = [
            (index, step)
            for index, step in enumerate(run.get("steps", []))
            if str(step.get("mode")) == "hysteresis-gap"
            and str(step.get("step_kind")) == "hysteresis-up-probe"
            and int(_to_number(step.get("rung_low_index"), -1) or -1) == low_idx
            and int(_to_number(step.get("rung_high_index"), -1) or -1) == high_idx
        ]

        down_latencies: list[float] = []
        up_latencies: list[float] = []
        for step_index, _ in down_steps:
            switches = [
                event
                for event in run.get("switch_events", [])
                if int(_to_number(event.get("step_index"), -1) or -1) == step_index
                and _to_number(event.get("to_variant_mbps"), None) is not None
                and _to_number(event.get("from_variant_mbps"), None) is not None
                and float(event["to_variant_mbps"]) < float(event["from_variant_mbps"])
            ]
            if switches:
                latency = _to_number(switches[0].get("time_from_limit_change_s"), None)
                if latency is not None:
                    down_latencies.append(float(latency))
        for step_index, _ in up_steps:
            switches = [
                event
                for event in run.get("switch_events", [])
                if int(_to_number(event.get("step_index"), -1) or -1) == step_index
                and _to_number(event.get("to_variant_mbps"), None) is not None
                and _to_number(event.get("from_variant_mbps"), None) is not None
                and float(event["to_variant_mbps"]) > float(event["from_variant_mbps"])
            ]
            if switches:
                latency = _to_number(switches[0].get("time_from_limit_change_s"), None)
                if latency is not None:
                    up_latencies.append(float(latency))

        down_med = _median(down_latencies) if down_latencies else None
        up_med = _median(up_latencies) if up_latencies else None
        gap = (up_med - down_med) if (down_med is not None and up_med is not None) else None
        pairs.append(
            {
                "rung_pair": f"V{low_idx + 1}<->V{high_idx + 1}",
                "alpha_down_median": down_med,
                "alpha_up_median": up_med,
                "hysteresis_gap": gap,
                "downshift_events": len(down_latencies),
                "upshift_events": len(up_latencies),
            }
        )

    gaps = [float(_to_number(item.get("hysteresis_gap"), None)) for item in pairs if _to_number(item.get("hysteresis_gap"), None) is not None]
    return {
        "pairs": pairs,
        "aggregate": {
            "gap_median": _median(gaps) if gaps else None,
        },
    }


def _build_emergency_downshift_summary(run: dict[str, Any]) -> dict[str, Any] | None:
    rows: list[dict[str, Any]] = []
    for cycle in sorted({int(_to_number(step.get("cycle_index"), 0) or 0) for step in run.get("steps", []) if str(step.get("mode")) == "emergency-downshift" and int(_to_number(step.get("cycle_index"), 0) or 0) > 0}):
        high_step_idx = next((idx for idx, step in enumerate(run.get("steps", [])) if str(step.get("mode")) == "emergency-downshift" and str(step.get("step_kind")) == "emergency-high" and int(_to_number(step.get("cycle_index"), 0) or 0) == cycle), None)
        low_step_idx = next((idx for idx, step in enumerate(run.get("steps", [])) if str(step.get("mode")) == "emergency-downshift" and str(step.get("step_kind")) == "emergency-low" and int(_to_number(step.get("cycle_index"), 0) or 0) == cycle), None)
        if high_step_idx is None or low_step_idx is None:
            continue

        down_switches = [
            event
            for event in run.get("switch_events", [])
            if int(_to_number(event.get("step_index"), -1) or -1) == low_step_idx
            and _to_number(event.get("to_variant_mbps"), None) is not None
            and _to_number(event.get("from_variant_mbps"), None) is not None
            and float(event["to_variant_mbps"]) < float(event["from_variant_mbps"])
        ]
        up_switches = [
            event
            for event in run.get("switch_events", [])
            if int(_to_number(event.get("step_index"), -1) or -1) == high_step_idx
            and _to_number(event.get("to_variant_mbps"), None) is not None
            and _to_number(event.get("from_variant_mbps"), None) is not None
            and float(event["to_variant_mbps"]) > float(event["from_variant_mbps"])
        ]
        low_samples = [sample for sample in run.get("samples", []) if int(_to_number(sample.get("step_index"), -1) or -1) == low_step_idx]
        stall_count_delta, stall_time_delta = _compute_stall_deltas(low_samples)
        min_buffer = min([_to_number(s.get("buffer_depth_s"), None) for s in low_samples if _to_number(s.get("buffer_depth_s"), None) is not None], default=None)
        rows.append(
            {
                "cycle": cycle,
                "first_downswitch_latency_s": _to_number(down_switches[0].get("time_from_limit_change_s"), None) if down_switches else None,
                "first_upswitch_latency_s": _to_number(up_switches[0].get("time_from_limit_change_s"), None) if up_switches else None,
                "stall_count_delta": stall_count_delta,
                "stall_time_delta_s": stall_time_delta,
                "minimum_buffer_s": min_buffer,
            }
        )

    if not rows:
        return None
    down = [float(_to_number(row.get("first_downswitch_latency_s"), None)) for row in rows if _to_number(row.get("first_downswitch_latency_s"), None) is not None]
    up = [float(_to_number(row.get("first_upswitch_latency_s"), None)) for row in rows if _to_number(row.get("first_upswitch_latency_s"), None) is not None]
    return {
        "cycles": rows,
        "aggregate": {
            "downshift_first_switch_latency_median_s": _median(down) if down else None,
            "upshift_first_switch_latency_median_s": _median(up) if up else None,
        },
    }


def _direction_from_step(step: dict[str, Any]) -> str | None:
    label = str(step.get("direction") or "").strip().lower()
    if "down" in label:
        return "down"
    if "up" in label:
        return "up"
    return None


def _build_accuracy_summary(run: dict[str, Any]) -> dict[str, Any] | None:
    samples = run.get("samples", []) if isinstance(run, dict) else []
    steps = run.get("steps", []) if isinstance(run, dict) else []
    if not isinstance(samples, list) or not isinstance(steps, list) or not samples:
        return None

    def build_pair_metrics(source_key: str, target_key: str) -> dict[str, Any]:
        abs_pct_errors: list[float] = []
        abs_errors: list[float] = []
        signed_errors: list[float] = []
        for sample in samples:
            source = _to_number(sample.get(source_key), None)
            target = _to_number(sample.get(target_key), None)
            if source is None or target is None:
                continue
            if target <= 0:
                continue
            err = float(source) - float(target)
            signed_errors.append(err)
            abs_errors.append(abs(err))
            abs_pct_errors.append(abs(err) / float(target) * 100.0)
        return {
            "sample_count": len(abs_pct_errors),
            "mape_pct": _mean(abs_pct_errors),
            "mae_mbps": _mean(abs_errors),
            "bias_mbps": _mean(signed_errors),
        }

    def build_settle_metrics(tolerance_ratio: float) -> dict[str, Any]:
        up_latencies: list[float] = []
        down_latencies: list[float] = []
        for step_index, step in enumerate(steps):
            direction = _direction_from_step(step)
            if direction is None:
                continue
            target = _to_number(step.get("target_mbps"), None)
            if target is None or target <= 0:
                continue
            step_samples = [
                sample
                for sample in samples
                if int(_to_number(sample.get("step_index"), -1) or -1) == step_index
            ]
            if not step_samples:
                continue

            base_ts = _parse_iso_z(step_samples[0].get("ts"))
            latency: float | None = None
            for sample in step_samples:
                player_estimate = _to_number(sample.get("network_bitrate_mbps"), None)
                if player_estimate is None:
                    continue
                if abs(float(player_estimate) - float(target)) > (float(target) * tolerance_ratio):
                    continue
                sample_ts = _parse_iso_z(sample.get("ts"))
                if base_ts is not None and sample_ts is not None:
                    latency = max(0.0, (sample_ts - base_ts).total_seconds())
                else:
                    latency = 0.0
                break

            if latency is None:
                continue
            if direction == "up":
                up_latencies.append(float(latency))
            elif direction == "down":
                down_latencies.append(float(latency))

        return {
            "up": {
                "observed": len(up_latencies),
                "median_s": _median(up_latencies),
                "p95_s": _percentile(up_latencies, 95) if up_latencies else None,
            },
            "down": {
                "observed": len(down_latencies),
                "median_s": _median(down_latencies),
                "p95_s": _percentile(down_latencies, 95) if down_latencies else None,
            },
        }

    summary = {
        "player_vs_limit": build_pair_metrics("network_bitrate_mbps", "target_mbps"),
        "player_vs_server": build_pair_metrics("network_bitrate_mbps", "throughput_mbps"),
        "player_settle_to_limit": {
            "pct10": build_settle_metrics(0.10),
            "pct20": build_settle_metrics(0.20),
        },
    }
    return summary


def _throughput_mbps(snapshot: dict[str, Any]) -> float | None:
    for key in (
        "mbps_wire_active_network",
        "mbps_wire_throughput",
        "mbps_wire_sustained_6s",
        "mbps_wire_sustained_1s",
        "mbps_wire_active_6s",
        "measured_mbps",
    ):
        value = _to_number(snapshot.get(key), None)
        if value is not None:
            return value
    return None


def _wire_throughput_mbps(snapshot: dict[str, Any]) -> float | None:
    return _to_number(
        snapshot.get("mbps_wire_active_network"),
        _to_number(
            snapshot.get("mbps_wire_throughput"),
            _to_number(
                snapshot.get("mbps_wire_sustained_6s"),
                _to_number(
                    snapshot.get("mbps_wire_sustained_1s"),
                    _to_number(snapshot.get("mbps_wire_active_6s"), _to_number(snapshot.get("measured_mbps"), None)),
                ),
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


def _pick_sparse_variant_urls(variants: list[dict[str, Any]], sparse_count: int) -> list[str]:
    rows: list[dict[str, Any]] = []
    for item in variants:
        if not isinstance(item, dict):
            continue
        url = str(item.get("url") or "").strip()
        bandwidth = _to_number(item.get("bandwidth"), None)
        if not url or bandwidth is None or bandwidth <= 0:
            continue
        rows.append({"url": url, "bandwidth": float(bandwidth)})
    if not rows:
        return []
    rows.sort(key=lambda row: row["bandwidth"])

    count = max(1, int(sparse_count))
    if count >= len(rows):
        return [str(row["url"]) for row in rows]
    if count == 1:
        return [str(rows[0]["url"])]

    chosen: list[str] = []
    seen: set[str] = set()
    for i in range(count):
        pos = i * (len(rows) - 1) / (count - 1)
        idx = int(round(pos))
        url = str(rows[idx]["url"])
        if url in seen:
            continue
        seen.add(url)
        chosen.append(url)
    return chosen


def _apply_content_variant_mode(
    api_base: str,
    session_id: str,
    mode: str,
    sparse_count: int,
    variants: list[dict[str, Any]],
    verbose: bool,
) -> dict[str, Any]:
    selected_mode = str(mode or "off").strip().lower()
    if selected_mode == "off":
        return {"applied": False, "mode": "off", "allowed_variants": []}

    if selected_mode == "all":
        allowed_variants: list[str] = []
    elif selected_mode == "sparse":
        allowed_variants = _pick_sparse_variant_urls(variants, sparse_count)
        if not allowed_variants:
            return {
                "applied": False,
                "mode": "sparse",
                "allowed_variants": [],
                "error": "no_variant_urls_available",
            }
    else:
        return {
            "applied": False,
            "mode": selected_mode,
            "allowed_variants": [],
            "error": "invalid_mode",
        }

    ok_patch = _patch_session_fields(
        api_base,
        session_id,
        {"content_allowed_variants": allowed_variants},
    )
    if not ok_patch:
        return {
            "applied": False,
            "mode": selected_mode,
            "allowed_variants": allowed_variants,
            "error": "patch_failed",
        }

    pre_snapshot = fetch_session_snapshot(api_base, session_id, verbose=False) or {}
    pre_restarts = int(_to_number(pre_snapshot.get("player_restarts"), 0) or 0)
    pre_position_s = _to_number(pre_snapshot.get("player_metrics_position_s"), None)
    ok_restart, restart_request_id, restart_status = _request_remote_restart(
        api_base=api_base,
        session_id=session_id,
        reason=f"content_variant_mode_{selected_mode}",
        timeout_seconds=30,
        verbose=verbose,
    )
    cold_start = _confirm_cold_start_after_restart(
        api_base=api_base,
        session_id=session_id,
        pre_restart_restarts=pre_restarts,
        pre_restart_position_s=pre_position_s,
        timeout_seconds=20,
    )

    return {
        "applied": True,
        "mode": selected_mode,
        "allowed_variants": allowed_variants,
        "restart_requested": True,
        "restart_ok": bool(ok_restart),
        "restart_request_id": restart_request_id,
        "restart_status": restart_status,
        "cold_start_confirmed": bool(cold_start.get("confirmed")),
    }


def _is_truthy(value: Any) -> bool:
    if value is True:
        return True
    if value is False or value is None:
        return False
    if isinstance(value, (int, float)):
        return float(value) != 0
    text = str(value).strip().lower()
    return text in {"1", "true", "yes", "on"}


def _request_remote_restart(
    api_base: str,
    session_id: str,
    reason: str,
    timeout_seconds: int = 30,
    verbose: bool = False,
) -> tuple[bool, str, str]:
    request_id = str(uuid.uuid4())
    requested_at = utc_now_iso()
    payload = {
        "player_restart_requested": True,
        "player_restart_request_id": request_id,
        "player_restart_request_reason": str(reason or "remote_command"),
        "player_restart_request_requested_at": requested_at,
        "player_restart_request_state": "requested",
        "player_restart_request_handled_at": "",
        "player_restart_request_handled_by": "",
        "player_restart_request_error": "",
    }
    if not _patch_session_fields(api_base, session_id, payload):
        return False, request_id, "patch_failed"

    deadline = time.time() + max(5, int(timeout_seconds))
    while time.time() < deadline:
        snap = fetch_session_snapshot(api_base, session_id, verbose=False) or {}
        observed_id = str(snap.get("player_restart_request_id") or "")
        if observed_id != request_id:
            time.sleep(0.5)
            continue

        requested = _is_truthy(snap.get("player_restart_requested"))
        state = str(snap.get("player_restart_request_state") or "").strip().lower()
        if (not requested) and state in {"completed", "succeeded", "done"}:
            return True, request_id, "completed"
        if state in {"failed", "error", "timed_out", "timeout"}:
            error_msg = str(snap.get("player_restart_request_error") or "failed")
            return False, request_id, error_msg
        time.sleep(0.5)

    if verbose:
        print(
            f"{utc_now_iso()} ABRCHAR restart_request_timeout session_id={session_id} request_id={request_id}",
            flush=True,
        )
    return False, request_id, "timeout"


def _confirm_cold_start_after_restart(
    api_base: str,
    session_id: str,
    pre_restart_restarts: int,
    pre_restart_position_s: float | None,
    timeout_seconds: int = 20,
) -> dict[str, Any]:
    deadline = time.time() + max(5, int(timeout_seconds))
    last_snapshot: dict[str, Any] | None = None

    while time.time() < deadline:
        snap = fetch_session_snapshot(api_base, session_id, verbose=False) or {}
        last_snapshot = snap
        restart_count = int(_to_number(snap.get("player_restarts"), 0) or 0)
        position_s = _to_number(snap.get("player_metrics_position_s"), None)
        buffer_depth_s = _to_number(snap.get("player_metrics_buffer_depth_s"), None)
        video_start_time_s = _to_number(snap.get("player_metrics_video_start_time_s"), None)

        restart_observed = restart_count > pre_restart_restarts
        position_reset = (
            pre_restart_position_s is not None
            and position_s is not None
            and float(position_s) <= max(3.0, float(pre_restart_position_s) * 0.25)
        )
        buffer_reset = buffer_depth_s is not None and float(buffer_depth_s) <= 1.0
        startup_time_seen = video_start_time_s is not None and float(video_start_time_s) <= 3.0

        if restart_observed and (position_reset or buffer_reset or startup_time_seen):
            return {
                "confirmed": True,
                "restart_observed": restart_observed,
                "position_reset": position_reset,
                "buffer_reset": buffer_reset,
                "startup_time_seen": startup_time_seen,
                "player_restarts": restart_count,
                "position_s": position_s,
                "buffer_depth_s": buffer_depth_s,
                "video_start_time_s": video_start_time_s,
                "ts": utc_now_iso(),
            }
        time.sleep(0.5)

    fallback = last_snapshot or {}
    return {
        "confirmed": False,
        "restart_observed": int(_to_number(fallback.get("player_restarts"), 0) or 0) > pre_restart_restarts,
        "position_reset": False,
        "buffer_reset": False,
        "startup_time_seen": _to_number(fallback.get("player_metrics_video_start_time_s"), None) is not None,
        "player_restarts": int(_to_number(fallback.get("player_restarts"), 0) or 0),
        "position_s": _to_number(fallback.get("player_metrics_position_s"), None),
        "buffer_depth_s": _to_number(fallback.get("player_metrics_buffer_depth_s"), None),
        "video_start_time_s": _to_number(fallback.get("player_metrics_video_start_time_s"), None),
        "ts": utc_now_iso(),
    }


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

    transient_summary = run.get("transient_shock_summary") if isinstance(run, dict) else None
    if isinstance(transient_summary, dict):
        severity_rows = transient_summary.get("severities") if isinstance(transient_summary.get("severities"), list) else []
        if severity_rows:
            lines.extend(["", "## Transient Shock Summary", ""])
            lines.append("| Severity | Drop % | Downswitch Count | Downswitch Latency Median (s) | Recovery Upshift Latency Median (s) | Stall Count Δ Total | Stall Time Δ Total (s) | Unexpected Recovery Downswitches |")
            lines.append("|:---|---:|---:|---:|---:|---:|---:|---:|")
            for row in severity_rows:
                lines.append(
                    "| "
                    f"{row.get('severity') or '—'} | "
                    f"{_fmt3(row.get('drop_pct'))} | "
                    f"{int(_to_number(row.get('downswitch_count'), 0) or 0)} | "
                    f"{_fmt3(row.get('downswitch_latency_median_s'))} | "
                    f"{_fmt3(row.get('recovery_upshift_latency_median_s'))} | "
                    f"{_fmt3(row.get('stall_count_delta_total'))} | "
                    f"{_fmt3(row.get('stall_time_delta_s_total'))} | "
                    f"{int(_to_number(row.get('unexpected_downswitch_during_recovery'), 0) or 0)} |"
                )

    startup_summary = run.get("startup_caps_summary") if isinstance(run, dict) else None
    if isinstance(startup_summary, dict):
        scenario_rows = startup_summary.get("scenarios") if isinstance(startup_summary.get("scenarios"), list) else []
        if scenario_rows:
            lines.extend(["", "## Startup Caps Summary", ""])
            lines.append("| Scenario | Cap Label | Target Variant (Mbps) | Next Variant (Mbps) | Cap Limit (Mbps) | Midpoint Media (Mbps) | Cold Start Confirmed | Video Start Time (s) | Startup Latency (s) | Reach Target (s) | Buffer Full Time (s) | Buffer Full Depth (s) | First Rendition (Mbps) | Variant Path | Min Buffer (s) | Stall Count Δ | Stall Time Δ (s) |")
            lines.append("|---:|:---|---:|---:|---:|---:|:---:|---:|---:|---:|---:|---:|---:|:---|---:|---:|---:|")
            for row in scenario_rows:
                lines.append(
                    "| "
                    f"{int(_to_number(row.get('scenario'), 0) or 0)} | "
                    f"{row.get('cap_label') or '—'} | "
                    f"{_fmt3(row.get('target_variant_mbps'))} | "
                    f"{_fmt3(row.get('next_variant_mbps'))} | "
                    f"{_fmt3(row.get('cap_target_mbps'))} | "
                    f"{_fmt3(row.get('cap_midpoint_media_mbps'))} | "
                    f"{'yes' if row.get('cold_start_confirmed') else 'no'} | "
                    f"{_fmt3(row.get('video_start_time_s'))} | "
                    f"{_fmt3(row.get('startup_latency_s'))} | "
                    f"{_fmt3(row.get('reached_target_latency_s'))} | "
                    f"{_fmt3(row.get('buffer_full_time_s'))} | "
                    f"{_fmt3(row.get('buffer_full_depth_s'))} | "
                    f"{_fmt3(row.get('first_rendition_mbps'))} | "
                    f"{(row.get('variant_path') or '—').replace('|', '/') } | "
                    f"{_fmt3(row.get('minimum_buffer_s'))} | "
                    f"{_fmt3(row.get('stall_count_delta'))} | "
                    f"{_fmt3(row.get('stall_time_delta_s'))} |"
                )

    downshift_summary = run.get("downshift_severity_summary") if isinstance(run, dict) else None
    if isinstance(downshift_summary, dict):
        bucket_rows = downshift_summary.get("buckets") if isinstance(downshift_summary.get("buckets"), list) else []
        if bucket_rows:
            lines.extend(["", "## Downshift Severity Summary", ""])
            lines.append("| Severity | Drop Range (%) | Samples | Min Latency (s) | Median Latency (s) | P95 Latency (s) | Max Latency (s) |")
            lines.append("|:---|:---|---:|---:|---:|---:|---:|")
            for row in bucket_rows:
                lines.append(
                    "| "
                    f"{row.get('severity') or '—'} | "
                    f"{row.get('drop_pct_range') or '—'} | "
                    f"{int(_to_number(row.get('sample_count'), 0) or 0)} | "
                    f"{_fmt3(row.get('min_latency_s'))} | "
                    f"{_fmt3(row.get('median_latency_s'))} | "
                    f"{_fmt3(row.get('p95_latency_s'))} | "
                    f"{_fmt3(row.get('max_latency_s'))} |"
                )

    hysteresis_summary = run.get("hysteresis_gap_summary") if isinstance(run, dict) else None
    if isinstance(hysteresis_summary, dict):
        pair_rows = hysteresis_summary.get("pairs") if isinstance(hysteresis_summary.get("pairs"), list) else []
        if pair_rows:
            lines.extend(["", "## Hysteresis Gap Summary", ""])
            lines.append("| Rung Pair | Alpha Down Median (s) | Alpha Up Median (s) | Gap (s) | Downshift Events | Upshift Events |")
            lines.append("|:---|---:|---:|---:|---:|---:|")
            for row in pair_rows:
                lines.append(
                    "| "
                    f"{row.get('rung_pair') or '—'} | "
                    f"{_fmt3(row.get('alpha_down_median'))} | "
                    f"{_fmt3(row.get('alpha_up_median'))} | "
                    f"{_fmt3(row.get('hysteresis_gap'))} | "
                    f"{int(_to_number(row.get('downshift_events'), 0) or 0)} | "
                    f"{int(_to_number(row.get('upshift_events'), 0) or 0)} |"
                )

    emergency_summary = run.get("emergency_downshift_summary") if isinstance(run, dict) else None
    if isinstance(emergency_summary, dict):
        cycle_rows = emergency_summary.get("cycles") if isinstance(emergency_summary.get("cycles"), list) else []
        if cycle_rows:
            lines.extend(["", "## Emergency Downshift Summary", ""])
            lines.append("| Cycle | First Downshift Latency (s) | First Upshift Latency (s) | Stall Count Δ | Stall Time Δ (s) | Min Buffer (s) |")
            lines.append("|---:|---:|---:|---:|---:|---:|")
            for row in cycle_rows:
                lines.append(
                    "| "
                    f"{int(_to_number(row.get('cycle'), 0) or 0)} | "
                    f"{_fmt3(row.get('first_downswitch_latency_s'))} | "
                    f"{_fmt3(row.get('first_upswitch_latency_s'))} | "
                    f"{_fmt3(row.get('stall_count_delta'))} | "
                    f"{_fmt3(row.get('stall_time_delta_s'))} | "
                    f"{_fmt3(row.get('minimum_buffer_s'))} |"
                )

    throughput_accuracy = run.get("throughput_accuracy_summary") if isinstance(run, dict) else None
    if isinstance(throughput_accuracy, dict):
        phase_rows = throughput_accuracy.get("phase_summary") if isinstance(throughput_accuracy.get("phase_summary"), list) else []
        if phase_rows:
            lines.extend(["", "## Throughput Accuracy By Phase", ""])
            lines.append("| Phase | Steps | Player vs Limit MAPE % | Player vs Server MAPE % | Variant vs Limit MAPE % |")
            lines.append("|:---|---:|---:|---:|---:|")
            for row in phase_rows:
                lines.append(
                    "| "
                    f"{row.get('phase') or '—'} | "
                    f"{int(_to_number(row.get('steps'), 0) or 0)} | "
                    f"{_fmt3(row.get('player_vs_limit_mape_pct'))} | "
                    f"{_fmt3(row.get('player_vs_server_mape_pct'))} | "
                    f"{_fmt3(row.get('variant_vs_limit_mape_pct'))} |"
                )
        bucket_rows = throughput_accuracy.get("headroom_buckets") if isinstance(throughput_accuracy.get("headroom_buckets"), list) else []
        if bucket_rows:
            lines.extend(["", "## Throughput Accuracy By Headroom", ""])
            lines.append("| Headroom Bucket (limit/variant) | Steps | Player vs Limit MAPE % | Player vs Server MAPE % | Variant vs Limit MAPE % |")
            lines.append("|:---|---:|---:|---:|---:|")
            for row in bucket_rows:
                lines.append(
                    "| "
                    f"{row.get('headroom_bucket') or '—'} | "
                    f"{int(_to_number(row.get('steps'), 0) or 0)} | "
                    f"{_fmt3(row.get('player_vs_limit_mape_pct'))} | "
                    f"{_fmt3(row.get('player_vs_server_mape_pct'))} | "
                    f"{_fmt3(row.get('variant_vs_limit_mape_pct'))} |"
                )

    accuracy_summary = run.get("accuracy_summary") if isinstance(run, dict) else None
    if isinstance(accuracy_summary, dict):
        pvl = accuracy_summary.get("player_vs_limit") if isinstance(accuracy_summary.get("player_vs_limit"), dict) else {}
        pvs = accuracy_summary.get("player_vs_server") if isinstance(accuracy_summary.get("player_vs_server"), dict) else {}
        settle = accuracy_summary.get("player_settle_to_limit") if isinstance(accuracy_summary.get("player_settle_to_limit"), dict) else {}
        lines.extend(["", "## Throughput Accuracy Summary", ""])
        lines.append("| Comparison | Samples | MAPE % | MAE (Mbps) | Bias (Mbps) |")
        lines.append("|:---|---:|---:|---:|---:|")
        lines.append(
            "| Player vs Limit | "
            f"{int(_to_number(pvl.get('sample_count'), 0) or 0)} | "
            f"{_fmt3(pvl.get('mape_pct'))} | "
            f"{_fmt3(pvl.get('mae_mbps'))} | "
            f"{_fmt3(pvl.get('bias_mbps'))} |"
        )
        lines.append(
            "| Player vs Server Throughput | "
            f"{int(_to_number(pvs.get('sample_count'), 0) or 0)} | "
            f"{_fmt3(pvs.get('mape_pct'))} | "
            f"{_fmt3(pvs.get('mae_mbps'))} | "
            f"{_fmt3(pvs.get('bias_mbps'))} |"
        )

        lines.extend(["", "## Player Settle Latency To Limit", ""])
        lines.append("| Band | Direction | Observed Steps | Median (s) | P95 (s) |")
        lines.append("|:---|:---|---:|---:|---:|")
        for band_key, band_label in (("pct10", "±10%"), ("pct20", "±20%")):
            band = settle.get(band_key) if isinstance(settle.get(band_key), dict) else {}
            for direction in ("down", "up"):
                row = band.get(direction) if isinstance(band.get(direction), dict) else {}
                lines.append(
                    "| "
                    f"{band_label} | "
                    f"{direction} | "
                    f"{int(_to_number(row.get('observed'), 0) or 0)} | "
                    f"{_fmt3(row.get('median_s'))} | "
                    f"{_fmt3(row.get('p95_s'))} |"
                )

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

    transient_config: dict[str, Any] | None = None
    startup_caps_config: dict[str, Any] | None = None
    downshift_severity_config: dict[str, Any] | None = None
    hysteresis_gap_config: dict[str, Any] | None = None
    emergency_downshift_config: dict[str, Any] | None = None
    throughput_accuracy_config: dict[str, Any] | None = None
    throughput_calcs_config: dict[str, Any] | None = None
    if test_mode == "steps":
        schedule = _build_huge_steps_schedule(
            ladder_mbps=ladder_mbps,
            cycles=repeat_count,
        )
        expanded_steps = schedule
    elif test_mode == "transient-shock":
        schedule, transient_config = _build_transient_shock_schedule(
            ladder_mbps=ladder_mbps,
            overhead_pct=net_overhead_pct,
            min_mbps=_to_number(getattr(config, "min_mbps", None), None),
            max_mbps=_to_number(getattr(config, "max_mbps", None), None),
        )
        expanded_steps = []
        for cycle_index in range(repeat_count):
            for template_index, template_step in enumerate(schedule):
                step_copy = dict(template_step)
                step_copy["cycle_index"] = cycle_index + 1
                step_copy["template_step_index"] = template_index
                base_order = int(_to_number(step_copy.get("shock_order"), 0) or 0)
                if base_order > 0:
                    step_copy["shock_order"] = base_order + (cycle_index * 3)
                expanded_steps.append(step_copy)
    elif test_mode == "startup-caps":
        schedule, startup_caps_config = _build_startup_caps_schedule(
            ladder_mbps=ladder_mbps,
            overhead_pct=net_overhead_pct,
            min_mbps=_to_number(getattr(config, "min_mbps", None), None),
            max_mbps=_to_number(getattr(config, "max_mbps", None), None),
            repeat_count=repeat_count,
        )
        expanded_steps = schedule
    elif test_mode == "downshift-severity":
        schedule, downshift_severity_config = _build_downshift_severity_schedule(
            ladder_mbps=ladder_mbps,
            overhead_pct=net_overhead_pct,
            min_mbps=_to_number(getattr(config, "min_mbps", None), None),
            max_mbps=_to_number(getattr(config, "max_mbps", None), None),
            repeat_count=repeat_count,
        )
        expanded_steps = schedule
    elif test_mode == "hysteresis-gap":
        schedule, hysteresis_gap_config = _build_hysteresis_gap_schedule(
            ladder_mbps=ladder_mbps,
            overhead_pct=net_overhead_pct,
            min_mbps=_to_number(getattr(config, "min_mbps", None), None),
            max_mbps=_to_number(getattr(config, "max_mbps", None), None),
            repeat_count=repeat_count,
        )
        expanded_steps = schedule
    elif test_mode == "emergency-downshift":
        schedule, emergency_downshift_config = _build_emergency_downshift_schedule(
            ladder_mbps=ladder_mbps,
            overhead_pct=net_overhead_pct,
            min_mbps=_to_number(getattr(config, "min_mbps", None), None),
            max_mbps=_to_number(getattr(config, "max_mbps", None), None),
            repeat_count=repeat_count,
        )
        expanded_steps = schedule
    elif test_mode == "throughput-accuracy":
        schedule, throughput_accuracy_config = _build_throughput_accuracy_schedule(
            ladder_mbps=ladder_mbps,
            overhead_pct=net_overhead_pct,
            min_mbps=_to_number(getattr(config, "min_mbps", None), None),
            max_mbps=_to_number(getattr(config, "max_mbps", None), None),
            repeat_count=repeat_count,
            max_limit_mbps=_to_number(getattr(config, "abrchar_accuracy_max_limit_mbps", 100.0), 100.0) or 100.0,
            sparse_variants=int(getattr(config, "abrchar_accuracy_sparse_variants", 2) or 2),
        )
        expanded_steps = schedule
    elif test_mode == "throughput-calcs":
        schedule, throughput_calcs_config = _build_throughput_calcs_schedule(
            ladder_mbps=ladder_mbps,
            overhead_pct=net_overhead_pct,
            min_mbps=_to_number(getattr(config, "min_mbps", None), None),
            max_mbps=_to_number(getattr(config, "max_mbps", None), None),
            repeat_count=repeat_count,
            max_limit_mbps=_to_number(getattr(config, "abrchar_accuracy_max_limit_mbps", 100.0), 100.0) or 100.0,
            sparse_variants=int(getattr(config, "abrchar_content_sparse_variants", 2) or 2),
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
            template_label = "transient_shock_template" if test_mode == "transient-shock" else "smooth_limit_template"
            print(f"{utc_now_iso()} ABRCHAR {template_label}", flush=True)
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
        "cold_start_events": [],
        "warnings": [],
        "summary": {},
    }
    if transient_config is not None:
        run["transient_shock_config"] = transient_config
    if startup_caps_config is not None:
        run["startup_caps_config"] = startup_caps_config
    if downshift_severity_config is not None:
        run["downshift_severity_config"] = downshift_severity_config
    if hysteresis_gap_config is not None:
        run["hysteresis_gap_config"] = hysteresis_gap_config
    if emergency_downshift_config is not None:
        run["emergency_downshift_config"] = emergency_downshift_config
    if throughput_accuracy_config is not None:
        run["throughput_accuracy_config"] = throughput_accuracy_config
    if throughput_calcs_config is not None:
        run["throughput_calcs_config"] = throughput_calcs_config
    run["content_variant_mode_events"] = []

    huge_mode = test_mode == "steps"
    track_loop_completion = test_mode in ("smooth", "steps")
    last_observed_player_restarts = int(_to_number(initial.get("player_restarts"), 0) or 0)
    last_variant = _timing_variant_mbps(initial)
    top_variant_mbps = float(ladder_mbps[-1])
    top_variant_tolerance = max(0.05, top_variant_mbps * 0.02)
    loop_completion_target_s = 30.0
    loop_state_by_cycle: dict[int, dict[str, Any]] = {}
    last_observed_stall_count = _to_number(initial.get("player_metrics_stall_count"), 0.0) or 0.0
    last_observed_stall_time_s = _to_number(initial.get("player_metrics_stall_time_s"), 0.0) or 0.0

    def _emit_plot_log(kind: str, payload: dict[str, Any]) -> None:
        if not bool(getattr(config, "abrchar_plot_logs", False)):
            return
        record = {
            "kind": kind,
            "run_number": run_number,
            "run_name": run_name,
            "test_mode": test_mode,
            "session_id": session_id,
            "payload": payload,
        }
        print(
            f"{utc_now_iso()} ABRCHAR_PLOT {json.dumps(record, separators=(',', ':'), sort_keys=True)}",
            flush=True,
        )

    def _emit_sample_plot_log(sample: dict[str, Any]) -> None:
        _emit_plot_log(
            "sample",
            {
                "ts": sample.get("ts"),
                "step_index": sample.get("step_index"),
                "cycle_index": sample.get("cycle_index"),
                "step_kind": sample.get("step_kind"),
                "direction": sample.get("direction"),
                "target_mbps": sample.get("target_mbps"),
                "throughput_mbps": sample.get("throughput_mbps"),
                "player_variant_mbps": sample.get("variant_mbps"),
                "server_variant_mbps": sample.get("server_variant_mbps"),
                "timing_variant_mbps": sample.get("timing_variant_mbps"),
                "network_bitrate_mbps": sample.get("network_bitrate_mbps"),
                "mbps_wire_active_1s": sample.get("mbps_wire_active_1s"),
                "mbps_wire_active_short_ewma": sample.get("mbps_wire_active_short_ewma"),
                "mbps_wire_active_long_ewma": sample.get("mbps_wire_active_long_ewma"),
                "mbps_wire_active_network": sample.get("mbps_wire_active_network"),
                "buffer_depth_s": sample.get("buffer_depth_s"),
                "frames_displayed": sample.get("frames_displayed"),
                "stall_count": sample.get("stall_count"),
                "stall_time_s": sample.get("stall_time_s"),
                "player_restarts": sample.get("player_restarts"),
            },
        )

    def _observe_loop_completion(sample: dict[str, Any], step: dict[str, Any]) -> None:
        if not track_loop_completion:
            return
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
            "mbps_wire_active_1s": _to_number(snap.get("mbps_wire_active_1s"), None),
            "mbps_wire_active_short_ewma": _to_number(snap.get("mbps_wire_active_short_ewma"), None),
            "mbps_wire_active_long_ewma": _to_number(snap.get("mbps_wire_active_long_ewma"), None),
            "mbps_wire_active_network": _to_number(snap.get("mbps_wire_active_network"), None),
            "video_start_time_s": _to_number(snap.get("player_metrics_video_start_time_s"), None),
            "video_first_frame_time_s": _to_number(snap.get("player_metrics_video_first_frame_time_s"), None),
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
        _emit_plot_log("event_restart", event)
        print(
            f"{utc_now_iso()} ABRCHAR recovery player_restart_observed "
            f"step_index={step_index + 1} delta={delta} total={current}",
            flush=True,
        )
        last_observed_player_restarts = current

    def _observe_stall_events(sample: dict[str, Any], step_index: int) -> None:
        nonlocal last_observed_stall_count, last_observed_stall_time_s
        current_count = _to_number(sample.get("stall_count"), None)
        current_time = _to_number(sample.get("stall_time_s"), None)
        if current_count is not None and float(current_count) > float(last_observed_stall_count):
            event = {
                "ts": sample.get("ts"),
                "type": "stall_count_increase",
                "step_index": step_index,
                "cycle_index": sample.get("cycle_index"),
                "stall_count": float(current_count),
                "stall_count_delta": round(float(current_count) - float(last_observed_stall_count), 3),
                "stall_time_s": current_time,
            }
            _emit_plot_log("event_stall", event)
        if current_time is not None and float(current_time) > float(last_observed_stall_time_s) + 0.01:
            event = {
                "ts": sample.get("ts"),
                "type": "stall_time_increase",
                "step_index": step_index,
                "cycle_index": sample.get("cycle_index"),
                "stall_time_s": float(current_time),
                "stall_time_delta_s": round(float(current_time) - float(last_observed_stall_time_s), 3),
                "stall_count": current_count,
            }
            _emit_plot_log("event_stall", event)
        if current_count is not None:
            last_observed_stall_count = float(current_count)
        if current_time is not None:
            last_observed_stall_time_s = float(current_time)

    try:
        active_phase: str | None = None
        for step_index, step in enumerate(run["steps"]):
            step_phase = str(step.get("phase") or "")
            if test_mode == "throughput-calcs" and step_phase and step_phase != active_phase:
                latest_snapshot = fetch_session_snapshot(api_base, session_id, verbose=False) or {}
                latest_variants = latest_snapshot.get("manifest_variants") if isinstance(latest_snapshot.get("manifest_variants"), list) else []
                phase_mode = "all" if step_phase == "all_variants" else "sparse"
                event = _apply_content_variant_mode(
                    api_base=api_base,
                    session_id=session_id,
                    mode=phase_mode,
                    sparse_count=int(getattr(config, "abrchar_content_sparse_variants", 2) or 2),
                    variants=latest_variants,
                    verbose=bool(config.verbose),
                )
                event["phase"] = step_phase
                event["step_index"] = step_index
                run["content_variant_mode_events"].append(event)
                if config.verbose:
                    print(
                        f"{utc_now_iso()} ABRCHAR content_variant_mode phase={step_phase} mode={phase_mode} "
                        f"applied={event.get('applied')} restart_ok={event.get('restart_ok')} "
                        f"allowed_count={len(event.get('allowed_variants', []))}",
                        flush=True,
                    )
                active_phase = step_phase
            elif test_mode != "throughput-calcs" and active_phase is None:
                selected_mode = str(getattr(config, "abrchar_content_variant_mode", "off") or "off").strip().lower()
                if selected_mode in ("all", "sparse"):
                    latest_snapshot = fetch_session_snapshot(api_base, session_id, verbose=False) or {}
                    latest_variants = latest_snapshot.get("manifest_variants") if isinstance(latest_snapshot.get("manifest_variants"), list) else []
                    event = _apply_content_variant_mode(
                        api_base=api_base,
                        session_id=session_id,
                        mode=selected_mode,
                        sparse_count=int(getattr(config, "abrchar_content_sparse_variants", 2) or 2),
                        variants=latest_variants,
                        verbose=bool(config.verbose),
                    )
                    event["phase"] = "pre_run"
                    event["step_index"] = step_index
                    run["content_variant_mode_events"].append(event)
                active_phase = "ready"

            target = float(step["target_mbps"])
            if config.verbose:
                print(
                    f"{utc_now_iso()} ABRCHAR step_begin index={step_index + 1}/{len(run['steps'])} "
                    f"cycle={step.get('cycle_index', 1)}/{repeat_count} "
                    f"direction={step.get('direction')} step_kind={step.get('step_kind', 'smooth')} "
                    f"phase={step.get('phase', '-') } "
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
                    _emit_sample_plot_log(sample)
                    _observe_player_restarts(sample, step_index)
                    _observe_stall_events(sample, step_index)
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
                        _emit_plot_log("event_switch", event)
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
                    _emit_sample_plot_log(sample)
                    _observe_player_restarts(sample, step_index)
                    _observe_stall_events(sample, step_index)
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
                        _emit_plot_log("event_switch", event)
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

            step_apply_mono = time.time()
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

            if bool(step.get("restart_playback_before_step")):
                restart_reason = str(step.get("step_kind") or "startup_caps_precondition")
                pre_restart_snapshot = fetch_session_snapshot(api_base, session_id, verbose=False) or {}
                pre_restart_restarts = int(_to_number(pre_restart_snapshot.get("player_restarts"), 0) or 0)
                pre_restart_position_s = _to_number(pre_restart_snapshot.get("player_metrics_position_s"), None)
                ok_restart, restart_request_id, restart_status = _request_remote_restart(
                    api_base=api_base,
                    session_id=session_id,
                    reason=restart_reason,
                    timeout_seconds=30,
                    verbose=config.verbose,
                )
                if config.verbose:
                    print(
                        f"{utc_now_iso()} ABRCHAR remote_restart request_id={restart_request_id} "
                        f"ok={ok_restart} status={restart_status} step_index={step_index + 1}",
                        flush=True,
                    )
                if not ok_restart:
                    run["warnings"].append(
                        {
                            "type": "remote_restart_failed",
                            "step_index": step_index,
                            "request_id": restart_request_id,
                            "status": restart_status,
                            "reason": restart_reason,
                        }
                    )
                cold_start = _confirm_cold_start_after_restart(
                    api_base=api_base,
                    session_id=session_id,
                    pre_restart_restarts=pre_restart_restarts,
                    pre_restart_position_s=pre_restart_position_s,
                    timeout_seconds=20,
                )
                cold_start_event = {
                    "step_index": step_index,
                    "cycle_index": step.get("cycle_index"),
                    "request_id": restart_request_id,
                    "confirmed": bool(cold_start.get("confirmed")),
                    "player_restarts": cold_start.get("player_restarts"),
                    "position_s": cold_start.get("position_s"),
                    "buffer_depth_s": cold_start.get("buffer_depth_s"),
                    "video_start_time_s": cold_start.get("video_start_time_s"),
                    "ts": cold_start.get("ts") or utc_now_iso(),
                }
                run["cold_start_events"].append(cold_start_event)
                if config.verbose:
                    print(
                        f"{utc_now_iso()} ABRCHAR cold_start_check step_index={step_index + 1} "
                        f"confirmed={cold_start_event.get('confirmed')} "
                        f"video_start_time_s={cold_start_event.get('video_start_time_s')} "
                        f"position_s={cold_start_event.get('position_s')} buffer_depth_s={cold_start_event.get('buffer_depth_s')}",
                        flush=True,
                    )
                if not bool(cold_start_event.get("confirmed")):
                    run["warnings"].append(
                        {
                            "type": "cold_start_unconfirmed",
                            "step_index": step_index,
                            "request_id": restart_request_id,
                            "details": cold_start,
                        }
                    )

            step_kind = str(step.get("step_kind") or "").strip().lower()
            adaptive_accuracy_dwell = step_kind in {"throughput-calcs-accuracy", "throughput-accuracy-sweep"}
            accuracy_ratio = 0.10
            accuracy_min_seconds = 15.0
            accuracy_max_seconds = 120.0
            skip_settle = bool(step.get("skip_settle") or step.get("force_hold_without_settle") or adaptive_accuracy_dwell)

            if not skip_settle:
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
            elif config.verbose:
                print(
                    f"{utc_now_iso()} ABRCHAR settle_skipped step_index={step_index + 1} "
                    f"target_mbps={target:.3f} adaptive_dwell={adaptive_accuracy_dwell}",
                    flush=True,
                )

            settle_deadline = time.time() + max(5, int(config.abrchar_settle_timeout))
            settle_needed = max(2, int(round(max(0.05, float(config.abrchar_settle_tolerance)) * 10)))
            settle_hits = 0
            settled = skip_settle
            last_settle_log_at = 0.0
            step_start_stall_count: float | None = None
            step_start_stall_time_s: float | None = None

            while (not skip_settle) and time.time() < settle_deadline:
                snap = fetch_session_snapshot(api_base, session_id, verbose=False) or {}
                throughput = _throughput_mbps(snap)
                variant = _timing_variant_mbps(snap)
                stall_count = _to_number(snap.get("player_metrics_stall_count"), None)
                stall_time = _to_number(snap.get("player_metrics_stall_time_s"), None)
                sample = _sample_from_snapshot(step_index, step, target, snap)
                run["samples"].append(sample)
                _emit_sample_plot_log(sample)
                _observe_player_restarts(sample, step_index)
                _observe_stall_events(sample, step_index)
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
                            "step_kind": step.get("step_kind"),
                            "cycle_index": step.get("cycle_index"),
                            "shock_severity": step.get("shock_severity"),
                            "shock_order": step.get("shock_order"),
                            "from_variant_mbps": last_variant,
                            "to_variant_mbps": variant,
                            "target_mbps": target,
                            "throughput_mbps": throughput,
                            "time_from_limit_change_s": round(max(0.0, time.time() - step_apply_mono), 3),
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
                        f"buffer_depth_s={sample.get('buffer_depth_s')} stall_count={stall_count} "
                        f"mbps_wire_active_1s={sample.get('mbps_wire_active_1s')} "
                        f"mbps_wire_active_short_ewma={sample.get('mbps_wire_active_short_ewma')} "
                        f"mbps_wire_active_long_ewma={sample.get('mbps_wire_active_long_ewma')} "
                        f"mbps_wire_active_network={sample.get('mbps_wire_active_network')}",
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
            adaptive_started_at = time.time()
            adaptive_matched = False
            hold_iteration = 0
            while True:
                if not adaptive_accuracy_dwell and hold_iteration >= hold_seconds:
                    break
                snap = fetch_session_snapshot(api_base, session_id, verbose=False) or {}
                throughput = _throughput_mbps(snap)
                variant = _timing_variant_mbps(snap)
                stall_count = _to_number(snap.get("player_metrics_stall_count"), None)
                stall_time = _to_number(snap.get("player_metrics_stall_time_s"), None)
                sample = _sample_from_snapshot(step_index, step, target, snap)
                run["samples"].append(sample)
                _emit_sample_plot_log(sample)
                _observe_player_restarts(sample, step_index)
                _observe_stall_events(sample, step_index)
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
                            "step_kind": step.get("step_kind"),
                            "cycle_index": step.get("cycle_index"),
                            "shock_severity": step.get("shock_severity"),
                            "shock_order": step.get("shock_order"),
                            "from_variant_mbps": last_variant,
                            "to_variant_mbps": variant,
                            "target_mbps": target,
                            "throughput_mbps": throughput,
                            "time_from_limit_change_s": round(max(0.0, time.time() - step_apply_mono), 3),
                            "stall_count_delta": stall_count_delta,
                            "stall_time_delta_s": stall_time_delta_s,
                        }
                    )
                    _emit_plot_log("event_switch", run["switch_events"][-1])
                if variant is not None:
                    last_variant = variant

                player_estimate_mbps = _to_number(sample.get("network_bitrate_mbps"), None)
                if adaptive_accuracy_dwell and player_estimate_mbps is not None and target > 0:
                    if abs(float(player_estimate_mbps) - float(target)) <= (float(target) * accuracy_ratio):
                        adaptive_matched = True

                if adaptive_accuracy_dwell:
                    elapsed = max(0.0, time.time() - adaptive_started_at)
                    if adaptive_matched and elapsed >= accuracy_min_seconds:
                        break
                    if elapsed >= accuracy_max_seconds:
                        if not adaptive_matched:
                            run["warnings"].append(
                                {
                                    "type": "throughput_accuracy_threshold_timeout",
                                    "step_index": step_index,
                                    "step_kind": step.get("step_kind"),
                                    "target_mbps": target,
                                    "signal": "network_bitrate_mbps",
                                    "threshold_ratio": accuracy_ratio,
                                    "min_seconds": accuracy_min_seconds,
                                    "max_seconds": accuracy_max_seconds,
                                }
                            )
                        break

                now = time.time()
                if config.verbose and (now - last_hold_log_at) >= 1.0:
                    print(
                        f"{utc_now_iso()} ABRCHAR hold_monitor step_index={step_index + 1} "
                        f"target_mbps={target:.3f} throughput_mbps={throughput} variant_mbps={variant} "
                        f"network_bitrate_mbps={sample.get('network_bitrate_mbps')} "
                        f"stall_count={stall_count} stall_time_s={stall_time} "
                        f"mbps_wire_active_1s={sample.get('mbps_wire_active_1s')} "
                        f"mbps_wire_active_short_ewma={sample.get('mbps_wire_active_short_ewma')} "
                        f"mbps_wire_active_long_ewma={sample.get('mbps_wire_active_long_ewma')} "
                        f"mbps_wire_active_network={sample.get('mbps_wire_active_network')}",
                        flush=True,
                    )
                    last_hold_log_at = now
                hold_iteration += 1
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
    if track_loop_completion:
        run["summary"]["loop_completion_count"] = len(run.get("loop_completion_events", []))
    if test_mode == "smooth":
        run["smooth_switch_summary"] = _build_smooth_switch_summary(run)
    if test_mode == "transient-shock":
        run["transient_shock_summary"] = _build_transient_shock_summary(run)
    if test_mode == "startup-caps":
        run["startup_caps_summary"] = _build_startup_caps_summary(run)
    if test_mode == "downshift-severity":
        run["downshift_severity_summary"] = _build_downshift_severity_summary(run)
    if test_mode == "hysteresis-gap":
        run["hysteresis_gap_summary"] = _build_hysteresis_gap_summary(run)
    if test_mode == "emergency-downshift":
        run["emergency_downshift_summary"] = _build_emergency_downshift_summary(run)
    if test_mode == "throughput-accuracy":
        run["throughput_accuracy_summary"] = _build_throughput_accuracy_summary(run)
    if test_mode == "throughput-calcs":
        run["throughput_accuracy_summary"] = _build_throughput_accuracy_summary(run)

    run["accuracy_summary"] = _build_accuracy_summary(run)

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

    if track_loop_completion:
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

    transient_summary = run.get("transient_shock_summary") if isinstance(run, dict) else None
    if isinstance(transient_summary, dict):
        severity_rows = transient_summary.get("severities") if isinstance(transient_summary.get("severities"), list) else []
        if severity_rows:
            print(f"{utc_now_iso()} ABRCHAR transient_shock_summary", flush=True)
            headers = [
                "Severity",
                "Drop %",
                "Downswitches",
                "Down Lat Med (s)",
                "Recovery Up Med (s)",
                "Stall Δ Count",
                "Stall Δ Time (s)",
                "Unexpected Recovery Down",
            ]
            rows = [
                [
                    str(row.get("severity") or "—"),
                    _fmt3(row.get("drop_pct")),
                    str(int(_to_number(row.get("downswitch_count"), 0) or 0)),
                    _fmt3(row.get("downswitch_latency_median_s")),
                    _fmt3(row.get("recovery_upshift_latency_median_s")),
                    _fmt3(row.get("stall_count_delta_total")),
                    _fmt3(row.get("stall_time_delta_s_total")),
                    str(int(_to_number(row.get("unexpected_downswitch_during_recovery"), 0) or 0)),
                ]
                for row in severity_rows
            ]
            for table_line in _render_plain_table(headers, rows):
                print(table_line, flush=True)

    startup_summary = run.get("startup_caps_summary") if isinstance(run, dict) else None
    if isinstance(startup_summary, dict):
        scenario_rows = startup_summary.get("scenarios") if isinstance(startup_summary.get("scenarios"), list) else []
        if scenario_rows:
            print(f"{utc_now_iso()} ABRCHAR startup_caps_summary", flush=True)
            headers = [
                "Scenario",
                "Cap",
                "Target Var",
                "Next Var",
                "Cap Limit",
                "Midpoint",
                "ColdStart",
                "VideoStart",
                "Startup(s)",
                "ReachTarget(s)",
                "BufferFull(s)",
                "BufferDepth(s)",
                "First Rend",
                "Variant Path",
                "Min Buffer",
                "Stall Δ Cnt",
                "Stall Δ Time",
            ]
            rows = [
                [
                    str(int(_to_number(row.get("scenario"), 0) or 0)),
                    str(row.get("cap_label") or "—"),
                    _fmt3(row.get("target_variant_mbps")),
                    _fmt3(row.get("next_variant_mbps")),
                    _fmt3(row.get("cap_target_mbps")),
                    _fmt3(row.get("cap_midpoint_media_mbps")),
                    "yes" if row.get("cold_start_confirmed") else "no",
                    _fmt3(row.get("video_start_time_s")),
                    _fmt3(row.get("startup_latency_s")),
                    _fmt3(row.get("reached_target_latency_s")),
                    _fmt3(row.get("buffer_full_time_s")),
                    _fmt3(row.get("buffer_full_depth_s")),
                    _fmt3(row.get("first_rendition_mbps")),
                    str(row.get("variant_path") or "—"),
                    _fmt3(row.get("minimum_buffer_s")),
                    _fmt3(row.get("stall_count_delta")),
                    _fmt3(row.get("stall_time_delta_s")),
                ]
                for row in scenario_rows
            ]
            for table_line in _render_plain_table(headers, rows):
                print(table_line, flush=True)

    downshift_summary = run.get("downshift_severity_summary") if isinstance(run, dict) else None
    if isinstance(downshift_summary, dict):
        bucket_rows = downshift_summary.get("buckets") if isinstance(downshift_summary.get("buckets"), list) else []
        if bucket_rows:
            print(f"{utc_now_iso()} ABRCHAR downshift_severity_summary", flush=True)
            headers = ["Severity", "Drop Range", "Samples", "Min", "Median", "P95", "Max"]
            rows = [
                [
                    str(row.get("severity") or "—"),
                    str(row.get("drop_pct_range") or "—"),
                    str(int(_to_number(row.get("sample_count"), 0) or 0)),
                    _fmt3(row.get("min_latency_s")),
                    _fmt3(row.get("median_latency_s")),
                    _fmt3(row.get("p95_latency_s")),
                    _fmt3(row.get("max_latency_s")),
                ]
                for row in bucket_rows
            ]
            for table_line in _render_plain_table(headers, rows):
                print(table_line, flush=True)

    hysteresis_summary = run.get("hysteresis_gap_summary") if isinstance(run, dict) else None
    if isinstance(hysteresis_summary, dict):
        pair_rows = hysteresis_summary.get("pairs") if isinstance(hysteresis_summary.get("pairs"), list) else []
        if pair_rows:
            print(f"{utc_now_iso()} ABRCHAR hysteresis_gap_summary", flush=True)
            headers = ["Rung Pair", "Alpha Down", "Alpha Up", "Gap", "Down Ev", "Up Ev"]
            rows = [
                [
                    str(row.get("rung_pair") or "—"),
                    _fmt3(row.get("alpha_down_median")),
                    _fmt3(row.get("alpha_up_median")),
                    _fmt3(row.get("hysteresis_gap")),
                    str(int(_to_number(row.get("downshift_events"), 0) or 0)),
                    str(int(_to_number(row.get("upshift_events"), 0) or 0)),
                ]
                for row in pair_rows
            ]
            for table_line in _render_plain_table(headers, rows):
                print(table_line, flush=True)

    emergency_summary = run.get("emergency_downshift_summary") if isinstance(run, dict) else None
    if isinstance(emergency_summary, dict):
        cycle_rows = emergency_summary.get("cycles") if isinstance(emergency_summary.get("cycles"), list) else []
        if cycle_rows:
            print(f"{utc_now_iso()} ABRCHAR emergency_downshift_summary", flush=True)
            headers = ["Cycle", "Downshift(s)", "Upshift(s)", "Stall Δ Cnt", "Stall Δ Time", "Min Buffer"]
            rows = [
                [
                    str(int(_to_number(row.get("cycle"), 0) or 0)),
                    _fmt3(row.get("first_downswitch_latency_s")),
                    _fmt3(row.get("first_upswitch_latency_s")),
                    _fmt3(row.get("stall_count_delta")),
                    _fmt3(row.get("stall_time_delta_s")),
                    _fmt3(row.get("minimum_buffer_s")),
                ]
                for row in cycle_rows
            ]
            for table_line in _render_plain_table(headers, rows):
                print(table_line, flush=True)

    throughput_accuracy = run.get("throughput_accuracy_summary") if isinstance(run, dict) else None
    if isinstance(throughput_accuracy, dict):
        phase_rows = throughput_accuracy.get("phase_summary") if isinstance(throughput_accuracy.get("phase_summary"), list) else []
        if phase_rows:
            print(f"{utc_now_iso()} ABRCHAR throughput_phase_accuracy_summary", flush=True)
            headers = ["Phase", "Steps", "Player vs Limit MAPE %", "Player vs Server MAPE %", "Variant vs Limit MAPE %"]
            rows = [
                [
                    str(row.get("phase") or "—"),
                    str(int(_to_number(row.get("steps"), 0) or 0)),
                    _fmt3(row.get("player_vs_limit_mape_pct")),
                    _fmt3(row.get("player_vs_server_mape_pct")),
                    _fmt3(row.get("variant_vs_limit_mape_pct")),
                ]
                for row in phase_rows
            ]
            for table_line in _render_plain_table(headers, rows):
                print(table_line, flush=True)
        bucket_rows = throughput_accuracy.get("headroom_buckets") if isinstance(throughput_accuracy.get("headroom_buckets"), list) else []
        if bucket_rows:
            print(f"{utc_now_iso()} ABRCHAR throughput_headroom_accuracy_summary", flush=True)
            headers = ["Headroom Bucket", "Steps", "Player vs Limit MAPE %", "Player vs Server MAPE %", "Variant vs Limit MAPE %"]
            rows = [
                [
                    str(row.get("headroom_bucket") or "—"),
                    str(int(_to_number(row.get("steps"), 0) or 0)),
                    _fmt3(row.get("player_vs_limit_mape_pct")),
                    _fmt3(row.get("player_vs_server_mape_pct")),
                    _fmt3(row.get("variant_vs_limit_mape_pct")),
                ]
                for row in bucket_rows
            ]
            for table_line in _render_plain_table(headers, rows):
                print(table_line, flush=True)

    accuracy_summary = run.get("accuracy_summary") if isinstance(run, dict) else None
    if isinstance(accuracy_summary, dict):
        pvl = accuracy_summary.get("player_vs_limit") if isinstance(accuracy_summary.get("player_vs_limit"), dict) else {}
        pvs = accuracy_summary.get("player_vs_server") if isinstance(accuracy_summary.get("player_vs_server"), dict) else {}
        settle = accuracy_summary.get("player_settle_to_limit") if isinstance(accuracy_summary.get("player_settle_to_limit"), dict) else {}

        print(f"{utc_now_iso()} ABRCHAR throughput_accuracy_summary", flush=True)
        headers = ["Comparison", "Samples", "MAPE %", "MAE (Mbps)", "Bias (Mbps)"]
        rows = [
            [
                "Player vs Limit",
                str(int(_to_number(pvl.get("sample_count"), 0) or 0)),
                _fmt3(pvl.get("mape_pct")),
                _fmt3(pvl.get("mae_mbps")),
                _fmt3(pvl.get("bias_mbps")),
            ],
            [
                "Player vs Server Throughput",
                str(int(_to_number(pvs.get("sample_count"), 0) or 0)),
                _fmt3(pvs.get("mape_pct")),
                _fmt3(pvs.get("mae_mbps")),
                _fmt3(pvs.get("bias_mbps")),
            ],
        ]
        for table_line in _render_plain_table(headers, rows):
            print(table_line, flush=True)

        print(f"{utc_now_iso()} ABRCHAR settle_latency_summary", flush=True)
        headers = ["Band", "Direction", "Observed", "Median (s)", "P95 (s)"]
        rows = []
        for band_key, band_label in (("pct10", "±10%"), ("pct20", "±20%")):
            band = settle.get(band_key) if isinstance(settle.get(band_key), dict) else {}
            for direction in ("down", "up"):
                row = band.get(direction) if isinstance(band.get(direction), dict) else {}
                rows.append(
                    [
                        band_label,
                        direction,
                        str(int(_to_number(row.get("observed"), 0) or 0)),
                        _fmt3(row.get("median_s")),
                        _fmt3(row.get("p95_s")),
                    ]
                )
        for table_line in _render_plain_table(headers, rows):
            print(table_line, flush=True)

    if config.verbose:
        print(f"{utc_now_iso()} ABRCHAR JSON report: {json_path}", flush=True)
        print(f"{utc_now_iso()} ABRCHAR Markdown report: {md_path}", flush=True)

    assert run["samples"], "Characterization collected no samples"
    assert run["summary"]["step_count"] > 0
