# Player-discovery coverage ledger

The fusion artifact #772 asks for: one table tying the three catalogues together —
**what we want to know** (the questions in [`matrix/TEST-PLAN.md`](matrix/TEST-PLAN.md)),
**what we can actually run today** (the `modes/*_test.go` drivers + `matrix/*.yaml` arms),
and **what we've already found** (`.claude/findings/`). Each row is one player
behaviour we set out to characterise.

Maintain this by hand when a mode, a matrix arm, or a finding lands. It is the
coverage side of TEST-PLAN.md's question side — read them together.

## Status legend

| Mark | Meaning |
|---|---|
| ✅ **Answered** | A finding answers the question (cited). |
| 🟡 **Runnable** | A mode/YAML can run it today; no finding written up yet. |
| 🟠 **Partial** | Related/approximate coverage only — a mode touches it, or a finding is adjacent, but the specific question isn't closed. |
| 🔴 **Gap** | The hooks exist but nothing runs it and nothing answers it. |
| ⚫ **Missing hook** | Needs an input/observation hook the harness doesn't have yet. |

⚠ = evidence lives on the **unpushed** `feat/sweep-lab` branch (not in `dev`).
Runnable column: `mode:` = a Go driver in `modes/`; `yaml:` = an arm in `matrix/`.

---

## Section 1 — Live-edge latency & offset control

| # | Question | Runnable | Finding | Status | Key hooks |
|---|---|---|---|---|---|
| 1.1 | `offset-response-curve` — where does the server offset clamp? | `yaml: live-offset` | ⚠ `user-live-offset-crossplatform`, `…-honored-band-ios` | ✅ | `proxy.live_offset`, `is.segment` |
| 1.2 | `offset-floor-by-segment` — same offset, opposite outcome by segment | `yaml: live-offset` (+seg) | ⚠ `user-live-offset-crossplatform` (holdback 21/9/6 for s6/s2/s1) | ✅ | `proxy.live_offset`, `is.segment` |
| 1.3 | `offset-precedence` — client vs server, who wins? | `yaml: precedence` | ⚠ `user-live-offset-crossplatform` (app lever wins the playhead both platforms) | ✅ | `proxy.live_offset` + `is.live_offset` |
| 1.4 | `offset-source-parity` — do the two mechanisms reach the same latency? | `yaml: precedence` (adjacent) | ⚠ partial (crossplatform shows server-holdback vs client-override differ) | 🟠 | `proxy.live_offset` vs `is.live_offset` |

## Section 2 — Platform & protocol parity

| # | Question | Runnable | Finding | Status | Key hooks |
|---|---|---|---|---|---|
| 2.1 | `platform-offset-parity` — AVPlayer vs ExoPlayer latency across offsets | `yaml: live-offset` (per platform) | ⚠ `user-live-offset-crossplatform` (iOS ≈N+27 vs Android ≈N on s6; converge on s1); supersedes `live-offset-androidtv-untestable` | ✅ | `proxy.live_offset`, `platform` |
| 2.2 | `protocol-parity` — LL-HLS vs LL-DASH on the same content | — | — | 🔴 | `is.protocol` (hls/dash) |
| 2.3 | `platform-abr-parity` — same ABR call under load, iOS vs Android? | `mode: pyramid/rampdown` (per platform) | — | 🟠 | shape pattern, `platform` |

## Section 3 — ABR under bandwidth shaping

