# Testing Framework Improvement Plan

## Current Strengths

Your testing framework is already quite robust with:
- **Comprehensive HTTP failure coverage** (404, 403, 500, timeouts, connection refused, DNS, rate limiting)
- **Socket-level failures** across request lifecycle (connect/first-byte/body phases with reset/hang/delay)
- **Multiple failure modes** (requests, seconds, failures_per_seconds)
- **Variant scoping** for manifest and segment failures
- **Transport-level faults** (packet drop/reject)
- **Pre/post validation** ensuring recovery between tests
- **Interactive web UI** with real-time charting and controls

## Recommended Improvements

### 1. DASH-Specific Scenarios
**Gap**: While DASH URLs are supported, DASH-specific failure modes aren't tested
**Additions**:
- MPD manifest syntax errors
- Period transitions during failures
- Multi-period content with failures
- SegmentTemplate vs SegmentList failure scenarios
- Initialization segment failures

### 2. Partial/Incomplete Content Delivery
**Gap**: Tests timeout or full corruption, but not partial delivery
**Additions**:
- Segment starts but truncates at 25%/50%/75%
- Manifest fetches that return incomplete M3U8
- Headers received but body never arrives (different from timeout)
- Content-Length mismatch scenarios

**Implementation Example**:
```python
# Add to hls_failure_probe.py
partial_failures = ["partial_25", "partial_50", "partial_75", "header_only"]

def make_partial_test(kind, cutoff_pct, schedule, mode):
    return {
        "name": f"{kind}_partial_{cutoff_pct}_pct_{mode['label']}_{schedule['label']}",
        "desc": f"{kind} partial delivery {cutoff_pct}% cutoff",
        "payload": {
            f"{kind}_failure_type": f"partial_{cutoff_pct}",
            f"{kind}_consecutive_failures": schedule["consecutive"],
            f"{kind}_failure_frequency": schedule["frequency"],
            f"{kind}_failure_units": mode["consecutive_units"],
            f"{kind}_consecutive_units": mode["consecutive_units"],
            f"{kind}_frequency_units": mode["frequency_units"],
            f"{kind}_failure_mode": mode["mode"],
        },
        "expect": {f"{kind}_http_error": 0, f"{kind}_partial": args.expect_partial},
        "kind": kind,
    }
```

### 3. Slow Transfer Testing
**Gap**: Bandwidth throttling exists, but not graduated slowdowns
**Additions**:
- Ultra-slow segment delivery (under playback rate but not timeout)
- Progressive slowdown during segment fetch
- Bandwidth that varies within a single segment fetch
- Test buffer exhaustion vs. ABR downshift behavior

**Implementation**:
```python
# Add test cases for slow delivery rates
slow_transfer_tests = [
    {"name": "ultra_slow_0.5x", "rate_mbps": 0.5, "expected": "buffer_exhaustion"},
    {"name": "ultra_slow_0.8x", "rate_mbps": 0.8, "expected": "stall_or_downshift"},
    {"name": "progressive_slowdown", "rate_pattern": [5, 3, 1, 0.5], "expected": "gradual_degradation"}
]
```

### 4. Network Jitter and Variability
**Gap**: Static bandwidth limits, no dynamic variation
**Additions**:
- Random jitter in latency (±50ms, ±200ms ranges)
- Packet loss bursts vs. distributed loss
- Bandwidth oscillation patterns
- Asymmetric upload/download delays

**Server Implementation** (go-proxy/cmd/server/main.go):
```go
type JitterConfig struct {
    BaseDelayMs int
    JitterMs    int
    Distribution string // "uniform", "normal", "burst"
}
```

