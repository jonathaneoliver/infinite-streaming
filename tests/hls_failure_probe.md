# HLS Failure Probe

This test uses the session REST API to inject failures and verify they appear in manifest/segment fetches.

## Usage

```bash
python3 tests/hls_failure_probe.py
```

With an explicit URL:

```bash
python3 tests/hls_failure_probe.py http://lenovo:30081/go-live/<content>/master_6s.m3u8
```

## Auto-selection behavior

If no URL is provided, the script calls `http://lenovo:30000/api/content`, sorts by `name`, and selects the first item that:

- has HLS content
- has a reachable `master_6s.m3u8`
- advertises H264 codecs (avc1/avc3) in the master manifest

The probe always includes a `player_id` query parameter. Use `--player-id` to set it explicitly; if omitted, a random UUID is generated.

## Test flow

By default (`--mode tests`), the script:

1. Creates a session by fetching the manifest with `player_id`.
2. Calls `/api/sessions` to find the matching session.
3. Verifies a short window of successful streaming before each failure test.
4. Applies failure settings via `/api/failure-settings/{session_id}`.
5. For manifest failures, it first fetches the manifest to warm up the session, applies the failure, then fetches the manifest again to verify.
6. Runs a short fetch window and verifies the expected failure is observed.
7. Resets the failure settings and verifies streaming recovers.
8. Repeats for each failure type.

## What it checks

Each test logs what is expected and what was observed. Defaults expect 1 failure per test.

By default, tests run from fastest to slowest. Socket delay tests use a ~12s timeout window, and hang tests wait 24s (they pass if no response arrives in that window).

The suite covers all failure types exposed by the testing session UI, including:

- HTTP failures: 404, 403, 500, timeout, connection_refused, dns_failure, rate_limiting
- Socket faults: request_connect_*, request_first_byte_*, request_body_* (reset/delayed/hang)
- Segment corruption
- Transport drop/reject
- Bandwidth throttling

If any expected count is not met, the script exits with code 2.

## Scheduling and variant scope

Scheduling controls how often failures occur:

- `units=requests` with `consecutive=X` and `frequency=0`: expect X failing requests, then no more.
- `units=requests` with `frequency=10`: expect failures, then at least 10 OK requests before failures repeat.
- `units=seconds`: expect failures for X seconds, then successes.
- `failures/seconds`: expect X failing requests, then X seconds of successes.
- `consecutive=0`: the test treats this as a manual toggle: set failure_type, wait for the first failure, then reset to `none`.

Variant scope controls which variants fail:

- `All`: every variant should fail.
- Subset: selected variants fail while other variants continue to succeed.

The probe treats `master.m3u8` as the one-time **master manifest** fetch and media manifest refreshes (plus all DASH MPD refreshes) as the ongoing **manifest** fetches. It infers variants from the master manifest and verifies that selected variants fail while unselected variants succeed. Full variant/scheduling coverage is only applied to `404`; all other failure types are tested as instantaneous failures (consecutive=0).

## Common flags

```bash
# Override ports or base URLs
python3 tests/hls_failure_probe.py --host lenovo --api-port 30000 --hls-port 30081

# Provide a specific player_id
python3 tests/hls_failure_probe.py --player-id my-debug-session

# Adjust expected counts
python3 tests/hls_failure_probe.py --expect-http 2 --expect-timeouts 2 --expect-resets 2 --expect-drop-reject 2 --expect-throttle 2

# Control corrupted segment expectations
python3 tests/hls_failure_probe.py --expect-corrupted 2

# Randomize order and repeat the full suite
python3 tests/hls_failure_probe.py --shuffle-tests --iterations 3

# Stop after first failure
python3 tests/hls_failure_probe.py --stop-on-failure

# Resume from the previous failing test
python3 tests/hls_failure_probe.py --continue

# Resume from a specific test id
python3 tests/hls_failure_probe.py --continue-from iter2:segment_404_requests_c0_f0_subset

# Store/read the last failed test id in a custom file
python3 tests/hls_failure_probe.py --continue --continue-file /tmp/hls_failure_last_failed

# Control throttle threshold and per-test duration
python3 tests/hls_failure_probe.py --throttle-mbps 1.0 --test-seconds 15

# Run the original probe loop (no auto failure injection)
python3 tests/hls_failure_probe.py --mode probe
```