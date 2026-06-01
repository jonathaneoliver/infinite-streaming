# #508 corpus-generation plan

The Phase-0 reconciliation (GitHub #508 comment, 2026-06-01) cleared the structural
gate but identified the **binding constraint**: the substantial-play corpus is only two
engines (AVPlayer 58, ExoPlayer 7) with **no Roku and no hls.js**. Variable-order
per-engine modelling needs a deliberately generated corpus before any model spend.

## Target corpus (minimum to start model R&D)

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
- **EXCLUDE from clean training, KEEP for validation positives + #507 study:**
  `transient-shock`, `emergency-downshift`, `downshift-severity` sweeps, and all
  FAULT/ABORT-bearing sessions. Tag `info=corpus_adversarial` / `info=corpus_abort`.

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

## Acceptance for "adequate corpus" (unblocks model R&D)

- ≥30 substantial clean sessions per engine for all four engines, OR an explicit
  decision to scope the first model to the engines we have (AVPlayer + ExoPlayer) with
  the rest as a follow-up.
- A held-out clean split per engine for the novelty-based validation regime.
- Fault/abort sessions tagged and excluded from clean training (they are #507's primary
  dataset).
