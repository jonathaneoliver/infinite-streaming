"""Telemetry data structures and collection"""

import json
from datetime import datetime
from typing import Optional, List


class TelemetryEvent:
    """A single telemetry event from the player"""
    
    def __init__(self, timestamp: datetime, variant_bitrate_mbps: Optional[float] = None,
                 buffer_depth_s: Optional[float] = None, stall_count: Optional[int] = None,
                 **kwargs):
        self.timestamp = timestamp
        self.variant_bitrate_mbps = variant_bitrate_mbps
        self.buffer_depth_s = buffer_depth_s
        self.stall_count = stall_count
        self.extra = kwargs
    
    @classmethod
    def from_dict(cls, data: dict) -> 'TelemetryEvent':
        """Create TelemetryEvent from dictionary"""
        # Parse timestamp
        ts_str = data.get('timestamp')
        if isinstance(ts_str, str):
            timestamp = datetime.fromisoformat(ts_str.replace('Z', '+00:00'))
        else:
            timestamp = datetime.now()
        
        return cls(
            timestamp=timestamp,
            variant_bitrate_mbps=data.get('variant_bitrate_mbps'),
            buffer_depth_s=data.get('buffer_depth_s'),
            stall_count=data.get('stall_count'),
            **{k: v for k, v in data.items() 
               if k not in ('timestamp', 'variant_bitrate_mbps', 'buffer_depth_s', 'stall_count')}
        )


class SegmentDownload:
    """Metrics for a single segment download"""
    
    def __init__(self, url: str, timestamp: datetime, bytes_downloaded: int,
                 duration_ms: float, throughput_mbps: Optional[float] = None,
                 **kwargs):
        self.url = url
        self.timestamp = timestamp
        self.bytes_downloaded = bytes_downloaded
        self.duration_ms = duration_ms
        self.throughput_mbps = throughput_mbps or (bytes_downloaded * 8 / 1_000_000) / (duration_ms / 1000)
        self.extra = kwargs
    
    @classmethod
    def from_dict(cls, data: dict) -> 'SegmentDownload':
        """Create SegmentDownload from dictionary"""
        # Parse timestamp
        ts_str = data.get('timestamp')
        if isinstance(ts_str, str):
            timestamp = datetime.fromisoformat(ts_str.replace('Z', '+00:00'))
        else:
            timestamp = datetime.now()
        
        return cls(
            url=data.get('url', ''),
            timestamp=timestamp,
            bytes_downloaded=data.get('bytes', 0),
            duration_ms=data.get('duration_ms', 0),
            throughput_mbps=data.get('throughput_mbps'),
            **{k: v for k, v in data.items() 
               if k not in ('url', 'timestamp', 'bytes', 'duration_ms', 'throughput_mbps')}
        )


def load_telemetry_events(filepath: str) -> List[TelemetryEvent]:
    """Load telemetry events from JSON lines file"""
    events = []
    with open(filepath, 'r') as f:
        for line in f:
            line = line.strip()
            if line:
                data = json.loads(line)
                events.append(TelemetryEvent.from_dict(data))
    return events


def load_segment_downloads(filepath: str) -> List[SegmentDownload]:
    """Load segment downloads from JSON lines file"""
    downloads = []
    with open(filepath, 'r') as f:
        for line in f:
            line = line.strip()
            if line:
                data = json.loads(line)
                downloads.append(SegmentDownload.from_dict(data))
    return downloads


class TelemetryCollector:
    """Collector for writing telemetry to JSON lines files"""
    
    def __init__(self, output_dir: str):
        import os
        self.output_dir = output_dir
        os.makedirs(output_dir, exist_ok=True)
        
        self.events_file = open(f"{output_dir}/telemetry_events.jsonl", 'w')
        self.downloads_file = open(f"{output_dir}/segment_downloads.jsonl", 'w')
    
    def record_event(self, event: dict):
        """Record a telemetry event"""
        json.dump(event, self.events_file)
        self.events_file.write('\n')
        self.events_file.flush()
    
    def record_download(self, download: dict):
        """Record a segment download"""
        json.dump(download, self.downloads_file)
        self.downloads_file.write('\n')
        self.downloads_file.flush()
    
    def close(self):
        """Close all files"""
        if self.events_file:
            self.events_file.close()
        if self.downloads_file:
            self.downloads_file.close()
