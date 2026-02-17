"""Output generation for analysis results"""

import json
from typing import Dict
from .analysis import AnalysisResult, Statistics, BoundaryMetrics


def write_json_summary(result: AnalysisResult, output_path: str):
    """Write analysis results as JSON"""
    summary = {
        "experiment_start": result.experiment_start.isoformat() if result.experiment_start else None,
        "experiment_end": result.experiment_end.isoformat() if result.experiment_end else None,
        "total_switches": result.total_switches,
        "variants": [
            {
                "index": i,
                "bandwidth_mbps": v.get_bandwidth_mbps(),
                "average_bandwidth_mbps": v.get_average_bandwidth_mbps() if v.average_bandwidth else None,
                "resolution": v.resolution
            }
            for i, v in enumerate(result.ladder.variants)
        ],
        "boundaries": [
            _boundary_to_dict(key, bm)
            for key, bm in result.boundary_metrics.items()
        ]
    }
    
    with open(output_path, 'w') as f:
        json.dump(summary, f, indent=2)


def _boundary_to_dict(key: str, bm: BoundaryMetrics) -> Dict:
    """Convert boundary metrics to dictionary"""
    return {
        "boundary": key,
        "lower_variant_idx": bm.lower_idx,
        "upper_variant_idx": bm.upper_idx,
        "lower_bandwidth_mbps": bm.lower_bandwidth,
        "upper_bandwidth_mbps": bm.upper_bandwidth,
        "downswitch": {
            "count": bm.downswitch_count,
            "thresholds": _stats_to_dict(Statistics(bm.downswitch_thresholds)),
            "safety_factors": _stats_to_dict(Statistics(bm.downswitch_safety_factors))
        },
        "upswitch": {
            "count": bm.upswitch_count,
            "thresholds": _stats_to_dict(Statistics(bm.upswitch_thresholds)),
            "safety_factors": _stats_to_dict(Statistics(bm.upswitch_safety_factors))
        }
    }


def _stats_to_dict(stats: Statistics) -> Dict:
    """Convert Statistics to dictionary"""
    return {
        "count": stats.count,
        "mean": stats.mean,
        "median": stats.median,
        "stddev": stats.stddev,
        "min": stats.min_val,
        "max": stats.max_val,
        "p25": stats.p25,
        "p75": stats.p75
    }


