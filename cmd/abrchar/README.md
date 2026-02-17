# abrchar - ABR Characterization Tool for HLS Players

## Overview

`abrchar` is a command-line tool for automated ABR (Adaptive Bitrate) characterization of HLS players. It helps you understand player behavior by:

- Executing controlled throttling experiments
- Collecting player telemetry and segment download metrics
- Analyzing variant switching behavior
- Computing downswitch/upswitch thresholds and safety factors
- Detecting hysteresis in ABR decisions
- Generating machine-readable (JSON) and human-friendly (Markdown) reports

## Installation

From the repository root:

```bash
cd cmd/abrchar
go build -o abrchar main.go
```

Or install globally:

```bash
cd cmd/abrchar
go install
```

## Quick Start

### 1. Validate HLS URL and Generate Config Template

Test that the tool can parse your HLS master playlist:

```bash
./abrchar run --hls-url https://example.com/master.m3u8
```

This will validate the URL and show the detected bitrate ladder but won't execute a full experiment.

### 2. Analyze Existing Telemetry Data

If you already have telemetry data from a player session:

```bash
./abrchar analyze \
  --data ./telemetry-logs \
  --hls-url https://example.com/master.m3u8 \
  --output ./analysis-results
```

Expected telemetry files in `--data` directory:
- `telemetry_events.jsonl` - Player telemetry events (JSON lines format)
- `segment_downloads.jsonl` - Segment download metrics (optional)

## Commands

### `run` - Execute an ABR Characterization Experiment

**Note:** The `run` command currently validates configuration and shows what would happen, but does not execute a full end-to-end experiment. To implement full execution, you would need to:

1. Integrate with a player automation framework (e.g., Selenium for web players)
2. Implement telemetry collection from the player
3. Execute the throttling schedule via the configured throttler

**Usage:**

```bash
abrchar run --config experiment.yaml
```

Or with command-line options:

```bash
abrchar run --hls-url https://example.com/master.m3u8 --output ./results
```

**Options:**
- `--config <file>` - Path to YAML or JSON config file
- `--hls-url <url>` - HLS multivariant playlist URL
- `--output <dir>` - Output directory for results

### `analyze` - Analyze Existing Telemetry Data

Analyzes collected telemetry to characterize ABR behavior.

**Usage:**

```bash
abrchar analyze \
  --data ./telemetry-logs \
  --hls-url https://example.com/master.m3u8 \
  --output ./analysis-results
```

**Options:**
- `--data <dir>` - Directory containing telemetry logs (required)
- `--hls-url <url>` - HLS multivariant playlist URL (required)
- `--output <dir>` - Output directory for analysis (default: `<data>/analysis`)

**Input Files:**

The `--data` directory should contain:

1. **`telemetry_events.jsonl`** (required) - Player telemetry events in JSON lines format:
   ```json
   {"timestamp":"2024-01-01T12:00:00Z","variant_bitrate_mbps":5.0,"buffer_depth_s":10.5,"stall_count":0}
   {"timestamp":"2024-01-01T12:00:02Z","variant_bitrate_mbps":3.0,"buffer_depth_s":8.2,"stall_count":0}
   ```

2. **`segment_downloads.jsonl`** (optional) - Segment download metrics:
   ```json
   {"url":"seg1.m4s","timestamp":"2024-01-01T12:00:00Z","bytes":500000,"duration_ms":200,"throughput_mbps":20.0}
   ```

**Output Files:**

The `--output` directory will contain:

1. **`summary.json`** - Machine-readable JSON with all metrics
2. **`report.md`** - Human-friendly Markdown report with tables and conclusions

## Configuration

### YAML Config Example

Create a file `experiment.yaml`:

```yaml
hls_url: https://example.com/master.m3u8
experiment_duration: 5m
player_id: hlsjs-test

throttle:
  method: http  # or "shell"
  http_url: http://localhost:8080
  port: 30081
  warmup_bandwidth: 20.0  # Mbps
  step_percent: 10.0      # % decrease per step
  hold_duration: 20s      # Hold each step for this long
  min_bandwidth: 1.0      # Mbps
  max_bandwidth: 20.0     # Mbps
  direction: down         # "down", "up", or "down-up"

output_dir: ./abrchar-output

analysis:
  throughput_window_size: 5  # Number of segments for throughput estimation
```