| # | Question | Runnable | Finding | Status | Key hooks |
|---|---|---|---|---|---|
| 3.1 | `shape-pattern-response` — ABR reaction per bandwidth profile | `yaml: shape-patterns`; `mode: pyramid/rampup/rampdown/transient_shock` | — | 🟡 | `proxy.shape.pattern` |
| 3.2 | `startup-bitrate-clamp` — does the startup cap change downshift? | `mode: startup_caps` | `avplayer-startup-variant-selection` (adjacent) | 🟠 | `is.peak_bitrate_mbps` |
| 3.3 | `segment-abr-responsiveness` — segment size vs reaction speed | `yaml: pyramid-seg`, `pyramid-1-s2` vs `-s6` | — | 🟡 | `is.segment`, shape ramp |
| 3.4 | `rate-step-sensitivity` — fastest bandwidth change the player can track | `mode: steps` | — | 🟡 | `proxy.shape.step_seconds/max_step` |
| 3.5 | `pyramid-timing` — same pattern, different timing | `yaml: pyramid-*` (12 arms) | — | 🟡 | `proxy.shape.step_seconds/dip_hold_s` |
| 3.6 | `constant-vs-dynamic-stepping` — uniform vs content-relative vs asymmetric | `yaml: const5-*`, `pyramid-*` | — | 🟡 | `proxy.shape.pattern/step_segments` |

## Section 4 — Manifest manipulation & signalling robustness

| # | Question | Runnable | Finding | Status | Key hooks |
|---|---|---|---|---|---|
| 4.1 | `avgbw-reliance` — does ABR lean on AVERAGE-BANDWIDTH? | `yaml: pyramid-2sim-s2-noavg` | — | 🟡 | `proxy.strip_avg_bandwidth` |
| 4.2 | `ladder-truncation` — graceful clamp when a rung disappears | — | `over-downshift` (47d16786), `ladder-density-over-downshift` (adjacent: density → hunting) | 🟠 | `proxy.allowed_variants` |
| 4.3 | `bandwidth-honesty` — trust manifest bitrate or measure it? | — | — | 🔴 | `proxy.overstate_bandwidth` |
| 4.4 | `codec-signalling` — codec pre-check / fallback | — | — | 🔴 | `is.codec`, `proxy.strip_codecs` |
| 4.5 | `resolution-signalling` — does ABR use RESOLUTION? | — | — | 🔴 | `proxy.strip_resolution` |
| 4.6 | `variant-order` — manifest order vs bandwidth sort (HLS-only) | — | — | 🔴 | `proxy.variant_order` |

## Section 5 — Fault recovery

| # | Question | Runnable | Finding | Status | Key hooks |
|---|---|---|---|---|---|
| 5.1 | `fault-by-request-kind` — recovery differs by what's faulted | `mode: fault_recovery` (iOS) | — | 🟡 | `fault.request_kind` (+ new CLI variant/kind flags) |
| 5.2 | `fault-by-type` — recovery envelope by failure class | `mode: fault_recovery` | — | 🟡 | `fault.type` (403/404/500/timeout/reset/…) |
| 5.3 | `fault-burst-tolerance` — transient vs sustained | `mode: fault_recovery` (partial) | — | 🟠 | `fault.frequency/consecutive/continuous` |
| 5.4 | `transfer-timeout-tolerance` — partial-delivery / slow-loris | — | — | 🔴 | `proxy.transfer_timeouts.*` |

## Section 6 — Interaction / cross-cutting

| # | Question | Runnable | Finding | Status | Key hooks |
|---|---|---|---|---|---|
| 6.1 | `offset-under-throttle` — does latency target survive bandwidth pressure? | — | — | 🔴 | `proxy.live_offset` + `proxy.shape` |
| 6.2 | `truncation-under-throttle` — ladder limits × bandwidth pressure | — | — | 🔴 | `proxy.allowed_variants` + `proxy.shape` |
| 6.3 | `platform-fault-parity` — recovery parity across stacks | `mode: fault_recovery` (iOS only — Android not wired) | — | 🟠 | `fault.*`, `platform` |

## Section 7 — Per-variant acceptance-band calibration

| # | Question | Runnable | Finding | Status | Key hooks |
|---|---|---|---|---|---|
| 7.1 | `variant-acceptance-band` — coarse staircase, both directions, per platform | `mode: hysteresis_gap` (adjacent) | — | 🟠 | shape staircase, `platform` |
| 7.2 | `variant-band-bisect` — precise edge per adjacent rung pair | sweep `bisect` kind (engine exists, not run to a table) | — | 🟠 | shape bisect axis |
| 7.3 | `floor-rung-rebuffer-edge` — lower bound config-class won't reach | `mode: emergency_downshift/abort` (adjacent) | `ipad-262s-stall`, `progressive-stall-wedge` | 🟠 | forced starvation (fault) |

