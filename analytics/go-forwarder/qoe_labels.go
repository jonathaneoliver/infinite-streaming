// qoe_labels.go — #553 Phase 3: threshold-based QoE auto-labels.
//
// Consumes the #550 Phase 1 (state-residency accumulators) + Phase 2
// (outcome status / structured error) columns and stamps synthesized
// `<sev>=*qoe_<event>` labels at ingest. Every numeric threshold is read
// from the loaded QoEThresholds config (qoe_thresholds.go) — no magic
// numbers here. These labels flow into row tint, chips, the severity
// filter, sessions-picker badge counts, and the classification-tier bump
// with no plumbing beyond this file (they obey the existing label
// grammar that labels.go documents).
//
// All labels are synthesized (cross-column / cross-row), so they carry
// the `*` synthMark. Severity vocab: `_concerning` → warning; `_breach`
// → critical; `_vsf`/`_msf` → error; `_ebvs` → warning (a user can bail
// during startup — not a failure); tier labels map the row's worst qoe
// severity onto premium/acceptable/unacceptable.
package main

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
	"time"
)

// qoeLabel renders a synthesized QoE label in the strict
// `<sev>=*<event>` grammar.
func qoeLabel(sev, event string) string {
	return sev + "=" + synthMark + event
}

// isPlayTerminalEvent reports whether this row is the play's authoritative
// terminal row — the client's end-of-play metrics POST carrying the final
// snapshot + outcome. We key the outcome labels on the discrete END EVENT,
// NOT on a sticky `playback_status`: the client re-sends the terminal
// status on every heartbeat after a failure, so gating on status alone
// stamps the outcome label on every row ("endless qoe_msf").
//
// `session_end` is the established cross-client contract today; `play_end`
// is accepted ahead of the planned cross-platform rename (issue #554) so
// the labeler is migration-tolerant and historical rows keep classifying.
// NOTE: the proxy's session-lifecycle `session_end` CONTROL event is a
// different, correctly session-scoped thing and is unrelated to this.
func isPlayTerminalEvent(r *row) bool {
	return r.LastEvent == "session_end" || r.LastEvent == "play_end"
}

// isFailedStatus reports whether a terminal status represents a failure
// (vs a clean completion or user stop). Matches any status carrying
// "fail" so start_failure / mid_stream_failure / failed all qualify.
// "abandoned_start" is handled separately (qoe_ebvs).
func isFailedStatus(s string) bool {
	return strings.Contains(strings.ToLower(s), "fail")
}

