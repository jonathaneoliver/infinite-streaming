"""ABR behavior analysis engine"""

import statistics
from datetime import datetime, timedelta
from typing import List, Dict, Optional, Tuple
from .playlist import Ladder, Variant
from .telemetry import TelemetryEvent, SegmentDownload


class SwitchEvent:
    """Represents a detected variant switch"""
    
    def __init__(self, timestamp: datetime, from_variant_idx: int, to_variant_idx: int,
                 from_bandwidth_mbps: float, to_bandwidth_mbps: float,
                 buffer_depth_s: Optional[float], est_throughput_mbps: float,
                 direction: str):
        self.timestamp = timestamp
        self.from_variant_idx = from_variant_idx
        self.to_variant_idx = to_variant_idx
        self.from_bandwidth_mbps = from_bandwidth_mbps
        self.to_bandwidth_mbps = to_bandwidth_mbps
        self.buffer_depth_s = buffer_depth_s
        self.est_throughput_mbps = est_throughput_mbps
        self.direction = direction  # "up" or "down"


class BoundaryMetrics:
    """Metrics for switches between two adjacent variants"""
    
    def __init__(self, lower_idx: int, upper_idx: int,
                 lower_bandwidth: float, upper_bandwidth: float):
        self.lower_idx = lower_idx
        self.upper_idx = upper_idx
        self.lower_bandwidth = lower_bandwidth
        self.upper_bandwidth = upper_bandwidth
        
        self.downswitch_count = 0
        self.downswitch_thresholds = []
        self.downswitch_safety_factors = []
        
        self.upswitch_count = 0
        self.upswitch_thresholds = []
        self.upswitch_safety_factors = []


