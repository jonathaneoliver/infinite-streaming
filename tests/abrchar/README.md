# ABR Characterization Tool

Python-based tool for automated ABR (Adaptive Bitrate) characterization of HLS players.

## Features

- **HLS Playlist Parsing**: Extracts BANDWIDTH and AVERAGE-BANDWIDTH from multivariant playlists
- **Telemetry Analysis**: Analyzes player telemetry to detect variant switches
- **Metrics Calculation**: Computes safety factors, switch thresholds, and hysteresis
- **Report Generation**: Creates JSON summaries and Markdown reports

## Installation

The tool is part of the test suite and uses standard Python libraries plus:

```bash
pip install requests  # For HTTP throttler (optional)
```

## Usage

### Analyze Existing Telemetry

```bash
python -m tests.abrchar.cli analyze \
  --data tests/abrchar/test_data \
  --hls-url tests/abrchar/test_data/master.m3u8 \
  --output ./analysis-results
```

### Run Tests

```bash
# Run all abrchar tests
pytest tests/abrchar/ -v

# Run only playlist parser tests
pytest tests/abrchar/test_playlist.py -v
```

## Telemetry Format

### Events (telemetry_events.jsonl)

```json
{"timestamp":"2024-01-01T12:00:00Z","variant_bitrate_mbps":5.0,"buffer_depth_s":10.5,"stall_count":0}
```

Required fields:
- `timestamp`: ISO 8601 timestamp
- `variant_bitrate_mbps`: Current variant bitrate in Mbps

Optional fields:
- `buffer_depth_s`: Buffer depth in seconds
- `stall_count`: Cumulative stall count

### Segment Downloads (segment_downloads.jsonl)

```json
{"url":"seg.m4s","timestamp":"2024-01-01T12:00:00Z","bytes":500000,"duration_ms":200,"throughput_mbps":20.0}
```

Required fields:
- `url`: Segment URL
- `timestamp`: Download timestamp
- `bytes`: Bytes downloaded
- `duration_ms`: Download duration in milliseconds
- `throughput_mbps`: Measured throughput in Mbps

## Output

### summary.json

Machine-readable JSON with:
- Experiment metadata
- Bitrate ladder
- Per-boundary metrics (downswitch/upswitch thresholds, safety factors)
- Statistical summaries (mean, median, stddev, percentiles)

### report.md

Human-friendly Markdown report with:
- Experiment summary
- Bitrate ladder table
- Boundary metrics with statistical analysis
- Hysteresis measurements
- Safety factor summary table

## Integration with go-proxy

The `HTTPThrottler` class can control go-proxy throttling:

```python
from tests.abrchar.throttle import HTTPThrottler

throttler = HTTPThrottler("http://localhost:21081", port=30081)
throttler.set_bandwidth(5.0)  # Set to 5 Mbps
```

## Module Structure

- `playlist.py` - HLS multivariant playlist parsing
- `telemetry.py` - Telemetry data structures and loading
- `throttle.py` - Network throttling control (HTTP and shell)
- `analysis.py` - Switch detection and metrics calculation
- `output.py` - Report generation (JSON and Markdown)
- `cli.py` - Command-line interface
- `test_playlist.py` - Pytest tests for playlist parsing

## Example Output

From 12 telemetry events and 7 segment downloads:
- Detected 5 variant switches
- Identified 3 boundaries
- Computed safety factors for each direction
- Generated detailed reports with statistics

Safety factors typically range from 0.2-0.4, meaning players choose variants when:
`variant_bitrate ≤ 0.3 × estimated_throughput` (approximately)

## Future Enhancements

- Full `run` command implementation with player automation
- Real-time telemetry collection
- Plot generation (throughput/variant time series)
- More sophisticated throughput estimation (EWMA)
