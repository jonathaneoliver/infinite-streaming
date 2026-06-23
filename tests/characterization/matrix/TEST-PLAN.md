# Characterization Test Plan — declarative matrix specs

A catalogue of the comparisons we'd actually want to run, written as self-documenting
YAML matrix specs (issue #811 model, namespaced-knob revision). Each spec is readable on
its own: **WHY** (the question), **WATCH** (which chart + series), **CONCLUDE** (a decision
table mapping what we see → what we infer), and **NOTE** (confounds / gotchas).

This is a *design* document — the spec grammar below (`is.*` / `proxy.*` namespaced knobs,
`groups:`, `compare:`) is the proposed model, not all of it exists in the harness yet.

---

## Controls — every knob and its values

Authoritative values pulled from source (`internal/sweep/experiment.go`, `go-proxy/cmd/server`,
the app launch-arg surface). **Type** column: `enum` = fixed set, `float`/`int` = numeric,
`bool` = true/false. `0` / empty = "unset / use default" for the numeric knobs.

### Client knobs — `is.*` (app launch arg → read once → **cold relaunch on change**)

| Knob | Type | Values | What it does |
|---|---|---|---|
| `is.segment` | enum | `s2` `s6` `ll` (empty ⇒ app default `s6`) | which master-variant duration the app requests |
| `is.protocol` | enum | `hls` `dash` | delivery format |
| `is.codec` | enum | `h264` `hevc` `av1` | codec filter the app selects |
| `is.live_offset` | float (s) | `0`=unset, else seconds | app-side target-latency override (`is.flag.live_offset_s`) |
| `is.peak_bitrate_mbps` | float (Mbps) | `0`=off, else cap | startup/steady bitrate clamp (`is.flag.peak_bitrate_mbps`, #683) |
| `is.starts_first_variant` | bool | `true`/`false` | start on first manifest rung vs let ABR pick |
| `is.auto_recovery` | bool | `true`/`false` | client auto-recovery on stall/error |
| `is.auto_recovery_base_delay_s` | float (s) | seconds | backoff base for auto-recovery |
| `is.live_resync_stall_s` | float (s) | seconds | stall duration before snapping back to live edge |
| `is.wedge_confirm_s` | float (s) | seconds | wall-clock no-progress window before declaring a wedge (#703) |
| `is.local_proxy` | bool | `true`/`false` | LocalHTTPProxy on (needed for client `network_bitrate`) |

*Launch/harness plumbing (not test variables): `is.player_id`, `is.skip_home`, `is.flag.dev_mode`,
`is.flag.muted`, `is.flag.go_live`, `is.flag.play_id_rotation_s`, `is.flag.preview_video_slots`,
`is.servers.*`, `is.lastPlayed`, `is.viewCounts`, `is.contentCache.*`, `is.proxy_query`.*

### Server knobs — `proxy.*` (config-on-connect → **no relaunch**)

**Content manipulation** (`proxy.*`, master-manifest rewrite — config class):

| Knob | Type | Values | What it does |
|---|---|---|---|
| `proxy.live_offset` | float (s) | `0`=unset, else seconds | manifest hold-back (server live edge). Floor ≈ 3× max segment dur |
| `proxy.allowed_variants` | spec str | `drop-top-rung` · `drop-top-<N>` · `keep-bottom-<N>` · explicit keep-set | truncate the advertised ladder |
| `proxy.variant_order` | enum | `default` `ascending` `descending` | reorder variants in the manifest (#682) |
| `proxy.strip_codecs` | bool | `true`/`false` | remove `CODECS` attrs |
| `proxy.strip_avg_bandwidth` | bool | `true`/`false` | remove `AVERAGE-BANDWIDTH` |
| `proxy.strip_resolution` | bool | `true`/`false` | remove `RESOLUTION` |
| `proxy.overstate_bandwidth` | float | multiplier (e.g. `1.5` `2.0`) | inflate advertised `BANDWIDTH` |

**Shape** (`proxy.shape.*`, bandwidth — config class; floor-guarded, never below lowest rung):

| Knob | Type | Values |
|---|---|---|
| `proxy.shape.rate_mbps` | float | static cap |
| `proxy.shape.pattern` | enum | `pyramid` `ramp_up` `ramp_down` `square_wave` `transient_shock` |
| `proxy.shape.step_seconds` | enum | `6` `12` `18` `24` `60` `120` |
| `proxy.shape.margin_pct` | enum | `0` `5` `10` `25` `50` |

**Shape *timing* — constant vs dynamic stepping.** A pattern is not just a name: `BuildPattern(template, rungs, stepSecs)` turns the limit ladder into a list of `Step{rate_mbps, duration_seconds}`. The *timing* of those steps is its own variable surface — today mostly `CHAR_*` env vars in the characterization harness, **proposed onto `proxy.shape.*`** so the matrix can drive it:

| Knob | Type | Values | Constant vs dynamic |
|---|---|---|---|
| `proxy.shape.step_seconds` | int (s) | fixed dwell per rung | **constant** — every step holds the same |
| `proxy.shape.step_segments` | int | dwell = N × segment duration (`CHAR_STEP_S` ⇐ `DefaultSegments × SegmentDurationSeconds`) | **content-relative** — timing tracks segment cadence, not wall-clock |
| `proxy.shape.max_step` | int | rungs jumped per step (`CHAR_LADDER_MAX_STEP`) | staircase granularity — 1 = gentle, large = coarse jumps |
| `proxy.shape.pyramid_floor` | float | bottom rung of the pyramid (`CHAR_PYRAMID_FLOOR`) | where ascent/descent turns |
| `proxy.shape.dip_hold_s` | int (s) | transient_shock dip hold (`CHAR_SHOCK_DIP_HOLD_S`) | **dynamic** — dip held ≠ high held |
| `proxy.shape.high_hold_s` | int (s) | transient_shock cap hold (`CHAR_SHOCK_HIGH_HOLD_S`) | **dynamic** — asymmetric vs `dip_hold_s` |
| `proxy.shape.reps` | int | pattern repetitions (`CHAR_PYRAMID_REPS` / `RAMPUP` / `RAMPDOWN`) | repeat the profile to test consistency |

So "pyramid" alone underspecifies a test — `pyramid @ step_seconds:12, max_step:1, floor:1.5` (slow gentle climb) is a different stimulus from `pyramid @ step_seconds:6, max_step:3` (fast coarse climb). **Constant** = uniform `step_seconds`; **dynamic** = `step_segments` (content-relative) or the asymmetric `dip_hold_s`/`high_hold_s` shock holds where each step's `duration_seconds` differs. The data model (`Step.DurationSeconds` per-entry) already supports a fully custom/non-uniform step list; it's just not exposed as one knob yet.

**Fault** (`proxy.fault.*`, error injection — **fault class only**):

| Knob | Type | Values |
|---|---|---|
| `proxy.fault.type` | enum | HTTP: `403` `404` `500` `rate_limiting`(429) · conn: `connection_refused` `dns_failure` `timeout` `hung` `corrupted` · hangs: `request_connect_{delayed,hang,reset}` `request_first_byte_{delayed,hang,reset}` `request_body_{delayed,hang,reset}` · socket: `socket_drop_{before_headers,after_headers,mid_body}` `socket_reject_after_headers` · transport: `drop` `reject` · body: `partial` `client_disconnect` |
| `proxy.fault.request_kind` | enum | `segment` `audio_segment` `init` `manifest` `master_manifest` |
| `proxy.fault.url_substr` | string | optional URL scope |
| `proxy.fault.frequency` | int | how often |
| `proxy.fault.mode` | enum | `requests` `seconds` `failures_per_seconds` `failures_per_packets` |
| `proxy.fault.consecutive` | int | failure run length |

**Transfer timeouts** (`proxy.transfer_timeouts.*`, slow origin — config class):

| Knob | Type | Values |
|---|---|---|
| `proxy.transfer_timeouts.active_seconds` | int | active-transfer deadline (`0`=off) |
| `proxy.transfer_timeouts.idle_seconds` | int | idle-stall deadline (`0`=off) |
| `proxy.transfer_timeouts.applies_{segments,manifests,master}` | bool | which request kinds it governs (segments default on) |

### Run-level knobs (on the spec / arm, not namespaced)

| Knob | Type | Values |
|---|---|---|
| `platform` | enum | `ipad-sim` `iphone` `appletv` `androidtv` `web` |
| `class` | enum | `config` (default) `fault` |
| `content` | string | catalogue name — **optional**; omit to inherit the default from `CHAR_CONTENT` (`.env`). Set it only to *pin* a specific clip. |
| `mode` | enum | `steps` `rampup` `rampdown` `pyramid` `hysteresis_gap` `downshift_severity` `transient_shock` `emergency_downshift` `startup` `startup_caps` `abort` |
| `duration_s` / `reps` | int | play window / confirmation reps |
| `parallel` | bool | concurrent (fleet) vs sequential |

> **Spec values that don't exist yet** (flagged where used below): `allowed_variants` has no
> *drop-bottom* / *keep-top* primitive — removing the floor rung needs a new spec. The §4.2/§6.2
> "drop-bottom-rung" arms assume it's added.

---

## Model (read this first)

### Knob namespaces — the prefix *is* the layer

| Prefix | Layer | Applied via | Per-arm cost |
|---|---|---|---|
| `is.*` | **client** | app launch arg (read once at launch) | **cold relaunch** when it changes |
| `proxy.*` | **server** | config-on-connect (proxy rewrites the response) | none — no relaunch |

There is no `lever` field. To impose a live offset from the server you set `proxy.live_offset`;
to impose it from the client you set `is.live_offset`. They are **orthogonal** — you may set
one, the other, both (the precedence case), or neither (`0` = unset/default).

Client knobs: `is.segment` (s2|s6|ll), `is.protocol` (hls|dash), `is.live_offset`,
`is.peak_bitrate_mbps`.

Server knobs: `proxy.live_offset`, `proxy.strip_avg_bandwidth`, `proxy.strip_codecs`,
`proxy.strip_resolution`, `proxy.allowed_variants`, `proxy.variant_order`,
`proxy.overstate_bandwidth`, `proxy.shape.*` (rate_mbps / pattern / step_seconds / margin_pct),
`proxy.fault.*` (type / request_kind / frequency / mode / consecutive), `proxy.transfer_timeouts`.

### Three run shapes

| Shape | `axes`/`groups` | `parallel` | `reps` | Use when |
|---|---|---|---|---|
| **SWEEP** | one grid axis, no `compare`/`groups` | `false` (sequential OK) | 1+ | mapping a *response curve* of one knob |
| **PAIR** | `compare:` axis (≤4) **or** `groups:` control+variants | `true` (concurrent) | `2` counterbalanced | isolating *one* causal knob — confounds must cancel |
| **GRID** | grid axes × an overlay axis (e.g. platform) | `true` if cross-traffic matters | 1+ | repeating a comparison across conditions |

Rule of thumb: **a causal "A differs from B" claim needs PAIR** (concurrent + counterbalanced reps,
so temporal and per-instance confounds cancel). A **SWEEP** maps a curve and tolerates sequential
runs. Platform is usually a **GRID/overlay** axis, not a counterbalanced compare — two platforms
can't share a box, so the per-device difference *is* the signal, not a confound.

### Charts we read (WATCH targets)

- **Bandwidth** — achieved throughput + the per-rendition variant-peak ladder (#812).
- **Bitrate/ABR** (`MetricsLineChart`) — which rung the player selected over time.
- **Buffer** — buffer level; dips toward 0 = rebuffer risk.
- **FPS** — rendered fps; drops = decode/perf stress.
- **RTT/TTFB** — request latency (client TTFB ≠ network RTT on HTTP/2 — label accordingly).
- **Events timeline** — discrete rebuffers / errors / downshifts / startup.
- **Achieved offset** — `AchievedOffsetFromEvents`; **ManipulationLanded** — did the server change take.

### Read-out caveats baked in

- `avg_network_bitrate` over-reads 2–3× the cap on iOS — use `full_segment_network_bitrate_mbps`
  / `mbps_transfer_rate`, not avg, for throughput conclusions.
- Simulator ≠ hardware — pair server data with sim logs; some codes (`-12174`) are sim-only noise.
- **n=1 is not a pattern** — no PAIR conclusion until `reps ≥ 2` agree.

---

## Relationship to the existing sweep / skills / triage tooling

This plan is **not** a new parallel system — it's a declarative front-end onto machinery that
already exists. Where it lands:

- **`internal/sweep` (the automated sweep, #772)** — *strong overlap, by design.* Every spec here
  compiles to a `sweep.Experiment`. The vocabulary is already there: `Class` (`config`/`fault`),
  `Kind` (our PAIRs = `hypothesis`; the "flip one knob" specs = `isolation`), and `Arm`/`Group`
  (the A/B pairing). The live-offset recipes in `seed.go` are literally hand-coded versions of
  §1 here. **What the matrix adds:** declarative cartesian expansion + the namespaced `is.*`/`proxy.*`
  knobs + the precedence cell `lever` couldn't reach. **What it should reuse, not duplicate:** the
  oracle (`analyze.go`) already turns a run into a `Verdict` (`clean`/`notable`/`aberration`/
  `inconclusive`) from the QoE `labels[]`. Our `CONCLUDE:` decision tables are doing *by hand* what
  the oracle does automatically — so the read-out blocks should be framed as "what the operator
  reads off the chart," with the oracle verdict as the machine-checkable backstop, not a competing
  judgment.
- **`harness sweep isolate --flip axis=value`** — this is *already* the one-knob A/B primitive. A
  matrix `groups:` block (control + one-knob variants) is the batch/declarative form of repeated
  `isolate` calls. Don't reimplement the flip logic — expand to it.
- **`fault` / `shape` skills** — the English→CLI translators for a *single* live injection on one
  player. The matrix is the *batch* form of the same knobs: every `proxy.fault.*` / `proxy.shape.*`
  value here is exactly what those skills emit. The skills own one-off interactive mutation; the
  matrix owns reproducible multi-arm runs. Same control surface, different cardinality.
- **`triage` skill** — *orthogonal, read-side.* Triage scans recent plays for the 3–6 worst signals
  at the menu level; it doesn't run experiments. It's where a matrix *result* gets noticed
  ("group X's variant arm is throwing rebuffers"), and `sweep seed-from-triage` already closes that
  loop. The matrix produces plays; triage surfaces the bad ones; the oracle/labels classify them.
- **`investigate` / `forensics`** — single-event and multi-event causal drill-down. These are the
  tools that turn a matrix arm's "something's off" into a tagged hypothesis. Our `CONCLUDE:` tables
  pre-register the interpretation; forensics is the runtime confirm/refute against the same evidence.

**Net:** the matrix sits *above* `sweep` (declarative authoring) and *upstream* of
`triage`/`investigate`/`forensics` (which read its output). The honest risk is **the `CONCLUDE:`
tables re-inventing the oracle** — they should defer to `labels[]`/`Verdict` for anything the oracle
already scores, and only add the human-readable "what to look for" the oracle can't express.

---

# Section 1 — Live-edge latency & offset control

## 1.1 `offset-response-curve` — where does the server offset clamp? (SWEEP)

```yaml
# WHY:   Map achieved live offset vs intended as we push proxy hold-back higher.
#        The hold-back floor is ~3× the max segment duration, so on s6 (~7s) the
#        floor is ~21s — below it the offset can't land.
# WATCH: Achieved offset (AchievedOffsetFromEvents) vs the intended axis value.
# CONCLUDE:
#   achieved tracks intended up to ~21s then flattens → confirms the 3×seg floor on s6.
#   achieved never reaches any intended value            → player ignores manifest hold-back entirely.
#   achieved overshoots intended                         → stepper/persisted offset contaminating the run (check is.live_offset pinned 0).
# NOTE:  Pure curve → SWEEP, sequential fine. ManipulationLanded must be true per arm or the point is inconclusive, not a finding.
name: offset-response-curve
class: config
parallel: false
duration_s: 90
defaults:
  platform: ipad-sim
  content: insane_newer_p200_h264
  is.segment: s6
axes:
  proxy.live_offset: [12, 18, 21, 24, 30, 36]
```

## 1.2 `offset-floor-by-segment` — same offset, opposite outcome by segment size (GRID)

```yaml
# WHY:   The 12s offset is sub-spec on s6 (below the ~21s floor) but legal on s2
#        (~6–9s floor). Proves the floor is segment-driven, not a fixed number.
# WATCH: Achieved offset + ManipulationLanded, per (segment, offset) cell.
# CONCLUDE:
#   s2 lands 12, s6 clamps 12          → floor scales with segment duration (expected).
#   both land / both clamp             → floor is fixed, not segment-driven (surprise — investigate).
#   s6 lands a deep 36 but not 12      → s6 CAN honour a moved offset; non-landing elsewhere is meaningful.
name: offset-floor-by-segment
class: config
parallel: false
duration_s: 90
defaults: { platform: ipad-sim, content: insane_newer_p200_h264 }
axes:
  is.segment:        [s2, s6]
  proxy.live_offset: [12, 36]
```

## 1.3 `offset-precedence` — when client and server disagree, who wins? (MATRIX)

```yaml
# WHY:   The cell the old `lever` model could never reach. Set BOTH a server
#        manifest hold-back and a client target-latency override and see which
#        the player honours.
# WATCH: Achieved offset per cell; compare the both-set cell against the two single-set cells.
# CONCLUDE:
#   both-set ≈ proxy-only value   → manifest hold-back wins; client override ignored when server speaks.
#   both-set ≈ is-only value      → client target-latency wins; manifest hold-back advisory.
#   both-set = some 3rd value     → they compose (e.g. additive / min / max) — characterise the rule.
#   both-set rebuffers/oscillates → the two controllers fight; this is a latency bug to file.
# NOTE:  0 = unset. The (0,0) cell is the baseline; (24,18) is the conflict cell.
name: offset-precedence
class: config
parallel: true          # the single-set cells are the controls for the both-set cell — run concurrently
reps: 2
duration_s: 90
defaults: { platform: ipad-sim, content: insane_newer_p200_h264, is.segment: s6 }
axes:
  proxy.live_offset: [0, 24]
  is.live_offset:    [0, 18]
# → (0,0) baseline · (24,0) server-only · (0,18) client-only · (24,18) PRECEDENCE
```

## 1.4 `offset-source-parity` — do the two mechanisms reach the same latency? (PAIR)

```yaml
# WHY:   At the SAME intended offset, does server hold-back land the same playback
#        latency as the client override? If not, the surfaces aren't equivalent.
# WATCH: Achieved offset + buffer level, server-arm vs client-arm.
# CONCLUDE:
#   both land ~24s, similar buffer   → surfaces equivalent; pick either operationally.
#   server lands, client short       → client target-latency under-applies (or is clamped by ABR buffer goals).
#   client smoother buffer           → client-side control reacts better to its own buffer than to a static manifest.
# NOTE:  PAIR → parallel + reps=2 counterbalanced (swap which sim runs which arm across reps).
name: offset-source-parity
class: config
parallel: true
reps: 2
duration_s: 120
defaults: { platform: ipad-sim, content: insane_newer_p200_h264, is.segment: s6 }
groups:
  - id: at24
    control:  { proxy.live_offset: 24 }   # server hold-back
    variants: [{ is.live_offset: 24 }]     # client override, same target
```

---

# Section 2 — Platform & protocol parity

## 2.1 `platform-offset-parity` — AVPlayer vs ExoPlayer latency across offsets (GRID)

```yaml
# WHY:   The same manifest hold-back, two player stacks. Where do iOS and Android TV
#        diverge on achieved latency?
# WATCH: Achieved offset overlay (ipad vs androidtv) at each offset; startup time.
# CONCLUDE:
#   curves overlap                    → stacks agree on hold-back honouring.
#   androidtv consistently deeper     → ExoPlayer holds further back (more conservative live edge).
#   one stack flattens earlier        → that stack has a higher effective floor (segment rounding differs).
# NOTE:  platform is an OVERLAY axis, not a counterbalanced compare — two devices, per-stack
#        difference IS the signal. parallel kills the temporal confound IF both devices run together.
name: platform-offset-parity
class: config
parallel: true
duration_s: 90
defaults: { content: insane_newer_p200_h264, is.segment: s6 }
compare: platform          # overlay ipad-sim vs androidtv per group
axes:
  proxy.live_offset: [12, 18, 24, 30]
  platform:          [ipad-sim, androidtv]
```

## 2.2 `protocol-parity` — LL-HLS vs LL-DASH on the same content (PAIR)

```yaml
# WHY:   Same source, two delivery formats. Do they achieve the same latency/ABR?
# WATCH: Achieved offset + ABR ladder, hls-arm vs dash-arm.
# CONCLUDE:
#   parity                            → format-agnostic; either is fine for latency work.
#   dash deeper latency               → DASH SegmentTimeline/availability window differs from HLS hold-back.
#   ABR ladders differ                → bitrate-selection logic isn't shared across the two pipelines.
# NOTE:  protocol is a client launch arg (is.protocol) → cold relaunch per arm.
name: protocol-parity
class: config
parallel: true
reps: 2
duration_s: 120
defaults: { platform: ipad-sim, content: insane_newer_p200_h264, is.segment: s6, proxy.live_offset: 24 }
compare: is.protocol
axes:
  is.protocol: [hls, dash]
```

## 2.3 `platform-abr-parity` — do the stacks make the same ABR call under load? (GRID)

```yaml
# WHY:   Under an identical bandwidth ramp-down, do iOS and Android TV downshift at
#        the same point and to the same rung?
# WATCH: ABR ladder overlay; events timeline (downshift / rebuffer) per platform.
# CONCLUDE:
#   same rung at same time            → ABR controllers behave equivalently under pressure.
#   androidtv downshifts later + rebuffers → ExoPlayer reacts slower / runs leaner buffer.
#   ios oscillates, androidtv steps   → different hysteresis; note which is steadier.
name: platform-abr-parity
class: config
parallel: true
duration_s: 120
defaults: { content: insane_newer_p200_h264, is.segment: s6 }
compare: platform
axes:
  platform: [ipad-sim, androidtv]
  proxy.shape: [{ pattern: ramp_down, step_seconds: 12 }]
```

---

# Section 3 — ABR under bandwidth shaping

## 3.1 `shape-pattern-response` — characterize ABR reaction per bandwidth profile (SWEEP)

```yaml
# WHY:   Map how the ABR controller reacts to each canonical bandwidth shape.
# WATCH: ABR ladder + buffer, one arm per pattern.
# CONCLUDE:
#   pyramid: clean up then down       → symmetric tracking, good.
#   transient_shock: rebuffer         → no headroom for sudden drops; emergency-downshift too slow.
#   square_wave: oscillation/flapping → hysteresis gap too small; flips rung every step.
#   ramp_down: late downshift + dip   → reaction lag; quantify the seconds-behind.
# NOTE:  characterization SWEEP — sequential fine; no control/variant.
name: shape-pattern-response
class: config
parallel: false
duration_s: 150
defaults: { platform: ipad-sim, content: insane_newer_p200_h264, is.segment: s6 }
axes:
  proxy.shape:
    - { pattern: pyramid,         step_seconds: 12 }
    - { pattern: ramp_up,         step_seconds: 12 }
    - { pattern: ramp_down,       step_seconds: 12 }
    - { pattern: square_wave,     step_seconds: 12 }
    - { pattern: transient_shock, step_seconds: 12 }
```

## 3.2 `startup-bitrate-clamp` — does the startup cap change downshift behaviour? (PAIR)

```yaml
# WHY:   With is.peak_bitrate_mbps clamping startup, does the player avoid the
#        over-pick-then-emergency-downshift it does uncapped? (#683)
# WATCH: ABR ladder + events (startup, emergency_downshift), clamped vs uncapped.
# CONCLUDE:
#   uncapped over-picks then crashes down → clamp prevents the startup overshoot.
#   both behave the same                  → startup picks conservatively anyway; clamp is a no-op here.
#   clamped starts too low and stays      → clamp is sticky past startup (regression — should release).
name: startup-bitrate-clamp
class: config
parallel: true
reps: 2
duration_s: 120
defaults: { platform: ipad-sim, content: insane_newer_p200_h264, is.segment: s6 }
groups:
  - id: clamp
    control:  {}                          # uncapped
    variants: [{ is.peak_bitrate_mbps: 5 }]
```

## 3.3 `segment-abr-responsiveness` — segment size vs reaction speed (GRID)

```yaml
# WHY:   Smaller segments → more frequent decision points → faster ABR reaction,
#        at the cost of request overhead. Quantify the trade.
# WATCH: ABR ladder reaction lag under ramp_down; request count / overhead.
# CONCLUDE:
#   s2 reacts fastest, ll fastest of all → confirms granularity drives responsiveness.
#   ll rebuffers from overhead           → partial-segment overhead outweighs the granularity win.
#   s6 sluggish but stable               → fewer decisions = smoother but slower — note the trade.
name: segment-abr-responsiveness
class: config
parallel: false
duration_s: 120
defaults: { platform: ipad-sim, content: insane_newer_p200_h264 }
axes:
  is.segment:  [s2, s6, ll]
  proxy.shape: [{ pattern: ramp_down, step_seconds: 12 }]
```

## 3.4 `rate-step-sensitivity` — how fast a bandwidth change can the player track? (SWEEP)

```yaml
# WHY:   Sweep how quickly bandwidth steps (step_seconds) to find the cadence at
#        which the ABR controller can no longer keep up.
# WATCH: ABR ladder lag + buffer dips per step cadence.
# CONCLUDE:
#   tracks at 24s/12s, lags at 6s     → controller bandwidth-estimate window is ~the lag point.
#   rebuffers only at the fastest step → that cadence exceeds the reaction budget — note threshold.
name: rate-step-sensitivity
class: config
parallel: false
duration_s: 150
defaults: { platform: ipad-sim, content: insane_newer_p200_h264, is.segment: s6 }
axes:
  proxy.shape:
    - { pattern: square_wave, step_seconds: 6 }
    - { pattern: square_wave, step_seconds: 12 }
    - { pattern: square_wave, step_seconds: 24 }
    - { pattern: square_wave, step_seconds: 60 }
```

## 3.5 `pyramid-timing` — the same pattern, different timing (GRID)

```yaml
# WHY:   "pyramid" underspecifies the test. Vary dwell (step_seconds) and jump
#        granularity (max_step) to see how stimulus shape changes ABR behaviour.
# WATCH: ABR ladder tracking + buffer, per (step_seconds, max_step) cell.
# CONCLUDE:
#   slow+gentle (12s,1) tracks clean       → controller keeps up with measured climbs.
#   fast+coarse (6s,3) overshoots/rebuffers → big jumps outrun the bandwidth estimate.
#   buffer dips only at fast dwell          → the dwell, not the jump size, is the limiter.
# NOTE:  pyramid_floor pins where the climb starts; hold it fixed so step×jump is the only IV.
name: pyramid-timing
class: config
parallel: false
duration_s: 180
defaults: { platform: ipad-sim, content: insane_newer_p200_h264, is.segment: s6 }
axes:
  proxy.shape:
    - { pattern: pyramid, step_seconds: 12, max_step: 1, pyramid_floor: 1.5 }
    - { pattern: pyramid, step_seconds: 6,  max_step: 1, pyramid_floor: 1.5 }
    - { pattern: pyramid, step_seconds: 12, max_step: 3, pyramid_floor: 1.5 }
    - { pattern: pyramid, step_seconds: 6,  max_step: 3, pyramid_floor: 1.5 }
```

## 3.6 `constant-vs-dynamic-stepping` — uniform dwell vs content-relative vs asymmetric (GROUP)

```yaml
# WHY:   Does the ABR controller care whether step timing is wall-clock-constant,
#        tied to segment cadence, or asymmetric (shock)? Same rungs, different timing law.
# WATCH: ABR reaction lag + rebuffers, constant vs segment-relative vs shock.
# CONCLUDE:
#   constant & segment-relative identical → controller is wall-clock; segment alignment irrelevant.
#   segment-relative smoother             → aligning steps to segment boundaries avoids mid-segment switches.
#   shock asymmetric rebuffers on dip     → the short high-hold doesn't refill buffer before the next dip.
name: constant-vs-dynamic-stepping
class: config
parallel: true
reps: 2
duration_s: 150
defaults: { platform: ipad-sim, content: insane_newer_p200_h264, is.segment: s6 }
groups:
  - id: stepping
    control:  { proxy.shape: { pattern: ramp_down, step_seconds: 12 } }          # constant
    variants:
      - { proxy.shape: { pattern: ramp_down, step_segments: 2 } }                 # content-relative
      - { proxy.shape: { pattern: transient_shock, high_hold_s: 6, dip_hold_s: 18 } }  # asymmetric/dynamic
```

---

# Section 4 — Manifest manipulation & signalling robustness

## 4.1 `avgbw-reliance` — does ABR lean on AVERAGE-BANDWIDTH? (PAIR)

```yaml
# WHY:   Strip AVERAGE-BANDWIDTH from the manifest. If ABR changes, it was using it;
#        if not, it only reads BANDWIDTH (peak).
# WATCH: ABR ladder, clean vs stripped, on the same segment.
# CONCLUDE:
#   stripped picks higher/oscillates  → player was using AVERAGE-BANDWIDTH to be conservative; now over-picks.
#   identical ladders                 → player ignores AVERAGE-BANDWIDTH; only BANDWIDTH matters.
name: avgbw-reliance
class: config
parallel: true
reps: 2
duration_s: 120
defaults: { platform: ipad-sim, content: insane_newer_p200_h264 }
groups:
  - id: s6
    control:  { is.segment: s6 }
    variants: [{ is.segment: s6, proxy.strip_avg_bandwidth: true }]
  - id: s2
    control:  { is.segment: s2 }
    variants: [{ is.segment: s2, proxy.strip_avg_bandwidth: true }]
```

## 4.2 `ladder-truncation` — graceful clamp when a rung disappears (GROUP, 3 arms)

```yaml
# WHY:   Drop the top or bottom rung from the advertised ladder; does ABR clamp
#        gracefully or stall reaching for a rung that's gone?
# WATCH: ABR ladder ceiling/floor + rebuffer events vs the control's full ladder.
# CONCLUDE:
#   drop-top: caps at new ceiling cleanly      → honours the truncated ladder.
#   drop-top: tries old top then rebuffers     → cached/assumed ladder; doesn't re-read.
#   drop-bottom: rebuffers under load          → lost its safety rung; no floor to fall to.
name: ladder-truncation
class: config
parallel: true
reps: 2
duration_s: 120
defaults: { platform: ipad-sim, content: insane_newer_p200_h264, is.segment: s6, proxy.shape: { pattern: ramp_down, step_seconds: 12 } }
groups:
  - id: rung-set
    control:  {}                                       # full ladder
    variants:
      - { proxy.allowed_variants: drop-top-rung }
      - { proxy.allowed_variants: drop-bottom-rung }
```

## 4.3 `bandwidth-honesty` — does the player trust manifest bitrate or measure it? (GROUP)

```yaml
# WHY:   Overstate BANDWIDTH so the manifest lies high. A player that trusts the
#        manifest over-picks and rebuffers; one that measures shrugs it off.
# WATCH: ABR ladder + buffer dips vs overstatement factor.
# CONCLUDE:
#   over-picks ∝ factor, rebuffers    → trusts manifest signalling; vulnerable to dishonest CDNs.
#   unaffected by factor              → measures actual throughput; ignores the lie.
#   over-picks at 2.0 not 1.5         → tolerance threshold between 1.5× and 2× — note it.
name: bandwidth-honesty
class: config
parallel: true
reps: 2
duration_s: 120
defaults: { platform: ipad-sim, content: insane_newer_p200_h264, is.segment: s6, proxy.shape: { rate_mbps: 8 } }
groups:
  - id: overstate
    control:  {}
    variants:
      - { proxy.overstate_bandwidth: 1.5 }
      - { proxy.overstate_bandwidth: 2.0 }
```

## 4.4 `codec-signalling` — codec pre-check / fallback (PAIR)

```yaml
# WHY:   Strip CODECS attributes; does the player still select/play, or refuse the
#        variant it can't pre-validate?
# WATCH: Startup success + which rungs are used, clean vs stripped.
# CONCLUDE:
#   stripped still plays all rungs    → no hard codec pre-check; probes the segment.
#   stripped drops/refuses variants   → relies on CODECS to gate selection; missing = unusable rung.
#   stripped fails startup            → CODECS is mandatory for this player — hard dependency.
name: codec-signalling
class: config
parallel: true
reps: 2
duration_s: 90
defaults: { platform: ipad-sim, content: insane_newer_p200_h264, is.segment: s6 }
groups:
  - id: codecs
    control:  {}
    variants: [{ proxy.strip_codecs: true }]
```

## 4.5 `resolution-signalling` — does ABR use RESOLUTION? (PAIR)

```yaml
# WHY:   Strip RESOLUTION; if rung selection changes, the player factors resolution
#        (e.g. screen-size capping) into ABR, not just bitrate.
# WATCH: Top rung actually used vs screen size, clean vs stripped.
# CONCLUDE:
#   stripped uses higher rungs        → was resolution-capping to the display; now uncapped.
#   identical                         → pure bitrate-driven selection; resolution ignored.
name: resolution-signalling
class: config
parallel: true
reps: 2
duration_s: 90
defaults: { platform: ipad-sim, content: insane_newer_p200_h264, is.segment: s6 }
groups:
  - id: resolution
    control:  {}
    variants: [{ proxy.strip_resolution: true }]
```

## 4.6 `variant-order` — manifest order vs bandwidth sort (PAIR, HLS-only)

```yaml
# WHY:   Reverse the variant order in the manifest (descending). Does the player
#        honour manifest order for initial pick, or always sort by BANDWIDTH?
# WATCH: First rung selected at startup, default order vs descending.
# CONCLUDE:
#   startup rung follows manifest order → honours order; first-listed is the initial pick.
#   startup rung identical to default   → sorts by bandwidth internally; order irrelevant.
# NOTE:  HLS-only knob; pin is.protocol: hls. variant_order ∈ default|ascending|descending.
name: variant-order
class: config
parallel: true
reps: 2
duration_s: 90
defaults: { platform: ipad-sim, content: insane_newer_p200_h264, is.segment: s6, is.protocol: hls }
groups:
  - id: order
    control:  {}
    variants: [{ proxy.variant_order: descending }]
```

---

# Section 5 — Fault recovery (recovery class)

## 5.1 `fault-by-request-kind` — recovery differs by what's faulted (GRID)

```yaml
# WHY:   A 500 on a media segment is recoverable (retry/skip); a 500 on the master
#        manifest may be fatal. Map recovery by request kind.
# WATCH: Events timeline (error → recovery vs stall); time-to-recover per kind.
# CONCLUDE:
#   segment/audio: brief blip, recovers → per-segment retry works.
#   init: stalls the rung               → init failure poisons the variant; no re-fetch.
#   manifest/master: fatal / no recovery → refresh path doesn't retry; player gives up.
# NOTE:  recovery class — judged against the recovery-expected envelope, not as raw aberrations.
name: fault-by-request-kind
class: fault
parallel: false
duration_s: 120
defaults: { platform: ipad-sim, content: insane_newer_p200_h264, is.segment: s6 }
axes:
  proxy.fault:
    - { type: "500", request_kind: segment,         frequency: 1, mode: requests }
    - { type: "500", request_kind: audio_segment,   frequency: 1, mode: requests }
    - { type: "500", request_kind: init,            frequency: 1, mode: requests }
    - { type: "500", request_kind: manifest,        frequency: 1, mode: requests }
    - { type: "500", request_kind: master_manifest, frequency: 1, mode: requests }
```

## 5.2 `fault-by-type` — recovery envelope by failure class (GRID)

```yaml
# WHY:   Same target (segment), different failure modes. Which does the player
#        survive, and how fast?
# WATCH: Time-to-recover + rebuffer count per fault type.
# CONCLUDE:
#   500 / corrupted: retry recovers     → handles clean HTTP/body errors.
#   timeout: long stall                 → no aggressive timeout; waits out the hang (tie to wall-clock detector).
#   connection_refused: fast fail or hang → depends whether it re-dials; note which.
name: fault-by-type
class: fault
parallel: false
duration_s: 120
defaults: { platform: ipad-sim, content: insane_newer_p200_h264, is.segment: s6 }
axes:
  proxy.fault:
    - { type: "500",                request_kind: segment, frequency: 1, mode: requests }
    - { type: timeout,              request_kind: segment, frequency: 1, mode: requests }
    - { type: corrupted,            request_kind: segment, frequency: 1, mode: requests }
    - { type: connection_refused,   request_kind: segment, frequency: 1, mode: requests }
```

## 5.3 `fault-burst-tolerance` — transient vs sustained (SWEEP)

```yaml
# WHY:   Sweep consecutive-failure run length; find where recoverable blip becomes
#        unrecoverable stall.
# WATCH: Recovers vs gives-up per consecutive count.
# CONCLUDE:
#   recovers up to N, fails at N+1    → that's the retry budget; document N.
#   never recovers                    → no retry on this kind at all.
name: fault-burst-tolerance
class: fault
parallel: false
duration_s: 120
defaults: { platform: ipad-sim, content: insane_newer_p200_h264, is.segment: s6 }
axes:
  proxy.fault:
    - { type: "500", request_kind: segment, consecutive: 1, mode: requests }
    - { type: "500", request_kind: segment, consecutive: 2, mode: requests }
    - { type: "500", request_kind: segment, consecutive: 4, mode: requests }
    - { type: "500", request_kind: segment, consecutive: 8, mode: requests }
```

## 5.4 `transfer-timeout-tolerance` — partial-delivery / slow-loris (PAIR)

```yaml
# WHY:   Deliver segment bodies slowly (transfer timeouts) rather than failing them.
#        Does the player abort+retry, or sit on a half-delivered segment and stall?
# WATCH: Buffer dip + abort/retry events, clean vs slowed.
# CONCLUDE:
#   slowed aborts + refetches          → has a transfer deadline; recovers.
#   slowed stalls on the partial body  → no body-level timeout; wedges until the socket closes.
name: transfer-timeout-tolerance
class: fault
parallel: true
reps: 2
duration_s: 120
defaults: { platform: ipad-sim, content: insane_newer_p200_h264, is.segment: s6 }
groups:
  - id: slowloris
    control:  {}
    variants: [{ proxy.transfer_timeouts: { segment_ms: 8000 } }]
```

---

# Section 6 — Interaction / cross-cutting

## 6.1 `offset-under-throttle` — does latency target survive bandwidth pressure? (GRID)

```yaml
# WHY:   Hold a deep live offset AND throttle bandwidth. Does the player sacrifice
#        latency (drift back) to keep playing, or hold latency and rebuffer?
# WATCH: Achieved offset AND buffer, per (offset, shape) cell.
# CONCLUDE:
#   offset drifts back, buffer holds   → latency is soft; sacrificed first under pressure.
#   offset held, buffer dips/rebuffers → latency is hard; protected at the cost of stalls.
#   both degrade                       → no clear priority — controller has no latency-vs-buffer policy.
name: offset-under-throttle
class: config
parallel: false
duration_s: 150
defaults: { platform: ipad-sim, content: insane_newer_p200_h264, is.segment: s6 }
axes:
  proxy.live_offset: [18, 30]
  proxy.shape:       [{ pattern: ramp_down, step_seconds: 12 }, { rate_mbps: 3 }]
```

## 6.2 `truncation-under-throttle` — ladder limits × bandwidth pressure (GRID)

```yaml
# WHY:   Combine a dropped bottom rung with a bandwidth ramp-down — does losing the
#        safety rung turn a survivable throttle into a rebuffer?
# WATCH: Rebuffer events: full ladder vs drop-bottom, under the same ramp.
# CONCLUDE:
#   full survives, drop-bottom rebuffers → the bottom rung was load-bearing under throttle.
#   both survive                          → headroom above the throttle; bottom rung wasn't needed.
name: truncation-under-throttle
class: config
parallel: true
reps: 2
duration_s: 120
defaults: { platform: ipad-sim, content: insane_newer_p200_h264, is.segment: s6, proxy.shape: { pattern: ramp_down, step_seconds: 12 } }
groups:
  - id: floor
    control:  {}
    variants: [{ proxy.allowed_variants: drop-bottom-rung }]
```

## 6.3 `platform-fault-parity` — recovery parity across stacks (GRID)

```yaml
# WHY:   Same segment fault, iOS vs Android TV. Do both recover, and equally fast?
# WATCH: Time-to-recover overlay, ipad vs androidtv.
# CONCLUDE:
#   both recover similarly             → recovery logic is equivalent.
#   one stalls / slower                → that stack's retry policy is weaker — platform-specific bug.
name: platform-fault-parity
class: fault
parallel: true
duration_s: 120
defaults: { content: insane_newer_p200_h264, is.segment: s6 }
compare: platform
axes:
  platform:    [ipad-sim, androidtv]
  proxy.fault: [{ type: "500", request_kind: segment, frequency: 1, mode: requests }]
```

---

# Section 7 — Per-variant acceptance-band calibration

The most *actionable* output: for each rung, on each platform, the bandwidth band within which
the player **selects AND sustains** that rung. Unlike Sections 1–6 (which characterize behaviour
under a stimulus), this is a **boundary search** that emits a calibration table.

Three things make it non-trivial, all handled below:
1. **Two edges, asymmetric (hysteresis).** Each rung has a *down-threshold* (rate it abandons the
   rung downward) and an *up-threshold* (rate needed to climb into it); up > down. So the staircase
   must run **both directions** — an ascending pass gives up-thresholds, descending gives down.
   This is the existing `hysteresis_gap` mode generalized across every rung.
2. **The floor rung's lower (rebuffer) edge is off-limits to config-class** — the `Shape` floor-guard
   refuses to shape below the lowest sustainable rate by design ("test decisions, never starvation").
   That edge needs the forced-starvation fault-class test in §7.3.
3. **Shaper accuracy is a precondition** — `TestRateSweep` / `throughput_calibration` must confirm
   the shaper actually hits the target rate before player thresholds mean anything.

## 7.1 `variant-acceptance-band` — coarse staircase, both directions, per platform (GRID)

```yaml
# WHY:   Per platform, per rung — the bandwidth band the player selects AND sustains it in.
#        Build the calibration table from where the settled rung changes across the cap ladder.
# WATCH: Steady-state ABR rung + rebuffer-free? + buffer level, per (platform, cap, direction).
# CONCLUDE (per platform, assemble the table):
#   settles on rung R, no rebuffer at cap C    → C is inside R's acceptable band.
#   climbs to R+1 at cap C (ascending)         → C ≈ R+1 up-threshold = upper edge of R.
#   drops to R-1 at cap C (descending)         → C ≈ R down-threshold = lower edge of R.
#   up-threshold ≫ down-threshold              → wide hysteresis band (sticky ABR) — record the gap.
#   ios vs androidtv thresholds diverge        → per-platform ABR calibration differs — the headline.
# NOTE:  Run BOTH passes — ascending ladder gives up-thresholds, descending gives down-thresholds.
#        Hold duration long enough to settle + confirm sustain (≥ a few segments). 0 rebuffers = sustained.
name: variant-acceptance-band
class: config
parallel: false              # sequential staircase; platform is the grid axis
duration_s: 60               # per cap — settle + confirm sustain
defaults: { content: insane_newer_p200_h264, is.segment: s6 }
axes:
  platform:              [ipad-sim, androidtv]
  proxy.shape.rate_mbps: [1.5, 2.0, 3.0, 4.0, 6.0, 8.0, 12.0, 16.0, 20.0]   # ascending pass
# Mirror with a descending pass (reverse the list, or pattern: ramp_down) for the down-thresholds.
```

## 7.2 `variant-band-bisect` — precise edge per adjacent rung pair (BISECT)

```yaml
# WHY:   Sharpen the coarse §7.1 edges. For each adjacent rung pair, bisect rate_mbps to pin the
#        switch point to within a tolerance — the exact up/down threshold, not a grid bucket.
# WATCH: Which rung settles at the probed rate; narrow the [lo,hi] bracket until |hi-lo| < tol.
# CONCLUDE:
#   converged switch rate S between R and R+1   → S is the precise boundary; band edge = S.
#   no clean convergence (flaps across probes)  → unstable region; ABR oscillates here — flag it.
# NOTE:  This is the sweep `bisect` Kind (depth-bounded, continuous axis). Only bisect edges that
#        matter — it costs ~log2(range/tol) runs per edge per platform. Seed brackets from §7.1.
name: variant-band-bisect
class: config
parallel: false
duration_s: 60
defaults: { content: insane_newer_p200_h264, is.segment: s6, platform: ipad-sim }
# Driven as a bisection, not a static axis: the runner narrows rate_mbps between the
# §7.1 bracket endpoints for each rung pair. Expressed here as the brackets to refine:
bisect:
  axis: proxy.shape.rate_mbps
  tolerance_mbps: 0.25
  brackets:                  # [lo, hi] seeded from 7.1 where the settled rung changed
    - { between: [rung2, rung3], lo: 3.0, hi: 4.0 }
    - { between: [rung3, rung4], lo: 6.0, hi: 8.0 }
```

## 7.3 `floor-rung-rebuffer-edge` — the lower bound config-class won't reach (FAULT/forced-starvation)

```yaml
# WHY:   §7.1/§7.2 stop at the floor-guard. The bottom rung's lower edge — the rate below which even
#        the lowest quality can't sustain → rebuffer — needs deliberate starvation (fault class).
# WATCH: Rebuffer onset vs cap; the cap where the floor rung can no longer hold playback.
# CONCLUDE:
#   sustains floor rung down to cap C, rebuffers below → C is the floor rung's lower (rebuffer) edge.
#   rebuffers well above the rung's encoded rate       → overhead/segment-fetch cost inflates the true floor.
# NOTE:  fault class — this is intentional starvation, judged as "expected to struggle," not a QoE bug.
#        Below-floor shaping bypasses the config-class floor-guard, hence fault-class.
name: floor-rung-rebuffer-edge
class: fault
parallel: false
duration_s: 60
defaults: { content: insane_newer_p200_h264, is.segment: s6 }
axes:
  platform:              [ipad-sim, androidtv]
  proxy.shape.rate_mbps: [1.2, 1.0, 0.8, 0.6, 0.4]   # below the lowest sustainable rung, descending
```

## Output — the calibration table this section produces

| Platform | Rung (res / bitrate) | Down-threshold | Up-threshold | Hysteresis gap | Acceptable band |
|---|---|---|---|---|---|
| ipad-sim | 1080p / 8M | … | … | … | [x, y] Mbps |
| ipad-sim | 720p / 4M | … | … | … | [x, y] Mbps |
| ipad-sim | floor / 1.5M | §7.3 | … | … | [rebuffer-edge, y] Mbps |
| androidtv | 1080p / 8M | … | … | … | [x, y] Mbps |
| … | … | … | … | … | … |

Per-rung **acceptable band** = [down-threshold, that rung's up-threshold). The **hysteresis gap** =
up-threshold − down-threshold (how sticky the rung is). The floor rung's lower edge comes from §7.3.
Diverging bands across platforms is the actionable finding — it tells you a single bandwidth assumption
can't drive both stacks.

---

# Section 8 — Startup / join-time characterization

Sections 1–7 mostly read *steady-state*. But join is its own QoE phase with its own failure modes —
slow first frame, joining too high (overshoot → emergency downshift) or too low (needless low quality),
and pre-roll rebuffer. These specs measure the **startup window**, not the settled stream.

**Startup metrics (the WATCH targets here):**
- **time-to-first-frame** — request-to-first-rendered-frame (the headline join time).
- **join rung** — the first sustained bitrate the player commits to.
- **startup rebuffer** — any pre-roll stall before first frame / right after.
- **time-to-stable** — when ABR stops adjusting and settles.
- **segments-before-play** — how much it buffered before starting.

Relevant existing machinery: the `startup` and `startup_caps` characterization modes, and the
`is.starts_first_variant` launch arg (join on first manifest rung vs let ABR choose).

## 8.1 `startup-time-by-bandwidth` — join time & join rung vs starting bandwidth (GRID)

```yaml
# WHY:   How does available bandwidth at join shape time-to-first-frame and the join rung, per platform?
# WATCH: time-to-first-frame + join rung + startup rebuffer, per (platform, cap).
# CONCLUDE:
#   first-frame time rises as cap falls        → join is bandwidth-bound (fetching the first segment).
#   join rung scales with cap, no overshoot    → conservative, bandwidth-aware join (good).
#   joins high at low cap then downshifts       → optimistic join → emergency downshift (overshoot bug).
#   startup rebuffer at low caps                → joins above what the link sustains; pre-roll stalls.
#   ios fast-joins, androidtv slow              → per-platform join policy differs — the headline.
# NOTE:  mode: startup. Hold duration long enough to capture join + time-to-stable.
name: startup-time-by-bandwidth
class: config
mode: startup
parallel: false
duration_s: 45
defaults: { content: insane_newer_p200_h264, is.segment: s6 }
axes:
  platform:              [ipad-sim, androidtv]
  proxy.shape.rate_mbps: [2.0, 4.0, 8.0, 20.0]
```

## 8.2 `startup-join-policy` — first-variant vs ABR-pick (PAIR)

```yaml
# WHY:   Does forcing the join onto the first manifest rung (is.starts_first_variant) join faster /
#        avoid overshoot vs letting ABR choose the join rung?
# WATCH: time-to-first-frame + join rung + startup overshoot, first-variant vs ABR-pick.
# CONCLUDE:
#   first-variant joins faster, lower quality  → safe-but-low join; ABR-pick trades join time for quality.
#   ABR-pick overshoots then downshifts        → ABR's join estimate is optimistic; first-variant avoids it.
#   no difference in time, only in rung        → join time is fetch-bound, not rung-bound.
name: startup-join-policy
class: config
mode: startup
parallel: true
reps: 2
duration_s: 45
defaults: { platform: ipad-sim, content: insane_newer_p200_h264, is.segment: s6, proxy.shape: { rate_mbps: 6 } }
groups:
  - id: join
    control:  { is.starts_first_variant: false }   # ABR picks the join rung
    variants: [{ is.starts_first_variant: true }]    # forced onto the first rung
```

## 8.3 `startup-cap` — does the peak-bitrate clamp improve join? (PAIR)

```yaml
# WHY:   With is.peak_bitrate_mbps clamping the startup pick, does the player avoid the
#        join-high-then-crash pattern, and what does it cost in join quality? (#683, startup_caps mode)
# WATCH: join rung + startup overshoot + time-to-first-frame, clamped vs uncapped.
# CONCLUDE:
#   uncapped overshoots + emergency downshift  → clamp prevents the startup spike.
#   clamped joins lower, never recovers up      → clamp is sticky past join (should release post-startup).
#   identical                                   → join pick already below the clamp; no-op here.
name: startup-cap
class: config
mode: startup_caps
parallel: true
reps: 2
duration_s: 60
defaults: { platform: ipad-sim, content: insane_newer_p200_h264, is.segment: s6, proxy.shape: { rate_mbps: 12 } }
groups:
  - id: cap
    control:  {}                              # uncapped
    variants: [{ is.peak_bitrate_mbps: 4 }]
```

## 8.4 `segment-startup` — segment size vs first-frame latency (GRID)

```yaml
# WHY:   Smaller / LL segments should reach first frame sooner (less to fetch before play).
#        Quantify the join-time win and any cost.
# WATCH: time-to-first-frame + segments-before-play, per segment size.
# CONCLUDE:
#   ll < s2 < s6 first-frame time             → confirms granularity speeds join.
#   ll fast but startup rebuffers              → partial-segment join is fragile under any jitter.
#   s6 slow but zero startup rebuffer          → fewer-but-larger fetches join slower, more stably — the trade.
# NOTE:  is.segment is a client launch arg → cold relaunch per arm.
name: segment-startup
class: config
mode: startup
parallel: false
duration_s: 45
defaults: { platform: ipad-sim, content: insane_newer_p200_h264, proxy.shape: { rate_mbps: 8 } }
axes:
  is.segment: [s2, s6, ll]
```

---

## Coverage map

| Knob / surface | Covered by |
|---|---|
| `proxy.live_offset` | 1.1, 1.2, 1.3, 1.4, 6.1 |
| `is.live_offset` | 1.3, 1.4 |
| `is.segment` (s2/s6/ll) | 1.2, 3.3, 4.1, 8.4 |
| `is.protocol` (hls/dash) | 2.2 |
| `is.peak_bitrate_mbps` | 3.2, 8.3 |
| `is.starts_first_variant` | 8.2 |
| startup / join-time | 8.1, 8.2, 8.3, 8.4 |
| `platform` (ios/androidtv) | 2.1, 2.3, 6.3 |
| `proxy.shape.pattern` | 2.3, 3.1, 3.3, 6.1, 6.2 |
| `proxy.shape.step_seconds` | 3.4 |
| `proxy.shape.rate_mbps` | 4.3, 6.1, 7.1, 7.2, 7.3 |
| `proxy.shape` timing (step/floor/holds) | 3.5, 3.6 |
| per-variant acceptance band | 7.1, 7.2, 7.3 |
| `proxy.strip_avg_bandwidth` | 4.1 |
| `proxy.strip_codecs` | 4.4 |
| `proxy.strip_resolution` | 4.5 |
| `proxy.allowed_variants` | 4.2, 6.2 |
| `proxy.variant_order` | 4.6 |
| `proxy.overstate_bandwidth` | 4.3 |
| `proxy.fault.*` | 5.1, 5.2, 5.3, 6.3 |
| `proxy.transfer_timeouts` | 5.4 |
| precedence (client×server) | 1.3 |

## Spec grammar — landed vs open

**Landed** (feat/811-namespaced-knobs):
- **Namespaced `is.*` / `proxy.*` knobs** — `is.` matches the real launch args (`-is.flag.live_offset_s`); `proxy.live_offset` + `is.live_offset` are orthogonal (the precedence cell).
- **`groups:`** control+variants and **`compare:`** axis — both stamp `group`/`role` onto the experiment, pre-paired for the dashboard. `compare:` requires `parallel: true` and ≤4 arms/group.
- **Flat content-manip conveniences** — `proxy.strip_*`, `proxy.allowed_variants`, `proxy.variant_order`, `proxy.overstate_bandwidth` fold onto `ContentManipulation` (nested `proxy.content_manipulation` wins per-field).
- **Object-valued axes** — `proxy.shape` / `proxy.fault` / `proxy.content_manipulation` / `proxy.transfer_timeouts` can be swept as lists of whole blocks (§3.1 / §3.5 / §5.x). Each value gets a `label:`-or-hash id slug (the `label:` is stripped before the block decodes); a typo'd key inside the block is rejected by strict decoding. See `shape-patterns.yaml`.

**Still open:**
1. **Counterbalancing** — every PAIR says `reps: 2`; the swap-assignment-across-reps logic lives
   in the runner, not the spec. Spec just declares the rep count.
2. **`allowed_variants` drop-bottom / keep-top primitive** — §4.2/§6.2 floor-removal arms need a
   spec the proxy doesn't resolve yet.

---

# Coverage gaps & roadmap

What this plan does **not** yet test, ranked by how much it limits the conclusions you can draw.
Each is honest about whether the *mechanism* exists today or needs building — so a gap is "not
written" vs "not possible."

## Tier 1 — gaps that bias today's conclusions

| Gap | Why it matters | Mechanism status |
|---|---|---|
| **Latency / loss / jitter** | Every spec varies *bandwidth only*. `Shape` deliberately omits `delay_ms`/`loss_pct` ("steady degradation out of scope"). But real links are latency+loss+jitter, which dominate LL-HLS/DASH (TTFB, partial-segment delivery, blocking reload). So §7's bands are "acceptable bandwidth **on a clean link**" — not real-world. | `tc` delay/loss exists in the `shape` **skill**; **missing from the sweep `Shape` model** → needs `proxy.shape.delay_ms` / `loss_pct` / `jitter_ms`. |
| **Content as a variable** | *Every* spec pins `insane_newer_p200_h264` — one complexity, one ladder. Can't separate a content-specific finding from a general one. | exists (catalogue) — **just needs a `content:` axis**. |
| **Startup measurement plumbing** | §8 specs exist now, but their `WATCH` targets (time-to-first-frame, join rung, time-to-stable) must be confirmed plumbed as metrics/labels end-to-end before the CONCLUDEs are machine-checkable. | partial — verify the `player_metrics_*` fields land (the 8-layer plumbing checklist). |

## Tier 2 — whole behaviour classes uncovered

| Gap | What's untested | Mechanism status |
|---|---|---|
| **Live / low-latency mechanics** | Live-edge tracking, DVR window, catch-up after pause, **LL-HLS partial segments / blocking reload / PRELOAD-HINT**, LL-DASH chunked CMAF, manifest-update cadence, discontinuities. This is the `go-live` core product and it's barely probed. | mostly exists (`go-live` LL variants, `is.live_resync_stall_s`); needs specs + maybe live-edge measurement helpers. |
| **Client recovery levers** | `is.auto_recovery`(+`base_delay_s`), `is.wedge_confirm_s`, `is.live_resync_stall_s` — in the controls table, **no spec varies them**. Obvious A/B: auto-recovery on/off under a segment fault (ties to #703 wedge detector). | knobs exist — just needs specs. |
| **Seek / trick-play** | ABR after seek, seeking in live/DVR window, scrub-induced rebuffer. | needs harness support for driving seeks. |
| **Audio + codec selection** | Video-centric throughout. `is.codec` (h264/hevc/av1) selection/fallback, audio renditions, A/V sync. §5.1 touches `audio_segment` faults only. | `is.codec` exists — needs specs; A/V-sync may need new metrics. |
| **Concurrent-session contention** | Fleet runs parallel arms, but "N players degrade each other" isn't a *variable*. | needs a contention/scale dimension in the runner. |

## Tier 3 — durability & breadth

| Gap | What's untested |
|---|---|
| **Endurance / soak** | All specs 60–180s; buffer creep, memory drift, long-run ABR misbehaviour invisible. Needs long-`duration_s` variants. |
| **Platform breadth** | Leans on `ipad-sim`+`androidtv`; `iphone`, `appletv`, and `web` (in the enum, **zero specs**) uncovered. |
| **Adversarial manifests** | Malformed/missing mid-stream segments, mid-stream codec switch, bad discontinuity — beyond §4's clean manipulations. |
| **DRM / content keys** | `ContentKeyRequestEvent` exists; key rotation / license faults untested. Confirm whether deliberately out of scope. |

## Cross-cutting

- **Measurement vocab not enumerated** — `WATCH:` names charts, not the `labels[]`/`player_metrics_*`
  fields each CONCLUDE keys on. Until that's listed, conclusions aren't machine-checkable (and risk
  duplicating the oracle — see "Relationship to existing tooling").
- **Counterbalancing** asserted (`reps: 2`) but the swap-assignment logic is a runner TODO.
- **Grammar gaps** — `allowed_variants` drop-bottom primitive (see "Spec grammar — landed vs open"
  above; namespacing, `groups:`/`compare:`, flat conveniences, and object-valued axes have landed).

## Suggested build order

1. **Latency/loss knobs** on `proxy.shape` + **`content:` axis** — the two Tier-1 mechanism gaps;
   they make §7 (and everything else) real-world and known-general. Then re-run §7 under realistic links.
2. **Live/LL mechanics specs** — biggest untested surface given LL is the product.
3. **Client-recovery A/Bs** + **startup metric verification** — cheap, knobs already exist.
4. **Breadth** (platforms incl. web, audio/codec) and **endurance** — fill out once the core holds.
