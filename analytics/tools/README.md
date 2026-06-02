# analytics/tools — #508 sequence-anomaly R&D

Two read-only tools for the variable-order sequence-anomaly work (#508). They **read the
archive** via the harness CLI and **write nothing back** (dashboard surfacing is #506).

## Run it

```sh
make report                 # → /tmp/report-conditions.md  (last 7d, AVPlayer)
make report REPORT_DAYS=14 REPORT_OUT=/tmp/r.md
# or directly:
python3 analytics/tools/report.py --kind conditions --days 7 --engine AVPlayer --out report.md
```

Prereq: `make harness-cli` (CLI on `$PATH`, pointed at test-dev). Network/event pulls are
cached in `/tmp/cnet_*.json` / `/tmp/cev_*.json`.

## Files

- **`report.py`** — the report generator. `--kind` is a registry (`KINDS`); today only
  `conditions`. Future report types (throughput, qoe, the trained scorer) are entries,
  not new files. **`vomm` is reserved** for the trained variable-order scorer.
- **`tokenize.py`** — the substrate. Turns a play's `network_requests` (+ optional
  `session_events`) into the delta-encoded token sequence and episode windows. Imported
  by `report.py`; also runnable standalone (`--episodes`, `--json`).
- **`CORPUS_PLAN.md`** — the design: token alphabet, fault taxonomy + back-off, the
  condition-anchored program (conditions-as-features → play-end-as-label), corpus plan.

## How to read the `conditions` report

Three sections, each *anchor → episode → grammar*:

- **Fault recovery** — `P(reaction | FAULT(surface,class))`. Headline so far: one-rung
  **staircase downshift** (`video 404 ≡ 5xx`; audio diverges).
- **Stall recovery** — playlist/segment shift after a `STALL_START`. Headline: stall
  **onset is mostly fault-free (timing-driven)**; **recovery** is backward-refetch +
  downshift.
- **Play-end lead-up** — pre-end grammar bucketed by end-type (incl. `silent` = no
  beacon; live plays censored). Headline: the trouble-then-vanish signal lives in the
  **`silent`** bucket.

## Caveats (do not over-read)

- **Descriptive, NOT a model.** These are empirical distributions — there is no trained
  surprise/anomaly score yet. The `conditions` report is the VOMM scorer's *precursor*.
- **Single engine, test-rig-heavy corpus.** AVPlayer only; no Roku/hls.js; few organic
  sessions. It's "AVPlayer under our harness," not "the fleet."
- **End-type labels are contaminated** — `mid_stream_failure` is often test-rig play
  rotation, and clean/QoE-abandonment ends are absent (see **#565**).
- **`STALL` is 0 in the play-end lead-up** — the play-end path doesn't pull
  `session_events` yet (the fault/stall sections do).
- Stall **duration** is not reported (the naive `stall_start→buffering_end` pairing is
  unreliable; needs the forwarder's paired `stall_end` / `buffering_duration_ms`).

See #508 (epic), #555 (fault-scope bug), #565 (end-of-session observability).