### Throttle Configuration

The tool supports two throttling methods:

#### 1. HTTP API (`method: http`)

Calls an HTTP endpoint to control bandwidth:

```yaml
throttle:
  method: http
  http_url: http://localhost:8080  # Base URL of throttling API
  port: 30081                       # Port to throttle
```

Expected API: `POST /api/nft/bandwidth/{port}` with JSON body `{"rate": <mbps>}`

#### 2. Shell Command (`method: shell`)

Executes a shell command to control bandwidth:

```yaml
throttle:
  method: shell
  command: "tc qdisc change dev eth0 root tbf rate {{.Rate}}mbit"
  reset_cmd: "tc qdisc del dev eth0 root"
```

The `{{.Rate}}` placeholder is replaced with the target bandwidth in Mbps.

### Throttle Schedule Parameters

- **`warmup_bandwidth`** - Starting bandwidth (typically 2x top variant)
- **`step_percent`** - Percentage decrease per step (e.g., 10 = 10% steps)
- **`hold_duration`** - Duration to hold each bandwidth level
- **`min_bandwidth`** - Minimum bandwidth to test
- **`max_bandwidth`** - Maximum bandwidth to test
- **`direction`** - Throttle direction:
  - `down` - Step down from max to min
  - `up` - Step up from min to max
  - `down-up` - Step down then back up

**Hold Duration Guidelines:**

For HLS with ~2s segments, use holds ≥ 20s (10 segments) to allow the player's throughput estimator to stabilize before expecting a switch.

## Telemetry Schema

### Player Events

The tool expects telemetry events with the following fields:

```go
{
  "timestamp": "2024-01-01T12:00:00Z",        // ISO 8601 timestamp
  "session_id": "session-123",                 // Optional session ID
  "selected_variant": "variant_720p.m3u8",     // Optional variant URI
  "variant_bitrate_mbps": 5.0,                 // Current variant bitrate (Mbps)
  "buffer_depth_s": 10.5,                      // Buffer depth (seconds)
  "buffer_end_s": 45.2,                        // Buffer end position (seconds)
  "position_s": 34.7,                          // Playback position (seconds)
  "stall_count": 0,                            // Cumulative stalls
  "stall_time_s": 0.0,                         // Cumulative stall time (seconds)
  "event_type": "variant_change",              // Event type
  "player_state": "playing",                   // Player state
  "network_bitrate_mbps": 6.2                  // Player's estimated throughput
}
```

**Required Fields:**
- `timestamp` - Event timestamp
- `variant_bitrate_mbps` - Current variant bitrate

**Recommended Fields:**
- `buffer_depth_s` - For analyzing buffer behavior during switches
- `network_bitrate_mbps` - Player's own throughput estimate (if available)

### Segment Downloads

Optional per-segment download metrics:

```go
{
  "url": "segment_001.m4s",
  "timestamp": "2024-01-01T12:00:00Z",
  "start_time": "2024-01-01T12:00:00.000Z",
  "end_time": "2024-01-01T12:00:00.200Z",
  "bytes": 500000,
  "duration_ms": 200,
  "throughput_mbps": 20.0,
  "variant_bitrate_mbps": 5.0
}
```

These are used to compute a throughput time series for more accurate switch analysis.

## Analysis Output

### Summary JSON

The `summary.json` file contains structured metrics:

```json
{
  "experiment_start": "2024-01-01T12:00:00Z",
  "experiment_end": "2024-01-01T12:05:00Z",
  "total_switches": 8,
  "variants": [
    {
      "index": 0,
      "bandwidth_mbps": 1.28,
      "average_bandwidth_mbps": 1.0,
      "resolution": "640x360"
    }
  ],
  "boundaries": [
    {
      "lower_variant_idx": 0,
      "upper_variant_idx": 1,
      "lower_bandwidth_mbps": 1.0,
      "upper_bandwidth_mbps": 2.0,
      "downswitch": {
        "count": 2,
        "thresholds": {
          "mean": 2.1,
          "median": 2.0,
          "std_dev": 0.15
        },
        "safety_factors": {
          "mean": 0.95,
          "median": 1.0
        }
      },
      "upswitch": {
        "count": 1,
        "thresholds": {
          "mean": 2.5,
          "median": 2.5
        }
      }
    }
  ]
}
```

