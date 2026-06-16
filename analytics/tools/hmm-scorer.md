# HMM latent-regime scorer (#445)

A research complement to the VOMM scorer (`scorer.py` / `derive_labels.py`). Same token
alphabet (`tokenize.py`), different question:

| | question it answers | signal |
|---|---|---|
| **VOMM** (`derive_labels.py`) | "is this *ordering* of requests one we see in clean traffic?" | per-transition surprise → `unexpected_<condition>` |
| **HMM** (`hmm_deriver.py`) | "what *regime* is the player in, and is this regime transition normal?" | latent-state timeline → `regime_<STATE>` + `unexpected_regime_transition` |

The HMM is a **whole-sequence** model: a `CategoricalHMM` (hmmlearn) decodes each play's
cross-stream token stream into a latent-state timeline — STARTUP / STEADY / RECOVERING /
STALLED / ENDING. The VOMM scores per-condition episode windows; the HMM scores the whole
play. They share nothing but the alphabet, so their `derived_labels` rows coexist (distinct
`model_version` = `hmm-tok-1`).

## Files

- `hmm_model.py` — the model core (CategoricalHMM train/decode, BIC, state auto-labelling
  from top emissions, RLE intervals, transition surprise, gzipped-JSON artifact I/O).
  Imported by both tools below.
- `hmm_deriver.py` — the **production path**: `--mode train` (slow) fits + persists the
  artifact and a `derived_models` manifest row; `--mode score` (fast) Viterbi-decodes
  out-of-sample plays and writes `derived_labels`. Mirrors `derive_labels.py`'s #608 split.
- `hmm_scorer.py` — the **verification harness**: K-sweep / BIC / state-labelability /
  clean-vs-failed separation. How you decide the deriver is trustworthy before enabling it.
- `compare_markov_hmm.py` — reads `derived_labels` and reports the plays where the VOMM and
  HMM derivers disagree most (Spearman ρ + rank-disagreement tables).
- `requirements.txt` — `hmmlearn` / `numpy` / `scikit-learn` (the only non-stdlib tools here).

## Label family (→ `derived_labels`, `model_version=hmm-tok-1`)

- `regime_<STATE>` — one row at the first token of each Viterbi state interval,
  `condition='regime'`, `severity='info'`, `score=interval length`. The regime ribbon, on
  the rows.
- `unexpected_regime_transition` — a state→state move whose `−log P(transition)` under the
  learned transition matrix exceeds the train-calibrated p99 threshold. `condition=
  'regime_transition'`, `severity` from the magnitude, `score=surprise`. The "this regime
  change is abnormal" signal point-surprise can't see.

Both anchor on the row that emitted the landing token (network rows by `entry_fingerprint`;
event/sentinel tokens land as `surface='event'`, `entry_fingerprint=0`, joined on
`(player_id, ts)`) — same read-path join as the VOMM labels and `derived_tokens`.

## Out-of-sample guarantee

Identical to the VOMM deriver: the artifact stamps `trained_at`; the scorer only scores
plays whose `max_ts` post-dates it. No `now − score_hours` arithmetic — `trained_at` IS the
cutoff. Training uses **all** plays in the window (bulk-normal), not a hand-split clean
corpus, so the failure regimes (STALLED/RECOVERING) actually emerge as latent states.

## Running locally (verification)

```bash
# one-off venv (the sidecars pip-install the same requirements.txt)
python3 -m venv /tmp/hmm_venv && /tmp/hmm_venv/bin/pip install -r requirements.txt

# pick K via BIC over the last 7 days, then inspect states + clean-vs-failed separation
/tmp/hmm_venv/bin/python hmm_scorer.py --days 7 --k-sweep 3,5,7,9 --out /tmp/hmm_sweep
/tmp/hmm_venv/bin/python hmm_scorer.py --days 7 --k 5 --out /tmp/hmm_report
# (defaults to FORWARDER_CLICKHOUSE_URL=http://localhost:8123; override --ch-url)

# dry-run the production deriver against CH without writing
/tmp/hmm_venv/bin/python hmm_deriver.py --mode train --train-days 7 --k 5 --model-path /tmp/hmm-model.json.gz
/tmp/hmm_venv/bin/python hmm_deriver.py --mode score --model-path /tmp/hmm-model.json.gz --dry-run

# where do the two models disagree? (reads derived_labels; pure stdlib, no venv needed)
python3 compare_markov_hmm.py --days 7 --out /tmp/compare.md
```

## Running as sidecars (production)

Opt-in behind the `hmm` compose profile (these are P3 research and, unlike the stdlib VOMM
deriver, pull in hmmlearn/scipy — pip-installed on container start):

```bash
docker compose --profile hmm up -d hmm-label-trainer hmm-label-scorer
```

Tunables (env): `HMM_K`, `HMM_TRAIN_DAYS`, `HMM_TRAIN_INTERVAL_SECONDS`,
`HMM_SCORE_HOURS`, `HMM_SCORE_INTERVAL_SECONDS`, `HMM_MODEL_PATH`, `HMM_MODEL_VERSION`.
The trainer and scorer share the `hmm-models` volume (artifact handoff).

## Verification checklist (#445)

Run `hmm_scorer.py` and confirm, on real CH data:

- [ ] K-sweep BIC has a clear minimum → set the deriver's `--k` to it.
- [ ] The K states are labelable from their top emissions (not a wall of `STATE_<i>`).
- [ ] Failed plays' per-token log-likelihood sits clearly below held-out-clean.
- [ ] A known-bad play's ribbon shows meaningful RECOVERING/STALLED time.
- [ ] `compare_markov_hmm.py` Spearman ρ lands in ~[0.4, 0.7], and the top disagreements
      are intuitively explainable.

## v1 limitations (follow-ups)

- **Single global HMM.** The CH-direct read path doesn't carry `player_kind`/`variant`, so
  v1 trains one model over all plays. Per-bucket models (the draft's old cascade) need those
  columns plumbed into the `network_requests`/`session_events` fetch first.
- **State labels are heuristic** (top-emission keyword match, `STATE_<i>` fallback). They're
  suggestions for the human reading the report, not ground truth — the report prints the
  full emission table so you can sanity-check them.
- **`tokenize.py` shadows stdlib `tokenize`.** Harmless for the pure-stdlib VOMM tools, but
  the HMM tools pull in scipy (which imports the stdlib `tokenize`), so `hmm_deriver.py` /
  `hmm_scorer.py` strip their own dir from `sys.path` and load siblings by file path. Any
  *new* numpy/scipy-using tool in this directory must do the same (or we rename `tokenize.py`).
