---
name: condition-report
description: Generate the #508 streaming condition report — a descriptive summary of what the player does AROUND playback conditions (faults / stalls / play-ends) over a recent window. Invoke when the user says "condition report", "streaming report", "run the #508 report", "what does the player do around faults / stalls", "report on recovery behaviour", or wants the periodic behaviour-around-events summary. Read-only and DESCRIPTIVE — this is the precursor to, NOT, the trained VOMM anomaly scorer. Wraps `analytics/tools/report.py --kind conditions`.
last_reviewed: 2026-06-02
---

# Condition report (#508)

One read-only command that queries a window of plays and writes a Markdown report of the
player's request/event grammar around three conditions. It does **not** write anything
back (dashboard surfacing is #506).

## Run

```sh
make report                              # last 7d, AVPlayer → /tmp/report-conditions.md
make report REPORT_DAYS=14
python3 analytics/tools/report.py --kind conditions --days 7 --engine AVPlayer --out f.md
```

Prereq: harness CLI on `$PATH` (`make harness-cli`), pointed at test-dev. First run is
slow (pulls + caches network/events per play in `/tmp`); re-runs are fast.

## What it reports (anchor → episode → grammar)

1. **Fault recovery** — `P(reaction | FAULT(surface,class))` + the rendition staircase.
2. **Stall recovery** — playlist/segment shift after `STALL_START` + the fault trigger.
3. **Play-end lead-up** — pre-end grammar bucketed by end-type (incl. `silent`).

## How to present it — caveats that MUST travel with the numbers

- **Descriptive, not a verdict.** No trained surprise/anomaly model exists yet; these are
  empirical distributions. Don't say a session "scored" anything.
- **Single engine, test-rig-heavy corpus** (AVPlayer; no Roku/hls.js — and Roku is out of
  scope). It's "AVPlayer under our harness," not the fleet.
- **End-type labels are contaminated** — `mid_stream_failure` is often play rotation; the
  clean/QoE-abandonment buckets are empty (#565). Trust the lead-up-grammar contrast
  (e.g. `silent` ends carry far more fault/downshift) over the labels.
- **`STALL` is 0 in the play-end section** (that path doesn't pull events yet); stall
  duration is not reported (unreliable pairing).

## Don't

- Don't call this the VOMM/anomaly scorer — `vomm` is a reserved future `--kind`.
- Don't add per-condition scripts; new conditions are `CONDITIONS` entries and new report
  types are `KINDS` entries in `report.py`.

See `analytics/tools/README.md`, `analytics/tools/CORPUS_PLAN.md`, and #508 / #555 / #565.