### Markdown Report

The `report.md` file contains human-readable analysis:

- **Experiment Summary** - Duration, total switches
- **Bitrate Ladder** - Table of all variants with BANDWIDTH and AVERAGE-BANDWIDTH
- **Boundary Metrics** - Per-boundary analysis including:
  - Downswitch/upswitch counts
  - Throughput threshold distributions (mean, median, stddev, range)
  - Safety factors α = variant_bw / throughput (mean, median, stddev, range)
  - Hysteresis (difference between up and down thresholds)
- **Key Conclusions** - Summary table of safety factors across all boundaries

## HLS Playlist Parsing

The tool parses HLS multivariant playlists to extract:

- **BANDWIDTH** (required) - Peak segment bitrate
- **AVERAGE-BANDWIDTH** (optional, recommended by Apple) - Average segment bitrate
- **RESOLUTION** (optional)
- **CODECS** (optional)
- **FRAME-RATE** (optional)

When both BANDWIDTH and AVERAGE-BANDWIDTH are present, the tool:
- Uses **AVERAGE-BANDWIDTH** for safety factor calculations (as recommended)
- Reports both values in outputs
- Uses AVERAGE-BANDWIDTH as the "effective bandwidth" for variant matching

## Integration with InfiniteStream

This tool is designed to work with the InfiniteStream go-proxy throttling API:

1. **Start InfiniteStream:**
   ```bash
   docker compose up -d
   ```

2. **Create a player session** through the testing dashboard

3. **Note the session port** (e.g., 30081)

4. **Configure abrchar** to use the HTTP throttler:
   ```yaml
   throttle:
     method: http
     http_url: http://localhost:21081  # Or wherever go-proxy is running
     port: 30081                        # Session port
   ```

5. **Export telemetry** from the player session to JSON lines format

6. **Run analysis:**
   ```bash
   ./abrchar analyze --data ./session-telemetry --hls-url <url> --output ./results
   ```

## Testing

Run unit tests:

```bash
go test ./pkg/... -v
```

Test specific packages:

```bash
go test ./pkg/playlist -v
go test ./pkg/analysis -v
```

## Assumptions and Limitations

### Current Implementation

- **Analysis only**: The `run` command validates config but doesn't execute full experiments
- **Telemetry collection**: You must provide telemetry in the expected JSON format
- **Player automation**: Not implemented - you must manually collect telemetry

### To Implement Full Experiments

To make the `run` command fully functional, you would need to add:

1. **Player automation**:
   - Web players: Use Selenium or Playwright
   - Native players: Use platform-specific APIs

2. **Telemetry collection**:
   - Hook into player events
   - Collect metrics at regular intervals
   - Write to JSON lines format

3. **Segment monitoring**:
   - Intercept or log segment downloads
   - Measure download times and throughput

### Known Limitations

- **Variant matching tolerance**: Uses ±0.1 Mbps tolerance for matching reported bitrates to ladder
- **Throughput estimation**: Uses simple median over last N segments (configurable)
- **Switch detection**: Detects switches from telemetry only (not from network logs)
- **Safety factor**: Assumes `α = variant_bw / throughput` model

## Troubleshooting

### "No variants found in master playlist"

- Verify the URL points to a multivariant (master) playlist, not a media playlist
- Check that the playlist contains `#EXT-X-STREAM-INF` tags

### "Failed to load telemetry events"

- Ensure `telemetry_events.jsonl` exists in the `--data` directory
- Verify the file is valid JSON lines format (one JSON object per line)
- Check that events have `timestamp` and `variant_bitrate_mbps` fields

### No switches detected

- Ensure telemetry includes variant bitrate changes over time
- Verify that the bitrates in telemetry match the ladder (within 0.1 Mbps tolerance)
- Check that there are enough events (need at least 2 with different bitrates)

## License

This tool is part of the InfiniteStream project. See the repository LICENSE file for details.
