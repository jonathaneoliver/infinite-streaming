# Automated fault-injection sweep — design (issue #772)

> **Status:** design approved; Phase 2 build in progress on `feat/772-fault-sweep`. The first milestone is the
> depth-first vertical slice (§10).
>
> **Terminology:** the runnable work-unit is an **experiment** (one `{platform × protocol × fault × shape ×
> content × HLS-config × duration}` recipe, run once and observed). Not a GitHub Actions "job." A confirmed
> failure becomes a durable **finding** (a deduped GitHub Issue). Experiments are ephemeral; findings are durable.

## Context

Today fault testing is manual: an operator applies one fault via the harness CLI, watches a player, tries a
variant. The matrix that *should* be swept — `platform × protocol × fault-type × bitrate/shape × content ×
HLS-config × duration` — is combinatorially huge, and each run is multi-minute (characterization modes ~1.5–6
min; cold-start/abort cycles longer). Brute force is infeasible, so **experiment ordering is the core
intelligence**. We want an **unattended, state-driven loop** that claims an experiment, runs it against a
faulted origin, drives a player headlessly, reads the QoE telemetry, and — on an aberration — **systematically
attributes the cause via A/B comparison** (platform-specific? content-specific? tied to an alterable HLS aspect
like live-offset or ladder?) before promoting a confirmed bug to a deduped Issue. The loop drains when the
queue has no Backlog left.

All the hard primitives already exist (fault injection, headless probe, QoE-label oracle, content-manipulation
knobs, broadcast groups, compare-mode viewing, capture skills). This system is an **orchestration layer** over
them — minimal new code.

---

## 0. Two sweep classes (the load-bearing scope decision)

Experiments belong to exactly one **class**. The two never mix — different purpose, different recipe
vocabulary, different oracle — and are seeded + run one class at a time (`harness sweep seed --class …`). The
`config` class is the default and primary focus.

| | **`config`** (default) | **`fault`** (separate) |
|---|---|---|
| Question | Does the player make **good decisions**? | Does it **recover from errors**, and are there bugs in recovery? |
| Knobs | content manipulation; rate caps (floor-guarded); pattern ladders (pyramid/ramp/…); server **transfer-timeouts** | explicit HTTP/connection errors: `4xx/5xx`, `corrupted`, `connection_refused`, `dns_failure`, `rate_limiting`, transport `drop`/`reject`, `request_*_hang/reset` |
| Oracle | **any** envelope-violating QoE label = the signal (§3) | the **recovery-expected envelope** (A.4) — a fault *within* envelope is fine; failing to recover, or a bug in recovery, is the signal |
| Target class of bug | ABR rung-choice quality, the over-downshift class, manifest-config robustness | resilience + recovery correctness |

**Explicitly out of scope for *both* classes:** packet **loss %** and **delay** (steady network degradation —
neither a realistic stream config nor an explicit error), and **any cap that totally chokes throughput**. A
config-class static rate cap is only ever placed **at or above the lowest variant's sustainable rate** (the
floor guard): the player can always play *something*, so we test decision quality and recovery, never forced
starvation. Pattern sweeps are built from the actual variant ladder, so they stay within sustainable rungs by
construction.

Findings are namespaced by class in the signature (`sig:<class>-…`), so a config finding and a fault finding
never dedup-collide.

---

## 1. How faults are applied, per protocol

Per-session (per proxy port) via the v2 REST API; the **harness CLI** is the programmatic entry point.

- **HTTP faults** — `POST /api/v2/players/{id}/fault_rules` (`go-proxy/internal/v2/server/handlers_fault_rules.go:26`).
  Types in `translate_faults.go:35-55`: `403/404/500/503`, `timeout`, `corrupted` (segment-only),
  `connection_refused`, `dns_failure`, `rate_limiting`, `request_{connect,first_byte,body}_{hang,reset,delayed}`.
  Scope via `filter.request_kind` + `url_match`; cadence via `frequency`/`mode`/`consecutive`.
  CLI: `harness fault add <target> --type … --kind … --frequency …` (`tools/harness-cli/cmd/harness/fault.go`).
- **Traffic shaping** — `PATCH /api/v2/players/{id}` `{shape:{rate_mbps,delay_ms,loss_pct,transport_fault,pattern}}`
  (`handlers_mutate.go:332`). `transport_fault` = nftables `drop|reject`; `pattern` = timed bandwidth-ladder
  steps. CLI: `harness shape <target> --rate|--delay|--loss` or `--pattern … --step-seconds …` (`shape.go`).
