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

**Test:** `tests/server_behavior/throughput_calibration_test.go::TestServerLimit`

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

## 1.2 Delay accuracy (#519)

**Test:** `tests/server_behavior/delay_accuracy_test.go::TestServerDelay`

**Methodology:** Sweep configured `nftables_delay_ms`; at each step keep
the connection warm with one segment pull per second and average the two
RTT signals the proxy already publishes per session — `client_path_ping_rtt_ms`
(ICMP path-ping, the clean signal) and `client_rtt_ms` (TCP_INFO smoothed
RTT). Baseline measured at delay=0; each higher delay reported as its
increase over baseline.

**Contract:** path-ping RTT ≈ baseline + configured delay (within a few
ms of noise). TCP RTT may inflate further under load (bufferbloat).

**Calibration data:** _TBD — populate from first run on test-dev._

| Configured | path-ping obs | tcp obs | delta vs base | accuracy |
|---|---|---|---|---|
| 0 ms   | — | — | — | — |
| 50 ms  | — | — | — | — |
| 100 ms | — | — | — | — |
| 250 ms | — | — | — | — |
| 500 ms | — | — | — | — |

---

## 1.3 Loss accuracy (#520)

**Test:** `tests/server_behavior/loss_accuracy_test.go::TestServerLoss`

**Methodology:** Pin a fixed rate cap so the ceiling is constant, sweep
`nftables_packet_loss`, pull segments for a window at each step. Packet
loss isn't directly observable from HTTP; its effect (retransmits +
congestion-window collapse → lower goodput) is. Report observed avg
throughput vs the 0%-loss baseline.

**Contract:** 0% → ~95% of cap (matches §1); throughput degrades
non-linearly as loss climbs; ~10% loss → severe degradation (TCP
slow-start dominates).

**Calibration data:** _TBD — populate from first run on test-dev._

| Configured loss | obs avg Mbps | % of 0%-loss baseline |
|---|---|---|
| 0%  | — | 100% |
| 1%  | — | — |
| 5%  | — | — |
| 10% | — | — |

---

## 1.4 Pattern fidelity (#521)

**Test:** `tests/server_behavior/pattern_fidelity_test.go::TestServerPattern`

**Methodology:** Install a deterministic multi-step pattern (default
`30,5,30` Mbps, 8s/step) via `POST /api/nftables/pattern/{port}`, pull
segments continuously, bucket each segment's throughput by the step
window its fetch started in. Cross-check the engine's runtime fields
(`nftables_pattern_step`, `nftables_pattern_rate_runtime_mbps`) advance.

**Contract:** each step's observed avg tracks its nominal rate within the
~95% band from §1; runtime step index advances monotonically.

**Calibration data:** _TBD — populate from first run on test-dev._

| Step | nominal Mbps | obs avg Mbps | accuracy |
|---|---|---|---|
| 1 | — | — | — |
| 2 | — | — | — |
| 3 | — | — | — |

---

## 1.5 Fault injection (#522)

**Test:** `tests/server_behavior/fault_injection_test.go::TestServerFault`

**Methodology:** Arm the count-based fault engine (consecutive=1,
frequency=N, units=requests) per kind via `PATCH /api/session/{id}`, pull
`>>N` requests of that kind, count failures by status. The engine is
deterministic — 1 fail per N requests of that kind — so observed ≈ K/N.
A second (un-faulted) kind is sampled to prove cross-kind isolation; the
`all` rule is verified to fault every kind at once.

**Contract:** frequency=N → ~1-in-N failures with the configured status;
no cross-kind leak; `all_failure_*` overrides per-kind rules.

**Calibration data:** _TBD — populate from first run on test-dev._

| Kind | status | freq | samples | failures | ~1/N | cross-kind leak |
|---|---|---|---|---|---|---|
| segment         | 503 | 10 | — | — | — | 0 |
| manifest        | 503 | 10 | — | — | — | 0 |
| master_manifest | 503 | 10 | — | — | — | 0 |
| all             | 503 | 10 | — | — | — | n/a |

---

## 1.6 Transport faults (#523)

**Test:** `tests/server_behavior/transport_fault_test.go::TestTransportFaults`

**Methodology:** Arm each fault, then a raw `net.DialTimeout` probe (not
http.Client) classifies the TCP-layer outcome. Baseline connect must
succeed first or the result is meaningless. Always-on is expressed as
`transport_consecutive_failures`=3600 (on-seconds) / `transport_failure_frequency`=0
(off-seconds).

**Contract + cadence notes:**
- `drop` and `reject` are **kernel** nftables faults and are mutually
  exclusive (proxy stores one `transport_fault_type` per session).
  - **drop:** SYN silently dropped → TCP connect **times out**.
  - **reject:** SYN gets an RST → connect fails fast with **connection
    refused**.
- `hang` is **not** a transport fault — it's the HTTP-layer
  `request_connect_hang` rule (armed via the `all` failure type). TCP
  connect still **succeeds**; the HTTP request stalls with no response.
  The test asserts that split (TCP connected / HTTP stalled).
