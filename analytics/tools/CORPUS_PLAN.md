# #508 corpus-generation plan

The Phase-0 reconciliation (GitHub #508 comment, 2026-06-01) cleared the structural
gate but identified the **binding constraint**: the substantial-play corpus is only two
engines (AVPlayer 58, ExoPlayer 7) with **no Roku and no hls.js**. Variable-order
per-engine modelling needs a deliberately generated corpus before any model spend.

## Scope: iOS/AVPlayer first

Current decision (2026-06-01): **focus on iOS/AVPlayer**; Roku + hls.js are a deferred
follow-up, not part of the first model. A single-engine model also avoids the
cross-engine segmentation/back-off complexity.

Measured iOS reality (58 substantial AVPlayer plays):
- **~11 clean** (3 reached `<E>`, 8 got first-frame but no explicit session-end), and
  one of those is a 19-h loop session — so **~10 distinct clean sessions**.
- **44 fault/error** — the validation positives + #507 abort/recovery dataset.
- **2 shaped**, 1 other.
- The clean set is **nearly all steady-2160p** (~all `V_SEG(+0,+1)`, almost no ΔP≠0):
  enough for the 1st-order baseline, **thin for VOMM shift-ordering** until we add
  shaped + episode-harvested shift sequences. (Shaped-inclusion vs exclusion is an open
  holdout-validated knob — see "How to generate".)

## Target corpus (eventual, all engines — deferred behind iOS)

| engine | organic-steady | gentle-shaped (smooth/steps) | fault/abort | notes |
|---|---|---|---|---|
| AVPlayer (iOS/tvOS) | have ~58 | add ~20 | add ~15 | best-covered today |
| ExoPlayer (Android) | ~7 → 30 | 20 | 15 | under-sampled |
| hls.js (web) | **0 → 30** | 20 | 15 | **missing entirely** |
| Roku | **0 → 30** | 20 | 15 | **missing entirely** |

"Substantial" = `playing_time_ms ≥ 30s` AND `net_events ≥ 50` (stubs below this polluted
the Phase-0 binary divergence — exclude them from training).

## How to generate

Use the existing characterization + fault harness (`tests/characterization/`, the
`shape` and `fault` skills). Per the #442 revised modeling note:

- **Clean / organic-steady:** unshaped playback to completion. Tag
  `info=corpus_clean`. These train the "normal" model.
- **Gentle-shaped (INCLUDE in training, down-weighted):** `smooth` / `steps` abrchar
  modes — gives coverage of *normal* downshift transitions so real customer shifts
  don't score as novel. Tag `info=corpus_shaped`.
- **Test / harness sessions are a FIRST-CLASS corpus, not just "excluded."** Adversarial
  sweeps (`transient-shock`, `emergency-downshift`, `downshift-severity`) and all
  FAULT/ABORT-bearing sessions are kept out of the *clean-baseline* model, but they are
  the **primary** training data for the conditional-recovery model (the "is the reaction
  typical?" question — see the fault-class section) and the validation positives. Tag
  `info=corpus_adversarial` / `info=corpus_abort`. Treat the rig's fault sessions as
  signal to model, not noise to discard.

Two corpora, two models (both valid, different jobs):
- **(A) clean-baseline detector** — trains on clean + gentle-shaped; catches *unknown*
  structural anomalies. Test/fault sessions are its validation positives.
- **(B) conditional-recovery model** — trains **on** the test/fault sessions; answers
  "given a `FAULT(surface,class)`, is the recovery ordering typical?" per engine. This is
  the #507 corpus flip: the sessions (A) excludes are exactly (B)'s training set.

Label every run with engine + mode so the per-engine split is queryable:

```sh
harness --insecure labels set <player> engine=hlsjs corpus=clean run_id=<UTC stamp>
# → info=engine_hlsjs, info=corpus_clean on the play row
```

## Benign-novelty signals the tokenizer MUST handle (from the gate)

These are real request-stream events that are NOT anomalies — the corpus will contain
them in every session and the tokenizer has to neutralise them or the detector cries
wolf:

1. **Startup ramp** — first segment(s) at the lowest rendition, then a jump to the
   sustained rendition. Score as a known startup pattern, not a shift.
2. **Transient single-segment excursion** — one low-rendition segment at a *backward*
   segment number, immediately reverting. Displayed rendition never changes. Must be
   distinguished from a sustained switch (persistence filter, default ≥2 segments).
3. **Loop boundary** — segment number resets to a low number (harness loops content).
   Emit `LOOP_BOUNDARY`, suppress the false rewind.

## Fault-class taxonomy + hierarchical back-off (conditional recovery scoring)

This is for the **conditional** question — "is the player's *reaction* to a failure
typical?" — not the marginal "are failures typical?". The model conditions on a fault
antecedent and scores the *continuation*; it excludes the antecedent's own probability
(otherwise the rare `FAULT` token dominates and you just re-measure "failures are
rare"). "Atypical reaction" = a low-probability recovery sequence even though the fault
itself was expected. **Ordering only** — recovery latency / buffer cost is timing (→ #445).

**Governing principle:** split an antecedent into its own token *iff the reaction
grammar diverges* (same rule #442 uses for segmentation). Don't pre-split per HTTP code;
let a divergence test promote/merge.

**Antecedent token:** `FAULT(surface, class)`

- `surface ∈ {video_seg, audio_seg, playlist, master, key, other}` — already in the #442
  alphabet; drives different recoveries (segment→retry/skip, playlist→refetch,
  key→re-acquire).
- `class` — mapped directly from the proxy's existing `fault_type` / `fault_category`
  (this mapping IS #507's deliverable; capture already exists, no schema work):

  | class | derived from | agency | role |
  |---|---|---|---|
  | `4xx` | HTTP 4xx (carve out `404`=stale-playlist, `401/403`=auth as candidates) | server-imposed | reaction antecedent |
  | `5xx` | HTTP 5xx (500/502/503 merged unless divergence test splits) | server-imposed | reaction antecedent |
  | `server_partial` | `transfer_idle_timeout` / `transfer_active_timeout` (partial body) | server-imposed | reaction antecedent |
  | `client_abandon` | `client_disconnect` / `transfer_abandoned` | **player-initiated** | behaviour grammar, NOT a reaction antecedent |
  | `injected_reset` | `request_connect_reset` / `_first_byte_reset` / `_body_reset` | test-rig | #507 study stimulus; keep separable, exclude from the organic model |

**Agency caveat:** `client_abandon` is the player's *own* action (it cancelled
in-flight, usually to switch rendition), so the "is the reaction typical?" question
targets the **server-imposed** classes (`4xx`/`5xx`/`server_partial`). The abandon token
tells you what the player switched *to* — it belongs in the behaviour grammar.

**Hierarchical back-off across the `(surface × class)` tree** — resolves the
granularity-vs-sparsity bind. Score under the most specific antecedent with support,
fall back to coarser:

```
FAULT(video_seg, 5xx)  →  FAULT(video_seg, *)  →  FAULT(*, *)
```

This mirrors the segmentation back-off in #442 (`hlsjs-LL → all-hlsjs → all-LL →
global`). So **define antecedents finely and don't fix the granularity upfront** — the
back-off uses the fine distinction where the corpus supports it and a coarse,
well-estimated one where it doesn't. A divergence test then **merges** classes whose
`P(next | class)` distributions are statistically indistinguishable. Err fine, merge
down: splitting is cheap to undo, over-merging is lossy.

**Why back-off is not optional here:** with only ~44 iOS fault sessions, `surface × class`
(~5 × 5) fans out far faster than the corpus supports — most cells would starve.
Back-off is what makes a fine taxonomy viable on the corpus we have.

### Two modelling layers on the same antecedent (build depth-1 first)

The conditional-recovery model is built in two layers on the same `FAULT(surface,class)`
antecedent. They are NOT rivals — the first is a strict subset of the second.

**Layer 1 — fault-conditional 1st-order = the immediate-reaction distribution.**
`P(next_token | FAULT(surface,class))` — the player's *first* move after the fault:
retry-same / refetch-playlist-first / downshift / skip / stall. Build this first.
- The "1st-order is weak" verdict was about **whole-session average NLL** (averaging over
  all transitions re-derives frequency, loses ordering). It does **not** apply here: this
  is a *targeted conditional on a rare, meaningful antecedent* that we inspect/compare,
  not average into a session score. The immediate reaction genuinely *is* a one-step
  phenomenon, so order-1 captures it fully.
- It is exactly the **depth-1 leaf of the back-off tree** above, and it's what a
  correctly-built VOMM falls back to in our thin-data regime anyway — plus it's directly
  *readable* (one distribution, not a blend across orders). Matches #507's "descriptive
  slice = direct aggregation is more interpretable than the model".

**Layer 2 — VOMM = the multi-step recovery *trajectory*.**
"Over the next k tokens, did the player recover or spiral?" This is where longer context
earns its keep — and only where the data supports depth.

**On "isn't VOMM always better (it's a superset)?"** — superset in *capacity* (set
max-order=1 and VOMM ≡ 1st-order), so asymptotically VOMM ≥ 1st-order. But on *finite*
data it is not strictly better: deeper contexts add variance/overfitting (→ false
positives for anomaly detection), and the back-off/smoothing scheme has to be good for
the advantage to materialise. VOMM exists *because* more-order-isn't-always-better — it
is the mechanism to use depth only where supported and degrade to ~1st-order elsewhere
(contrast fixed-high-order, which #442 rejects for sparsity collapse). A correctly-built
VOMM is therefore **never worse, but only meaningfully better where the corpus supports
depth** — which our fault/shift corpus mostly does not yet. Hence: ship Layer 1 first
(cheap, interpretable, what VOMM reduces to here); add Layer 2 as the corpus grows.

### Fault-taxonomy census — what's actually in the archive (2026-06-01)

Sampled the top-30 fault plays by `net_faults` (all engines) via the read API
(`faulted_only=true`). The `FAULT(surface,class)` taxonomy maps onto **real** data — we
do NOT have to wait for #507 to generate it:

| `fault_category` | rows (sample) | taxonomy class | engines seen | example `fault_type` / `fault_action` |
|---|---|---|---|---|
| `client_disconnect` | **2242** | `client_abandon` | AVPlayer, blank | `client_disconnect` / `transfer_abandoned` |
| `http` | 1520 | `4xx` / `5xx` | AVPlayer, blank | 404, 503, 502, 500, 429, 403, `connection_refused`, `dns_failure`, `timeout` |
| `transfer_timeout` | 23 | `server_partial` | **blank only** | `transfer_active_timeout_mid_body`, `transfer_idle_timeout_mid_body` |
| `socket` | 4 | `injected_reset` | **blank only** | `request_body_reset`, `request_first_byte_hang`, `request_body_hang` |

(Sample, not exhaustive. The spec's `NetworkRowFaultCategory` enum also defines
`corruption` and `transport`, not seen in this sample.)

**The critical nuance — abundant ≠ useful for the reaction question.** The 2242
`client_disconnect/transfer_abandoned` rows are **client-initiated** (the player
abandoning in-flight, usually to switch rendition) → behaviour grammar, **not** a fault
the player *reacts* to (the agency caveat above). The **server-imposed** aborts where
"how does the player react?" applies (`transfer_timeout`) are **rare (23) and `blank`-engine
only — none on AVPlayer**; same for injected `socket` resets.

**Consequence for iOS-first:**
- Client-abandon behaviour + HTTP-fault reaction: **rich AVPlayer data, Layer 1 runnable now.**
- Server-imposed abort/partial *reaction* on iOS: **essentially absent** → this is the
  exact slice #507's harness sweep (inject mid-body aborts per engine) must generate.

**Access (no schema change):**
- `harness query network <play> --json` carries `fault_type`/`fault_category`/`fault_action`
  on faulted rows; `harness query network <play> --faulted-only` / `--fault-category <cat>`
  filter to them; `harness raw GET "/analytics/api/v2/network_requests?...&faulted_only=true"`
  for the server-side filter. CLI live `tail`/`network` now render the fault inline.

## Acceptance for "adequate corpus" (unblocks model R&D)

Scoped iOS-first (Roku/hls.js deferred):
- **(A) clean-baseline detector:** ≥~30 substantial clean AVPlayer sessions (have ~10 —
  generate more, optionally + gentle-shaped), with a held-out clean split for the
  novelty-based validation regime.
- **(B) conditional-recovery model:** enough `FAULT(surface,class)` antecedents with
  recovery continuations that the back-off tree has support at useful depth (have ~44
  iOS fault sessions — likely need more per class). Test/fault sessions are the training
  set here, not excluded.
- All sessions tagged by engine + corpus role so clean / shaped / fault / abort are
  queryable and the two models draw from the right slices.