### 5. Malformed Content Testing
**Gap**: Corruption is zero-fill only; no syntax errors
**Additions**:
- Invalid M3U8 syntax (missing #EXTM3U, malformed tags)
- Invalid MPD XML structure
- Playlists with missing required attributes
- Segments referenced in playlist but returning 404
- Empty playlists (no segments)
- Duplicate segment entries
- Out-of-order segment sequences

**Test Cases**:
```python
malformed_types = [
    "missing_header",           # No #EXTM3U
    "invalid_extinf",          # Malformed #EXTINF tags
    "missing_segment",         # Segments in playlist return 404
    "empty_playlist",          # Playlist with no segments
    "duplicate_segments",      # Same segment URL twice
    "invalid_sequence",        # EXT-X-MEDIA-SEQUENCE errors
    "missing_required_attrs",  # Missing BANDWIDTH, CODECS, etc.
]
```

### 6. Advanced Player Resilience
**Gap**: Basic playback testing, limited player behavior validation
**Additions**:
- ABR switching during failure conditions
- Seek operations during active failures
- Live edge catching behavior when behind
- Fast channel switching with failures
- Concurrent multi-track failures (audio + video)
- Subtitle/caption track failure isolation

**Metrics to Track**:
```python
player_behavior_metrics = {
    "abr_switches": 0,
    "quality_downgrades": 0,
    "quality_upgrades": 0,
    "seek_events": 0,
    "rebuffer_events": 0,
    "rebuffer_duration_ms": 0,
    "startup_time_ms": 0,
}
```

### 7. Failure Combinations
**Gap**: Tests run one failure type at a time
**Additions**:
- Simultaneous manifest timeout + segment corruption
- Transport drop + application-level 500 errors
- Multi-layer cascading failures
- Failure during recovery from previous failure

**Implementation**:
```python
combination_tests = [
    {
        "name": "manifest_timeout_segment_corrupt",
        "manifest_failure": "timeout",
        "segment_failure": "corrupted",
        "expect_recovery": True,
    },
    {
        "name": "transport_drop_http_500",
        "transport_failure": "drop",
        "segment_failure": "500",
        "expect": "cumulative_errors",
    },
    {
        "name": "cascading_manifest_segment",
        "sequence": ["manifest_404", "wait_2s", "segment_timeout"],
        "expect_complete_failure": False,
    }
]
```

### 8. Advanced Recovery Scenarios
**Gap**: Basic recovery validation exists but limited depth
**Current**: Just checks if playback recovers
**Enhancements**:
- Time-to-recovery measurement
- Quality degradation during recovery (did it downshift?)
- Recovery retry count/strategy detection
- Permanent vs. transient failure differentiation

**Implementation**:
```python
def measure_recovery_metrics(url, args, failure_start_time):
    recovery_metrics = {
        "time_to_first_success_ms": None,
        "time_to_stable_playback_ms": None,
        "quality_before_failure": None,
        "quality_after_recovery": None,
        "retry_attempts": 0,
        "final_state": None,  # "recovered", "degraded", "failed"
    }

    # Poll for recovery with detailed tracking
    start = time.time()
    stable_count = 0

    while time.time() - start < args.recovery_timeout:
        status, data, dt, err = http_fetch(url, timeout=args.timeout)
        if status == 200 and not err:
            if recovery_metrics["time_to_first_success_ms"] is None:
                recovery_metrics["time_to_first_success_ms"] = int((time.time() - failure_start_time) * 1000)
            stable_count += 1
            if stable_count >= 3:
                recovery_metrics["time_to_stable_playback_ms"] = int((time.time() - failure_start_time) * 1000)
                recovery_metrics["final_state"] = "recovered"
                break
        else:
            recovery_metrics["retry_attempts"] += 1
            stable_count = 0
        time.sleep(0.5)

    return recovery_metrics
```

### 9. Test Organization & Reporting
**Gap**: Flat test list, limited organization
**Additions**:
- Group tests by category (HTTP, Socket, Transport, Corruption)
- Add test tags/metadata (severity, likelihood, player-specific)
- Generate test coverage matrix
- Add HTML test report output
- Test dependency chains (run X only if Y passes)

**Test Metadata Structure**:
```python
test_metadata = {
    "category": "http_errors",  # http_errors, socket_faults, transport, corruption, malformed
    "severity": "high",         # high, medium, low
    "likelihood": "common",     # common, uncommon, rare
    "player_support": ["hlsjs", "shaka", "native"],
    "requires": [],             # list of test IDs that must pass first
    "tags": ["edge_case", "production_blocker", "regression"],
}
```

**HTML Report Generation**:
```python
def generate_html_report(test_results, output_path="test_report.html"):
    template = """
    <html>
    <head><title>Failure Injection Test Report</title></head>
    <body>
        <h1>Test Results</h1>
        <table>
            <tr><th>Category</th><th>Test</th><th>Status</th><th>Duration</th><th>Details</th></tr>
            {% for result in results %}
            <tr class="{{ result.status }}">
                <td>{{ result.category }}</td>
                <td>{{ result.name }}</td>
                <td>{{ result.status }}</td>
                <td>{{ result.duration }}s</td>
                <td>{{ result.details }}</td>
            </tr>
            {% endfor %}
        </table>
    </body>
    </html>
    """
    # Render and save
```

### 10. Enhanced Metrics Collection
**Gap**: Basic counters, limited timing data
**Additions to run_probe_window()**:
- TTFB (time to first byte) per request
- Segment download time percentiles (p50, p95, p99)
- Player buffer events timeline
- Quality switch events with timestamps
- Error recovery duration

**Implementation**:
```python
class DetailedMetrics:
    def __init__(self):
        self.ttfb_samples = []
        self.download_times = []
        self.buffer_events = []
        self.quality_switches = []
        self.errors = []

    def record_fetch(self, url, ttfb_ms, total_ms, bytes_received, status):
        self.ttfb_samples.append(ttfb_ms)
        self.download_times.append(total_ms)

    def percentiles(self, data, percentiles=[50, 95, 99]):
        sorted_data = sorted(data)
        return {
            f"p{p}": sorted_data[int(len(sorted_data) * p / 100)]
            for p in percentiles
        }

    def summary(self):
        return {
            "ttfb": self.percentiles(self.ttfb_samples),
            "download_time": self.percentiles(self.download_times),
            "buffer_events": len(self.buffer_events),
            "quality_switches": len(self.quality_switches),
        }
```

### 11. Variant Selection Improvements
**Current** (hls_failure_probe.py:284-290): Only selects first 2 variants

**Enhancements**:
```python
def select_variant_groups(variants, strategy="first_two"):
    """
    Strategies:
    - first_two: Original behavior
    - random: Random selection
    - all_but_one: All variants except one
    - alternating: Every other variant
    - bitrate_based: High/low/mid based on bandwidth
    """
    if strategy == "first_two":
        return variants[:2], variants[2:]
    elif strategy == "random":
        import random
        selected_count = max(1, len(variants) // 2)
        selected = random.sample(variants, selected_count)
        unselected = [v for v in variants if v not in selected]
        return [v["name"] for v in selected], [v["name"] for v in unselected]
    elif strategy == "all_but_one":
        if len(variants) <= 1:
            return [v["name"] for v in variants], []
        unselected = [variants[0]]
        selected = variants[1:]
        return [v["name"] for v in selected], [v["name"] for v in unselected]
    elif strategy == "bitrate_based":
        # Assume variants are sorted by bitrate
        if len(variants) < 3:
            return [v["name"] for v in variants[:1]], [v["name"] for v in variants[1:]]
        low = variants[0]
        high = variants[-1]
        mid = variants[len(variants) // 2]
        selected = [high, low]
        unselected = [v for v in variants if v not in selected]
        return [v["name"] for v in selected], [v["name"] for v in unselected]
    else:
        return variants[:2], variants[2:]
```

### 12. Web UI Enhancements
**testing-session.html improvements**:

**Quick Test Presets**:
```html
<div class="preset-tests">
    <h4>Quick Test Scenarios</h4>
    <button onclick="runPreset('network_outage')">Network Outage (30s)</button>
    <button onclick="runPreset('congestion')">Network Congestion</button>
    <button onclick="runPreset('cdn_failure')">CDN Failure</button>
    <button onclick="runPreset('progressive_degradation')">Progressive Degradation</button>
</div>
```

**Export/Import Configurations**:
```javascript
function exportTestConfig(sessionId) {
    const card = document.querySelector(`[data-session-id="${sessionId}"]`);
    const config = TestingSessionUI.readSessionSettings(card);
    const json = JSON.stringify(config, null, 2);
    downloadAsFile(`test-config-${sessionId}.json`, json);
}

function importTestConfig(sessionId, file) {
    const reader = new FileReader();
    reader.onload = (e) => {
        const config = JSON.parse(e.target.result);
        applyConfig(sessionId, config);
    };
    reader.readAsText(file);
}
```

**Failure Scheduling Calendar**:
```javascript
// Schedule failures at specific times
const schedule = {
    "2024-01-15T10:00:00Z": {type: "segment_timeout", duration: 60},
    "2024-01-15T10:05:00Z": {type: "manifest_404", duration: 30},
};
```

**Player Error Event Visualization**:
```javascript
// Add timeline visualization of player errors
function renderErrorTimeline(errors) {
    const timeline = document.createElement('div');
    timeline.className = 'error-timeline';
    errors.forEach(error => {
        const marker = document.createElement('div');
        marker.className = `error-marker ${error.type}`;
        marker.style.left = `${(error.timestamp / totalDuration) * 100}%`;
        marker.title = `${error.type} at ${error.timestamp}s`;
        timeline.appendChild(marker);
    });
    return timeline;
}
```

### 13. Missing Failure Types
**Add to tests array**:

```python
additional_http_failures = [
    "413",  # Payload Too Large
    "416",  # Range Not Satisfiable (for byte-range requests)
    "429",  # Too Many Requests (exists but needs rate-limit simulation)
    "502",  # Bad Gateway
    "503",  # Service Unavailable (with Retry-After header)
    "504",  # Gateway Timeout
]

# For each:
for status_code in additional_http_failures:
    tests.append(make_http_test(kind, status_code, instant_schedule, base_mode))
```

**Network-level failures**:
```python
network_failures = [
    "ssl_handshake_failure",
    "certificate_invalid",
    "redirect_loop",
    "redirect_to_invalid_url",
    "tls_version_mismatch",
]
```

### 14. Pattern-Based Failures
**Leverage the shaping pattern system for failure patterns**:

```python
# Server-side implementation
failure_pattern = {
    "steps": [
        {"type": "404", "duration_seconds": 5, "frequency": 2},
        {"type": "timeout", "duration_seconds": 3, "frequency": 1},
        {"type": "none", "duration_seconds": 10},
    ],
    "repeat": True,  # Loop the pattern
}
```

```javascript
// UI for pattern creation
function renderFailurePattern(pattern) {
    return `
        <div class="failure-pattern-editor">
            <h4>Failure Pattern</h4>
            <div class="pattern-steps">
                ${pattern.steps.map((step, i) => `
                    <div class="pattern-step">
                        <select data-step="${i}" data-field="type">
                            <option value="none">No Failure</option>
                            <option value="404">404 Not Found</option>
                            <option value="timeout">Timeout</option>
                            <option value="corrupted">Corrupted</option>
                        </select>
                        <input type="number" data-step="${i}" data-field="duration"
                               value="${step.duration_seconds}" min="1" max="300">
                        <label>seconds</label>
                    </div>
                `).join('')}
            </div>
            <button onclick="addPatternStep()">Add Step</button>
        </div>
    `;
}
```

## Priority Recommendations

### High Priority (Implement First)
1. **Partial content delivery** (#2) - Common real-world scenario
2. **Slow transfer testing** (#3) - Critical for buffer exhaustion testing
3. **Malformed content** (#5) - Edge cases players must handle
4. **Failure combinations** (#7) - Realistic failure scenarios

### Medium Priority
5. **Enhanced recovery metrics** (#8) - Better validation
6. **Network jitter** (#4) - Real network conditions
7. **Test organization** (#9) - Maintainability
8. **Missing failure types** (#13) - Complete coverage

### Low Priority (Nice-to-Have)
9. **DASH-specific scenarios** (#1) - If DASH is important
10. **Web UI enhancements** (#12) - UX improvements
11. **Pattern-based failures** (#14) - Advanced scenarios
12. **Variant selection improvements** (#11) - Test flexibility

## Implementation Approach

### Phase 1: Quick Wins (1-2 days)
- Add missing HTTP status codes (413, 416, 502, 503, 504)
- Implement partial content delivery failures
- Add slow transfer rate testing

**Files to modify**:
- `go-proxy/cmd/server/main.go` - Add new failure types
- `tests/hls_failure_probe.py` - Add test cases
- Update failure type enums/constants

### Phase 2: Core Functionality (3-5 days)
- Malformed content test cases
- Enhanced recovery metrics
- Failure combination framework
- Network jitter testing

**Files to modify**:
- `go-proxy/cmd/server/main.go` - Jitter/malformed content handlers
- `tests/hls_failure_probe.py` - Combination test framework
- Add new metrics collection module

### Phase 3: Advanced Features (Ongoing)
- Pattern-based failures
- DASH-specific scenarios
- Web UI enhancements
- Comprehensive reporting

**New files to create**:
- `tests/metrics_collector.py` - Detailed metrics
- `tests/test_report_generator.py` - HTML reports
- `tests/preset_scenarios.json` - Predefined test scenarios

## Testing the Tests

### Validation Strategy
For each new test type, validate that:
1. The failure is actually triggered (check server logs)
2. The failure is detected by the test (counters increment)
3. The player behaves as expected (recovery or graceful degradation)
4. The test passes when conditions are met
5. The test fails when conditions are not met

### Example Validation Test
```python
def test_partial_delivery_validation():
    """Ensure partial delivery tests actually deliver partial content"""
    # Enable partial_50 failure
    payload = base_failure_payload()
    payload["segment_failure_type"] = "partial_50"
    apply_failure_settings(api_base, session_id, payload)

    # Fetch a segment
    status, data, dt, err = http_fetch(segment_url)

    # Validate it's actually partial
    assert status == 206 or len(data) < expected_full_size
    assert len(data) > 0  # Not completely empty

    # Validate player detected the issue
    time.sleep(5)
    snapshot = fetch_session_snapshot(api_base, session_id)
    assert snapshot["fault_count_partial_50"] > 0
```

## Coverage Matrix

| Category | Current Coverage | Gaps | Priority |
|----------|-----------------|------|----------|
| HTTP Status Codes | 7/15 codes | 413, 416, 502, 503, 504, 301, 307 | High |
| Socket Failures | Complete | None | N/A |
| Transport Faults | Basic | Jitter, burst loss | Medium |
| Content Corruption | Zero-fill only | Syntax errors, partial | High |
| Recovery Testing | Basic pass/fail | Metrics, timing | Medium |
| Player Behavior | Minimal | ABR, seeking, live edge | Low |
| Multi-failure | None | Combinations | High |
| Reporting | Text logs | HTML, metrics, graphs | Medium |

## Success Metrics

Track these metrics to measure improvement:
- **Test coverage**: % of realistic failure scenarios covered
- **Bug detection rate**: # of player bugs found per 100 tests
- **False positive rate**: % of tests that fail incorrectly
- **Execution time**: Total time to run full suite
- **Maintenance burden**: Time to add new test type
