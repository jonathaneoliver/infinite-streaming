---
name: test-author
description: Author a new characterization test (a `tests/characterization/matrix/*.yaml` arm spec or a `tests/server_behavior/server_*_test.go` case) by walking the test-contract gate with the real knob vocabulary. Invoke when the user says "write/add a characterization test for…", "new matrix for…", "add a server-behavior case for…", "set up an A/B comparing X vs Y", or otherwise wants a NEW test spec authored. This skill owns the intent-quiz + resolved-config echo + dry-run verify BEFORE any run. NOT for running an existing test (→ `make characterize-*` / `harness char matrix`), the unattended fault loop (→ `sweep`), or analysing results (→ `forensics`).
last_reviewed: 2026-06-23
---

# Test-author — walk the test-contract gate when writing a new test

This skill operationalises the **test contract** in [`.claude/standards/characterization-principles.md`](../../standards/characterization-principles.md) ("state this first"). The standard says *confirm intent + echo the resolved config before any run*; this skill carries the **authoritative knob vocabulary** so the quiz is concrete and the echo-back is a filled spec, not prose. The config surface churns constantly — one wrong assumption silently builds a test that measures the wrong thing or passes for the wrong reason.

**Conventions:** follows [`.claude/skills/CONVENTIONS.md`](../CONVENTIONS.md). Most load-bearing here:
- **No guessing (§2)** — quiz the operator on every blank in the contract; never fill one with an assumption.
- **Pass signal is DATA, not a screenshot (§5 / the don't-over-read-screenshots rule)** — the contract's pass/fail line must name a `player_metric` / label / histogram + threshold.
- **Bash discipline (§1)** — lead verify commands with `harness` / `go` / `make`.

**This skill authors specs; it does not run them.** Running is `make characterize-<platform>` / `harness char matrix <file>` / `make characterize-server`; analysis is `forensics` / `characterize-report`.

## Procedure

### 1. Classify — family + class (pick one, never mix)
- **char-matrix YAML** → `tests/characterization/matrix/<name>.yaml`, run by `harness char matrix` / the fleet drivers. Class:
  - `class: config` — benign network/manifest variation (content manipulation, rate caps, pattern ladders, transfer-timeouts). Oracle: any bad QoE label. Targets ABR decision quality.
  - `class: fault` — injected errors (`4xx/5xx`, `corrupted`, transport `drop`/`reject`, `request_*_hang`). Oracle: the recovery-expected envelope.
- **server-behavior** → `tests/server_behavior/server_*_test.go`, run by `make characterize-server`. A control-surface **contract** (delay / loss / rate / fault / pattern / limit / transfer), measured as *baseline + configured-effect within tolerance*.

### 2. Run the contract quiz (from the standard, made concrete — quiz every blank)
- **Behavior under test** — the one thing this run measures.
- **Platform** — `ipad-sim | iphone | appletv | androidtv | web`. (`parallel: true` ⟹ **single-platform** — the fleet boots N sims of ONE platform.)
- **Target held constant** — `content` clip + `duration_s` + `reps` (≥3 before generalizing — principles §2). The clip/endpoint is pinned in `defaults:`, never varied across arms (principles §1).
- **What varies** — the swept knob(s): `axes:` for a cartesian product, or `groups:` / `compare:` / `control:` for an A/B pairing.
- **Pass/fail signal** — the **data** that confirms it (a `player_metric`, a label, a settled-variant histogram) + threshold. Not a green test process.

### 3. Draft the spec (knob reference below), then 4. ECHO the resolved config
Spell out every knob with its namespace, e.g.:
> "config class, ipad-sim, content=insane_new_p200_h264 held constant, 3 reps, 90 s. Varying `axes: proxy.live_offset:[0,24] × is.live_offset:[0,18]` (4 cells). Pass = achieved offset matches the honoured side. `proxy.*` = server, config-on-connect; `is.*` = client, cold relaunch."

Never summarise as "the usual pyramid test".

### 5. Verify before any run
- Matrix: `harness char matrix tests/characterization/matrix/<name>.yaml --dry-run` — prints the expanded arm cartesian; confirm the cell count + per-arm knobs match the contract.
- Server-behavior: name the exact run — `go test ./tests/server_behavior -run TestServer<X> -timeout 10m` (or `make characterize-server`).

## char-matrix knob reference (authoritative — from `internal/charmatrix/spec.go`)

**Top-level / run-level:** `name`, `class` (config|fault), `platform`, `content`, `duration_s`, `reps`, `parallel`, `defaults:` (per-arm baseline), and either `axes:` (cartesian sweep) or `groups:`/`control:`/`compare:` (A/B).

**Client knobs `is.*`** — launch arg, **cold relaunch** on change:
`is.segment` (s2|s6|ll) · `is.protocol` (hls|dash) · `is.codec` (h264|hevc|av1) · `is.live_offset` (client target-latency) · `is.peak_bitrate_mbps` (startup clamp; 0=off) · `is.starts_first_variant` (join first rung vs ABR pick).

**Server knobs `proxy.*`** — config-on-connect, **no relaunch**:
`proxy.live_offset` (manifest hold-back) · `proxy.shape` · `proxy.fault` · `proxy.transfer_timeouts` · `proxy.content_manipulation` (nested), with flat conveniences `proxy.strip_codecs` / `proxy.strip_avg_bandwidth` / `proxy.strip_resolution` / `proxy.allowed_variants` (`drop-top-rung`|`drop-top-<N>`|`keep-bottom-<N>`) / `proxy.variant_order` (default|ascending|descending) / `proxy.overstate_bandwidth`.

**Worked examples (the living templates — read, don't duplicate):** `matrix/precedence.yaml` (axes cartesian + precedence), `matrix/shape-patterns.yaml` (shape), `matrix/pyramid-1-s2-firstvar-cap.yaml` (client caps). `0 means "unset"` for the numeric knobs.

## server-behavior skeleton (from `sb_common_test.go`)

A `TestServer<X>(t)` that: builds a `newProbe(t)`; sweeps the control surface via `setShapeFull(...)` / `patchSession(...)`; measures baseline at the zero/identity setting, then reports each setting's delta over baseline within a stated tolerance; emits `postServerReport(...)`. The contract is a comment at the top ("path-ping RTT ≈ baseline + configured_delay within a few ms"). Mirror the closest existing `server_*_test.go`.

## Out of scope
Running tests, aggregation (`cmd/characterize-report`), result analysis (→ `forensics`/`investigate`), and **Roku**.

## See also
- [`.claude/standards/characterization-principles.md`](../../standards/characterization-principles.md) — the correctness rules a confirmed contract must satisfy (constant-target, n=1, kill/relaunch, cycle labels). Per-mode docs: `abort-`, `startup-`, `retry-backoff-characterization-test.md`.
- [`.claude/standards/server-behavior.md`](../../standards/server-behavior.md) — the control-surface catalogue + calibration baselines.
- `sweep` — once a test class is authored, the unattended loop explores it. `forensics` — analyse a run's results.