def write_markdown_report(result: AnalysisResult, output_path: str):
    """Write analysis results as Markdown report"""
    lines = []
    
    lines.append("# ABR Characterization Report\n")
    
    # Experiment summary
    lines.append("## Experiment Summary\n")
    if result.experiment_start and result.experiment_end:
        lines.append(f"- **Start:** {result.experiment_start.strftime('%Y-%m-%d %H:%M:%S')}")
        lines.append(f"- **End:** {result.experiment_end.strftime('%Y-%m-%d %H:%M:%S')}")
        duration = (result.experiment_end - result.experiment_start).total_seconds()
        lines.append(f"- **Duration:** {duration:.0f} seconds")
    lines.append(f"- **Total Switches:** {result.total_switches}\n")
    
    # Bitrate ladder
    lines.append("## Bitrate Ladder\n")
    lines.append("| Index | Bandwidth | Avg Bandwidth | Resolution |")
    lines.append("|-------|-----------|---------------|------------|")
    for i, variant in enumerate(result.ladder.variants):
        avg_bw = f"{variant.get_average_bandwidth_mbps():.2f} Mbps" if variant.average_bandwidth else "—"
        res = variant.resolution or "—"
        lines.append(f"| {i} | {variant.get_bandwidth_mbps():.2f} Mbps | {avg_bw} | {res} |")
    lines.append("")
    
    # Boundary metrics
    lines.append("## Boundary Metrics\n")
    
    for key, bm in result.boundary_metrics.items():
        lines.append(f"### Boundary: Variant {bm.lower_idx} ↔ Variant {bm.upper_idx}\n")
        lines.append(f"**Bitrates:** {bm.lower_bandwidth:.2f} Mbps ↔ {bm.upper_bandwidth:.2f} Mbps\n")
        
        # Downswitch
        if bm.downswitch_count > 0:
            lines.append("#### Downswitch (High → Low)\n")
            lines.append(f"**Count:** {bm.downswitch_count}\n")
            
            stats = Statistics(bm.downswitch_thresholds)
            lines.append("**Throughput Thresholds:**\n")
            lines.append(f"- Mean: {stats.mean:.2f} Mbps")
            lines.append(f"- Median: {stats.median:.2f} Mbps")
            lines.append(f"- StdDev: {stats.stddev:.2f} Mbps")
            lines.append(f"- Range: {stats.min_val:.2f} - {stats.max_val:.2f} Mbps\n")
            
            safety_stats = Statistics(bm.downswitch_safety_factors)
            lines.append("**Safety Factors (α = variant_bw / throughput):**\n")
            lines.append(f"- Mean: {safety_stats.mean:.2f}")
            lines.append(f"- Median: {safety_stats.median:.2f}")
            lines.append(f"- StdDev: {safety_stats.stddev:.2f}")
            lines.append(f"- Range: {safety_stats.min_val:.2f} - {safety_stats.max_val:.2f}\n")
        
        # Upswitch
        if bm.upswitch_count > 0:
            lines.append("#### Upswitch (Low → High)\n")
            lines.append(f"**Count:** {bm.upswitch_count}\n")
            
            stats = Statistics(bm.upswitch_thresholds)
            lines.append("**Throughput Thresholds:**\n")
            lines.append(f"- Mean: {stats.mean:.2f} Mbps")
            lines.append(f"- Median: {stats.median:.2f} Mbps")
            lines.append(f"- StdDev: {stats.stddev:.2f} Mbps")
            lines.append(f"- Range: {stats.min_val:.2f} - {stats.max_val:.2f} Mbps\n")
            
            safety_stats = Statistics(bm.upswitch_safety_factors)
            lines.append("**Safety Factors (α = variant_bw / throughput):**\n")
            lines.append(f"- Mean: {safety_stats.mean:.2f}")
            lines.append(f"- Median: {safety_stats.median:.2f}")
            lines.append(f"- StdDev: {safety_stats.stddev:.2f}")
            lines.append(f"- Range: {safety_stats.min_val:.2f} - {safety_stats.max_val:.2f}\n")
        
        # Hysteresis
        if bm.downswitch_count > 0 and bm.upswitch_count > 0:
            down_stats = Statistics(bm.downswitch_thresholds)
            up_stats = Statistics(bm.upswitch_thresholds)
            lines.append("#### Hysteresis\n")
            lines.append(f"- Downswitch median: {down_stats.median:.2f} Mbps")
            lines.append(f"- Upswitch median: {up_stats.median:.2f} Mbps")
            hysteresis = up_stats.median - down_stats.median
            lines.append(f"- **Hysteresis:** {hysteresis:.2f} Mbps\n")
        
        lines.append("---\n")
    
    # Key conclusions
    lines.append("## Key Conclusions\n")
    if result.boundary_metrics:
        lines.append("### Safety Factor Summary\n")
        lines.append("| Boundary | Direction | Median α | Mean α | StdDev α |")
        lines.append("|----------|-----------|----------|--------|----------|")
        
        for key, bm in result.boundary_metrics.items():
            if bm.downswitch_count > 0:
                stats = Statistics(bm.downswitch_safety_factors)
                lines.append(f"| {bm.lower_idx}→{bm.upper_idx} | Down | {stats.median:.2f} | {stats.mean:.2f} | {stats.stddev:.2f} |")
            if bm.upswitch_count > 0:
                stats = Statistics(bm.upswitch_safety_factors)
                lines.append(f"| {bm.lower_idx}→{bm.upper_idx} | Up | {stats.median:.2f} | {stats.mean:.2f} | {stats.stddev:.2f} |")
        lines.append("")
    
    with open(output_path, 'w') as f:
        f.write('\n'.join(lines))
