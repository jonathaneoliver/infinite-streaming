# v2.1.0 — Release notes

**Headline:** v2.0.0 made the rig *observable*. v2.1.0 makes it
*run itself* — across many devices at once, on a declarative spec,
unattended.

The through-line of this release is **scale of experiment**. Where v2.0.0
could characterize one player on one device, v2.1.0 drives a fleet of
simulators and real hardware in parallel, configures each one from the
server at connect time, runs a YAML-declared matrix across them, and
leaves an overnight sweep hunting for aberrations while you sleep.

262 commits since v2.0.0 (2026-05-27) — 127 features, 87 fixes.
**No breaking changes.**

---

## TL;DR

- **Critical fixes first.** Several v2.0.0 bugs produced silently *wrong*
  test results rather than visible failures — sessions running uncapped,
  freeze detection that could not fire during a freeze, and Android
  receiving no shaping or fault injection at all. Read
  [Critical fixes](#critical-fixes) before the feature list.
- **Device fleets.** An Appium Device Farm integration plus a
  device-aware concurrent test pool runs mixed fleets of iOS sims,
  Android emulators, and real hardware — grouped, reserved, and
  isolated per run.
- **Config-on-connect.** `proxy.*` URL args now materialize a session
  *before* the 302, and per-play `app_config` is pushed from the server
  and applied by iOS and Android at the play boundary — reconfigure a
  client without relaunching it.
- **Declarative characterization.** Runs are described in a
  `tests/characterization/matrix/*.yaml` spec with axes for segment
  length, transfer timeout, LocalProxy, server URL and more, bridged
  round-trip to the sweep queue.
- **Unattended fault sweeps (QE Lab).** A sweep engine, probe, and
  dashboard tab author and run fault-injection campaigns concurrently
  across the fleet, on an overnight loop.
- **Structure-driven fault scoping.** A native `fault_rules` evaluator
  replaces v1 translation, and faults now scope to real variants and
  resolutions for both HLS and DASH.
- **Richer shaping.** Latency / loss / jitter knobs, named link
  profiles, `valley` and `transient_shock` patterns, per-flow fairness,
  and a degraded HTTP-only mode that runs without `NET_ADMIN`.
- **QoE labels that explain themselves.** Threshold-based auto-labels,
  whole-play tiers, exit states, a complete hover glossary, and two
  statistical surprise scorers (VOMM and HMM).
- **1-second low-latency rung** end to end — `s1`/`master_1s` for HLS
  and `manifest_1s.mpd` for DASH.

---

## Upgrading

**No breaking changes.** No `feat!:` / `fix!:` subjects, no
`BREAKING CHANGE:` trailers, no PRs labelled `breaking`.

### ClickHouse

The schema gained roughly 20 columns this cycle (state-residency
accumulators, derived QoE rate metrics, live-offset fields, play-scoped
`start_time`), and two field families were renamed:

| Was | Now |
|---|---|
| `dropped_frames` | `frames_dropped` |
| `stall_count` / `stall_time_s` | `stalling_count` / `stalling_time_ms` |

**This upgrade is non-destructive and needs no manual migration.** As of
#913 the ClickHouse container re-applies `01-schema.sql` on *every* boot
via `CREATE ... IF NOT EXISTS` / `ADD COLUMN IF NOT EXISTS`, so an
existing volume is upgraded in place. The renamed columns are *added*
alongside the originals rather than swapped — the old columns keep their
history, and the forwarder mirror-writes both pairs during the
deprecation window. Expect historical rows to read empty in the *new*
columns only; nothing is lost, and nothing needs to be run by hand.

```bash
# Standard upgrade — the schema self-heal runs on container boot.
make build && make run
make analytics-rebuild-forwarder
make harness-cli              # rebuild the CLI; go install writes the wrong path
```

A follow-up release will drop the deprecated `stall_*` columns.

### Deploy target renames

`make deploy` is now the everyday local-tree → test-dev deploy (alias for
`test-deploy-dev`). The k3d targets are explicit: `deploy-k3d-dev` and
`deploy-k3d-release`.

---

## Critical fixes

**Read this before the feature list.** v2.0.0 shipped several bugs whose
symptom was a silently *wrong result* rather than a visible failure —
the worst failure mode for a measurement rig, because a corrupted run
looks exactly like a good one.

Every entry below was verified to **predate v2.0.0** by blaming the code
each fix replaced back to its introducing commit and confirming that
commit is an ancestor of the v2.0.0 tag. Regressions introduced *and*
fixed inside this release cycle are deliberately excluded — they never
reached you.

### Cross-session shaping clobber (#818)

Concurrent sessions clobbered each other's `tc` caps. Every per-session
u32 classifier filter shares `prio 1` on `parent 1:0`, and deleting one
by match-spec could match the **wrong** filter at that priority —
collaterally removing another live session's cap. The victim's traffic
then fell through to the uncapped 10 Gbps HTB `default 999` class until
its next rate-set happened to re-add the filter.

The production trigger was `ClearPortShaping`, the sweep that runs on
**every session allocation** — so on a busy rig this fired constantly.
**Any multi-session run on v2.0.0 may have had one or more sessions
silently running uncapped.** Both delete paths now resolve the port's
exact u32 leaf handle and delete by handle.

### Lost-update races on the session list (#742, #743)

`App.sessionsSnap` was read-modify-written throughout the proxy with no
lock held across the read and the write, making every full-list writer a
last-writer-wins lost update against every other concurrent writer.
Session deletes, transport-fault state, group updates, inactive-session
reaping and the entire v2 mutation surface were all exposed.

Writes now go through `mutateSessions(fn)` — load snapshot, mutate a
private clone, compare-and-swap, retry on conflict — with side effects
hoisted out so they run exactly once on the committed result. Reads are
clone-free via `sessionsView`.

### Analytics schema never upgraded on an existing volume (#913)

The schema was applied only through `docker-entrypoint-initdb.d`, which
runs on first-ever boot with an empty data dir — **on an existing volume
it silently did nothing.** Every schema change since then had to be
applied by hand via `make analytics-migrate`, and any that wasn't
backported to `01-schema.sql` never reached a rebuilt stack at all.

That is why this upgrade needs no manual migration: the container now
re-applies the idempotent schema on every boot, so a v2.0.0 volume picks
up all ~20 new columns by itself.

The drift this mechanism allowed is not theoretical — during development
it removed `control_revision` from fresh installs, and because the
dashboard's timeseries query selects that column, the missing identifier
aborted the backfill loop and **both the Network Log and the PlayLog
rendered empty while every container booted green and video played
normally.** That particular incident was caught and fixed inside this
cycle, but the mechanism that produced it was present in v2.0.0.

### Network rows unattributed at the start of every play (#914)

`network_requests` rows were attributed through a forwarder map
populated from `session_events`. That map is cold on a fresh stack **and
at the start of every new play**, so roughly the first six rows of each
play landed with an empty `player_id` and never appeared in the
per-player Network Log. The proxy now stamps `player_id` from the
request's query param, and the forwarder prefers that value and learns
the session map from it.

### Freeze detection could not fire during a freeze (#706)

`checkFrozenState()` was invoked only from
`AVPlayer.addPeriodicTimeObserver`, whose callback runs off the
**playback** clock — it goes silent the instant the playhead stops,
which is exactly when a freeze begins. Both the observer wiring and the
frozen detector shipped in v2.0.0, so on that release `frozen_count`
could never be raised by a real freeze.

Characterized live on a real iPhone: a textbook hard wedge (`-12880`
"removing variants", playhead frozen for ~5 minutes, no recovery when
the cap lifted back to 60 Mbps) reported `frozen_count: 0`. Detection is
now driven by a wall-clock timer, so it keeps running precisely when
playback does not — which also makes the new #703 wedge detector
(see §10) able to fire at all.

### Android bypassed the proxy entirely (#863)

`composeUrlAndLoad` selected the per-session proxy port only when
LocalProxy was enabled, so ordinary Android playback went straight to
the origin.

Data-confirmed on a live compare group: the Android session had **0
requests through the per-session proxy** (its paired iPhone had 229), a
null `master_manifest_url`, and no rows carrying `manifest_variants` —
meaning no variant ladder, no Displayed Variant line, and **no shaping
or fault injection reaching Android at all**. Main playback now always
routes through go-proxy.

### Silent segment-length substitution (#647)

`preflightMasterPlaylist` carried a fallback chain probing
`requested → _6s → _2s → plain`. When the requested master 404'd it
silently played a **different segment length** than the one selected —
making 6s-vs-2s characterization untrustworthy and masking real content
errors. The chain is gone: a play now serves exactly what was asked,
segment length included, or fails visibly.

### Measurement corrections

- **Negative gauges from ExoPlayer** — position, buffer and rate could
  emit below zero; now clamped to 0 for iOS parity (#731).
- **`total_ms`** is lifted to ttfb+transfer at the `logEntry`
  chokepoint (#628).
- **`video_bitrate`** snaps to the nearest published peak instead of
  drifting off-ladder (#620).

---

## What's new

### 1. Device farm + concurrent test fleets

The largest capability in the release. An Appium Device Farm integration
(capability launch, fleet roster, config-on-connect bind, on by default)
is paired with a device-aware concurrent test pool (#946, #948–952) that
claims, reserves, and reports real allocations rather than nominal picks.

- **Mixed-platform fleets** with hybrid real-iOS playback — farm-locked
  sims alongside off-farm physical hardware.
- **Fleet group mode** — born-grouped sims, a proxy `group_id`, and
  harness 412-retry so a group forms atomically.
- **Parallel pyramid across N sims** via `CHAR_FLEET_COUNT`.
- **`farm.sh` + the `/appium-farm` skill** for setup, reset, and
  recovery of the known failure modes (app-unknown, cold WDA, sims stuck
  busy, large-fleet bootstrap 503).
- Config-on-connect is **deferred to the probe** on large fleets, which
  is what removed the bootstrap 503.

### 2. Config-on-connect and per-play `app_config` (#712, #714, #800)

`proxy.*` URL arguments now materialize a proxy session *before* the
redirect, so a client is bound to its shaping and fault configuration
from its very first request rather than after a race.

On top of that, `app_config` is per-play and server-pushed: the proxy
exposes it on `/api/sessions`, the harness has an `app-config` command
backed by `Session.ApplyAppConfig`, and both iOS and Android apply
incoming config at the play boundary. A client can be reconfigured mid-
session without a relaunch.

### 3. Launch levers, namespaced (#811, #797, #266, #683, #838)

Client knobs moved to a namespaced `is.*` / `proxy.*` scheme and grew
considerably:

- **Android levers** — `is.segment`, `is.protocol`,
  `is.flag.peak_bitrate_mbps`, then codec, 4k, `go_live`,
  `play_id_rotation_s`, `starts_first_variant`, `content`.
- **Per-launch server override** — `-is.server_url` / `is.server_url`
  and `ArmConfig.ServerURL` pin each arm or sim to its own backend,
  which ends simulator server-drift and enables multi-server runs.
- **iOS Advanced settings** — peak-bitrate cap and start-on-first-variant,
  with the startup clamp auto-released after first frame.
- **Muted by default** across iOS and Android, on every config path.
- **`content_variant_order`** reorders master variants to probe
  AVPlayer's startup pick.

### 4. Declarative characterization matrices (#811)

Runs are now described rather than scripted — a YAML matrix spec under
`tests/characterization/matrix/` with axes for segment length, transfer
timeout, LocalProxy, server URL, and A/B strip-average-bandwidth arms.

- **Matrix ↔ queue bridge** (#873) round-trips specs into the sweep queue.
- **Multi-cycle** rampup / rampdown / pyramid on a single live play.
- **`harness char report`** aggregates study comparisons and repetitions
  over the archive (#880).
- **Auto-recovery ON by default** for char-matrix runs.
- Default playback content centralized to `.env` via `CHAR_CONTENT`.

### 5. QE Lab — unattended fault sweeps (#772, #873, #874)

An automated fault-injection sweep: engine, probe, and dashboard tab,
with non-blocking `sweep add` authoring and an activated overnight loop.
Run isolation fans out concurrently across the device farm.

*QE Lab is a developer-only page — append `?developer=1` to reach it.*

### 6. Structure-driven fault scoping (#919, #922, #926)

A **native `fault_rules` evaluator** retires v1 translation for fault
decisions. Built on that, a structure-driven rendition model scopes
faults to the variants and resolutions that actually exist in the
manifest — Tier 2 for HLS, and the equivalent for DASH `.mpd`. Faults
gained `--variant-scope` and `--continuous` flags.

### 7. Traffic shaping

- **Latency / loss / jitter** knobs and **named link profiles** on
  `proxy.shape`.
- **New patterns** — `valley` (high→low→high, the inverse of pyramid)
  and `transient_shock` (a deepening-drop staircase) — plus 60s and 120s
  step durations.
- **Degraded HTTP-only mode** and a no-`NET_ADMIN` build (#910), so the
  stack runs on Cloud Run / Fargate / PaaS where kernel shaping is
  unavailable. `shape.mode` (`kernel` | `http_only`) is on the v2 player
  model.
- **Per-flow fairness** (SFQ) on throttled media ports, off by default.
- **Honest delivery rate** (#850) — `delivery_rate_mbps` end to end,
  with chart dots gated on the kernel `app_limited` flag.
- **Group-pattern fan-out** with a single owner and driven-slave shaping
  in the UI.

### 8. QoE labels, anomalies, and surprise scorers

- **Threshold-based QoE auto-labels** in the forwarder (#553), whole-play
  `qoe_tier_*`, `qoe_exit_*` state-at-close, and
  `qoe_downshift_overshoot` for over-correcting downshifts (#669).
- **Derived surprise labels** — a VOMM (variable-order Markov model)
  scorer producing condition-anchored episode surprise (#508), split
  into a nightly train and a fast score pass (#608), surfaced as
  `unexpected_<condition>` chips per row (#506).
- **An HMM latent-regime scorer** (#445) producing `regime_*` labels.
- **A `testing` severity tier** (#571) for operator and test-harness KV
  metadata, outside the `error|critical|warning|info` ranking.
- **A complete label glossary** — hover tips for every label — plus a
  derived `net_failure` signature that collapses network-failure facets
  into one chip.
- **Aberration crawl** (#607) — an invariants catalogue with a
  version-keyed census runner and calibrated assertions.

### 9. Live-offset coverage (#793, #266)

Window set, variant rewrite, an app-side lever, UI and docs — plus a
**manipulation-check gate** that establishes test validity before a
result is trusted, a segment matrix, and a tight label.
`recommended_offset_s` and `configured_offset_s` are plumbed end to end
through Android, the forwarder, and the events archive.

### 10. iOS auto-recovery and wedge detection (#703, #703a, #778)

An application **wedge detector** auto-restarts the player on a `-12880`
hard wedge, with live-aware auto-recovery and fault-recovery
characterization. The forwarder gained a recovery vocabulary —
`player_stuck`, `live_resync`, and split `auto_recovery_*` labels.

Auto-recovery now defaults **ON**, with a `CHAR_AUTO_RECOVERY` harness
toggle for raw wedge-observation runs.

### 11. AVMetrics as a first-class target (#693)

The iOS 18 AVMetrics spike is wired through the dashboard and heartbeat,
and AVMetrics is now a first-class harness query and stream target.
Compare mode shows variant-peak and AVMetrics throughput side by side,
and `analytics/tools/startup_view.py` renders a client-side
per-segment/chunk startup timeline.

### 12. Observability plumbing (#550, #587, #554, #556, #563)

- **State-residency accumulators** — `playing_*`, `pausing_*`,
  `buffering_*`, `stalling_*`, seeking and trickplay — each as a
  cumulative total plus a forwarder-computed per-snapshot `_delta`.
- **Play-scoped `start_time`** with per-play resets and pan-back history
  refetch (#587).
- `session_end` → **`play_end`** for the client play-terminal event
  (#554), and the proxy synthesizes a terminal frame on inactive
  timeout (#556).
- **Derived QoE rate-metric columns** in the sessions picker (#563).
- `player_tech_version` (ExoPlayer / Media3 library version) logged.

### 13. Dashboard

- **Sessions list** — hierarchical scenario facet filter
  (OS/device/segment/build/…), categorical-vs-results filters,
  click-to-filter, a Scenario column with platform and test facets, and
  population-aware triage tooling (#783).
- **Compare mode** — grouped plays from `sessions.html` open in the
  session viewer, with per-session variant-peak ladders and a
  "Displayed Variant (Sx)" series.
- **Charts** — event bars on metric charts, a Player State timeline,
  server-loop and lifecycle vertical lines, and bandwidth
  variant-ladder legend ordering.
- **A 4-category event taxonomy** across the viewer and sessions list.
- **Study Report** — a new comparison page, currently developer-only
  (`?developer=1`) while it settles.

### 14. Encoding and delivery

- **A 1-second low-latency rung** — `s1` / `master_1s` for HLS and
  `manifest_1s.mpd` for DASH, on a converged LL generator.
- **Distinct-height geometric ABR ladder**, two-pass software encode,
  and a common-resolution VMAF audit.
- **`--ladder apple` / `apple-uniq`**, a `$ENCODE_STAGING_DIR` default,
  and alignment with the separate Encoder project's
  `apple-uniq-live-xs`. The built-in encoder is now explicitly the
  fallback, not the primary.
- **Dual-rung (avg+peak) filled limit ladder** shared across
  characterization and shape patterns (#551), with top-headroom raised
  from 25% to 50% over top peak.
- `.byteranges` sidecars are **off by default** — DASH now reads
  fragment byte ranges from the manifest itself (#986).

---

**Full changelog:**
https://github.com/jonathaneoliver/infinite-streaming/compare/v2.0.0...v2.1.0
