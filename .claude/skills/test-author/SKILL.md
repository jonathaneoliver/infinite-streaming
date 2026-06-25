---
name: test-author
description: Turn a free-form test request into a ready-to-run characterization spec (a `tests/characterization/matrix/*.yaml` arm spec or a `tests/server_behavior/server_*_test.go` case). Say it in one line — "run a pyramid test on 1 isim and jonathan's iphone for 10min", "startup under a 2Mbps cap on ipad-sim" — and this skill FILLS THE BLANKS from the verified defaults + aliases registry below, echoes the resolved spec, and only asks when a blank both changes what's measured AND has no safe default. Invoke on "write/add/run a characterization test", "new matrix for…", "add a server-behavior case", "A/B comparing X vs Y". NOT for running an existing named test (→ `make characterize-*`), the unattended loop (→ `sweep`), or analysing results (→ `forensics`).
last_reviewed: 2026-06-23
---

# Test-author — fill the blanks for a new test, ask only when it matters

Operationalises the **test contract** in [`.claude/standards/characterization-principles.md`](../../standards/characterization-principles.md) — but as a *fill-the-blanks* helper, not an interrogation. Parse the request, resolve every blank from the registry below, **echo the resolved spec so the operator sees what was assumed**, and dry-run verify. The echo-back is the safety net; questions are the exception.

**Conventions:** follows [`.claude/skills/CONVENTIONS.md`](../CONVENTIONS.md) — no-guessing (§2), pass-signal-is-DATA (§5), Bash-first (§1). **This skill authors specs; it does not run them** (running is `make characterize-*` / `harness char matrix`).

## The rule: assume-and-surface, ask-only-on-consequential-ambiguity

1. **Resolve every blank** from the registry. Don't ask for anything that has a safe default.
2. **Surface — always — in the echo** (even when not asked): the resolved **content**, the **platform/device** targets, **parallel vs sequential**, and the **cost** (arms × reps × duration). These are the error-prone fields.
3. **Ask only when** a blank *changes what's measured* and has *no safe default*. The usual triggers:
   - The request names an effect with two distinct mechanisms — e.g. "under a cap" = **network** rate cap (`proxy.shape`, tests bw-estimation) **vs** **client** clamp (`is.peak_bitrate_mbps`, tests config-honoring). Different tests → ask.
   - A real-device target is ambiguous or its `.env` alias doesn't resolve.
   - The run is long/expensive and scope is unclear (e.g. no platform given at all).
   - The pass criterion isn't implied by a known mode.
   When you do ask, ask the ONE consequential question — don't re-quiz the whole contract.

## Defaults & aliases registry (preferred values — VERIFY content live)

| Blank | Default | Notes |
|---|---|---|
| **content** | `insane_newer_p200_h264` | **Always confirm against the live catalogue** (`curl -sk $BASE/api/content`). The example matrices say `insane_new_p200_h264` — that's **STALE / invalid**; never copy content from them. Surface the resolved content every time. |
| **class** | `config` | → `fault` only if the ask mentions errors / 4xx-5xx / corrupt / drop / recovery. |
| **platform aliases** | — | "isim"/"ipad sim"→`ipad-sim`; "N isim"→ipad-sim × N; "jonathan's iphone"/"the iphone"→`platform: iphone` (real device, resolved from `.env IPHONE_XCODE_ID` via the fleet roster, `-launch-mode=appium`); "appletv"→`appletv` (`.env APPLETV_XCODE_ID`); "androidtv"/"google tv"→`androidtv`; "web"→`web`. |
| **multi-platform** | `parallel: false` | A `parallel` matrix MUST be single-platform. Two+ platforms ⟹ sequential legs — surface it. |
| **duration** | `90` s | Parse "Nmin"→N×60, "Ns"→N. |
| **reps** | `3` | n=1 rule (principles §2). "smoke"/"quick"→1. |
| **segment** | `is.segment: s6` | s2 / ll only if stated. |
| **forced flags** | LocalProxy OFF, auto-recovery OFF | The fleet forces these (override via `CHAR_LOCAL_PROXY` / `CHAR_AUTO_RECOVERY`). |
| **mode → shape** | — | `pyramid`→`proxy.shape: {pattern: pyramid, step_seconds: 12, rate_mbps: 1.5}`; `ramp_up`/`ramp_down`/`square_wave`/`transient_shock`→`{pattern: <m>, step_seconds: 12}`; "const N Mbps cap"→`{rate_mbps: N}`; "uncapped"→`{rate_mbps: 0}` (0 = no cap). |

## Procedure
1. **Parse** the one-liner → mode/class, platform(s), duration, any named knob.
2. **Resolve** every blank from the registry; verify content against `/api/content`.
3. **Echo the resolved spec** — a short table: behavior, class, platform(s), content (confirmed), held-constant, what-varies (namespaced knobs), pass signal, cost. Mark anything assumed.
4. **Ask** only the consequential blank(s), if any (per the rule).
5. **Verify** — write the YAML to `tests/characterization/matrix/scratch/<name>.yaml` (gitignored — throwaway by default, no `git status` noise) and `harness char matrix <file> --dry-run` (shows the expanded arms); for server-behavior, name the exact `go test … -run TestServer<X>`.
6. **Promote on request** — specs default to `scratch/`; most are throwaway. After a run the user likes, OFFER to keep it: `git mv tests/characterization/matrix/scratch/<name>.yaml tests/characterization/matrix/ && git add` (now tracked + covered by the `matrix/*.yaml` validation glob). Never promote unasked.

## char-matrix knob reference (authoritative — `internal/charmatrix/spec.go`)
Run-level: `name`, `class`, `platform`, `content`, `duration_s`, `reps`, `parallel`, `defaults:`, `axes:` (cartesian) | `groups:`/`compare:`/`control:` (A/B).
Client `is.*` (launch arg, **cold relaunch**): `is.segment` · `is.protocol` (hls|dash) · `is.codec` (h264|hevc|av1) · `is.live_offset` · `is.peak_bitrate_mbps` (0=off) · `is.starts_first_variant`.
Server `proxy.*` (config-on-connect, **no relaunch**): `proxy.live_offset` · `proxy.shape` (object-axis: a whole shape block, optional `label:`) · `proxy.fault` · `proxy.transfer_timeouts` · `proxy.allowed_variants` (drop-top-N|keep-bottom-N) · `proxy.variant_order` · `proxy.strip_*` · `proxy.overstate_bandwidth`. `0 = unset` for numeric knobs.
**Living examples:** `matrix/precedence.yaml`, `matrix/shape-patterns.yaml`, `matrix/pyramid-1-s2-firstvar-cap.yaml` (copy structure, NOT their stale `content:`).

## server-behavior skeleton (`sb_common_test.go`)
`TestServer<X>(t)`: `newProbe(t)`; sweep the control surface via `setShapeFull`/`patchSession`; measure baseline at the identity setting, report each setting's delta within a stated tolerance; `postServerReport`. Contract as a top comment ("RTT ≈ baseline + configured_delay within a few ms"). Mirror the closest `server_*_test.go`.

## Out of scope
Running tests, aggregation (`characterize-report`), analysis (→ `forensics`/`investigate`), and **Roku**.

## See also
- [`.claude/standards/characterization-principles.md`](../../standards/characterization-principles.md) — correctness rules a resolved spec must satisfy (constant-target, n=1, kill/relaunch). Per-mode: `abort-`, `startup-`, `retry-backoff-characterization-test.md`.
- [`.claude/standards/server-behavior.md`](../../standards/server-behavior.md) — control-surface catalogue + baselines.
- `sweep` (explore an authored class) · `forensics` (analyse a run).
