# Characterization results — architecture / design

> **Status:** design captured (design-doc-first); no build started. This records what characterization
> runs should *produce* and how the pieces that already exist relate, so we decide the model before
> building the reporting.
>
> **Terminology:** a **session / play** is one player's run (`play_id`/`player_id`). A **group**
> (`group_id`) is the ≤4 *concurrent* sessions of **one physical fleet run** sharing a bandwidth
> pattern/clock (the pool runs only 3–4 sims at once; the fleet caps a group at 1 control + ≤3
> variants — `charmatrix/expand.go:19 maxArmsPerGroup`). A **study** is the full logical
> characterization — **N reps × M variations** — spanning many groups/runs. **Report / good-bad / AI**
> are three *projections* over the archived per-play record.

## Context

The platform runs characterization-style tests two ways: the newer **`harness char matrix`** declarative
config/fault sweeps, and older **mode tests** (`tests/characterization/modes/`: rampup/pyramid/startup/…).
A recent run — `seg-trio-valley` (segments s6/s1/s2 × one shared valley) — **drove playback but produced
no report**; the comparison table was hand-queried from the archive. That exposed the open question:
*what should these tests produce, and how do the existing pieces relate?*

## The three consumers (outputs)

1. **Written report** — quantitative "how well did it perform" (a performance artifact valuable in its own
   right, e.g. startup TTFF).
2. **Good/bad** — a characterization verdict (pass/fail-ish).
3. **AI input** — feed the sweep to find oddities and generate secondary tests.

## Four composing reframings

1. **Experiment record (hypothesis → verdict → evidence).** The sweep's `Experiment`
   (`internal/sweep/experiment.go`, with `Why`/`WhyText`/`Verdict`) *is* the general shape. char-matrix
   and the mode tests are that model **minus the hypothesis**. The three consumers become **three fields of
   one record**: evidence = report, verdict = good/bad, hypothesis+evidence = AI input.
2. **Absolute + relative good/bad.** Labels give *absolute* badness (a stall is bad, baseline-free).
   *Relative* badness — TTFF drifting 6 s → 9 s with **no stall/label** — is a regression labels can't see.
   Catching it needs a **baseline/budget** (this run vs a golden/prior). Nothing does this today.
3. **Response function, not a table.** The real output is the *relationship* between the swept axis
   (segment/cap/pattern = IV) and the metric (TTFF/stalls = DV) — the **curve and its knee**, not a
   row-per-arm grid.
4. **Substrate + projections.** One **per-play record** = { config (IV), quantitative summary (DV,
   continuous), labels (thresholded severity), optional hypothesis } is the atom. Report / verdict / AI are
   **pure projections** over it.

## Linchpin: labels are a *lossy* projection

Labels are emitted only at thresholds. `qoe_vst_breach` fires when TTFF is slow; **s1=1.2 s and s6=5 s both
get no label** → the label view says "both fine," hiding that **s1 is 4× better**. The good/bad view
**discards the gradient among the good runs**. The quantitative report preserves the continuum → lets you
**rank** configs and find the *best*, not just avoid the *bad*. This is exactly why consumer 1 (report,
continuous) and consumer 2 (good/bad, thresholded) are **distinct first-class things** — the continuous and
thresholded views of the same metric — and both are needed.

## Identifier hierarchy — the join key

The pool runs only **3–4 sessions concurrently**, so a physical **group** can't hold N reps or >4 variations.
A real characterization spans many runs. The report's unit is therefore the **study**, not the group — and a
study can be defined **two ways** (which compose):

```
play_id / player_id   — one session
   └── group_id       — one run's ≤4 concurrent sessions (shared pattern; A/B on the dashboard)
          └── STUDY   — N reps × M variations across many groups/runs   ← the report's unit
```

- **Explicit `study_id` (stamp at launch).** Precise, intentional membership; good for a deliberate
  "same test ×20" or ">4 configs" campaign. The sweep already has **`rep_group`** (ties a rep-batch of *one*
  experiment) — a partial answer for reps of a single config, not a multi-config campaign. A first-class
  `study_id` (operator passes `--study <id>`, carried on every session) would generalise it.