class Statistics:
    """Statistical summary of a set of values"""
    
    def __init__(self, values: List[float]):
        self.count = len(values)
        if not values:
            self.mean = self.median = self.stddev = 0
            self.min_val = self.max_val = 0
            self.p25 = self.p75 = 0
        else:
            self.mean = statistics.mean(values)
            self.median = statistics.median(values)
            self.stddev = statistics.stdev(values) if len(values) > 1 else 0
            self.min_val = min(values)
            self.max_val = max(values)
            sorted_vals = sorted(values)
            self.p25 = sorted_vals[len(sorted_vals) // 4]
            self.p75 = sorted_vals[len(sorted_vals) * 3 // 4]


class AnalysisResult:
    """Results of ABR analysis"""
    
    def __init__(self, ladder: Ladder):
        self.ladder = ladder
        self.switch_events: List[SwitchEvent] = []
        self.boundary_metrics: Dict[str, BoundaryMetrics] = {}
        self.experiment_start: Optional[datetime] = None
        self.experiment_end: Optional[datetime] = None
    
    @property
    def total_switches(self) -> int:
        return len(self.switch_events)


class Analyzer:
    """Analyzes telemetry to characterize ABR behavior"""
    
    def __init__(self, ladder: Ladder, throughput_window_size: int = 5):
        self.ladder = ladder
        self.throughput_window_size = throughput_window_size
    
    def analyze(self, events: List[TelemetryEvent],
                downloads: List[SegmentDownload]) -> AnalysisResult:
        """Perform full ABR analysis
        
        Args:
            events: List of telemetry events
            downloads: List of segment downloads
            
        Returns:
            AnalysisResult with computed metrics
        """
        result = AnalysisResult(self.ladder)
        
        if events:
            result.experiment_start = events[0].timestamp
            result.experiment_end = events[-1].timestamp
        
        # Build throughput time series
        throughput_map = self._build_throughput_timeseries(downloads)
        
        # Detect switches
        switches = self._detect_switches(events, throughput_map)
        result.switch_events = switches
        
        # Compute boundary metrics
        result.boundary_metrics = self._compute_boundary_metrics(switches)
        
        return result
    
    def _build_throughput_timeseries(self, downloads: List[SegmentDownload]) -> Dict[datetime, float]:
        """Build throughput estimates using sliding window median"""
        throughput_map = {}
        window = []
        
        for dl in downloads:
            if dl.throughput_mbps > 0:
                window.append(dl.throughput_mbps)
                if len(window) > self.throughput_window_size:
                    window.pop(0)
                
                # Use median of window
                throughput_map[dl.timestamp] = statistics.median(window)
        
        return throughput_map
    
    def _detect_switches(self, events: List[TelemetryEvent],
                        throughput_map: Dict[datetime, float]) -> List[SwitchEvent]:
        """Detect variant switch events from telemetry"""
        switches = []
        last_bitrate = None
        last_idx = None
        
        for event in events:
            if event.variant_bitrate_mbps is None:
                continue
            
            # Find variant index
            variant_idx = self._find_variant_by_bitrate(event.variant_bitrate_mbps)
            if variant_idx < 0:
                continue
            
            # Check for switch
            if last_idx is not None and variant_idx != last_idx:
                # Get throughput estimate
                est_throughput = self._get_throughput_at_time(throughput_map, event.timestamp)
                
                direction = "up" if variant_idx > last_idx else "down"
                
                switch = SwitchEvent(
                    timestamp=event.timestamp,
                    from_variant_idx=last_idx,
                    to_variant_idx=variant_idx,
                    from_bandwidth_mbps=last_bitrate,
                    to_bandwidth_mbps=event.variant_bitrate_mbps,
                    buffer_depth_s=event.buffer_depth_s,
                    est_throughput_mbps=est_throughput,
                    direction=direction
                )
                switches.append(switch)
            
            last_bitrate = event.variant_bitrate_mbps
            last_idx = variant_idx
        
        return switches
    
    def _find_variant_by_bitrate(self, bitrate_mbps: float) -> int:
        """Find variant index matching the given bitrate"""
        tolerance = 0.1  # 0.1 Mbps tolerance
        
        for i, variant in enumerate(self.ladder.variants):
            avg_bw = variant.get_average_bandwidth_mbps()
            if abs(avg_bw - bitrate_mbps) < tolerance:
                return i
            
            bw = variant.get_bandwidth_mbps()
            if abs(bw - bitrate_mbps) < tolerance:
                return i
        
        return -1
    
    def _get_throughput_at_time(self, throughput_map: Dict[datetime, float],
                                target_time: datetime) -> float:
        """Get throughput estimate at a given time"""
        # Find closest measurement before target time
        best_time = None
        best_value = 0
        min_diff = timedelta(hours=1)
        
        for ts, value in throughput_map.items():
            diff = target_time - ts
            if diff >= timedelta(0) and diff < min_diff:
                min_diff = diff
                best_time = ts
                best_value = value
        
        return best_value
    
    def _compute_boundary_metrics(self, switches: List[SwitchEvent]) -> Dict[str, BoundaryMetrics]:
        """Compute metrics for each boundary"""
        boundaries = {}
        
        for switch in switches:
            # Determine boundary (always lower -> upper)
            if switch.direction == "down":
                lower_idx = switch.to_variant_idx
                upper_idx = switch.from_variant_idx
            else:
                lower_idx = switch.from_variant_idx
                upper_idx = switch.to_variant_idx
            
            key = f"{lower_idx}->{upper_idx}"
            
            if key not in boundaries:
                boundaries[key] = BoundaryMetrics(
                    lower_idx=lower_idx,
                    upper_idx=upper_idx,
                    lower_bandwidth=self.ladder.variants[lower_idx].get_average_bandwidth_mbps(),
                    upper_bandwidth=self.ladder.variants[upper_idx].get_average_bandwidth_mbps()
                )
            
            bm = boundaries[key]
            
            if switch.direction == "down":
                bm.downswitch_count += 1
                bm.downswitch_thresholds.append(switch.est_throughput_mbps)
                if switch.est_throughput_mbps > 0:
                    alpha = bm.upper_bandwidth / switch.est_throughput_mbps
                    bm.downswitch_safety_factors.append(alpha)
            else:
                bm.upswitch_count += 1
                bm.upswitch_thresholds.append(switch.est_throughput_mbps)
                if switch.est_throughput_mbps > 0:
                    alpha = bm.upper_bandwidth / switch.est_throughput_mbps
                    bm.upswitch_safety_factors.append(alpha)
        
        return boundaries