- Cadence: on-seconds (`transport_consecutive_failures`) / off-seconds
  (`transport_failure_frequency`) gate when the kernel rule is live;
  on=3600/off=0 keeps it always-on for a test window.

**Calibration data:** _TBD — confirm each classification on first run._

| Fault | layer | expected probe outcome |
|---|---|---|
| drop   | kernel | connect timeout |
| reject | kernel | connection refused / reset |
| hang   | http   | TCP connect OK, HTTP stalls |

---

## 1.7 Transfer timeouts (#524)

**Test:** `tests/server_behavior/transfer_timeout_test.go::TestServerTransfer`

**Methodology:** Rate cap is derived from the discovered segment size so a
full segment pull takes ~5× the active timeout (otherwise the segment
finishes before the guard fires and the test is vacuous). Active timeout:
read a slow segment to completion-or-cut. Idle timeout: read one chunk,
stop reading so TCP backpressure stalls the proxy's writes, then resume.
Counters read off the session record before/after.

**Contract + per-path scoping notes:**
- Active timeout bounds total response duration; fires within a few
  seconds of the deadline, leaving a partial body.
- Idle timeout bounds time since the server's last write; fires ~idle
  seconds after the client goes quiet.
- `transfer_timeout_applies_segments` / `_manifests` / `_master` gate
  which request kinds are subject. Verified directly: with
  `applies_segments=false` the same slow segment runs to **completion**
  (guard gated off).
- Counters `fault_count_transfer_active_timeout` /
  `fault_count_transfer_idle_timeout` increment on each fire.

**Calibration data:** _TBD — confirm cut timing + counter ticks on first run._

| Guard | configured | observed cut | partial body | counter ticked |
|---|---|---|---|---|
| active           | 3s | — | — | — |
| idle             | 2s | — | — | — |
| active (scoped off) | 3s | completes (not cut) | n/a | n/a |

---

## 2. Future server-behavior tests (proposed)

Each row below is an as-yet-unwritten test in `tests/server_behavior/`.
Priority bands reflect what's most likely to silently regress without
this kind of calibration coverage.

### P0 — controls that DEFINE the server's value proposition

**Status: all six P0 tests are implemented (#519–#524).** Calibration
tables in §1.2–1.7 above are placeholders pending the first run against a
live deployment.

| Test file | Control surface | Contract being verified |
|---|---|---|
| `delay_accuracy_test.go` ✅ | `nftables_delay_ms` | Configured delay matches path-ping RTT (within ~5ms noise). Sweep 0/50/100/250/500ms. |
| `loss_accuracy_test.go` ✅ | `nftables_packet_loss` | Configured loss % matches observed throughput drop. Sweep 0/1/5/10%. |
| `pattern_fidelity_test.go` ✅ | `nftables_pattern_*` | Each step's nominal rate is enforced at the correct time + duration. |
| `fault_injection_test.go` ✅ | per-kind HTTP fault rules (`segment_failure_*`, `manifest_failure_*`, `master_manifest_failure_*`, `all_failure_*`) | At frequency=N requests, ~1/N return the configured status; cross-kind isolation; `all` override. |
| `transport_fault_test.go` ✅ | `transport_fault_type` ∈ {drop, reject} (kernel) + `hang` (HTTP-layer) | drop=SYN timeout, reject=RST, hang=TCP-connects/HTTP-stalls. |
| `transfer_timeout_test.go` ✅ | `transfer_active_timeout_seconds`, `transfer_idle_timeout_seconds`, `transfer_timeout_applies_*` | In-flight request killed at the configured wall-clock deadline; `applies_*` flags scope which kinds. |

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
go test -v -run TestServerLimit -timeout 10m

# Single rate, short:
THROUGHPUT_RATES=50 THROUGHPUT_DURATION_S=10 go test -v -run TestServerLimit -timeout 60s

# Target a different deployment:
THROUGHPUT_HOST=other-host.local THROUGHPUT_API_PORT=31000 \
  go test -v -run TestServerLimit -timeout 10m

# Skip in CI (short mode auto-skips):
go test -short ./...
```

### Sibling P0 tests (#519–#524)

All share the `THROUGHPUT_*` connection env vars (host / api-port /
insecure / content) and skip under `-short`. Each prints a matrix.

```bash
cd tests/server_behavior

go test -v -run TestServerDelay       -timeout 5m   # DELAY_SWEEP_MS, DELAY_SETTLE_S
go test -v -run TestServerLoss        -timeout 5m   # LOSS_SWEEP_PCT, LOSS_RATE_CAP, LOSS_DURATION_S
go test -v -run TestServerPattern  -timeout 10m  # PATTERN_RATES, PATTERN_STEP_S
go test -v -run TestServerFault   -timeout 10m  # FAULT_FREQUENCY, FAULT_SAMPLES, FAULT_STATUS
go test -v -run TestTransportFaults  -timeout 5m   # TRANSPORT_PROBE_TIMEOUT_S, TRANSPORT_ARM_WINDOW_S
go test -v -run TestServerTransfer -timeout 5m   # TIMEOUT_ACTIVE_S, TIMEOUT_IDLE_S
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