- **Faceted query (no new id).** Join by the dimensions the per-play record **already carries** — and which
  the forwarder already assembles into a per-play **`scenario`** object (`enrichScenario`, `find.go:51`,
  built from the `testing`-tier config labels — `platform`/`recipe`/`segment`/`arm`/`exp_id` — plus device
  columns `device_class/model`/`app_version`/`os_version`). "All s6 / valley / iPhone-15-sim runs this week"
  is a **`scenario` + time-range** query **today**. Retroactive, zero stamping.

Aligns with the reframings: the explicit id fits **experiment-record**; the faceted (`scenario`) query fits
**substrate+projections** (a study is a *query*, not an entity). Either way the response curve (#3) and the
rep→distribution aggregation (n=1 is not a pattern) need **all** the study's sessions, not one group.

**Scale-dependence + the temporal-confound trap.** The two forms suit different N. At **large scale** the
faceted + wide-time-range join is fine — transient/environmental failures wash out statistically. At
**small N** (our reality: ≤4 concurrent → a 20-rep × 3-config study is 60 sessions spread over hours/days) a
wide-time facet-join is **dangerous**: one session that failed for an *environmental* reason (a
`server_start`/restart, a `shaper_changed`, a network blip at 3am) gets baked into that config's aggregate as
if the config caused it. Three defenses, in order of leverage:
- **Bounded / explicit study for small-N** — narrow the window (or stamp a `study_id`); don't span 48 h.
- **Interleave configs within reps** (blocked design) — run all M configs close together each rep, so time is
  balanced across configs, not "all s6 Monday, all s1 Tuesday."
- **Correlate failures with `control_events`** (`server_start`/`shaper_changed`/`session_start`, filterable
  `--label-has info=*server_start`): a session whose failure coincides with a control event is an
  *environment* failure, not a *config* failure — flag/exclude it from the config's score. This is the
  report's guard against baking a temporal blip into the characterization.

## What exists today (code map)

| Consumer | Where it lives | State |
|---|---|---|
| **Report** | `runner.Report.Summary` (mode tests, computed **in-process from live samples**) → `renderMarkdown` (`runner/report.go:715`) + `characterize-report` (`tests/characterization/cmd/characterize-report/main.go`, **mode×platform rollup, reads on-disk JSON**). **Also** `plays-summary` (`/api/v2/plays` → `analytics/go-forwarder/internal/plays/find.go`, computed in **ClickHouse**) carries the same metrics. | **Duplicated**; **char-matrix produces none**. |
| **Good/bad** | (a) mode-test `t.Errorf` (ad-hoc, e.g. "stalled at non-bottom variant" `pyramid_test.go:519`); (b) forwarder **`worstSeverity()`** (`labels.go:820`) + **`qoe_tier_unacceptable/acceptable/premium`** (`qoe_labels.go:245`) — systematic, per-play, label-derived; (c) char-matrix **`class` oracle + `verdict` column** — **declared but never wired** (`charmatrix/table.go:18,66`). | **Three partial oracles**; `verdict` column is dead. |
| **AI input** | The **sweep loop** (#772): `analyze` → `verdict` (clean/notable), LLM reasons on notable, `isolate` spawns secondary experiments; records `/api/v2/sweep/runs {verdict, why}` (`sweep_runs.go`). | **Already exists** — this *is* consumer 3. |

**Overlap in one line:** the mode tests re-implement, in-process, the report + a partial oracle that the
**archive already computes for everyone** (plays-summary + `worstSeverity`/`qoe_tier_*`); the **dashboard**
does A/B on born-grouped arms but there is **no config-aware quantitative study report**; and
**relative/baseline regression is done nowhere**.

## Target capability: a UI study report

Given a **study** (explicit id *or* facet filter), the dashboard generates a comparison that **joins per-arm
config (the IV — what varied) × per-play continuous metrics (DV — TTFF/stalls/shifts, from plays-summary) ×
labels (absolute good/bad)** — and **always shows the metric value, even when "good"**, so configs can be
**ranked**, reps aggregated into a distribution, and the IV→DV response drawn. The plays-summary already
returns `group_id` + `label_histogram` + the device/config dimensions, so the data join exists; the
*renderer* is new.

## Target architecture

**One atom, three projections, one substrate (the archive).** char-matrix's "**drive → archive → project**"
decoupling is the correct foundation; the mode-test "compute report + assert in-process" is the legacy
pattern that duplicates what the archive now produces.

- **Atom — the per-play record:** `{ config(IV), quantitative summary(DV, continuous), labels(thresholded),
  hypothesis? }`, sourced from `plays-summary` + labels + the arm's config (labels / run plan), joined into a
  study by id or facet.
- **Projection 1 — Report (UI-first, optional CLI):** config × continuous-metric × label table **and** the
  IV→DV response across the swept axis; value always shown; reps → distribution; supports a
  **baseline/prior-run diff** (relative regression).
- **Projection 2 — Verdict:** `worstSeverity(labels)` + `qoe_tier_*` (absolute), class-aware (fault runs
  *expect* recovery labels). Wire the dead char-matrix `verdict` column to this — flagged as the *thresholded*
  view (the report is its continuous complement).
- **Projection 3 — AI / sweep:** already consumes the archive + verdict; feed it the *same* record, ideally
  carrying a **hypothesis** (which char-matrix/mode runs lack today).

## Gaps (when this moves to a build)

0. **Study join key** — a first-class `study_id` (stamp at launch) and/or a saved **`scenario` facet filter**,
   so N reps / >4 variations across the ≤4-concurrent boundary stitch into one report; plus a
   **`control_events` correlation** to strip environmental (non-config) failures at small-N, and (design
   convention) **interleave configs within reps** so time is balanced across configs.
1. A **study comparison renderer** joining per-arm config + `plays-summary` + labels (UI-first; extend the
   dashboard's `group_id`/A-B surface to a config-aware quantitative comparison + response curve). *New.*
2. **Wire the char-matrix `verdict`** from `worstSeverity()`/`qoe_tier_*` (currently `-` on every row).
3. **Baseline / relative regression** — decide where a golden-run/prior-run diff lives (nothing does this).
4. **Hypothesis field** on char-matrix/mode runs (optional) to complete the experiment-record model + feed AI.
5. **Mode-test convergence:** do the mode tests drop their in-process `Report.Summary` for the archive
   `plays-summary` (dedup), or keep only their sample-rich per-step detail the archive can't reproduce
   (`report.go:770` Steps table)?

## Key files (source of truth)

- Report: `tests/characterization/cmd/characterize-report/main.go` (`renderMatrix:100`);
  `tests/characterization/runner/report.go` (`Summary:359`, `renderMarkdown:715`).
- char-matrix table: `tools/harness-cli/internal/charmatrix/table.go` (`RenderTable:27`, dead `Verdict:18/66`);
  `expand.go` (`maxArmsPerGroup:19`, `IntendedLiveOffset:327`, group/role stamping `:118/:173`);
  `cmd/harness/char.go` (`measureArm:646`).
- Substrate — play summary + labels: `analytics/go-forwarder/internal/plays/find.go`
  (`runPlaysQuery:119`, `label_histogram:345`, device dims); `analytics/go-forwarder/labels.go`
  (`worstSeverity:820`); `qoe_labels.go`/`qoe_thresholds.go` (`qoe_tier_*:245`, VST/CIRR/CIRT/stall_burst).
- AI: `analytics/go-forwarder/sweep_runs.go`/`sweep_experiments.go` (`Verdict`, `Why`, `rep_group`);
  `internal/sweep/experiment.go`.

## Next step

Agree the model, then pick the build order. Highest-leverage first cut: **Gap 0 + Gap 1** together — define
the study (facet filter is available now; add `study_id` if intentional campaigns want it) and build the
study report over the archive. It delivers consumer 1, subsumes the hand-query, and reuses the existing
`config-as-labels × plays-summary × device-dims` join. Verdict-wiring (Gap 2) and baselines (Gap 3) layer on
cleanly afterward.
