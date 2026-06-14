---
name: sweep
description: Drive the automated fault-injection sweep (issue #772, docs/sweep-design.md) — the unattended claim→apply→probe→analyze→isolate→promote loop over the ClickHouse-master queue. Invoke under /goal ("drive the sweep until backlog is empty"), or for a single hand-run iteration. Each clean run is a mechanical oracle check (no model call); the LLM only reasons on a notable/aberration hit — picking which axes to flip for isolation and writing the finding. Runs on the Mac with the sims (drives appium/adb), against the test-dev deploy.
last_reviewed: 2026-06-14
---

# Sweep — drive the automated fault-injection loop

This skill is the **driver** for the sweep designed in `docs/sweep-design.md` (issue #772). **ClickHouse is the master queue** (CH-master migration) — every `harness sweep` subcommand reads/writes it over the forwarder API; there are no local files. **You** (the LLM, under `/goal`) are the investigator — but only on a hit: a clean run is a pure oracle check with no reasoning, so cost scales with *findings*, not the (vast) experiment count.

> **The queue is remote.** Because the queue lives in CH on the deploy, every `harness sweep` command needs the deploy URL + self-signed-cert skip. Export `HARNESS_BASE_URL=https://dev.jeoliver.com:21000` and `HARNESS_INSECURE=1` (per `.env`) so the bare `harness sweep …` commands below resolve — or pass `--insecure --base …` inline. Operators control *what runs* by toggling scope on the dashboard Sweep tab (or `POST /api/v2/sweep/scope`); disabled platform/protocol/class/mode values stay pending but are never claimed.

**Conventions:** follows `.claude/skills/CONVENTIONS.md`. Most load-bearing here:
- **Bash discipline** — lead every command with `harness`, `go`, `gh`, or `jq` (first token matches the allowlist). Never `cd …`/`VAR=…`/`export …` prefixes; pass values inline.
- **No guessing during triage** (§2) — tag every causal claim `confirmed`/`refuted`/`needs-test`; the `n=1 is not a pattern` rule is enforced mechanically by the confirm-reps step below.
- **Recall before investigate** — grep `.claude/findings/` + `.claude/memory/` before reasoning about a hit (the `forensics`/`finding` skills).

## Where this runs

On the **Mac with the sims + attached devices** (the probe drives appium/adb/WDA — cloud CI can't). Target is the **test-dev deploy** (`HARNESS_BASE_URL=https://dev.jeoliver.com:21000`, per `.env`). Single content (`insane_new`) for now. Depth-first: a narrow seed, isolation/bisect dominate, so you can *watch the investigate→insert→re-run chain work* before widening.

## Two classes — pick one, don't mix (§0 of the design)

- **`config`** (default) — realistic stream + benign-network variation: content manipulation, rate caps (floor-guarded — never starve below the lowest sustainable rung), pattern ladders, server transfer-timeouts. **No** injected errors, **no** delay/loss. Oracle: any bad QoE label is the signal. Targets ABR decision quality / over-downshift / manifest-config robustness.
- **`fault`** — explicit error-recovery: `4xx/5xx`, `corrupted`, `connection_refused`, `dns_failure`, `rate_limiting`, transport `drop`/`reject`, `request_*_hang`. Oracle: the recovery-expected envelope (a fault the player should survive isn't a finding; failing to recover is).

Seed one class at a time: `harness sweep seed --class config` (default) or `--class fault`. Findings are namespaced `sig:<class>-…` so they never dedup-collide. Default to `config` unless explicitly chasing error-recovery.

## The loop (one iteration)

Run these in order. Under `/goal`, repeat until `harness sweep status` shows `backlog 0` (and any in-flight `running` drained), then stop — the sweep is state-driven, not clocked.

### 0. Reap + health-check
```
harness sweep reap --max-age-min 60
```
Returns claims orphaned by a dead runner. Then confirm the deploy + a sim are healthy; if not, don't blame the player — skip this tick.

### 1. Claim the top-scored experiment
```
harness sweep next --claim --owner <runner-id>
```
Server-side concurrency-safe claim (the forwarder arbitrates a deterministic winner, so parallel runners never double-claim; scope-disabled values are skipped). `nothing to claim` ⇒ backlog empty/gated ⇒ you're done. It prints the claimed `exp_id`; `bootstrap`/`apply` reload the full recipe from CH by id — you don't need to capture it here.

### 2. Materialise the recipe, then drive the probe (config-on-connect)
The probe is the **characterization harness**. The robust path for ANY recipe — including arbitrary fault + content_manipulation, not just shape — is **config-on-connect**: configure the session BEFORE the app launches, then launch the app bound to that same `player_id`. The cap/fault/content is live from the player's first byte, no PATCH race.

```
harness sweep bootstrap <exp-id> --player <uuid>     # mints uuid if omitted; --group G for A/B
```
This GETs the bootstrap master URL on the shaper port carrying a full-fidelity `proxy.cfg` patch built from the recipe — slider shape, HTTP fault, `content_manipulation`, and the `sweep=1`/`exp_id`/`kind`/`arm`/`group`/`why=…` labels (slug-safe; `testing=` tier, no good/bad tint). Verified to materialise all four onto a live test-dev session. It prints the `player_id` to launch with.

Then drive the probe bound to that id. The sweep's generic probe is **`TestSweepProbe`** — it reattaches to the bootstrapped session (no re-config), optionally arms the recipe's **pattern** (the bandwidth motion), plays for a window, and logs the play_id + a session-viewer URL:
```
CHAR_PLAYER_ID=<from bootstrap> HARNESS_BASE_URL=… LAUNCH_MODE=appium \
CHAR_CONTENT=insane_new_p200_h264 CHAR_SWEEP_DURATION_S=90 \
CHARACTERIZATION_DEVICE_UDID=<booted sim> \
CHAR_SWEEP_PATTERN=pyramid CHAR_SWEEP_STEP_S=12 CHAR_SWEEP_MARGIN=5 \
go test ./tests/characterization/modes -run TestSweepProbe -count=1 -v -timeout 8m
```
(`LAUNCH_MODE`: `appium` for iOS-sim + Apple TV; `cli`/`adb` for Android TV.) For a **pattern recipe** (`shape.pattern` set), pass `CHAR_SWEEP_PATTERN` (+ `_STEP_S` / `_MARGIN` from the recipe's `shape`); the probe waits for the manifest then arms the pattern via `harness shape --pattern` — the same path the characterization modes use, so the cap actually sweeps. Omit `CHAR_SWEEP_PATTERN` for a plain-play recipe (rate-cap / content-only). **Verified end-to-end** against test-dev + a booted iOS sim, both plain-play and a pyramid that drove real downshifts → `analyze` → verdict.

> Pattern shapes still apply post-launch via `harness shape --pattern` (they need the fetched manifest's ladder). For an ALREADY-live player, `harness sweep apply <id> --target <player>` does the reset-then-apply variant.

**Capture the `play_id`** (the *play*, distinct from the bootstrapped `player_id`) from the Report JSON (`tests/characterization/modes/artifacts/<mode>-<platform>-<short>-<runid>-cyc1.json`, field `play_ids[0]`) or the test log line `play_id: <uuid>`. No `play_id` / probe crash ⇒ **`inconclusive`** (infra, not the player): return the file to backlog for a bounded retry, then `review/`. Don't analyze.

### 3. Analyze — the oracle verdict (mechanical, no reasoning)
```
harness sweep analyze <exp-id> --play <play_id> --confirm-reps 3
```
Pulls the play's QoE labels, classifies the trichotomy (`clean`→`done/`, `notable`/`aberration`→`found/`), and — on a *first* single-rep hit — enqueues 3 confirmation reps to `backlog/` (the n=1 guard, sharing a `rep_group`; reps don't recurse). A `clean` verdict ends the iteration here. **This step needs no LLM judgment.**

### 4. On a confirmed hit — investigate + insert (this is where you reason)
A hit is *confirmed* only once the rep batch agrees (don't act on n=1). Then:

1. **Recall** — grep `.claude/findings/` + `.claude/memory/` for the signature/symptom (`forensics` skill). If a prior finding explains it, say so and skip to promote.
2. **Reason about the cause** from the evidence — *which* labels fired, *when* in the play, on *which* request kind — and pick the most-informative axes to flip. This is the OFAT isolation fan; it is **LLM-reasoned, not a fixed checklist** (a startup VSF on a 4K ladder → flip `platform` + `ladder` first; a mid-play freeze after a drop → vary drop duration + `liveoffset`, not codecs). Cheap/likely-first: Tier 1 = `platform`/`protocol` (different devices → simultaneous), Tier 2 = manifest knobs.
   ```
   harness sweep isolate <exp-id> --flip platform=androidtv --flip ladder=drop-top-rung --flip protocol=dash
   ```
   This materializes a `control` + one one-axis-flip `variant` per flip into `backlog/` (each enforced to differ from control in exactly one axis; capped at 8). The scheduler will pick them up *next* (they outrank seeds), so the chain runs itself.
3. **Promote** to a deduped Issue:
   ```
   harness sweep promote <exp-id> --dry-run        # inspect signature + body first
   harness sweep promote <exp-id> --axis platform  # append the attributed axis once isolation confirms it
   ```
   Comments on the open Issue carrying the signature label if one exists, else creates one (`sweep` + `sig:…` + `bug`/`notable`). Uses `--body-file` (heredoc rule honored internally).

### 5. Drain
When `harness sweep status` shows no `backlog`/`running`, summarize what was found and **stop** (don't reschedule — that's `/loop`'s job as an outer cadence, not this inner loop).

## What you do NOT do
- Don't FIFO — always `sweep next` (discovery-first scoring).
- Don't promote on n=1 — wait for the confirm-reps batch.
- Don't run an unfaulted play and call it a fault test (the integration-seam note above).
- Don't hand-pick a fixed isolation checklist — reason from the specific failure.

## See also
- `docs/sweep-design.md` — the full design (oracle trichotomy §3, store §4, scheduler §5, A/B isolation §6, the loop §7, robustness/prereqs §11).
- `forensics` / `investigate` / `finding` / `triage` — the reasoning + capture skills this loop reuses on a hit.
- `shape` / `fault` — the underlying control surfaces `sweep apply` wraps.
- `harness sweep help` — the subcommand surface.