## Section 8 — Startup / join-time characterization

| # | Question | Runnable | Finding | Status | Key hooks |
|---|---|---|---|---|---|
| 8.1 | `startup-time-by-bandwidth` — join time & join rung vs starting bandwidth | `mode: startup` | — | 🟡 | `proxy.shape.rate_mbps` |
| 8.2 | `startup-join-policy` — first-variant vs ABR-pick | `mode: startup` | `avplayer-startup-variant-selection` | ✅ | `is.starts_first_variant` |
| 8.3 | `startup-cap` — does the peak-bitrate clamp improve join? | `mode: startup_caps` | `avplayer-startup-variant-selection` (+ `reference_avplayer_cold_start_wedge`) | ✅ | `is.peak_bitrate_mbps` |
| 8.4 | `segment-startup` — segment size vs first-frame latency | `mode: startup` (+seg) | — | 🟠 | `is.segment` |

---

## Roll-up

33 questions across 8 sections:

| Status | Count | Which |
|---|---|---|
| ✅ Answered | 6 | 1.1, 1.2, 1.3, 2.1, 8.2, 8.3 *(the 1.x cluster shares one live-offset study)* |
| 🟡 Runnable, unwritten | 9 | 3.1, 3.3, 3.4, 3.5, 3.6, 4.1, 5.1, 5.2, 8.1 |
| 🟠 Partial | 10 | 1.4, 2.3, 3.2, 4.2, 5.3, 6.3, 7.1, 7.2, 7.3, 8.4 |
| 🔴 Gap (hooks exist) | 8 | 2.2, 4.3, 4.4, 4.5, 4.6, 5.4, 6.1, 6.2 |
| **Total** | **33** | |

### The three things this ledger makes obvious

1. **All five "answered" questions are latency/startup**, and **three of the answers are unpushed** (`feat/sweep-lab`). Backing up that branch is the single highest-leverage move — it's where the confirmed live-offset knowledge lives.
2. **Section 4 (manifest signalling) is almost entirely 🔴** — `overstate_bandwidth`, `strip_codecs`, `strip_resolution`, `variant_order` all have knobs (and TEST-PLAN specs) but **zero** runnable arms. Cheapest high-value expansion: these are pure `config`-class YAMLs, no new hooks needed.
3. **10 questions are 🟡 Runnable-but-unwritten** — the harness can answer them *today*; they just need an arm authored and a run. That's the backlog #772's loop is meant to burn down.

### Not in TEST-PLAN at all (⚫ missing-hook behaviours)

Discoverable classes with **no** question row and **no** hook, flagged for scope decisions (from the Part-C gap scan):

- **Seek / trick-play** — no harness support to drive seeks; ABR-after-seek untested.
- **Concurrent-session contention** — fleet runs parallel arms but "N players degrade each other" is never the swept variable.
- **DRM / key rotation** — `ContentKeyRequestEvent` is plumbed client-side; license faults untested (confirm if out of scope).
- **Endurance / soak** — all drivers run 45–180 s; buffer creep / memory drift invisible.
- **Latency/loss/jitter as an ABR axis** — `proxy.shape.delay_ms/loss_pct/jitter_ms` shipped (#826) but **every** mode still varies bandwidth only.
- **Content as a variable** — everything pins one clip (`insane_newer_p200_h264`); the `content:` axis is never swept.

---

*Source anchors — questions: [`matrix/TEST-PLAN.md`](matrix/TEST-PLAN.md); runnable: `modes/*_test.go` + `matrix/*.yaml`; findings: `.claude/findings/`; input hooks: `tools/harness-cli/internal/sweep/experiment.go` + `api/openapi/v2/proxy.yaml`; loop engine: issue #772.*
