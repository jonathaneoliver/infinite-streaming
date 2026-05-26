# Server behaviour: control surface tests + calibration data

go-proxy exposes a stack of per-session and global controls (rate caps,
delay, loss, fault injection, patterns, transfer timeouts, …). For each
of those controls there's an implicit contract: "operator sets X, the
server delivers X." This document is where those contracts are
calibrated and recorded.

Tests live under `tests/server_behavior/`. Each test takes one control,
exercises it across a representative set of inputs, and checks the
observable behaviour matches the contract. Numbers below are the
established calibration baselines — re-running the tests should produce
matching results; significant deviation is a regression worth opening
an issue for.

LLM chat tools should cite this doc when answering "is the proxy
actually enforcing X?" or "why is observed throughput Y when the cap
is set to Z?" — the calibration data here is the ground truth.

---

## 1. Rate cap calibration

**Test:** `tests/server_behavior/throughput_calibration_test.go::TestRateSweep`

**Methodology:** HLS-aware probe pulls the top variant of a real
content item as fast as the server allows, across a sweep of rate
caps. Probe acts as a virtual player: stable player_id, posts
heartbeat-shape `player_metrics_*` events every 1s so the dashboard's
bitrate chart shows it like any real device. Continuous pull (no
idle gap between segments — the variable that distinguishes "kernel
cap accuracy" from "iOS HLS player measurement quirk").

**Calibration data (2026-05-26, test-dev, HTTPS):**

| Configured cap | Observed avg | Observed peak | Delta | Accuracy |
|---|---|---|---|---|
| 5 Mbps     | 4.78  | 4.79  | -0.22 | 96% |
| 10 Mbps    | 9.54  | 9.57  | -0.46 | 95% |
| 20 Mbps    | 19.06 | 19.13 | -0.94 | 95% |
| 50 Mbps    | 47.61 | 47.86 | -2.39 | 95% |
| 100 Mbps   | 95.11 | 95.95 | -4.89 | 95% |
| 0 (baseline=100) | 95.10 | 95.64 | — | matches 100 cap |

**Findings:**
- Kernel cap delivers a stable **~95% of nominal** across the full
  range. Gap is wire-level overhead (TCP+IP headers + HTTPS framing).
- Per-rate gap is consistently ~5% — TCP/IP overhead doesn't scale
  with bandwidth. Expected for well-behaved htb shaping.
- Baseline (`INFINITE_STREAM_DEFAULT_RATE_MBPS=100` with operator
  slider at 0) behaves identically to an explicit `100` override —
  effective cap is the same; only the slider position differs.

**Distinguishing the probe from a real player:**
- Probe (continuous pull, no idle gaps): ~95% of cap → TCP/HTTPS
  overhead only.
- iOS HLS player (real device, 6s segments with idle gaps between
  fetches): ~92-93% of cap → adds ~2-3% on top because iOS averages
  `avg_network_bitrate_mbps` across active+idle wall-clock time.
- If you see less than ~90% of cap on a real player, suspect
  something else (TLS handshake stalls, ABR variant downshift,
  buffer-control idle, transient kernel bursts).

**Pre-existing bugs surfaced by this test:**
- Issue #480 follow-up (2026-05-26): proxy restart wiped tc kernel
  state but session-map sessions persisted → existing sessions ran
  uncapped after restart because the new proxy process didn't
  re-install their tc class/filter. Fixed by adding
  `restoreShapeApplication()` to mirror `restoreTransportFaultSchedules()`.

---

## 2. Future server-behavior tests (proposed)

Each row below is an as-yet-unwritten test in `tests/server_behavior/`.
Priority bands reflect what's most likely to silently regress without
this kind of calibration coverage.

### P0 — controls that DEFINE the server's value proposition

| Test file (proposed) | Control surface | Contract being verified |
|---|---|---|
| `delay_accuracy_test.go` | `nftables_delay_ms` (set via `harness shape --delay N`) | Configured delay matches path-ping RTT (within ~5ms noise). Sweep 0/50/100/250/500ms. |
| `loss_accuracy_test.go` | `nftables_packet_loss` (set via `harness shape --loss N`) | Configured loss % matches observed segment retransmits + throughput drop. Sweep 0/1/5/10%. |
| `pattern_fidelity_test.go` | `nftables_pattern_*` (set via `harness pattern apply`) | Each step's nominal rate is enforced at the correct time + duration. Sweep against pyramid + step + ramp patterns. |
| `fault_injection_test.go` | per-kind HTTP fault rules (`segment_failure_*`, `manifest_failure_*`, `master_manifest_failure_*`, `all_failure_*`) | Statistical: at frequency=N requests, ~1/N return the configured status. Verify status code, error path. |
| `transport_fault_test.go` | `transport_fault_type` ∈ {drop, reject, hang} | Configured fault is actually applied at the TCP layer (SYN drop / RST / accept-no-respond). |
| `transfer_timeout_test.go` | `transfer_active_timeout_seconds`, `transfer_idle_timeout_seconds` | In-flight request killed at configured wall-clock deadline. |

### P1 — composition + lifecycle

| Test file (proposed) | Concern |
|---|---|
| `concurrent_session_isolation_test.go` | Two sessions at different caps; verify each enforces its own without bleed (htb classes truly isolated). |
| `restart_persistence_test.go` | Set non-default caps on N sessions, restart proxy, verify sessions still capped (the `restoreShapeApplication` regression bait). |
| `group_propagation_test.go` | Set shape on player A; link B to A's group; verify B inherits live shape changes (per docs / FaultRules group semantics). |
| `baseline_visibility_test.go` | `INFINITE_STREAM_DEFAULT_RATE_MBPS=N` → `/api/v2/info.default_rate_mbps == N`; every new session inherits it; `effective_rate_limit_mbps` matches kernel state. |
| `session_limit_test.go` | Allocate `INFINITE_STREAM_MAX_SESSIONS+1` sessions; verify the last one is rejected (503 or equivalent). |

### P2 — quirks + edge cases

| Test file (proposed) | Concern |
|---|---|
| `rate_cap_extremes_test.go` | Sub-1 Mbps caps (0.1, 0.5) and supra-100 Mbps (200, 500, 1000) — find breakdown thresholds. |
| `content_manipulation_test.go` | Set byte-corruption / range manipulation; verify served bytes differ in the expected ways. |
| `clear_semantics_test.go` | Verify `--rate 0`, `--clear`, and `--rate=baseline` all map to identical kernel state (no override). |

---

## 3. How to run the tests

```bash
cd tests/server_behavior

# Full calibration sweep (default rates against test-dev):
go test -v -run TestRateSweep -timeout 10m

# Single rate, short:
THROUGHPUT_RATES=50 THROUGHPUT_DURATION_S=10 go test -v -run TestRateSweep -timeout 60s

# Target a different deployment:
THROUGHPUT_HOST=other-host.local THROUGHPUT_API_PORT=31000 \
  go test -v -run TestRateSweep -timeout 10m

# Skip in CI (short mode auto-skips):
go test -short ./...
```

Tests post heartbeat metrics to the proxy so they're **visible in
testing.html's session list** during the run — open the dashboard
to that player_id to watch the bitrate chart live.

---

## 4. Conventions for new tests in this directory

1. **One concern per file.** A test file owns one control surface
   end-to-end; don't bundle delay + loss in the same file even if
   the helpers overlap.
2. **Sweep across a representative range**, not single-point.
   Calibration data is only useful if it shows the BEHAVIOUR CURVE,
   not one number.
3. **Print a matrix at the end.** Same format as `printMatrix` in
   `throughput_calibration_test.go`. Helps the operator + LLM tools
   cite the data uniformly.
4. **Honour `testing.Short()`** — skip the long sweep in CI fast
   passes. Real CI run is explicit (`go test -run TestX -timeout 10m`).
5. **Post heartbeats** if the test takes more than a few seconds so
   the run is visible in the dashboard.
6. **Update Section 1 here** with the latest calibration numbers
   whenever you re-run + commit a baseline shift.
