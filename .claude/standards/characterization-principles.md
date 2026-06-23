# Characterization-test principles

Cross-test rules that apply to every characterization driver under `tests/characterization/modes/*`. **Read this before writing or modifying any characterization test.** Per-test docs (`abort-characterization-test.md`, `startup-characterization-test.md`, `retry-backoff-characterization-test.md`, …) inherit these rules; they only need to document their own variables.

## The test contract — state this first

Before writing or modifying any test — a `matrix/*.yaml` arm spec, a `modes/*_test.go` driver, or a `tests/server_behavior` case — state the contract below and get an explicit ✓ from the operator. **Quiz on any blank; do not fill it with an assumption.** The config surface (20+ matrix arms, per-arm knobs like `firstvar-cap` / `noavg` / `2sim`, server-behavior params) changes often enough that one wrong assumption silently produces a test that measures the wrong thing — or passes for the wrong reason.

State it in one short block:
- **Behavior under test** — the single thing this run measures (e.g. "startup variant selection under a 2 Mbps cap").
- **Platform / device** — iPhone (real), iPad (sim), Android, or server-only. Real vs sim changes the launch path (`-launch-mode=appium`) and what's trustworthy.
- **Boundary + reps** — `app_cold` / `channel_change`, and `CHAR_<TEST>_REPS` (≥3 before generalizing — §2).
- **What VARIES vs what's HELD CONSTANT** — the knob being swept (the point of the run) and everything pinned across arms/cycles (clip, endpoint, caps — §1). If you can't name what's held constant, the run is unanalysable.
- **Pass/fail signal** — the DATA that confirms the behavior (a player_metric, a label, a settled-variant histogram) and its threshold — NOT a screenshot or a green test process.

Then **echo back the resolved config** — the literal arms / `-is.flag.*` launch args / `CHAR_*` env you produced (the actual YAML), so the operator confirms what was set *before* any run. "Pyramid shape, s2 segments, 3 reps, firstvar-cap on, holding clip=X" — spelled out, never summarized as "the usual pyramid test".

This gate is the test-authoring sibling of the data-contract gate; the numbered sections below are the correctness rules a confirmed contract must then satisfy.

## 1. The constant-target rule

**Every cycle in a characterization run measures the SAME target — same clip, same starting state, same network endpoint.** The boundary / fault / variant / cap / cycle index is the ONLY thing that varies between cycles.

