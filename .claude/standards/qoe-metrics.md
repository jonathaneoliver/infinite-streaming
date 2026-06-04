# QoE metrics — CIRR, CIRT, VST, EBVS

What the `qoe_*` auto-labels actually measure, where the definitions come from, and where our implementation diverges from the industry's.

## Provenance — these are Conviva names, not standards

- **CIRR** (Connection-Induced Rebuffering Ratio) and **CIRT** (Connection-Induced rebuffering Time, mean per interruption) are **Conviva's proprietary metric names**. They feel standardized because Conviva's dashboards made them lingua franca; the only formal standard in this space is **CTA-2066**, which defines plain "rebuffering ratio" with *no* connection-induced/user-induced split. Don't cite CIRR/CIRT as "the standard" in findings — cite them as "Conviva-convention".
- "Connection-induced" = **excludes user-induced rebuffering**: seek/scrub recovery and quality-change flushes. Conviva counts seek-to-resume time as seek-induced, not connection-induced.
- Threshold tiers in `qoe_thresholds.go` are calibrated against Conviva's published "best"/"good" tiers (CIRR 0.2%/0.4%, VST 5s/10s, EBVS 8s/10s). Defaults bake in the "good" tier; campaign overrides via `FORWARDER_QOE_THRESHOLDS_PATH`.

## What we compute (forwarder `qoe_labels.go`)

- `CIRR = stalling_time_ms / (stalling_time_ms + playing_time_ms)` → `qoe_cirr_concerning` (≥0.002) / `qoe_cirr_breach` (≥0.004). Denominator is play+stall only — paused/idle/seeking residency doesn't dilute the ratio.
- `CIRT = stalling_time_ms / stalling_count` → `qoe_cirt_concerning` (≥1000ms) / `qoe_cirt_breach` (≥2000ms). Same CIRR can be 1×4s stall or 8×0.5s stalls — CIRT is what distinguishes them.
- `VST = video_start_time_ms` → `qoe_vst_concerning` (≥5000) / `qoe_vst_breach` (≥10000). Sticky field — labels are edge-triggered (#595), not re-stamped per heartbeat.
- **EBVS** — startup still in progress past `ebvs_threshold_ms` (10s) ⇒ terminal outcome `abandoned_start`. Outcome classifier, not a label tier.
- Stall burst — >3 `stall_start` in 60s ⇒ `qoe_stall_burst` (catches flapping that CIRR averages away).

## ⚠️ Known divergence — seek exclusion (CONFIRMED, #607 Phase 3)

The label math uses **raw `stalling_time_ms`**, and the archive shows it **does include seek recovery**: 74% of stalling-delta rows co-occur with seeking deltas and 70% directly follow one (full-archive census 2026-06-04; evidence concentrated in the 35 seek-plays, n=9,678 rows). Our CIRR is therefore plain rebuffer ratio inflated by user-induced seek recovery — **strictly worse than Conviva would report for the same session** on seek-heavy plays. Don't compare our `qoe_cirr_*` labels to Conviva tiers on plays with `seeking_count > 0`. Candidate fixes: forwarder subtracts the seek-overlapped stalling portion, or the iOS state machine reclassifies seek-recovery stalls (#550's `buffering − seeking` derivation pattern).

## Common mistakes

- Quoting `qoe_cirr_*` labels as CTA-2066 conformant — CTA-2066 has no connection-induced split; the tiers are Conviva convention.
- Comparing CIRR across sessions with very different play lengths — a 10s play with one 0.5s stall reads 4.8%, breach-level. Gate on `playing_time_ms` before treating the ratio as meaningful (the ABR labels have `startup_grace_ms` for this reason; CIRR has no equivalent gate).
- Reading CIRT alone as severity — 1 stall of 2s and 30 stalls of 2s have identical CIRT. It's a *companion* to CIRR/stall-burst, never standalone.
- Forgetting thresholds are deploy-time config — a finding that says "breached" must note which tier the deployment ran (`[QoE]` startup log line has the resolved values).

## See also

- `analytics/go-forwarder/qoe_labels.go` (computation), `qoe_thresholds.go` (tiers + layered config)
- `data-fields.md` — the #550 state-residency accumulator columns these derive from
- Issue #550 (accumulators + seeking semantics), #553 (Phase 3 label conditions), #595 (edge-triggering)
