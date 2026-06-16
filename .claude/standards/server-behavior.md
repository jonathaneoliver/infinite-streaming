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

**Test:** `tests/server_behavior/server_limit_test.go::TestServerLimit`

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

**Test:** `tests/server_behavior/server_delay_test.go::TestServerDelay`

**Methodology:** Sweep configured `nftables_delay_ms`; at each step keep
the connection warm with one segment pull per second and average the two
RTT signals the proxy already publishes per session — `client_path_ping_rtt_ms`
(ICMP path-ping, the clean signal) and `client_rtt_ms` (TCP_INFO smoothed
RTT). Baseline measured at delay=0; each higher delay reported as its
increase over baseline.

**Contract:** path-ping RTT ≈ baseline + configured delay (within a few
ms of noise). TCP RTT may inflate further under load (bufferbloat).

**Calibration data (2026-05-27, test-dev, HTTPS):**

| Configured | path-ping obs | tcp obs | delta vs base | accuracy |
|---|---|---|---|---|
| 0 ms   | 0.48 ms   | 3.40 ms   | —       | —    |
| 50 ms  | 49.31 ms  | 44.70 ms  | +48.83  | 98%  |
| 100 ms | 105.00 ms | 98.92 ms  | +104.52 | 100% |
| 250 ms | 257.94 ms | 208.30 ms | +257.46 | 100% |
| 500 ms | 474.70 ms | 510.77 ms | +474.22 | 95%  |

**Findings:** path-ping RTT tracks the configured delay to within a few ms
(95–100%) across the full range — it's the clean signal. TCP_INFO RTT is
noisier (it smooths over retransmits / queueing) and drifts a few % either
way; use path-ping for delay verification, TCP RTT for bufferbloat trends.

---

## 1.3 Loss accuracy (#520)

**Test:** `tests/server_behavior/server_loss_test.go::TestServerLoss`

**Methodology:** Pin a fixed rate cap so the ceiling is constant, sweep
`nftables_packet_loss`, pull segments for a window at each step. Packet
loss isn't directly observable from HTTP; its effect (retransmits +
congestion-window collapse → lower goodput) is. Report observed avg
throughput vs the 0%-loss baseline.

**Contract:** 0% → ~95% of cap (matches §1); throughput degrades
non-linearly as loss climbs; ~10% loss → severe degradation (TCP
slow-start dominates).

**Calibration data (2026-05-27, test-dev, HTTPS; rate cap 50 Mbps):**

| Configured loss | obs avg Mbps | % of 0%-loss baseline |
|---|---|---|
| 0%  | 47.59 | 100% |
| 1%  | 47.50 | 100% |
| 5%  | 18.04 | 38%  |
| 10% | 5.04  | 11%  |

**Findings:** 1% loss is nearly free (TCP fast-retransmit absorbs it) — still
~100% of the 50 Mbps cap. The cliff is between 1% and 5%: at 5% goodput
collapses to ~38%, and 10% loss is catastrophic (~11%) as TCP spends most of
its time in slow-start recovery. The degradation is steeply non-linear, as
expected for loss-driven congestion-window collapse.

---

## 1.4 Pattern fidelity (#521)

**Test:** `tests/server_behavior/server_pattern_test.go::TestServerPattern`