// computeQoEEventLabels stamps the threshold-based QoE labels for one
// event row. Caller (computeEventLabelsWithState) holds the labelState
// mutex, so mutating the per-play windowed slices here is safe.
func computeQoEEventLabels(cfg *QoEThresholds, ps *playLabelState, r *row, now time.Time) []string {
	if cfg == nil {
		cfg = fallbackThresholds
	}
	// #603 — leading-terminal defense. A play terminal (play_end/session_end)
	// arriving as a play_id's FIRST row is the previous play's terminal frame
	// mis-bucketed onto this play (the client emits play_end carrying the next
	// play's id at the boundary; there's no play_start marker yet). Its
	// accumulators + status belong to the prior play, so emit nothing and do
	// NOT consume the emit-once terminal guard — the real terminal arrives
	// after the play opens. everOpened is set on the first non-terminal row
	// (play_start / restart / state_change / …).
	if isPlayTerminalEvent(r) && !ps.everOpened {
		return nil
	}
	if !isPlayTerminalEvent(r) {
		ps.everOpened = true
	}
	// firstTerminal is true only on the FIRST authoritative terminal row
	// (last_event == session_end|play_end). The client re-emits the
	// terminal POST for delivery reliability, so the emit-once guard stops
	// vsf/msf/ebvs/tier from being stamped on each re-emit.
	firstTerminal := isPlayTerminalEvent(r) && !ps.terminalEmitted

	// ── Collect every level/sticky qoe condition TRUE on this row ──────
	// These are emitted edge-triggered below — sticky inputs like
	// VideoStartTimeMs (set once, then repeated on every heartbeat) must
	// not re-stamp their label every row. #595.
	var current []string

	// Startup — VST (video start time). Breach takes precedence.
	if vst := r.VideoStartTimeMs; vst > 0 {
		switch {
		case vst >= cfg.Startup.VSTBreachMs:
			current = append(current, qoeLabel(SevCritical, "qoe_vst_breach"))
		case vst >= cfg.Startup.VSTConcerningMs:
			current = append(current, qoeLabel(SevWarning, "qoe_vst_concerning"))
		}
	}

	// Continuity — CIRR (rebuffer ratio) and CIRT (mean interruption).
	if denom := r.StallingTimeMs + r.PlayingTimeMs; denom > 0 {
		cirr := float64(r.StallingTimeMs) / float64(denom)
		switch {
		case cirr >= cfg.Continuity.CIRRBreach:
			current = append(current, qoeLabel(SevCritical, "qoe_cirr_breach"))
		case cirr >= cfg.Continuity.CIRRConcerning:
			current = append(current, qoeLabel(SevWarning, "qoe_cirr_concerning"))
		}
	}
	if r.StallingCount > 0 {
		cirt := float64(r.StallingTimeMs) / float64(r.StallingCount)
		switch {
		case cirt >= float64(cfg.Continuity.CIRTBreachMs):
			current = append(current, qoeLabel(SevCritical, "qoe_cirt_breach"))
		case cirt >= float64(cfg.Continuity.CIRTConcerningMs):
			current = append(current, qoeLabel(SevWarning, "qoe_cirt_concerning"))
		}
	}
	// Stall burst — > N stall_start within the window. The window slice is
	// fed by the event switch; trim to the window here before counting.
	{
		window := time.Duration(cfg.Continuity.StallBurstWindowS) * time.Second
		ps.stallBurstTimes = trimBefore(ps.stallBurstTimes, now.Add(-window))
		if uint32(len(ps.stallBurstTimes)) > cfg.Continuity.StallBurstThreshold {
			current = append(current, qoeLabel(SevCritical, "qoe_stall_burst"))
		}
	}

	// ABR / quality. abr_conservative / ladder_gap and throughput_divergence
	// only make sense once playback has settled past the startup ramp — a low
	// rung under high throughput is expected during startup, not a defect — so
	// gate them behind StartupGraceMs of accumulated playing time (#595).
	settled := r.PlayingTimeMs >= cfg.ABR.StartupGraceMs
	if settled {
		current = append(current, abrBitrateLabels(cfg, r)...)
	}
	{
		window := time.Duration(cfg.ABR.DownshiftStormWindowS) * time.Second
		ps.downshiftTimes = trimBefore(ps.downshiftTimes, now.Add(-window))
		if uint32(len(ps.downshiftTimes)) > cfg.ABR.DownshiftStormThreshold {
			current = append(current, qoeLabel(SevWarning, "qoe_downshift_storm"))
		}
	}
	if l := minVariantStuckLabel(cfg, ps, r, now); l != "" {
		current = append(current, l)
	}
	if l := fpsDipLabel(cfg, r); l != "" {
		current = append(current, l)
	}
	if settled {
		if l := throughputDivergenceLabel(cfg, ps, r, now); l != "" {
			current = append(current, l)
		}
	}

	// Live.
	current = append(current, liveOffsetLabels(cfg, r)...)

	// Network (event-row inputs).
	if l := rateCapBreachLabel(cfg, r); l != "" {
		current = append(current, l)
	}
	if l := downshiftOvershootLabel(cfg, r); l != "" {
		current = append(current, l)
	}
	if l := cmcdMTPDriftLabel(cfg, r); l != "" {
		current = append(current, l)
	}

	// ── Edge-trigger + clear-cooldown re-arm ──────────────────────────
	// A label not in qoeActive is a rising edge (first determination, or a
	// recurrence after it cleared). Edge-triggering alone (#595) still
	// re-chips on EVERY rising edge, so a noisy condition that flaps above/
	// below its threshold spawns a chip per re-crossing. The cooldown gates
	// re-fires: a rising edge emits only if the label has been off for
	// ≥ RefireCooldownS since it was last true (qoeLastOn) — so a flapping
	// episode is one chip, not dozens. A continuously-true label stays
	// suppressed; a condition going false re-arms it once the cooldown
	// elapses. #595, #657.
	if ps.qoeActive == nil {
		ps.qoeActive = make(map[string]struct{})
	}
	if ps.qoeLastOn == nil {
		ps.qoeLastOn = make(map[string]time.Time)
	}
	cooldown := time.Duration(cfg.RefireCooldownS) * time.Second
	prevActive := ps.qoeActive
	next := make(map[string]struct{}, len(current))
	var out []string
	for _, l := range current {
		if _, dup := next[l]; dup {
			continue // de-dup within this row
		}
		next[l] = struct{}{}
		if _, on := prevActive[l]; !on {
			// Rising edge — emit only if never fired or clear for ≥ cooldown.
			if last, seen := ps.qoeLastOn[l]; !seen || now.Sub(last) >= cooldown {
				out = append(out, l)
			}
		}
		ps.qoeLastOn[l] = now // true on this row — extend the episode
	}
	ps.qoeActive = next

	// ── Terminal row: summary (current-at-end) + outcome + tier ────────
	if firstTerminal {
		// Force-emit every still-true condition not already emitted this row
		// (edge-trigger AND cooldown both suppress repeats), so the summary
		// row carries the full current-at-end set and qoe_tier_* reads it right.
		emitted := make(map[string]struct{}, len(out))
		for _, l := range out {
			emitted[l] = struct{}{}
		}
		for _, l := range current {
			if _, e := emitted[l]; !e {
				emitted[l] = struct{}{}
				out = append(out, l)
			}
		}
		// Startup outcome (terminal-only).
		switch {
		case r.PlaybackStatus == "abandoned_start":
			// Exit before video start — a user can simply bail during
			// startup. Warning, not a system failure.
			out = append(out, qoeLabel(SevWarning, "qoe_ebvs"))
		case isFailedStatus(r.PlaybackStatus) && r.VideoFirstFrameTimeMs == 0:
			out = append(out, qoeLabel(SevError, "qoe_vsf"))
		case isFailedStatus(r.PlaybackStatus) && r.VideoFirstFrameTimeMs > 0:
			out = append(out, qoeLabel(SevError, "qoe_msf"))
		}
		// Tier from the full terminal-row label set (worst severity).
		out = append(out, qoeTierLabel(out))
		ps.terminalEmitted = true
	}

	return out
}

