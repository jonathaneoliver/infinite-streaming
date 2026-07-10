# Sweep queue ↔ char-matrix fleet path aren't integrated; confirm-reps are undrivable

**Date:** 2026-07-10 · **Where:** `harness sweep` (issue #772) driven against test-dev (`:21000`), ipad-sim fleet
**Tag:** **confirmed** (both gaps reproduced live in one loop iteration)

## TL;DR

Driving two full sweep iterations end-to-end (`config` class, `import-F-s1-00` and
`-s1-02`, s1/live_offset 0 and 2) surfaced **three gaps**: two structural, between the
single-device sweep state machine and the multi-device char-matrix fleet runner (A, B),
and one oracle calibration bug — the cold first-of-batch play systematically false-flags
`severe_startup` (C). The n=1 confirmation guard still reached the *correct* answer both
times (the hits were startup transients, **refuted**), but only because the verdict was
recovered by querying play labels directly — the tooling could not carry the reps through
the queue.

## What happened (timeline)

1. `sweep next --claim` → `sweep bootstrap` → `TestSweepProbe` on Fleet iPhone 15 #1 →
   `sweep analyze --confirm-reps 3`. Verdict **`aberration` → `found/`**, primary
   `critical=*qoe_tier_unacceptable`. The n=1 guard enqueued 3 reps
   (`rep-5314f5-1/2/3`, shared `rep_group`). **This half worked perfectly** — the
   experiment moved backlog→running→found, and the board showed it in `running`.
2. The reps landed in `backlog/` with **score 0**. `sweep next --claim` kept claiming
   the score-197 seeds instead — the reps never got picked. There is **no claim-by-id**,
   so the single-device path (`bootstrap`/`analyze`, which only load from `running/`)
   can't drive them at all.
3. Fell back to the documented rep-batch path: `sweep export --rep-group` →
   `harness char matrix reps-5314f5.yaml`. All 3 reps **played fine** (79–80s each,
   back-to-back on one sim — the appium-leak fix `dac1941c` holding). But:
   - The QE Lab **`running` column stayed empty** the whole time (operator-visible
     symptom: "I see a test on testing.html but nothing in QE Labs/running").
   - `sweep analyze rep-5314f5-1 --play <id>` → **`load running/rep-5314f5-1: experiment
     not found`**. `analyze` only reads `running/`; the char-matrix path never claimed
     the reps into it.

## The three gaps

### Gap A — the fleet runner bypasses the sweep queue state machine
`harness char matrix` drives the probe via config-on-connect but does **not** transition
its experiments through the queue buckets (`backlog → running → done/found`). Consequences:
- The QE Lab board (reads the `running/` bucket, i.e. `sweep_claims`) shows nothing
  while a fleet batch runs — the operator has no board-level visibility.
- `sweep analyze <id>` can't resolve a play produced by the fleet path (it loads from
  `running/` only), so fleet-produced verdicts can't be recorded back to CH via the
  normal command. The play *rows* exist and are queryable; the *experiment verdict* is orphaned.

### Gap B — confirm-reps are unschedulable via the single-device path
The n=1 guard's reps get **`score = 0`** and there is **no claim-by-id**. `sweep next`
is strictly score-ordered, so reps sit behind every score>0 seed indefinitely (the
skill doc's "reps outrank seeds" does not hold in the CH scheduler). The only way to run
them today is the fleet path (Gap A), which then can't record their verdict.

### Gap C — the oracle false-positives on the cold first-of-batch play
The `*qoe_buffering_severe_startup` → `qoe_tier_unacceptable` threshold trips on the sim's
**first play after idle** (cold app launch), independent of the arm under test.
**2 of 2** first-of-batch plays hit it with a near-identical signature:

| exp | play | ffs_s | startup buf ms | verdict |
|---|---|---|---|---|
| import-F-s1-00 (N=0) | `176f259f` | 2.88 | 4588 | severe_startup → unacceptable |
| import-F-s1-02 (N=2) | `b6e81396` | 2.70 | 4800 | severe_startup → unacceptable |

Every follow-on play in the same batch ran ~1.0s ffs / ~1.5s buf (`tier_acceptable`).
So every fresh sweep batch manufactures one false `aberration` on its first arm, which
then costs a 3-rep confirmation cycle to refute. **Fix:** the probe should run a discarded
**warmup play** (or the oracle should suppress `severe_startup` on the first cold launch
of a session/device) so the first *scored* arm isn't paying the cold-start penalty.

## Evidence — the guard was right anyway (aberration refuted)

Recovered by `harness query play <id>` (label histogram), since `analyze` couldn't:

| play | role | first_frame_s | startup buf ms | tier | driver label |
|---|---|---|---|---|---|
| `176f259f` | hit | 2.88 | 4588 | `critical=*qoe_tier_unacceptable` | `*qoe_buffering_severe_startup` + `*qoe_live_offset_concerning` |
| `5f593c03` | rep 1 | 1.05 | 1593 | `warning=*qoe_tier_acceptable` | `*qoe_buffering_long_startup` |
| `a689db98` | rep 2 | 1.22 | 1763 | `warning=*qoe_tier_acceptable` | `*qoe_buffering_long_startup` |
| `51263097` | rep 3 | 0.97 | 1522 | `warning=*qoe_tier_acceptable` | `*qoe_buffering_long_startup` |

The hit was a one-off **severe startup-buffering transient** (~3× the reps' first-frame +
startup buffer) — the sim cold-launch/startup-join artifact already documented for N=0
arms in `user-live-offset-crossplatform-2026-07-07.md`. `*qoe_live_offset_concerning`
did not recur. Correct disposition: **not a real finding**; `import-F-s1-00` is a false
positive in `found/`.

## Fix directions (not yet implemented)

- **Gap A:** have `char matrix` (when arms carry `exp_id`, i.e. sweep-sourced) claim each
  arm into `running/` on launch and call the analyze/verdict path on completion — so the
  fleet path *is* a valid sweep runner and the board reflects it. Alternatively teach
  `sweep analyze` to accept an experiment from `backlog/` when a matching play exists.
- **Gap B:** give confirm-reps a score that actually outranks seeds (or a dedicated
  rep-claim path / `next --rep-group`), and/or add `sweep claim <id>` for by-id claiming.

## Caveats

- One iteration; both gaps are structural (not jitter), so n=1 is sufficient to confirm
  *the gaps*. The refuted-transient disposition is n=3 (all reps agreed).
- Observed on the `feat/sweep-lab` branch's harness build; verify against `dev` after merge.

## Related
- `.claude/findings/user-live-offset-crossplatform-2026-07-07.md` — the N=0 sim startup-join artifact this hit matched.
- `docs/sweep-design.md` §5 (scheduler) / §7 (the loop); `tests/characterization/COVERAGE-LEDGER.md`.
- Loop-engine epic **#772**.