**Methodology:** Install a built-in pattern **template** (square_wave /
ramp_up / ramp_down / pyramid / transient_shock) via `POST /api/nftables/pattern/{port}`, sweep
the per-step duration (6s / 12s / 24s), pull segments continuously, and bucket
each segment by the engine's **live** step (`nftables_pattern_step` /
`nftables_pattern_rate_runtime_mbps`) — bucketing by the engine's own step
index, not wall-clock, makes the comparison timing-robust. Target rates are
the variant-ladder bandwidths offset by the measured **delivery factor** from
§1 (replacing the old hardwired ~5% margin). The step-1 entry transition is
excluded (the kernel hasn't ramped from the prior cap yet).

**Contract:** each settled step's delivered throughput tracks its
delivery-factor-adjusted target; fidelity improves with longer step durations
(more settled samples per step); runtime step index advances monotonically.

**Calibration data (2026-05-27, test-dev, HTTPS): pyramid template, all PASS.**

- Measured **delivery factor = 0.954** (50 Mbps cap → 47.71 Mbps observed),
  used to offset every step's target — consistent with the ~95% from §1.
- Variant ladder targeted (Mbps): 1.0 / 1.84 / 3.46 / 7.06 / 15.36 / 29.86.
- Pyramid sweep PASS at all three step durations: **6s, 12s, 24s**. Fidelity
  scales with step length — longer steps give the kernel more settled time and
  more samples per bucket, tightening the per-step error band.
- **Entry transition (step 1) is not throttled to target**: it overshoots
  toward the previous/baseline cap before the pattern engages — observed
  delivered 5.90 (6s), 10.02 (12s), 13.62 (24s) against a 1.0 Mbps target.
  This is expected (the cut-in happens mid-segment) and is excluded from
  assertions; it's the reason the test skips step 1.

---

## 1.5 Fault injection (#522)

**Test:** `tests/server_behavior/server_fault_test.go::TestServerFault`

**Methodology:** Arm the count-based fault engine (consecutive=1,
frequency=N, units=requests) per kind via `PATCH /api/session/{id}`, pull
`>>N` requests of that kind, count failures by status. The engine is
deterministic — 1 fail per N requests of that kind — so observed ≈ K/N.
A second (un-faulted) kind is sampled to prove cross-kind isolation; the
`all` rule is verified to fault every kind at once.

A `type_coverage` subtest separately arms every status-returning failure
type (named: timeout→504, connection_refused→503, dns_failure→502,
rate_limiting→429; plus the generic numeric path: 404/403/500/502/503/429)
and asserts each fires ≥1 with its mapped status — catching a broken
type→status switch independently of the frequency math. (corrupted and the
socket-phase types are covered by `server_content` / `server_socket`.)

**Contract:** frequency=N → ~1-in-N failures with the configured status;
no cross-kind leak; `all_failure_*` overrides per-kind rules; every failure
type produces its mapped observable.

**Calibration data (2026-05-27, test-dev, HTTPS): all PASS.**

Frequency fidelity + cross-kind isolation (status 503, freq=10):

| Kind | rate | samples | failures | ~C·K/N | cross-kind leak |
|---|---|---|---|---|---|
| segment         | 1-in-10 | 120 | 12 | 12 | 0 |
| manifest        | 1-in-10 | 120 | 12 | 12 | 0 |
| master_manifest | 1-in-10 | 120 | 12 | 12 | 0 |
| segment         | 2-in-10 | 120 | 24 | 24 | 0 |
| manifest        | 2-in-10 | 120 | 24 | 24 | 0 |
| master_manifest | 2-in-10 | 120 | 24 | 24 | 0 |
| all             | 1-in-10 | 120 | 12 per kind | 12 | n/a (faults all) |

Failure-type coverage (each armed at consec=5, 8 samples; ≥1 of the mapped
status required):

| Type | mapped status | hits / 8 |
|---|---|---|
| timeout            | 504 | 7 |
| connection_refused | 503 | 7 |
| dns_failure        | 502 | 6 |
| rate_limiting      | 429 | 7 |
| 404 (numeric)      | 404 | 7 |
| 403 (numeric)      | 403 | 6 |
| 500 (numeric)      | 500 | 7 |
| 502 (numeric)      | 502 | 7 |
| 503 (numeric)      | 503 | 6 |
| 429 (numeric)      | 429 | 7 |

The deterministic count engine hits its target exactly (12 / 24 over 120) with
zero cross-kind leakage, and every failure type maps to its expected status.

---

## 1.6 Transport faults (#523)

**Test:** `tests/server_behavior/transport_fault_test.go::TestTransportFaults`

**⚠️ A fresh TCP connect is the WRONG signal here — it's masked by Docker.**
On test-dev (Docker Compose) the published session port is fronted by
`docker-proxy`, which completes the client handshake on the *host* and then
opens a separate connection into the container. The nftables rule
(`tcp dport <port> counter drop|reject`, **no `ct state new`** — it matches
ALL packets, not just SYNs) lives on the *container* leg. So a brand-new
outside-in connect **succeeds** at `docker-proxy` even with the fault fully
armed; the failure only manifests once bytes must traverse the container leg
(the actual request/response). The original `net.DialTimeout`-only test read
this as a false negative ("drop didn't work") — it did work; the probe was at
the wrong layer.

**Methodology (characterization, three dimensions):** arm the fault held
(on-seconds via `transport_consecutive_failures`=0 takes the hold path), then
record (1) **fresh connect** (`net.DialTimeout` — the masked signal, for
contrast), (2) **data level** — an HTTP GET through the proxy with the fault
held, classified as stall / reset / complete, and (3) **kernel packets** —
the delta on `transport_fault_drop_packets` / `_reject_packets`, read off
`/api/sessions` from the live nftables counters (the un-maskable ground
truth).

**Contract (what each fault actually produces):**
- `drop` and `reject` are **kernel** nftables faults, mutually exclusive
  (one `transport_fault_type` per session).
  - **drop:** packets silently dropped on the container leg → an in-flight /
    new request **STALLS** with no response (client times out). Fresh
    connect to `docker-proxy` still shows "connected".
  - **reject:** RST on the container leg → the request is **CUT** (connection
    reset, sub-second). Fresh connect still shows "connected".
- `hang` is **not** a transport fault — it's the HTTP-layer
  `request_connect_hang` rule (armed via the `all` failure type). Same
  client-visible stall as drop, but **zero kernel packets** — the tell that
  it's an application-layer fault, not a kernel rule.

**Calibration data (2026-05-27, test-dev, HTTPS): all PASS.**

| Fault | fresh connect | data-level effect | kernel pkts | active |
|---|---|---|---|---|
| drop   | connected (masked) | STALLED — no response, timed out 8s | drop +28   | true  |
| reject | connected (masked) | CUT before response — RST/EOF, ~0.1s | reject +10 | true  |
| hang   | connected          | STALLED — no response, timed out 8s | 0 / 0      | false |

**Findings:** both kernel faults work — the drop/reject packet counters are
the proof (the rule matched 28 / 10 packets during the probe). drop and hang
look identical to the client (a stall) but are distinguishable server-side by
the kernel-packet column. reject is the only one that surfaces a fast,
unambiguous client-side error. The RST sometimes surfaces as EOF rather than
"connection reset" through the TLS layer (same quirk as §1.8) — the kernel
counter, not the client errno, is authoritative.

---

## 1.7 Transfer timeouts (#524)

**Test:** `tests/server_behavior/server_transfer_test.go::TestServerTransfer`

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

**Calibration data (2026-05-27, test-dev, HTTPS; 10 Mbps cap, ~21.4 MB segment → ~18s full pull): all PASS.**

| Guard | configured | observed cut | partial body | counter ticked |
|---|---|---|---|---|
| active              | 3s | cut after 4.3s        | 5.17 MB (partial) | 0→1 |
| idle                | 2s | cut ~7.6s after quiet | n/a               | 0→1 |
| active (scoped off) | 3s | completes (ran 13.7s) | 16.35 MB (full)   | n/a |

**Findings:** active timeout fires a beat past its nominal deadline (4.3s vs
3s — the proxy checks on write boundaries) leaving a partial body; idle
timeout fires after the client stalls; both tick their counter. With
`applies_segments=false` the same slow segment runs to **completion** — the
scope flag genuinely gates enforcement. Idle cut latency (~7.6s) runs longer
than the nominal 2s because TCP backpressure has to drain the proxy's in-flight
write buffer before the idle clock starts counting.

---

## 1.8 Socket-phase faults (#523)

**Test:** `tests/server_behavior/server_socket_test.go::TestServerSocketFaults`

**Methodology:** Each of the nine socket faults is armed as a segment fault;
a raw HTTP GET with a bounded deadline observes what the client saw. Two
orthogonal axes are asserted — PHASE (did headers / body bytes arrive?) and
TERMINATOR (how the socket died). Two environment realities shape the
measurement:

- The count-based engine has **no "every request" setting** — `freq=1` is
  1-in-2 (the recovery window consumes a count). So the probe **retries**
  until a fault actually fires (a complete fetch = no fault landed).
- An RST surfaces as **EOF / "unexpected EOF"** through the TLS layer here,
  not "connection reset" — so reset-vs-FIN can't be told apart by errno.
  The terminator is classified by **timing**: reset ≈ immediate, delayed ≈
  `socketDelayDuration` (12s), hang = the probe deadline (20s) firing.

**Contract:** PHASE — connect_* deliver no status line; first_byte_* deliver
200 + chunked headers then 0 body; body_* deliver headers + ~64KB real
bytes. TERMINATOR — reset/delayed/hang distinguishable as above.

**Calibration data (2026-05-27, test-dev, HTTPS): all 9 PASS.**

| Fault | phase observed | terminator | body bytes | elapsed |
|---|---|---|---|---|
| connect_reset      | no headers          | reset   | 0     | ~0s  |
| connect_hang       | no headers          | hang    | 0     | 20s  |
| connect_delayed    | no headers          | delayed | 0     | 12s  |
| first_byte_reset   | headers, 0 body     | reset   | 0     | ~0s  |
| first_byte_hang    | headers, 0 body     | hang    | 0     | 20s  |
| first_byte_delayed | headers, 0 body     | delayed | 0     | 12s  |
| body_reset         | headers + bytes     | reset   | 65536 | ~0s  |
| body_hang          | headers + bytes     | hang    | 65536 | 20s  |
| body_delayed       | headers + bytes     | delayed | 65536 | 12s  |

Every fault's per-type counter (`fault_count_request_*`) ticked exactly once
per fired probe — confirms the proxy applied the specific fault, not a generic
fallback. Constants: `socketDelayDuration=12s`, `socketHangDuration=90s`,
`socketMidBodyBytes=64KB` (verify in source — they can drift). Full wire
contract: `.claude/standards/fault-injection-wire-contract.md`.

---

## 1.9 Fault scope checkboxes (#522)

**Test:** `tests/server_behavior/server_scope_test.go::TestServerScope`

**Methodology:** Arm a segment fault scoped to ONE variant's directory token
(`segment_failure_urls=[<dir>]`), then pull segments from the in-scope variant
(faults must appear) and an out-of-scope variant (must stay clean). The proxy
advances the fault cycle ONLY for in-scope requests (`handleSegmentFailure`
returns "none" before `HandleFailure` on a URL miss), so the out-of-scope
variant is clean by construction — this verifies that wiring end to end.

**Contract:** `*_failure_urls` scopes a fault to matching variants/URLs;
non-matching requests are never faulted, even of the same kind.

**Calibration data (2026-05-27, test-dev, HTTPS): PASS.**

| Variant | scope | rate | samples | faults |
|---|---|---|---|---|
| 2160p (top)    | in-scope     | 1-in-3 | 30 | 10 |
| 360p (bottom)  | out-of-scope | n/a    | 30 | 0  |

Sibling scope axes: request-KIND scope (segment/manifest/master) → §1.5
`server_fault`; transfer-timeout `applies_segments` scope → §1.7
`server_transfer`.

---

## 1.10 Content manipulation (#525 / P2)

**Test:** `tests/server_behavior/server_content_test.go::TestServerContent`

**Methodology:** "Just fetch with and without things" — for each manipulation
control, fetch the affected resource OFF (baseline) then ON, and assert the
served bytes differ in the expected way. Combined subtests enable two master
manipulations at once to prove the controls compose (one edit doesn't clobber
another through the re-encode).

**Contract:** `content_strip_codecs` removes CODECS; `content_strip_average_bandwidth`
removes AVERAGE-BANDWIDTH; `content_overstate_bandwidth` inflates BANDWIDTH +
AVERAGE-BANDWIDTH by 10%; segment `corrupted` zero-fills the body at the same
length. Combinations apply all their edits to one served playlist.

**Calibration data (2026-05-27, test-dev, HTTPS): all PASS.**

| Control | observed |
|---|---|
| master/strip_codecs            | 1169→1049 B; CODECS removed |
| master/strip_average_bandwidth | 1169→1018 B; AVERAGE-BANDWIDTH removed |
| master/overstate_bandwidth     | 1169→1176 B; BANDWIDTH inflated |
| master/combo:codecs+avg_bandwidth       | 1169→892 B; CODECS **and** AVERAGE-BANDWIDTH both gone |
| master/combo:codecs+overstate_bandwidth | 1169→1050 B; CODECS gone **and** peak BANDWIDTH 29,857,251→32,842,976 (×1.10) |
| segment/corrupted              | 14,251,514 B → 14,251,514 B, body zero-filled (same length) |

**Findings:** controls compose cleanly — the combined subtests prove a
re-encode applying one manipulation doesn't drop another's edit. overstate
multiplies both BANDWIDTH and AVERAGE-BANDWIDTH by exactly 1.10. corruption
preserves Content-Length (honest header) while zeroing every body byte, so the
failure surfaces as a decode error, not a truncated transfer.

---

## 1.11 Config-on-connect (#712)

`sb_config_on_connect_test.go` — `TestConfigOnConnect_Shape` /
`TestConfigOnConnect_FaultRule`.

Instead of bootstrap → PATCH, the bootstrap manifest URL itself carries the
session config as `proxy.*` args. The base-port handler materializes the config
atomically, then 302s to the session port with the args **stripped**:

```
GET …:30081/go-live/<content>/master_6s.m3u8
      ?player_id=<uuid>&proxy.shape.rate_mbps=2.5&proxy.labels.test=cfg712
  → 302 …:301N1/go-live/<content>/master_6s.m3u8?player_id=<uuid>   # proxy.* gone
```

The tests assert (a) the redirected URL carries no `proxy.*`, (b) the session
record on `/api/sessions` already reflects the config (`nftables_bandwidth_mbps`,
`segment_failure_type`, …) **before any PATCH**. Full vocabulary + tier
precedence + the "segment length is the filename, not an arg" rule live in
[`api/openapi/v2/DESIGN.md` § 5b](../../api/openapi/v2/DESIGN.md). Config-on-
connect rides the **same** v2→v1 translator as PATCH, so it accepts exactly the
PATCH-supported field set (no `filter.variant`/`.codec` yet) and rejects the
rest with 400.

---

## 2. Future server-behavior tests (proposed)

Each row below is an as-yet-unwritten test in `tests/server_behavior/`.
Priority bands reflect what's most likely to silently regress without
this kind of calibration coverage.

### P0 — controls that DEFINE the server's value proposition

**Status: all six P0 tests are implemented (#519–#524) and calibrated
against test-dev (2026-05-27) — see the filled tables in §1.2–1.7.** P2
content manipulation (§1.10), plus socket-phase faults (§1.8) and fault
scope (§1.9), are also implemented and calibrated.

| Test file | Control surface | Contract being verified |
|---|---|---|
| `server_delay_test.go` ✅ | `nftables_delay_ms` | Configured delay matches path-ping RTT (within ~5ms noise). Sweep 0/50/100/250/500ms. |
| `server_loss_test.go` ✅ | `nftables_packet_loss` | Configured loss % matches observed throughput drop. Sweep 0/1/5/10%. |
| `server_pattern_test.go` ✅ | `nftables_pattern_*` | Each step's nominal rate is enforced at the correct time + duration. |
| `server_fault_test.go` ✅ | per-kind HTTP fault rules (`segment_failure_*`, `manifest_failure_*`, `master_manifest_failure_*`, `all_failure_*`) | At frequency=N requests, ~1/N return the configured status; cross-kind isolation; `all` override; every failure type produces its expected observable. |
| `transport_fault_test.go` ✅ | `transport_fault_type` ∈ {drop, reject} (kernel) + `hang` (HTTP-layer) | Characterized per §1.6: drop=data stall + kernel drop pkts, reject=RST/cut + kernel reject pkts, hang=stall w/ 0 kernel pkts. Fresh connect is masked by docker-proxy. |
| `server_transfer_test.go` ✅ | `transfer_active_timeout_seconds`, `transfer_idle_timeout_seconds`, `transfer_timeout_applies_*` | In-flight request killed at the configured wall-clock deadline; `applies_*` flags scope which kinds. |

### P1 — composition + lifecycle

| Test file (proposed) | Concern |
|---|---|
| `concurrent_session_isolation_test.go` | Two sessions at different caps; verify each enforces its own without bleed (htb classes truly isolated). |
| `restart_persistence_test.go` | Set non-default caps on N sessions, restart proxy, verify sessions still capped (the `restoreShapeApplication` regression bait). |
| `group_propagation_test.go` | Set shape on player A; link B to A's group; verify B inherits live shape changes (per docs / FaultRules group semantics). |
| `baseline_visibility_test.go` | `INFINITE_STREAM_DEFAULT_RATE_MBPS=N` → `/api/v2/info.default_rate_mbps == N`; every new session inherits it; `effective_rate_limit_mbps` matches kernel state. |
| `session_limit_test.go` | Allocate `INFINITE_STREAM_MAX_SESSIONS+1` sessions; verify the last one is rejected (503 or equivalent). |

### P2 — quirks + edge cases

| Test file | Concern |
|---|---|
| `rate_cap_extremes_test.go` (proposed) | Sub-1 Mbps caps (0.1, 0.5) and supra-100 Mbps (200, 500, 1000) — find breakdown thresholds. |
| `server_content_test.go` ✅ | Content manipulations (strip codecs / avg-bandwidth, overstate bandwidth, segment corruption) + combined; verify served bytes differ as expected. See §1.10. |
| `server_socket_test.go` ✅ | Nine socket-phase faults (connect/first_byte/body × reset/hang/delayed); verify client-observed wire shape per the contract. See §1.8. |
| `server_scope_test.go` ✅ | Fault scope checkboxes — fault scoped to one variant fires there and nowhere else. See §1.9. |
| `clear_semantics_test.go` (proposed) | Verify `--rate 0`, `--clear`, and `--rate=baseline` all map to identical kernel state (no override). |

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

go test -v -run TestServerDelay        -timeout 5m   # DELAY_SWEEP_MS, DELAY_SETTLE_S
go test -v -run TestServerLoss         -timeout 5m   # LOSS_SWEEP_PCT, LOSS_RATE_CAP, LOSS_DURATION_S
go test -v -run TestServerPattern      -timeout 15m  # PATTERN_STEP_DURATIONS (6,12,24)
go test -v -run TestServerFault        -timeout 10m  # FAULT_FREQUENCY, FAULT_SAMPLES, FAULT_TYPE, FAULT_CONSECUTIVE, FAULT_TYPE_SAMPLES
go test -v -run TestTransportFaults    -timeout 5m   # TRANSPORT_PROBE_TIMEOUT_S, TRANSPORT_DATA_TIMEOUT_S, TRANSPORT_ARM_WINDOW_S
go test -v -run TestServerTransfer     -timeout 5m   # TIMEOUT_ACTIVE_S, TIMEOUT_IDLE_S
go test -v -run TestServerSocketFaults -timeout 10m  # SOCKET_PROBE_TIMEOUT_S, SOCKET_FAULT_ATTEMPTS
go test -v -run TestServerScope        -timeout 5m   # SCOPE_FREQUENCY, SCOPE_SAMPLES, FAULT_TYPE
go test -v -run TestServerContent      -timeout 5m
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
   `server_limit_test.go`. Helps the operator + LLM tools
   cite the data uniformly.
4. **Honour `testing.Short()`** — skip the long sweep in CI fast
   passes. Real CI run is explicit (`go test -run TestX -timeout 10m`).
5. **Post heartbeats** if the test takes more than a few seconds so
   the run is visible in the dashboard.
6. **Update Section 1 here** with the latest calibration numbers
   whenever you re-run + commit a baseline shift.