// qoeTierLabel collapses the row's worst qoe severity into one tier
// label. No warning/critical/error ⇒ premium; worst == warning ⇒
// acceptable; worst >= critical ⇒ unacceptable.
func qoeTierLabel(labels []string) string {
	switch worstSeverity(labels) {
	case SevError, SevCritical:
		return qoeLabel(SevCritical, "qoe_tier_unacceptable")
	case SevWarning:
		return qoeLabel(SevWarning, "qoe_tier_acceptable")
	default: // info or none
		return qoeLabel(SevInfo, "qoe_tier_premium")
	}
}

// ── ABR helpers ──────────────────────────────────────────────────────

// abrBitrateLabels distinguishes a conservative ABR algorithm from an
// encoding-ladder gap. Precondition: the chosen variant uses less than
// BitrateUnderutilizedRatio of available throughput AND isn't already at
// the top rung. Throughput source is the responsive client measurement
// (network_bitrate, needs LocalProxy) with the laggy whole-play average
// as fallback. Then:
//   - a fitting higher rung exists (next ≤ throughput × headroom) ⇒
//     qoe_abr_conservative (player should have climbed)
//   - headroom exists but no rung fits ⇒ qoe_ladder_gap (player correct)
//
// With no parseable ladder we can't attribute the cause, so we stay
// silent rather than guess.
func abrBitrateLabels(cfg *QoEThresholds, r *row) []string {
	cur := float64(r.VideoBitrateMbps)
	if cur <= 0 {
		return nil
	}
	denom := float64(r.NetworkBitrateMbps)
	if denom <= 0 {
		denom = float64(r.AvgNetworkBitrateMbps)
	}
	if denom <= 0 {
		return nil
	}
	if cur/denom >= cfg.ABR.BitrateUnderutilizedRatio {
		return nil // using a healthy fraction of the link — not underutilized
	}
	ladder := parseVariantLadder(r.ManifestVariants)
	if len(ladder) == 0 {
		return nil // no ladder knowledge → can't attribute
	}
	if cur >= ladder[len(ladder)-1]*0.999 {
		return nil // already at the top rung — correctly ceilinged, not underutilized
	}
	if next, ok := nextRungAbove(ladder, cur); ok && next <= denom*cfg.ABR.AbrHeadroomMargin {
		return []string{qoeLabel(SevWarning, "qoe_abr_conservative")}
	}
	return []string{qoeLabel(SevInfo, "qoe_ladder_gap")}
}

