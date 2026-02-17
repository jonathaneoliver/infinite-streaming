"""HLS multivariant playlist parsing"""

import re
import urllib.request
from typing import List, Optional, Tuple


class Variant:
    """Represents a single HLS variant stream"""
    
    def __init__(self, uri: str, bandwidth: int, average_bandwidth: Optional[int] = None,
                 resolution: Optional[str] = None, codecs: Optional[str] = None,
                 frame_rate: Optional[float] = None, index: int = 0):
        self.uri = uri
        self.bandwidth = bandwidth
        self.average_bandwidth = average_bandwidth
        self.resolution = resolution
        self.codecs = codecs
        self.frame_rate = frame_rate
        self.index = index
    
    def get_bandwidth_mbps(self) -> float:
        """Return bandwidth in Mbps"""
        return self.bandwidth / 1_000_000.0
    
    def get_average_bandwidth_mbps(self) -> float:
        """Return average bandwidth in Mbps (or regular bandwidth if not set)"""
        if self.average_bandwidth:
            return self.average_bandwidth / 1_000_000.0
        return self.get_bandwidth_mbps()
    
    def get_effective_bandwidth(self) -> int:
        """Return AVERAGE-BANDWIDTH if available, otherwise BANDWIDTH"""
        return self.average_bandwidth or self.bandwidth
    
    def __repr__(self):
        parts = [f"{self.get_bandwidth_mbps():.2f} Mbps"]
        if self.resolution:
            parts.append(self.resolution)
        if self.average_bandwidth:
            parts.append(f"avg={self.get_average_bandwidth_mbps():.2f} Mbps")
        return ", ".join(parts)


class Ladder:
    """Represents a parsed HLS multivariant playlist with ordered variants"""
    
    def __init__(self, variants: List[Variant], base_url: str):
        self.variants = sorted(variants, key=lambda v: v.bandwidth)
        self.base_url = base_url
    
    def find_variant_by_bandwidth(self, bandwidth_mbps: float) -> Optional[Variant]:
        """Find the variant closest to the given bandwidth (in Mbps)"""
        if not self.variants:
            return None
        
        target_bps = int(bandwidth_mbps * 1_000_000)
        closest = min(self.variants, 
                     key=lambda v: abs(v.get_effective_bandwidth() - target_bps))
        return closest


def parse_hls_master(url_or_path: str) -> Ladder:
    """Parse an HLS multivariant (master) playlist
    
    Args:
        url_or_path: URL or local file path to master playlist
        
    Returns:
        Ladder object with parsed variants
    """
    # Read content
    if url_or_path.startswith("http://") or url_or_path.startswith("https://"):
        with urllib.request.urlopen(url_or_path) as response:
            content = response.read().decode('utf-8')
    else:
        with open(url_or_path, 'r') as f:
            content = f.read()
    
    # Parse variants
    variants = []
    lines = content.strip().split('\n')
    current_stream_inf = None
    variant_index = 0
    
    for line in lines:
        line = line.strip()
        if not line:
            continue
        
        if line.startswith('#EXT-X-STREAM-INF:'):
            current_stream_inf = line
        elif current_stream_inf and not line.startswith('#'):
            # This is the URI line following a STREAM-INF
            variant = _parse_variant(current_stream_inf, line, variant_index)
            variants.append(variant)
            variant_index += 1
            current_stream_inf = None
    
    if not variants:
        raise ValueError("No variants found in master playlist")
    
    return Ladder(variants, url_or_path)


def _parse_variant(stream_inf: str, uri: str, index: int) -> Variant:
    """Parse a single variant from EXT-X-STREAM-INF line"""
    # Extract attributes using regex
    bandwidth_match = re.search(r'BANDWIDTH=(\d+)', stream_inf)
    avg_bandwidth_match = re.search(r'AVERAGE-BANDWIDTH=(\d+)', stream_inf)
    resolution_match = re.search(r'RESOLUTION=(\d+x\d+)', stream_inf)
    codecs_match = re.search(r'CODECS="([^"]+)"', stream_inf)
    frame_rate_match = re.search(r'FRAME-RATE=([\d.]+)', stream_inf)
    
    bandwidth = int(bandwidth_match.group(1)) if bandwidth_match else 0
    average_bandwidth = int(avg_bandwidth_match.group(1)) if avg_bandwidth_match else None
    resolution = resolution_match.group(1) if resolution_match else None
    codecs = codecs_match.group(1) if codecs_match else None
    frame_rate = float(frame_rate_match.group(1)) if frame_rate_match else None
    
    return Variant(
        uri=uri,
        bandwidth=bandwidth,
        average_bandwidth=average_bandwidth,
        resolution=resolution,
        codecs=codecs,
        frame_rate=frame_rate,
        index=index
    )