Why this is non-negotiable:
- Cross-cycle comparison is the entire point of characterization. If app_cold cycle 1 measures clip A and channel_change cycle 2 measures clip B, any difference between them conflates "boundary type" with "clip-specific segment sizes / variant ladder / origin caching / etc." The result is unanalysable.
- Per-run aggregation (median ttff, settled-variant histogram, etc.) requires identically-prepared samples. Different content invalidates the aggregate.
- Manifest-derived cap matrices (e.g. computeStartupCaps reading the target's variant bitrates) only make sense if every cycle uses the manifest of THE target. A "setup" clip in a channel_change cycle is scene-setting only; its variant ladder must not influence cycle decisions.

How this shows up in code:
- A `defaultClipTarget` constant near the top of the test driver. Configurable via env (`CHAR_<TEST>_CLIP_TARGET`) but defaulted, never randomized.
- The cycle loop never picks the clip — it picks the boundary/fault/cap. The clip is set once at run start.
- Any "setup" or "warmup" content is named distinctly (`defaultClipSetup`) and the doc explains it isn't the measurement target.

If you find yourself wanting to vary the clip across cycles, that's a separate characterization run, not one with multiple variables. File a new test.

## 2. n=1 is not a pattern

A single cycle's values are a data point, not a conclusion. Don't claim player behavior or generalize to "the player does X" without at least 3 identical-conditions repetitions (`CHAR_<TEST>_REPS=3+`). ABR jitter, transient TCP behavior, and AVPlayer's internal scheduling all produce per-run variance large enough to invent false patterns.

Report raw cycle values without interpretation. Aggregation comes from the dashboard or a deliberate cross-run query, not from the test driver's log line.

Cross-link: [[~/.claude/projects/-Users-jonathanoliver-Projects-smashing/memory/feedback_n1_not_a_pattern.md]].

## 3. Cumulative load + the kill-relaunch boundary

Long-running characterization tests that don't `appium.Kill` between reps see the player degrade by cycle 6–7 — "have 0 players", AVPlayer surfaces blank, accessibility tree empty. The simulator accumulates state (URLSession leaks, app-side memory growth, half-torn-down AVAsset references) that the test doesn't otherwise control for.

Rule: any test running ≥4 cycles SHOULD kill + relaunch the app between reps, OR document why it doesn't and cap the rep count at 3.

Exceptions:
- `channel_change` cycles INSIDE a startup test legitimately keep the app alive — that's the boundary they're measuring. The cycle still does its own kill+launch before the NEXT `app_cold` cycle.
- A test specifically characterizing cumulative-load behavior (e.g. "how many channel changes until the player wedges") shouldn't kill. Document it.

## 4. Same observation window per cycle type

Per-test cycle observation windows (e.g. 30s for startup, 60s for abort) MUST be constant across all cycles of the same type within a run. Don't shorten "because cycle 7 was already settled" — you lose the comparability with cycle 1's full window.

If a cycle reaches an unambiguous terminal state (the player stalled, the play ended, the variant settled and held for ≥10s), the test may early-exit and record the early-exit reason — but the FIELD VALUES still come from a window that ends at the configured boundary, not at the early-exit instant.

## 5. Bandwidth annotations on every variant log line

Every log line that names a target variant, resolution, or cap MUST include the matching average + peak Mbps from the variant manifest, plus the active cap:

```
first_var=3840x2160  (avg=21.299 peak=29.857 cap=3.000)
```

Use `runner.AnnotateVariant(bws, resolution, capMbps)` — don't roll your own. This is what makes a row of test output operationally readable; without it the reader has to cross-reference the manifest manually.

## 6. Per-cycle field-set discipline

Per-test cycle result structs (`StartupCycleResult`, `RetryCycleResult`, `AbortCycleResult`, …) are append-only. Adding a field is fine; renaming or removing a field invalidates archived runs. If you must rename, add the new field, keep the old one populated for at least one release, and update the per-test standards doc's "Per-cycle fields" table in the SAME commit.

Cross-link: `.claude/standards/<test>-characterization-test.md § Per-cycle fields`.

## 7. Standards-doc-per-test

Every test under `modes/` has a matching `.claude/standards/<test>-characterization-test.md`. The doc must cover:
- What the test models in real-world terms
- The independent variable(s) and what's held constant
- Each per-cycle field, its source, and what it tells you
- How to interpret a row
- Known anomalies / limitations
- Pointers to the driver, runner helpers, AX-ids the test depends on

Skip the doc and the test result is unreadable by anyone other than the author at write-time. Standards docs are read by both the dashboard's row-detail UI and by future-me looking at archived runs.

## 8. Constant-target ⊕ variable-fault, never both

If a test is varying the fault shape across cycles (abort test: 5 fault types), it MUST hold the clip constant (rule 1). If a test is varying the clip (a hypothetical content-comparison run), it MUST hold the fault shape constant. Two variables = unanalysable.

The corollary: per-cycle env-var overrides should let you change ONE axis at a time. `CHAR_<TEST>_FAULTS=foo,bar` is fine; combining with `CHAR_<TEST>_CLIPS=a,b` in the same run is not.

## 9. Standard cycle label schema

Every characterization test marks its cycle boundaries through the **player labels** surface — the same surface `harness labels set` writes via PATCH `/api/v2/players/{id}`. The forwarder emits a `label_changed` control_event on every PATCH, so the cycle timeline lands in `control_events` for free.

PATCH semantics are **merge with overwrite**. Writing `cycle_id=B` after `cycle_id=A` replaces A with B on the player record — the player's current `labels` map only ever shows the LATEST cycle. History lives in `control_events.label_changed` rows, one per change, timestamped.

### Required label keys at cycle start

Every cycle's start-PATCH must set:

| Key | Value format | Example | When |
|---|---|---|---|
| `test` | identifier | `startup`, `abort`, `retry`, `rampup` | once per run; constant across cycles |
| `cycle_id` | `<test>:<axis>:<cap>:<rep>` | `startup:app_cold:cap30:rep2` | **per-cycle, the joinable key** |
| `cycle_idx` | integer | `4` | per-cycle |
| `rep` | integer | `2` | per-cycle |
| `cap_mbps` | integer or `none` | `30`, `none` | per-cycle if a cap applies |
| `boundary` | identifier | `app_cold`, `channel_change` | startup-style tests |
| `fault` | identifier | `server_timeout`, `request_body_reset` | abort/retry/cascade tests |

Values MUST contain only `[A-Za-z0-9_:.-]`. **No `,` and no `=`** — `LabelPlay` silently drops the offending value (see `~/.claude/projects/-Users-jonathanoliver-Projects-smashing/memory/reference_labelplay_value_encoding.md`). Use `:` as the in-value separator, never `,`. Multi-value labels (lists of caps, lists of variants) are forbidden — split into one label per value or hash.

### Cycle boundary helper

Drivers MUST call `runner.StartCycle(ctx, sess, runner.CycleID{…})` at cycle start and `runner.EndCycle(ctx, sess)` at cycle end. These are the only sanctioned ways to mutate cycle labels. Direct `sess.LabelPlay(…)` calls for cycle metadata are a code smell — they bypass the schema check and let inconsistent keys leak.

The helper:
- PATCHes the full required label set in ONE call (one `label_changed` row per cycle start, not N).
- Sets `cycle_id=""` on `EndCycle` so the band has an explicit closing edge in the dashboard render (a `label_changed` with empty cycle_id terminates the band).
- Returns the wall-clock `started_at` so the cycle struct records the same instant the `label_changed` row was emitted.

### How the dashboard reads cycles back

The session-viewer's cycle-band overlay queries `control_events` where `event = 'label_changed'` and `info` contains `"cycle_id"`. It then walks the rows chronologically: each non-empty `cycle_id` opens a band that ends at the NEXT `cycle_id` change (or at end-of-window). The chip on the band shows `cycle_id` verbatim.

This means consumers don't need cycle_start / cycle_end as distinct event types — the existing `label_changed` semantics already encode "cycle boundary." Don't introduce new event types unless a future test needs metadata that can't fit on a label value.

### OpenTelemetry spans (parallel transport)

Issue #493. Every cycle ALSO emits an OpenTelemetry `cycle` span with
the same semantic content as the cycle_id label (test, boundary,
fault, cap_mbps, cycle_idx, rep). Cycle spans nest under a per-test-
invocation `test_run` span carrying the run-scope labels. Failed
cycles get `span.status = error` so trace backends surface them in
default filtering.

Spans are additive — the `label_changed` control_events path keeps
working unchanged; the dashboard's CycleBandsRail still reads from
control_events. The trace surface is for cross-cycle aggregates,
TraceQL queries, and CI integration that the bespoke control_events
SQL doesn't offer.

Configuration via env vars (in `tests/characterization/runner/otel.go`):
- `CHAR_OTEL_ENDPOINT` — OTLP HTTP collector URL (e.g.
  `http://localhost:4318`). Spans stream to the configured backend
  in real time.
- `CHAR_OTEL_STDOUT` — non-empty enables the stdout exporter
  (debug only — pollutes test output).
- `CHAR_OTEL_DISABLE` — non-empty forces the no-op tracer.

With nothing set, spans accumulate in-memory and are dropped at
shutdown — same effective behavior as before #493, zero cost. Point
at a local Jaeger (`docker run --rm -p 4318:4318 -p 16686:16686
jaegertracing/all-in-one:1.x`) to view the trace.

### What does NOT belong in cycle labels

- **Free-form notes / descriptions** — go in `harness finding add` instead.
- **Variant lists, cap matrices, ladder shapes** — these change per-cycle but explode the label space. Put them in the cycle result struct (`StartupCycleResult` etc.); the label set stays a stable, low-cardinality identity.
- **Wall-clock timestamps** — `label_changed.ts` already carries this.
- **Anything that varies sub-second** — labels are persisted on a PATCH; if it changes faster than that, it's a sample, not a label.

## See also

- `.claude/standards/startup-characterization-test.md` — startup-specific
- `.claude/standards/abort-characterization-test.md` — abort-specific
- `.claude/standards/fault-injection-wire-contract.md` — fault wire-format contracts (DO NOT CHANGE rules)
- `tests/characterization/runner/variants.go § AnnotateVariant` — the bandwidth-annotation helper
- `~/.claude/projects/-Users-jonathanoliver-Projects-smashing/memory/feedback_n1_not_a_pattern.md` — the n=1 rule in user memory