// parseVariantLadder parses manifest_variants (JSON array of
// {bandwidth(bps), average_bandwidth, resolution, url}) into the rung
// bitrates in Mbps, sorted ascending. Malformed/empty ⇒ nil.
func parseVariantLadder(raw string) []float64 {
	if raw == "" || raw == "null" {
		return nil
	}
	var vs []struct {
		Bandwidth        float64 `json:"bandwidth"`
		AverageBandwidth float64 `json:"average_bandwidth"`
	}
	if err := json.Unmarshal([]byte(raw), &vs); err != nil {
		return nil
	}
	out := make([]float64, 0, len(vs))
	for _, v := range vs {
		bw := v.Bandwidth
		if bw <= 0 {
			bw = v.AverageBandwidth
		}
		if bw > 0 {
			out = append(out, bw/1e6) // bps → Mbps
		}
	}
	sort.Float64s(out)
	return out
}

// nextRungAbove returns the smallest rung strictly above cur (with a
// small epsilon so the current rung doesn't match itself on float jitter).
func nextRungAbove(ladder []float64, cur float64) (float64, bool) {
	for _, m := range ladder { // ascending
		if m > cur*1.001 {
			return m, true
		}
	}
	return 0, false
}

// minVariantStuckLabel fires when the play has dwelt at (within 5% of)
// the lowest rung for ≥ MinVariantStuckS. Tracks the dwell start on the
// per-play state; resets the moment it climbs off the floor. Needs ≥2
// rungs for "at the floor" to mean anything.
func minVariantStuckLabel(cfg *QoEThresholds, ps *playLabelState, r *row, now time.Time) string {
	cur := float64(r.VideoBitrateMbps)
	ladder := parseVariantLadder(r.ManifestVariants)
	if cur <= 0 || len(ladder) < 2 {
		ps.minVariantSince = time.Time{}
		return ""
	}
	if cur > ladder[0]*1.05 { // off the floor
		ps.minVariantSince = time.Time{}
		return ""
	}
	if ps.minVariantSince.IsZero() {
		ps.minVariantSince = now
		return ""
	}
	if now.Sub(ps.minVariantSince) >= time.Duration(cfg.ABR.MinVariantStuckS)*time.Second {
		return qoeLabel(SevWarning, "qoe_min_variant_stuck")
	}
	return ""
}

// fpsDipLabel fires when the dropped-frame ratio reaches FPSDipRatio —
// the proxy for "displayed fps fell below (1 - ratio) of nominal".
// Uses the cumulative displayed/dropped counters (per-interval rate is
// Phase 4 work).
func fpsDipLabel(cfg *QoEThresholds, r *row) string {
	total := r.FramesDisplayed + uint64(r.FramesDropped)
	if total == 0 {
		return ""
	}
	if float64(r.FramesDropped)/float64(total) >= cfg.ABR.FPSDipRatio {
		return qoeLabel(SevWarning, "qoe_fps_dip")
	}
	return ""
}

// throughputDivergenceLabel fires when the client measured MATERIALLY LESS
// throughput than the server actually delivered — nb < recent server-peak ×
// (1 - ThroughputDivergenceFactor). Only this under-read direction is kept:
// the over-read direction (client bitrate >> server, the AVPlayer burst quirk;
// see reference_ios_avg_bitrate_overreads) was the bulk of the per-heartbeat
// noise and is not a delivery problem, so it no longer trips this label. The
// under-read is a genuine client/server disagreement — the client believes it
// has less bandwidth than was delivered, which can drive over-conservative ABR.
// Always records the current server throughput into the windowed peak trail
// (even when network_bitrate is absent) so the peak stays warm. #657.
func throughputDivergenceLabel(cfg *QoEThresholds, ps *playLabelState, r *row, now time.Time) string {
	window := time.Duration(cfg.ABR.ThroughputPeakWindowS) * time.Second
	var peak float64
	ps.peakSamples, peak = recentPeak(ps.peakSamples, now, float64(r.MbpsTransferRate), window)

	nb := float64(r.NetworkBitrateMbps)
	if nb <= 0 {
		return "" // no responsive client measurement to compare
	}
	factor := cfg.ABR.ThroughputDivergenceFactor
	if peak > 0 && nb < peak*(1-factor) {
		return qoeLabel(SevWarning, "qoe_throughput_divergence")
	}
	return ""
}

