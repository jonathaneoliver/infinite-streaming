# Invariants — the aberration-crawl rule catalogue

Machine-checkable behaviour rules distilled from the standards library, run
against the CH archive grouped by version taxonomy (#607). The canonical
catalogue is **`tests/aberration_crawl/invariants.yaml`** — this page is the
operating manual, not the rule list.

## The 3 facts

- **Every rule has a validity window, and git dates lie.** The #550 taxonomy
  hit test-dev on 2026-05-29 — two days *before* its PR merged to dev
  (hot-deploys). `applicable_since` must be data-calibrated (Phase 3), not
  copied from `git log`. A rule crawled outside its window "discovers"
  migrations, not bugs.
- **Version keys are usable from 2026-05-29 only.** Before that, all rows are
  version-blind (0% coverage). The crawler still runs version-independent
  rules (ID case, label grammar, vocabularies) on older rows but reports them
  in an `unversioned` bucket. External HLS players (unmatched UA) form a
  separate always-empty group — report it, never merge it.
- **Census before assert.** Every rule starts `mode: census` (count, never
  fail). Promotion to `assert` requires one clean calibration pass over the
  full retained archive. Two rules are structurally census-locked for now:
  `qoe-cirr-label-recompute` (the seek-exclusion question in
  [[qoe-metrics]] is unsettled) and `play-start-unique` (#604/#605/#606 not
  deployed, `pending: true`).

## Common mistakes

- Adding a rule from the **NON-rules list** at the bottom of invariants.yaml
  (bitrate ≤ cap, resolution stable at play start, error fields empty on sim,
  …). Each is documented-false; that's why the list exists.
- Forgetting the **known-noise exclusions** (`-12174` sim noise, first-2-
  samples carry-forward) — violations vanish or explode depending on whether
  they're applied. The runner logs dropped-row counts per exclusion; a census
  with zero logged exclusions on an iOS group is suspect.
- Treating **QoE thresholds as constants** — they're deploy-time config
  (`FORWARDER_QOE_THRESHOLDS_PATH`). Resolve the running tier from the
  `[QoE]` startup log line before comparing recomputed values to labels.
- Comparing rule output **across the 2026-06-02 rename boundary**
  (`session_end`→`play_end`, qoe_* namespace consolidation #570) without
  splitting the window.

## See also

- `tests/aberration_crawl/invariants.yaml` — the catalogue (canonical)
- [[data-fields]] — the field semantics the rules encode
- [[qoe-metrics]] — formulas + the open seek-exclusion divergence
- Issue #607 — phases, census results, validity-window anchors