- **Content / HLS-aspect manipulation** — the proxy rewrites the master manifest per session via
  `manipulateHLSMaster` / `manipulateDASHManifest` (`go-proxy/cmd/server/main.go:6563/6828`): knobs are
  `liveOffset`, `allowedVariants` (ladder density), `variantOrder` (HLS-only), `stripCodecs`,
  `stripAvgBandwidth`, `stripResolution`, `overstateBandwidth`. **These are the levers for HLS-aspect
  comparison (§6).** (Being folded into a `ContentManipulation` struct under in-flight #767/#766.)
- **Composed timed profiles** — `harness procedure {soak|abr-sweep|fault-soak}` (`procedure.go`).
- **Protocol** is selected by manifest URL extension at nginx (`docker/nginx-content.conf.template:110/123`):
  `master.m3u8` → HLS, `manifest.mpd` → DASH. An experiment carries `protocol` → picks the URL the probe plays.
- **Concurrency:** every PATCH needs an `If-Match` ETag; `shape`/`fault_rules` edits don't collide.
  **Group broadcast** (`?broadcast=`, `harness groups create`) fans an identical profile to a fleet — the
  mechanism that lets an A/B pair run under *identical* shaping simultaneously (§6).

## 2. What plays back / probes it

The **characterization harness** (`tests/characterization/`) is the probe. It mints a fresh `player_id`,
applies the shape *before* the manifest fetch, runs a **1 Hz sampler** (`runner/sample.go`) capturing
state/buffer/stalls/bitrate/resolution/TTFF/frames + per-state residency ms, and POSTs a `Report`.

- **Launch mode:** **`appium` for iOS Simulator AND Apple TV**; **`adb`/`cli` for Android TV**.
  iOS-sim-via-appium uses a WDA against the booted sim (confirmed working). Web has no headless mode →
  manual-review lane (§7).
- **An experiment "recipe" = characterization mode** (`steps`, `transient_shock`, `downshift_severity`,
  `hysteresis_gap`, `emergency_downshift`, `startup`, `abort`, …) **× fault/shape profile × content ×
  HLS-config × protocol × platform**.

## 3. Telemetry + the aberration oracle

The queryable aberration signals are **synthesized QoE labels** computed at forwarder ingest, stored on
ClickHouse `session_events` (`analytics/go-forwarder/qoe_labels.go`, thresholds `qoe_thresholds.go`). OTel
(`runner/otel.go`, opt-in `CHAR_OTEL_ENDPOINT`) is only a cross-cycle trace view for humans, not the oracle.

**Oracle A — absolute (reuse QoE labels).** After a run, `harness query events <play_id> --label-has …`
(`query.go`). A run is an **aberration** iff it carries an envelope-violation label:

| Aberration kind | Label(s) |
|---|---|
| startup failure | `error=*qoe_vsf` |
| mid-play failure / no-recovery | `error=*qoe_msf`, `error=player_error` |
| excess rebuffer | `critical=*qoe_cirr_breach`, `critical=*qoe_cirt_breach` |
| stall burst / frozen | `critical=*qoe_stall_burst`, `critical=stall_frozen` |
| ladder collapse | `warning=*qoe_min_variant_stuck`, `warning=*qoe_downshift_storm` |
| auto-recovery restart | `critical=restart_auto_recovery` |

The **kind** seeds the finding signature (§4). `*qoe_tier_premium` = clean. A/V desync, visual crash →
`needs-human` → manual-review lane. Reuses production semantics.

**The envelope applies to the `fault` class only.** A `config`-class run has no injected error to forgive, so
**any** envelope-violating label above is the signal directly — and the floor guard (§0) means a config run is
never starved into a "deserved" stall. A `fault`-class run instead is judged against the per-fault
recovery-expected envelope (starter table A.4): a fault the player *should* survive isn't a false positive; the
signal is failure-to-recover (or a bug in recovery).

**Oracle B — differential (for paired A/B experiments, §6).** Some questions have no absolute "bad" — e.g.
*does ABR behave the same with vs without `AVERAGE-BANDWIDTH`?* Here the signal is **divergence between the two
arms**, not a QoE breach. The differential oracle compares the two grouped plays' Reports (`runner/report.go`):
per-step `variant_idx` timeline, `mean_bitrate_mbps`, `profile_shifts`, `total_stalls`, `lowest_sustainable_cap`.
Divergence beyond a threshold (default in §9, tunable) = a finding ("dropping AVERAGE-BANDWIDTH changes ABR on
platform X"). This is what makes a clean-vs-clean comparison productive.

**Oracle C — surprise / novelty (highlight unexpected, non-error behaviour).** Not every notable result is an
error. The classic example is **iOS over-downshift** — the player drops 2160→1080 (skips fine rungs) and then
over-corrects, all while playback "succeeds." No error, no A/B partner, but it's the kind of thing we want
flagged. Two cheap signals already exist:
- **`warning`-tier QoE labels** — `*qoe_downshift_overshoot` (settles ≥N rungs below cap), `*qoe_downshift_storm`
  (churn), `*qoe_min_variant_stuck`, `*qoe_abr_conservative`, `*qoe_ladder_gap`, `*qoe_throughput_divergence`.
  These are "unexpected but tolerated." Today they don't fail a run; here they make it **`notable`**.
- **VOMM surprise scorer** (`analytics/tools/scorer.py`, PPM-C `surprise_rate`/`peak`) — flags runs whose
  state-transition sequence deviates from a "clean" reference corpus, catching novel patterns no threshold
  anticipated. Run post-hoc on the experiment's network/event token stream.

So each run resolves to a **trichotomy** (three-way outcome): **`clean`** (only `info`/`*qoe_tier_premium`),
**`notable`** (a `warning`-tier label *or* a high VOMM surprise score, no error), or **`aberration`** (`error`/
`critical`). `notable` outcomes are surfaced and can spawn their own A/B isolation fan (§6) to characterize the
surprise — exactly how we'd chase whether the over-downshift is iOS-specific, ladder-density-driven, etc.

## 4. Experiment record + the store

> **⚠️ Superseded — migrated to ClickHouse-master (#772 CH-master).** This section's original design (a local
> `.sweep/` directory of JSON files, atomic-`rename` claim, `harness sweep publish` to mirror into CH) was
> replaced: **ClickHouse is now the single master store.** The `harness sweep` CLI reads/writes the queue over
> the forwarder API (`/api/v2/sweep/{experiments,claim,scope,delete}`); the `status` is a column, the full
> recipe is `raw_json`, claims are arbitrated server-side (a `sweep_claims` ledger, deterministic
> `argMin(owner)` winner — no file lock), and there is no `publish` step (the dashboard is always live). Scope
> gating (`sweep_scope`) lets the dashboard enable/disable platform/protocol/class/mode. Wherever the text
> below says "`.sweep/` files / atomic-rename / publish", read "CH rows / server-side claim / always-live". The
> status *names* (backlog → running → done/found/review/feedback) and every other concept are unchanged.

Experiments live in a queue keyed by `exp_id` (no GitHub board). The status buckets:

```
.sweep/
  backlog/   <experiment-id>.json   # awaiting a runner; scheduler sorts these by score
  running/   <experiment-id>.json   # claimed (the file's presence here + owner field = the lock)
  done/      <experiment-id>.json   # clean, no aberration
  found/     <experiment-id>.json   # aberration confirmed → promoted to an Issue
  review/    <experiment-id>.json   # manual-review lane (Web / un-provisioned ATV / needs-human)
  feedback/  <experiment-id>.json   # awaiting a human severity rating (§8)
```

Each `<experiment-id>.json` (the full recipe + bookkeeping):

| Field | Purpose |
|---|---|
| `id`, `created_at` | identity |
| `class` | `config` (default) / `fault` — the sweep tier (§0); never mixed |
| `platform`, `protocol`, `content`, `mode`, `duration` | the recipe (the matrix axes) |
| `content_manipulation`, `shape` (rate/pattern), `transfer_timeouts` | config-class knobs |
| `fault` | fault-class only (nil on a config experiment) |
| `kind` | `seed` / `isolation` (OFAT probe) / `hypothesis` (proactive A/B) / `bisect` (recursive) |
| `arm`, `group` | `control`/`variant` + the A/B group id pairing them (§6) |
| `reps`, `rep_group` | confirmation reps this experiment wants (1 for seed; ≥3 to confirm) + the id tying a rep-batch together (§5) |
| `depth`, `parent` | recursive bisection depth (0–3 bound) + origin experiment id |
| `score` | scheduler sort key (§5), recomputed each pass |
| `owner` | runner/worktree id stamped at claim |
| `play_id`, `result` | filled after the run (the play + oracle verdict) |

**Findings stay durable on GitHub:** a confirmed aberration is promoted to a deduped **Issue** labeled `sweep` +
signature `sig:<class>-<protocol>-<recipe>-<kind>[-<attributed_axis>]` (recipe = the fault family for fault-class,
or the dominant config knob for config-class). Experiments never become Issues; dead-end
experiments just sit in `done/`. Human-readable, git-diffable, and inspectable with a `harness sweep status`
summary command (Phase 2).

### 4.1 Dashboard visibility, run labels & post-mortem archive

**Every sweep run is viewable in the sessions dashboard — `done`, `found`, all of it.** Each experiment drives
the harness, which mints a `player_id` and archives the play to ClickHouse via the forwarder, so it shows in
`sessions.html` / `session-viewer.html` with the full charts/events/network exactly like any session — **no new
plumbing**. A/B arms share `group` → `group_id`, so compare-mode overlays control vs variant side-by-side
(#579/#736).

**Labels — the WHAT.** At run start the loop calls `harness labels set <target> k=v …` (merge-patch); the
forwarder stamps these as the **`testing=` tier** — test metadata that does *not* tint a row good/bad (#571).
We tag every run so the dashboard chips + `harness query --label-has` can filter to one sweep / signature / A/B
group / rep-batch:
`sweep=1`, `exp_id=<id>`, `kind=<seed|isolation|hypothesis|bisect>`, `mode`, `platform`, `protocol`, `fault`,
`arm`, `group`, `parent`, `depth`, `rep_group`, `verdict=<clean|notable|aberration|inconclusive>`.
> **Value-encoding gotcha (load-bearing):** forwarder label *values* must not contain `,` or `=` — the value is
> silently dropped (no error, no chip). Keep values slug-safe (`_`/`;`/`|` or a hash), never raw prose.

**Labels + control-event — the WHY (why the LLM chose this run).** A rationale is prose, so it can't live in a
label value. Record it two ways: (1) a short slug label `why=<slug>` (e.g. `why=startup_vsf_on_4k_ladder`) for
at-a-glance filtering; (2) the **full rationale as a harness `control_event`** (`source=harness`, #474) — which
already renders in the session-viewer event stream — and in the `.sweep/<id>.json` + the finding. All three are
linked by `exp_id`.

**Post-mortem archive (`done` is kept):**
- `.sweep/done/<id>.json` is **retained, never auto-pruned** — recipe + verdict + rationale + `play_id`. Browse
  with `harness sweep ls done` / `status`. (`.sweep/` stays gitignored — machine-local, large.)
- The rich detail (charts/events/network) is the ClickHouse play archive, reachable from the dashboard by the
  `sweep=1` / `exp_id` filter — for `done` runs too, not only `found`.
- **Retention caveat:** clean `done` plays are `classification=other` → **30-day** TTL. To keep post-mortems
  longer, the loop marks sweep plays at least **`interesting`** (90-day; `favourite`/★ = forever).

## 5. Scheduler — *discovery-first* ordering

The loop never takes FIFO; it picks the **max-`score` Backlog experiment**, recomputed each pass:

```
score =  W_kind    * (isolation? > bisect? > hypothesis? > seed)   # attribution + narrowing jump the queue
       + W_adj     * aberration_adjacency                          # matrix cells neighbouring a confirmed hit
       - W_cost    * est_runtime_minutes                           # mild tiebreaker only (NOT cost-first)
       - W_redun   * covered_by_finding                            # suppress cells already explained by an Issue
```

- **Discovery-first** ⇒ the instant an aberration is confirmed, its A/B isolation probes and bisection
  follow-ups outrank any untouched far cell. Breadth is sacrificed deliberately.
- **Depth-first to start — validate the machinery before widening.** The novel, risky part is the **LLM
  investigation + experiment-insertion chain** (forensics → isolation fan → bisect → re-run). Initially we want
  to *watch that work*, not cover the matrix. Bootstrap config: seed a **narrow** set (start with `ipad-sim` + a
  couple of fault families), crank `W_kind`/depth so isolation+bisect dominate hard, and let the loop go **deep**
  on the first hits. A `--depth-first` flag caps open seed breadth (e.g. ≤N seed cells in flight) so the queue
  can't widen until the depth machinery is trusted. Flip to full-breadth seeding once proven.
- **Cost model** = a small `mode × platform → minutes` table; ties break cheap-first (appium ATV is slow).
- **Redundancy** = skip cells already explained by an open finding (recall `.claude/findings/` +
  `gh issue list --label sig:…`).
- **Weights are learned, not fixed.** `W_*` and the kind→priority map start from `triage`'s heuristic defaults
  but are overridden by human severity feedback (§8) — the loop learns priorities over time.
- **Seed** = one cheap broad experiment per `platform × protocol × fault-family` cell at `kind=seed`.
- **Repetition is the sweep's job, not the test's.** Modes run through **once** (they own only their intrinsic
  cycles — startup_caps' 5 launches, abort's per-cycle faults; pyramid is a single sweep). The sweep owns
  *statistical* reps: it re-runs an experiment recipe N times as **separate experiments** sharing a `rep_group`,
  then aggregates. Spend is adaptive — `reps=1` on a seed; on a `notable`/`aberration` first hit, escalate to
  `reps≥3` and promote only if the verdict is stable across them (the n=1 guard, A.3). `CHAR_*_REPS` stays a
  manual convenience the loop doesn't depend on.

**Two independent kinds of concurrency:**
- **Throughput parallelism (ungrouped).** Any N *independent* experiments run concurrently as long as each has
  its own device/sim + proxy session — grouping is **not** required. Pool = **4 iOS sims + 1 iPhone + 1 Apple
  TV + 1 Android TV**: up to 4 iOS-sim experiments concurrently; the physical devices one-at-a-time. Worktree
  agents each claim from `.sweep/backlog/` via atomic rename (§7); a per-platform runner-pool counter stops the
  scheduler handing two experiments to one device.
- **Broadcast-group co-execution (grouped).** Only A/B pairs (§6) need a group — purely to guarantee
  **byte-identical, simultaneous shaping** for control vs variant. A group is scheduled as a unit onto ≥2
  runners; if only one runner is free, its arms run back-to-back.

## 6. A/B comparisons — isolation + open-ended hypotheses

A/B pairs serve **two** purposes, both on the same machinery (broadcast group → identical shaping → grouped
compare-mode view). Each pair = a `control` experiment + a `variant` experiment sharing a `group`.

**(a) Reactive isolation** — when the oracle confirms an aberration, attribute its cause via a **one-factor-at-
a-time (OFAT) ablation**: hold the aberrant config fixed as the **control**, emit `kind=isolation` variants each
flipping **exactly one axis**:

| Attribution question | Axis flipped (control held) | Lever |
|---|---|---|
| Platform-specific? | **different platform** | run on iOS-sim / ATV / AndroidTV |
| Content-specific? | *(deferred — single content for now)* | `content` field — disabled until >1 clip |
| Protocol-specific? | **HLS ↔ DASH** | manifest URL extension |
| Live-offset related? | **live_offset** ± steps | `content_manipulation.liveOffset` |
| Ladder related? | **ladder density / allowed_variants** | `allowedVariants` |
| Variant-order related? | **variant_order** reordered (HLS) | `variantOrder` |
| Manifest-metadata related? | **strip codecs / avg-bandwidth / resolution / overstate** | `strip*` / `overstate` |
| Fault-parameter related? | bitrate / frequency / duration midpoint | `shape.rate_mbps` / `fault.frequency` |

**The fan is LLM-reasoned, not a fixed checklist.** The table above is a *menu of levers*, not a script. Given
the **nature of the specific failure**, the loop's analyze step (the `forensics`/`investigate` skills) proposes
the *most-informative* next experiments — e.g. a startup-only VSF on a 4K-heavy ladder suggests flipping
ladder-density and the top rung *first*; a mid-play freeze after a transport drop suggests varying drop duration
and `liveOffset`, not codec strings. The agent reasons from the evidence (which labels fired, when in the play,
on which request kind) to pick axes and even propose *novel* configs the menu doesn't list. **Ordering =
cheap/likely-first, escalate:** Tier 1 = platform + protocol (few, high-signal, run simultaneously on different
devices — *content is fixed to `insane_new` for now, A.2, so the content axis is deferred*); only if Tier 1
doesn't attribute does it spend Tier 2 on the manifest knobs (ladder, live-offset, variant-order, strip-*) the
agent judged relevant. Hard cap `MAX_ISOLATION_AXES` (~8) so a single failure can't monopolize the pool.

**(b) Proactive hypotheses** — standalone `kind=hypothesis` pairs that ask a general comparative question with
no aberration required, e.g. *"does ABR behave the same on a session with `AVERAGE-BANDWIDTH` and without?"*
(control = unmodified master; variant = `content_manipulation.stripAvgBandwidth=true`). Same one-axis-flip
structure, judged by **Oracle B** (the differential, §3) — the finding is "the two arms diverge," not "variant
breached an envelope." Two sources:
- **Starter library:** **strip-AVERAGE-BANDWIDTH**, **ladder density (sparse 6-rung vs dense 11-rung)**,
  **variant-order present/absent**. A small file the loop seeds at start.
- **LLM-generated, failure-aware:** the agent also proposes *new* comparative hypotheses suggested by what it's
  seeing — it is not limited to the starter list. The library is a seed, not a ceiling.

**Run as A/B broadcast pairs (both kinds).** Each variant is paired with its control under one
`harness groups create … --broadcast` group so **shaping is byte-identical and simultaneous**; results land
**grouped**, viewable side-by-side in the existing **compare-mode** session-viewer (#579/#736). Verdict:
isolation ⇒ "aberration reproduces in variant but not control ⇒ flipped axis implicated"; hypothesis ⇒ "arms
diverge beyond threshold ⇒ that axis affects behaviour."

**Bound:**
- `isolation` + `hypothesis` experiments are a **bounded, non-recursive fan** (≤ `MAX_ISOLATION_AXES`, default
  ~8) — they never spawn children.
- `bisect` experiments (the continuous axis an isolation probe implicates) keep the bound: **≤2 recursive
  follow-ups, stop at `depth ≥ 3`.**
So tree *depth* stays bounded; attribution *breadth* is a finite capped fan.

## 7. The iteration (self-contained; survives `/clear`, `--resume`, restart)

All state is in `.sweep/`, so each iteration is stateless and idempotent:

0. **Reset + health-check** — clear any residual `shape`/`fault_rules` on the target (confirm via `harness shape
   --show`), force-kill+relaunch the app, and verify the deploy + device/sim are healthy. If unhealthy → mark
   `inconclusive` and skip (§11), don't blame the player.
1. **Claim** — pick the top-`score` file in `backlog/`; **atomically `rename` it to `running/`** and stamp
   `owner`. The atomic rename is the lock — if rename fails (another worktree took it), pick the next.
2. **Run** — apply the experiment's fault/shape + `content_manipulation` profile, `harness labels set` the
   what/why labels + emit the rationale `control_event`, mark the play `interesting` for retention (§4.1), then
   drive the platform's probe (appium/adb) through the chosen mode, capture `play_id`. A/B pairs run as a
   broadcast group.
3. **Analyze** — Oracle A (`harness query events <play_id> --label-has`) for single runs; Oracle B (differential
   over the grouped Reports) for A/B pairs. Clean → move file to `done/`. No `play_id` / probe failure →
   `inconclusive` (retry-bounded, §11), not an aberration.
4. **On `notable` / `aberration`** (the trichotomy, §3) — if this was a 1-rep result, first **enqueue `reps≥3`
   confirmation experiments** sharing a `rep_group`; only act once the verdict is stable across them (n=1 guard).
   Then reuse **`triage` → `investigate`/`forensics` → `finding`** (recall `.claude/findings/` first). If confirmed:
   - **Enqueue** the §6 isolation fan + ≤2 bisect follow-ups (`depth=parent.depth+1`, stop at 3) into `backlog/`.
   - **Promote** to a deduped Issue: signature `sig:…`; if an open Issue with that label exists, comment the new
     repro; else `gh issue create --label sweep,sig:…`. Move the file to `found/` (a `notable` becomes a
     lower-priority Issue, not a `bug`).
5. **Drain** — when `backlog/` is empty, report a summary and **do not reschedule** (state-driven, not clock).

**Manual-review lane:** Web experiments, Apple TV experiments if WDA isn't provisioned, and `needs-human`
aberrations (A/V desync, visual artifacts) → moved to `review/`, never auto-claimed.

**Drivers — `/goal` inner, `/loop`/cron outer.** The sweep has a finish line (drain + budget), so the **primary
driver is `/goal`**: `/goal "drive the sweep until .sweep/backlog is empty or --max-experiments/--max-wall-clock
trips."` **`/loop` (or cron + headless `claude -p`) is only the *outer cadence*** — for a perpetual posture that
re-seeds and re-runs the goal nightly / per-build. Both trigger the same `.sweep/` mechanics. Local store ⇒ no
GitHub rate-limit concern for the queue; only Issue creation (findings) touches GitHub — batch those.

## 8. Human severity feedback — learning what matters (active learning)

Early on the loop can't know which signals matter — a `*qoe_downshift_overshoot` might be a real product bug or
an accepted trade-off. So the sweep treats **severity/importance as learned, not fixed**.

**Reuses existing skills:**
- **`triage`** is already the severity model — it ranks plays by weighted "badness" (`user_marked×100 >
  frozen×50 > error×40 > restart×20 > stall`) and invites tuning. The learned priors *are* those weights.
- **`finding`** is the capture surface — `harness finding add --tag --note` + `interesting`/`favourite` (★)
  retention tiers. A human rating lands here as a tag/★.
- **`forensics`** recall-before-investigate + the `.claude/findings/` & `.claude/standards/` libraries give
  cross-session persistence.
- Existing **`user_marked` 911** + `favourite`/`interesting` tiers are human-severity signals already in the
  label vocabulary.

**Net-new (the closed loop no skill does today):**
- **Ask, especially at first.** A confirmed finding (or `notable`) lands in a `feedback/` queue. You rate
  `severity ∈ {ignore, minor, major, critical}` + optional "why". Interactive (`/goal`) runs prompt inline;
  headless runs queue ratings for a later `harness sweep review` pass.
- **The rating is a durable label** — onto the finding (tag/★), the Issue (`sev:*` label), and a `feedback.jsonl`
  keyed by `(aberration_kind, signal-labels, platform, axis)`.
- **Feedback retrains the scorer** — §5 weights start from triage defaults but are overridden by learned priors.
- **Active-learning cadence:** cold-start asks ~every finding; as priors firm up it shifts to **uncertainty
  sampling** (ask only when uncertain/novel). Re-openable anytime.
- **Distinct from the manual-review lane (§7)** — that lane is for runs the oracle *can't judge*; this is for
  runs it *has* judged, where you calibrate *how much it matters*.

## 9. Decisions

| Decision | Resolution |
|---|---|
| Work-item terminology | **experiment** (not "job"/Actions-job) |
| Queue store | **ClickHouse-master** (migrated from the original local `.sweep/` files) — `harness sweep` reads/writes via the forwarder API; server-side concurrency-safe claim; no publish step; dashboard scope-gating |
| Findings | **deduped GitHub Issues** (`sweep` + `sig:*` + `sev:*` labels) |
| Runner pool | **4 iOS sims + 1 each iPhone / Apple TV / Android TV** |
| Launch mode | **appium for iOS-sim (WDA confirmed) + Apple TV; adb/cli for Android TV; Web → review lane** |
| Content | **fixed to `insane_new` for now** — content-isolation axis deferred until >1 clip |
| A/B axis policy | **LLM-reasoned + cheap/likely-first escalation** (Tier 1 platform+protocol → Tier 2 manifest knobs); cap `MAX_ISOLATION_AXES`=8 |
| Hypothesis library | seeds = **strip-AVERAGE-BANDWIDTH, ladder density, variant-order**; **LLM also generates failure-specific hypotheses** |
| Severity feedback | **batched `harness sweep review` + inline when interactive**; cold-start asks ~all → uncertainty-sampling; weights seeded from `triage` |
| Initial strategy | **depth-first** — narrow seed + isolation/bisect dominate to validate the LLM investigate→insert→re-run chain end-to-end; widen later (`--depth-first` flag, §5) |

**Remaining tunables (defaults baked in — adjust during Phase 2):**
- **Recovery-expected envelope** (§3, Oracle A) — starter table in **A.4**.
- **Oracle-B divergence threshold** — start at *≥1 rung sustained `variant_idx` difference OR ≥15%
  `mean_bitrate_mbps` delta*.

## 10. Phase 2 build outline

**First milestone = the depth-first vertical slice:** narrow seed → one real hit → the LLM investigate→isolation-
fan→bisect→re-run chain runs end-to-end, labeled + archived + dashboard-visible. Prove that thread works before
building out breadth seeding. The pieces below are ordered to serve that slice first.

- **Local store + `harness sweep` commands** — `seed`, `status`, `next`, `claim`, `ls`, plus the `.sweep/` dir
  layout. Small Go tool under `tools/harness-cli/cmd/harness`.
- **Seed generator** — defaults to a **narrow depth-first seed** (`ipad-sim` + a couple of fault families). A
  `--full` switch (later) populates the broad `kind=seed` matrix once the depth machinery is proven.
- **Per-iteration command/skill** — the §7 loop (claim→run→analyze→isolate+narrow/promote→done; drain & stop).
- **A/B experiment generator** — (a) given a confirmed aberrant experiment, emits the §6 OFAT isolation fan; (b)
  reads the comparative-hypothesis library and seeds `kind=hypothesis` pairs. Both emit control+variant pairs.
- **Differential evaluator** — compares two grouped Reports and flags divergence beyond the Oracle-B threshold.
- **Run labeling + visibility (§4.1)** — `harness labels set` the what-labels (slug-safe), emit the why-rationale
  control_event, mark plays `interesting`, keep `done/` files for post-mortem. Confirm runs surface in `sessions.html`.
- **Severity-feedback loop (§8)** — `feedback/` queue + `harness sweep review` + `feedback.jsonl`; scorer-prior
  updater; uncertainty-sampling gate.
- **Tests** — unit tests for the oracles + isolation-fan generation against **sample telemetry fixtures**:
  label→kind→signature mapping, one-axis-flip correctness, bounded-fan cap, recursive depth bound, atomic-claim race.
- **README** — interactive (`/goal`) vs headless (`claude -p`) run instructions.

## 11. Robustness, operational prerequisites & gaps

- **A 4th outcome — `inconclusive` (infra, not player).** appium/WDA hang, sim wedge, deploy down, harness error,
  "0 players registered" must **not** count as `aberration`. Add `inconclusive` → file returns to `backlog/` for
  a bounded retry count, then `review/`. Detect via probe/harness exit status + missing `play_id`.
- **Reset between experiments.** Faults + shaping are per-session kernel state (nftables/tc) that *persists*.
  Clear `shape` + `fault_rules` (confirm with `harness shape --show`) before applying its own, or state leaks
  (cf. #738/#739). Force-kill+relaunch to dodge the stale-socket wedge after a proxy restart.
- **Stale-claim reaper.** A runner that dies mid-run orphans its file in `running/`. A reaper returns `running/`
  files whose `owner` heartbeat is older than ~`2× max_run_minutes` to `backlog/`.
- **Sim/device seeding prerequisites (the real unattended blocker).** Fresh/erased sims hit the blocking
  server-picker, and a sim seeded with the wrong host fetches an **empty catalogue** (TLS hostname mismatch).
  Unattended iOS-sim runs require seeding `is.servers.v2` to `HARNESS_BASE_URL=https://dev.jeoliver.com:21000`
  and `.env` copied into each worktree.
- **Target / environment binding.** Each experiment runs against the **test-dev deploy** (`THROUGHPUT_HOST` /
  `:21000`, per `.env`); `<target>` = the `player_id`/proxy-port the harness mints per session. Parallel sessions
  are isolated at the proxy but share the one per-content go-live worker — fine for single-content.
- **Stop conditions beyond "backlog empty."** Add operator budgets: `--max-wall-clock`, `--max-experiments`, and
  a per-signature find-cap (stop chasing a signature after K confirmations).
- **Oracle-C corpus bootstrapping.** The VOMM surprise scorer needs a "clean" reference corpus. Cold-start has
  none → seed from existing ClickHouse clean-history, or build it from the first clean runs and enable Oracle C
  only once warm. Until then rely on Oracle A + `warning`-tier labels.
- **Runner host, not cloud CI.** "headless `claude -p`" must run **on the Mac with the sims + attached devices**
  (cron or a self-hosted runner) — generic GitHub-hosted Actions can't drive appium/WDA/adb.
- **Cost property (good news).** The LLM is invoked **per `aberration`/`notable`, not per run** — clean runs are
  pure mechanical oracle checks. Cost scales with *findings*, not the (vast) experiment count.
- **Live-origin caveat.** The origin loops VOD-as-live, so the exact segment under a fault depends on wall-clock.
  A/B broadcast groups stay fair; standalone fault reproducibility should key on segment *kind / position-in-loop*.

## Appendix A — the full test + configuration matrix (what sets the scale)

### A.1 Tests that already exist

**Characterization modes** (`tests/characterization/modes/`, **52 test funcs = modes × platforms**):
`steps`, `rampup`, `rampdown`, `pyramid`, `transient_shock`, `downshift_severity`, `hysteresis_gap`,
`emergency_downshift`, `startup`, `startup_caps`, `state_residency`, `playback_end`, `abort` (+ `fleet.go`,
`sweep.go`/`runconfig.go` infra).

| Platform | Launch | Mode funcs |
|---|---|---|
| iPad Sim | appium | 13 |
| iPhone | appium | 11 |
| Apple TV | appium | 11 |
| Android TV | adb/cli | 11 |
| Web (Chrome) | manual only → review lane | 6 |

**Server-behavior suites** (`tests/server_behavior/`): `content`, `delay`, `fault`, `limit`, `loss`, `pattern`,
`scope`, `socket`, `transfer`, `transport_fault`, `config_on_connect`. **Aberration crawl**
(`tests/aberration_crawl/`): `TestAberrationCensus`.

### A.2 Every configuration axis

| Axis | Values | Source |
|---|---|---|
| **Platform** | iPad-sim, iPhone, Apple TV, Android TV, Web(manual) | A.1 |
| **Protocol** | HLS, DASH — each in LL / 2s / 6s segment variants | nginx, go-live |
| **Mode** | the 13 characterization recipes | A.1 |
| **Fault type** (~17, **`fault` class only**) | `403/404/500/503`, `timeout`, `corrupted`, `connection_refused`, `dns_failure`, `rate_limiting`, `request_{connect,first_byte,body}_{hang,reset,delayed}`; transport `drop`/`reject` | `translate_faults.go:35-55` |
| **Fault scope** (fault class) | request_kind ∈ {master_manifest, manifest, segment, partial, init, audio_segment, audio_manifest} + url_match | `fault.go` |
| **Fault cadence** (fault class) | `frequency` × `mode` {requests, seconds, failures_per_seconds, failures_per_packets} × `consecutive` | FaultRule |
| **Rate cap** (config class) | `rate_mbps` — **floor-guarded** (never below the lowest sustainable rung, §0). `delay_ms`/`loss_pct` are **excluded** (out of scope for both classes) | `shape.go` |
| **Shape patterns** (config class) | pyramid, ramp_up, ramp_down, square_wave, transient_shock, sliders; `step-seconds` {6,12,18,24,60,120}; `margin` {0,5,10,25,50}; `max-step`, `top-headroom` — ladder-derived, sustainable by construction | `shape.go`, #733 |
| **Server transfer-timeouts** (config class) | `active_timeout_seconds`, `idle_timeout_seconds` + `applies_{segments,manifests,master}` — a slow/stalled origin | `transfer_timeouts.*` |
| **Content manipulation** (config class) | `liveOffset`, `allowedVariants`, `variantOrder` (HLS), `stripCodecs`, `stripAvgBandwidth`, `stripResolution`, `overstateBandwidth` | `manipulate*` |
| **Content item** | **FIXED to `insane_new` for now**; ladder density still a tested axis via `allowedVariants` | go-upload catalogue |
| **Reps / duration** | `CHAR_*_REPS` (≥3), `CHAR_STEP_S`, step count, `CHAR_FLEET_COUNT` | env flags |

Multiplied out this is **≫10⁶ cells** — which is why the design is discovery-first + bounded A/B fan +
finding-dedup + cell-redundancy suppression, never an exhaustive walk.

### A.3 Factors that bound scale / capability

- **Device concurrency is the hard cap. Pool = 4 iOS sims + 1 iPhone + 1 Apple TV + 1 Android TV** (physical
  devices 1-apiece; iOS sims scale to 4 via `CHAR_FLEET_COUNT`, #734). One device = one play at a time.
- **Per-run wall-clock is minutes** (~1.5–6 min/mode). Dominant term in `est_runtime_minutes`.
- **Web is manual-only** → review lane.
- **iOS-sim runs via a booted-sim WDA** (confirmed working).
- **Session pool** — proxy port per session (test-dev 21xxx; default ≈700 ports).
- **ClickHouse retention** — 30/90/forever TTL bounds redundancy + VOMM-corpus lookback.
- **n=1 is not a pattern** — ABR jitter is real; a verdict from a single rep needs ≥3 reps before promotion.

### A.4 Recovery-expected envelope — starter proposal (**`fault` class only**)

This envelope only applies to the `fault` class — it's how an injected error is forgiven if the player copes.
The `config` class has no injected error and is floor-guarded (§0), so it has no envelope: any
envelope-violating QoE label is the signal directly.

| Fault | Tolerated (no aberration) | Aberration if… |
|---|---|---|
| single-shot segment `500`/`503` | ≤1 stall, recovery ≤5 s | no recovery, or >1 stall, or VSF |
| segment `corrupted` (one-shot) | ≤1 stall, recovers next segment | repeated stalls / freeze / give-up |
| persistent manifest `404`/`5xx` | — (terminal) | always (`*qoe_vsf`/`player_error`) |
| `timeout` / `request_body_hang` (brief) | ≤1 rebuffer, recovery ≤10 s | freeze, or no recovery |
| transport `drop`/`reject` (brief burst) | downshift + recover ≤10 s | sustained stall / wedge |

## Appendix B — worked examples

### B.1 A seed experiment file (`.sweep/backlog/seed-ipad-hls-ratecap.json`)

```json
{
  "id": "seed-ipad-hls-ratecap",
  "created_at": "2026-06-13T18:00:00Z",
  "kind": "seed", "depth": 0, "parent": null, "reps": 1, "score": 12.0,
  "platform": "ipad-sim", "protocol": "hls", "content": "insane_new", "mode": "pyramid",
  "shape": { "pattern": "pyramid", "step_seconds": 30, "margin_pct": 5 },
  "fault": null, "content_manipulation": null, "duration_s": 360
}
```

### B.2 An aberration → isolation fan (OFAT: each variant flips exactly ONE axis)

A seed on `ipad-sim / hls / hard 0.4 Mbps cap` returns `aberration` (`error=*qoe_vsf`, startup failure). The loop
confirms with `reps=3`, then — because the failure is a startup VSF on a 4K-heavy ladder — the agent *reasons*
that platform and the top-rung ladder are the likely culprits and emits this fan (content stays `insane_new`):

| file | flips | group/arm | tier |
|---|---|---|---|
| `iso-<h>-control.json` | — (the reproducing config) | `g1 / control` | — |
| `iso-<h>-platform.json` | `platform: ‹androidtv›` | `g2 / variant` | 1 |
| `iso-<h>-protocol.json` | `protocol: ‹dash›` | `g3 / variant` | 1 |
| `iso-<h>-ladder.json` | `content_manipulation: {‹allowedVariants›: "drop-top-rung"}` | `g4 / variant` | 2 |
| `iso-<h>-liveoffset.json` | `content_manipulation: {‹liveOffset›: 6}` | `g5 / variant` | 2 |

Verdict: if only `iso-<h>-platform` comes back clean while the rest still fail ⇒ **not** iOS-specific; if
dropping the top rung clears it ⇒ a 4K-startup bug. Signature gets the attributed axis, e.g.
`sig:hls-ratecap-vsf-ladder`. (Content-specific attribution returns once a second clip is added.)

### B.3 A proactive hypothesis pair (the `AVERAGE-BANDWIDTH` question)

```json
// .sweep/backlog/hyp-avgbw-ipad-control.json   (group h-avgbw, arm control)
{ "kind":"hypothesis","group":"h-avgbw","arm":"control","platform":"ipad-sim",
  "protocol":"hls","content":"insane_new","mode":"steps","content_manipulation":null }

// .sweep/backlog/hyp-avgbw-ipad-variant.json   (group h-avgbw, arm variant)
{ "kind":"hypothesis","group":"h-avgbw","arm":"variant","platform":"ipad-sim",
  "protocol":"hls","content":"insane_new","mode":"steps",
  "content_manipulation": { "stripAvgBandwidth": true } }
```

Run as one broadcast group → identical step ladder on both arms. **Oracle B** diffs the two Reports: if the
stripped arm's `variant_idx` timeline or `mean_bitrate_mbps` diverges past threshold ⇒ finding; if they track
together ⇒ clean, recorded as "no effect" (still useful).

### B.4 The iOS over-downshift as a `notable` (non-error) flow

1. A `downshift_severity` seed on `ipad-sim` returns no error but carries `warning=*qoe_downshift_overshoot`
   (settled 3 rungs below cap) ⇒ verdict **`notable`**, not clean.
2. Loop escalates `reps=3`; overshoot reproduces in 3/3 (not jitter).
3. Emits an isolation fan flipping platform (iPhone, Apple TV, Android TV) and ladder density (sparse 6-rung vs
   dense 11-rung) — directly testing whether *density drives over-downshift hunting, not bitrate overage*.
4. Files a **lower-priority** Issue `sig:hls-ratecut-downshift_overshoot` (labelled `notable`, not `bug`).

### B.5 Commands the loop actually runs (per iteration)

```sh
# claim (atomic): mv .sweep/backlog/<id>.json .sweep/running/<id>.json   # via rename(2)
harness groups create --label iso-<h> --broadcast            # only for A/B pairs
harness shape <target> --pattern pyramid --step-seconds 30 --margin 5
harness fault add <target> --type 500 --kind segment --frequency 1 --mode requests   # if faulted
harness labels set <target> sweep=1 exp_id=iso-h7 kind=isolation arm=variant \
  axis=ladder why=startup_vsf_on_4k_ladder   # what + why (slug-safe: no , or =)  → testing= tier
# (full prose rationale emitted as a source=harness control_event, linked by exp_id; play marked interesting)
# (probe drives the platform via appium/adb through the mode, yields <play_id>)
harness query events <play_id> --label-has 'error=*qoe_vsf' --json       # Oracle A
harness query events <play_id> --label-has 'warning=*qoe_downshift_overshoot' --json  # notable
python3 analytics/tools/scorer.py --play <play_id>                       # Oracle C surprise
gh issue list --label 'sig:hls-ratecap-vsf' --state open                 # dedup check
gh issue create --label sweep,bug,sig:hls-ratecap-vsf --body-file repro.md   # promote (heredoc body)
```

### B.6 Throughput parallelism (ungrouped, §5)

4 sims free, 9 independent seeds in `backlog/` → 4 worktree agents each `rename`-claim one and run concurrently;
as each finishes it claims the next highest-`score`. No groups involved. An A/B group that needs 2 runners waits
until 2 of the 4 are free.