// rateCapBreachLabel fires when the SERVER actually delivered above the
// effective cap by more than RateCapBreachFactor. The cap is kernel-enforced
// (tc/nftables), so it can't be breached at the wire by the client — the
// client's network_bitrate over-reads 2-3× the cap on AVPlayer burst (a known
// quirk, not a breach; see reference_ios_avg_bitrate_overreads). So we gate on
// the kernel-measured served rate (nftables_bandwidth_mbps), falling back to
// the proxy's per-segment transfer rate — making this a real over-delivery /
// shaping-failure detector (e.g. the boot restore-window spike, #671) instead
// of a per-heartbeat over-read alarm. #657.
func rateCapBreachLabel(cfg *QoEThresholds, r *row) string {
	limit := float64(r.EffectiveRateLimitMbps)
	if limit <= 0 {
		return ""
	}
	served := float64(r.NftablesBandwidthMbps)
	if served <= 0 {
		served = float64(r.MbpsTransferRate)
	}
	if served <= 0 {
		return ""
	}
	if served > limit*cfg.Network.RateCapBreachFactor {
		return qoeLabel(SevWarning, "qoe_rate_cap_breach")
	}
	return ""
}

// downshiftOvershootLabel fires when the SELECTED variant sits
// DownshiftOvershootRungs or more rungs below the rung the applied cap
// actually supports — i.e. the player over-corrected downward (#669).
// One rung below the ceiling is normal conservative ABR; two or more is
// the overshoot we want flagged on rampdown / pyramid-descent runs.
//
// All inputs are on the row: EffectiveRateLimitMbps (applied cap),
// parseVariantLadder(ManifestVariants) (ascending rung rates),
// VideoBitrateMbps (selected). With no cap or no parseable ladder we
// can't define a ceiling, so we stay silent. A selection AT or ABOVE
// the ceiling is fine here (an above-ceiling rate is qoe_rate_cap_breach's
// concern, not this).
func downshiftOvershootLabel(cfg *QoEThresholds, r *row) string {
	cap := float64(r.EffectiveRateLimitMbps)
	if cap <= 0 {
		return ""
	}
	cur := float64(r.VideoBitrateMbps)
	if cur <= 0 {
		return ""
	}
	ladder := parseVariantLadder(r.ManifestVariants)
	if len(ladder) < 2 {
		return "" // need ≥2 rungs for "rungs below" to mean anything
	}
	// ceilingIdx: highest rung whose rate ≤ cap (the rung the cap
	// supports). If even rung 0 exceeds the cap, the ceiling is rung 0 —
	// the player has nowhere lower to go, so it can't overshoot.
	ceilingIdx := 0
	for i, m := range ladder { // ascending
		if m <= cap*1.001 {
			ceilingIdx = i
		} else {
			break
		}
	}
	// curIdx: rung nearest the selected bitrate.
	curIdx, best := 0, math.Abs(ladder[0]-cur)
	for i, m := range ladder {
		if d := math.Abs(m - cur); d < best {
			curIdx, best = i, d
		}
	}
	if ceilingIdx-curIdx >= cfg.ABR.DownshiftOvershootRungs {
		return qoeLabel(SevWarning, "qoe_downshift_overshoot")
	}
	return ""
}

// cmcdMTPDriftLabel fires when the client-measured throughput (CMCD mtp,
// measured_mbps) diverges from the server-observed transfer rate by more
// than CMCDMTPDriftRatio.
func cmcdMTPDriftLabel(cfg *QoEThresholds, r *row) string {
	measured := float64(r.MeasuredMbps)
	actual := float64(r.MbpsTransferRate)
	if measured <= 0 || actual <= 0 {
		return ""
	}
	if math.Abs(measured-actual)/actual > cfg.Network.CMCDMTPDriftRatio {
		return qoeLabel(SevWarning, "qoe_cmcd_mtp_drift")
	}
	return ""
}

// ── Live helpers ─────────────────────────────────────────────────────

// liveOffsetLabels covers live-edge drift. Only meaningful when the
// manifest advertised a recommended offset (rec > 0) — VOD has none, so
// these stay silent there.
func liveOffsetLabels(cfg *QoEThresholds, r *row) []string {
	rec := float64(r.RecommendedOffsetS)
	if rec <= 0 {
		return nil
	}
	var out []string
	if off := float64(r.LiveOffsetS); off > 0 {
		excess := off - rec
		switch {
		case excess >= cfg.Live.OffsetBreachMarginS:
			out = append(out, qoeLabel(SevCritical, "qoe_live_offset_breach"))
		case excess >= cfg.Live.OffsetConcerningMarginS:
			out = append(out, qoeLabel(SevWarning, "qoe_live_offset_concerning"))
		}
	}
	if cfgd := float64(r.ConfiguredOffsetS); cfgd > 0 && math.Abs(cfgd-rec) > cfg.Live.HoldbackDeviationS {
		out = append(out, qoeLabel(SevWarning, "qoe_holdback_deviation"))
	}
	return out
}
