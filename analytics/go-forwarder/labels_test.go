// labels_test.go — coverage for the write-time labeler (issues #473,
// #474, #550 Phase 3). TestEventSwitchUnchanged is a characterization
// guard: it pins every label the LastEvent switch emitted BEFORE the
// Phase 3 refactor that converted the switch from early-return to an
// additive `out []string`. If a future change to the switch alters one
// of these, this test fails loudly rather than silently.
package main

import (
	"reflect"
	"sort"
	"testing"
)

// minimalRow builds a row that triggers no Phase 3 qoe_* labels: zero
// residency/error/bitrate inputs, non-terminal status, no manifest
// ladder. So whatever the LastEvent switch emits is the entire output.
func minimalRow(lastEvent string) *row {
	return &row{
		Ts:             "2026-06-01 00:00:00.000",
		PlayerID:       "p-char",
		PlayID:         "play-char",
		PlaybackStatus: "in_progress",
		LastEvent:      lastEvent,
	}
}

func TestEventSwitchUnchanged(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(r *row)
		want   []string
	}{
		// info — normal lifecycle
		{"rate_shift_up", nil, []string{"info=shift_up"}},
		{"rate_shift_down", nil, []string{"info=shift_down"}},
		{"video_first_frame", nil, []string{"info=first_frame"}},
		{"video_start_time", nil, []string{"info=playback_start"}},
		// warning — degraded but functioning
		{"timejump", nil, []string{"warning=timejump"}},
		{"segment_stall", nil, []string{"warning=stall_segment"}},
		// critical — user-visible impact
		{"frozen", nil, []string{"critical=stall_frozen"}},
		{"user_marked", nil, []string{"critical=user_marked_911"}},
		// restart splits on restart_reason (#603); legacy trigger_type fallback
		{"restart_reload", func(r *row) { r.LastEvent = "restart"; r.TriggerType = "reload" }, []string{"info=restart_reload"}},
		{"restart_auto", func(r *row) { r.LastEvent = "restart" }, []string{"critical=restart_auto_recovery"}},
		{"restart_user_retry", func(r *row) { r.LastEvent = "restart"; r.RestartReason = "user_retry" }, []string{"warning=restart_user_retry"}},
		{"restart_reason_auto", func(r *row) { r.LastEvent = "restart"; r.RestartReason = "auto_recovery" }, []string{"critical=restart_auto_recovery"}},
		// error
		{"error", nil, []string{"error=player_error"}},
		// pair-open arms emit nothing on the opening row
		{"stall_start", nil, nil},
		{"buffering_start", nil, nil},
		// pair-close arms: sticky per-event duration drives the label.
		// 1.5s, midplay context (no first-frame witnessed) → long_midplay.
		// Prefixed qoe_ in #568 (meaning/severity unchanged).
		{"stall_end", func(r *row) { r.StallDurationMs = 1500 }, []string{"warning=*qoe_stall_long_midplay"}},
		{"buffering_end", func(r *row) { r.BufferingDurationMs = 1500 }, []string{"warning=*qoe_stall_long_midplay"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Fresh state per case so pair-open/close windows and
			// first-frame witnesses don't bleed across subtests.
			s := newLabelState()
			r := minimalRow(tc.name)
			if tc.mutate != nil {
				tc.mutate(r)
			}
			got := computeEventLabelsWithState(s, r)
			assertLabelsEqual(t, got, tc.want)
		})
	}
}

// hasLabel reports whether want is present in labels.
func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

// evalQoE runs one row through the labeler on a fresh state (default
// thresholds) and returns the labels.
func evalQoE(r *row) []string {
	return computeEventLabelsWithState(newLabelState(), r)
}

const tsBase = "2026-06-01 00:00:00.000"

// --- Startup: VST boundaries -----------------------------------------

func TestQoEVSTBoundaries(t *testing.T) {
	// Defaults: concerning 5000ms, breach 10000ms. Non-terminal rows so
	// no tier chip lands.
	cases := []struct {
		vst  uint32
		want string // "" = neither
	}{
		{4999, ""},
		{5000, qoeLabel(SevWarning, "qoe_vst_concerning")},
		{9999, qoeLabel(SevWarning, "qoe_vst_concerning")},
		{10000, qoeLabel(SevCritical, "qoe_vst_breach")},
		{20000, qoeLabel(SevCritical, "qoe_vst_breach")},
	}
	for _, tc := range cases {
		got := evalQoE(&row{Ts: tsBase, PlayerID: "p", PlayID: "x", PlaybackStatus: "in_progress", VideoStartTimeMs: tc.vst})
		if tc.want == "" {
			if hasLabel(got, qoeLabel(SevWarning, "qoe_vst_concerning")) || hasLabel(got, qoeLabel(SevCritical, "qoe_vst_breach")) {
				t.Fatalf("vst=%d: expected no VST label, got %v", tc.vst, got)
			}
			continue
		}
		if !hasLabel(got, tc.want) {
			t.Fatalf("vst=%d: want %q in %v", tc.vst, tc.want, got)
		}
	}
}

// --- Startup: legacy video_startup_* retired (#568) ------------------

func TestStartupLegacyLabelsRetired(t *testing.T) {
	// A 5s first frame would have tripped the old error=*video_startup_severe
	// (>4s hardcoded). Post-#568 the video_first_frame arm must emit only its
	// info=first_frame lifecycle chip — no video_startup_* label.
	r := minimalRow("video_first_frame")
	r.VideoFirstFrameTimeS = 5.0
	got := computeEventLabelsWithState(newLabelState(), r)
	for _, retired := range []string{
		SevError + "=" + synthMark + "video_startup_severe",
		SevWarning + "=" + synthMark + "video_startup_long",
	} {
		if hasLabel(got, retired) {
			t.Fatalf("retired label %q must no longer be produced, got %v", retired, got)
		}
	}
	if !hasLabel(got, "info=first_frame") {
		t.Fatalf("video_first_frame arm should still emit info=first_frame, got %v", got)
	}

	// Startup latency is now keyed solely on VideoStartTimeMs (qoe_vst_*),
	// independent of VideoFirstFrameTimeS: a high first-frame time with no VST
	// yields no startup-latency label.
	r2 := &row{Ts: tsBase, PlayerID: "p", PlayID: "x", PlaybackStatus: "in_progress", VideoFirstFrameTimeS: 5.0}
	got2 := evalQoE(r2)
	if hasLabel(got2, qoeLabel(SevWarning, "qoe_vst_concerning")) || hasLabel(got2, qoeLabel(SevCritical, "qoe_vst_breach")) {
		t.Fatalf("VideoFirstFrameTimeS must not drive qoe_vst_*, got %v", got2)
	}
}

// --- Continuity: CIRR / CIRT boundaries ------------------------------

func TestQoECIRRBoundaries(t *testing.T) {
	// CIRR = stalling/(stalling+playing). concerning 0.002, breach 0.004.
	cases := []struct {
		stalling, playing uint32
		want              string
	}{
		{999, 499001, ""}, // 0.001998 < 0.002
		{1000, 499000, qoeLabel(SevWarning, "qoe_cirr_concerning")}, // exactly 0.002
		{2000, 498000, qoeLabel(SevCritical, "qoe_cirr_breach")},    // exactly 0.004
	}
	for _, tc := range cases {
		got := evalQoE(&row{Ts: tsBase, PlayerID: "p", PlayID: "x", PlaybackStatus: "in_progress", StallingTimeMs: tc.stalling, PlayingTimeMs: tc.playing})
		if tc.want == "" {
			if hasLabel(got, qoeLabel(SevWarning, "qoe_cirr_concerning")) || hasLabel(got, qoeLabel(SevCritical, "qoe_cirr_breach")) {
				t.Fatalf("cirr(%d,%d): expected none, got %v", tc.stalling, tc.playing, got)
			}
			continue
		}
		if !hasLabel(got, tc.want) {
			t.Fatalf("cirr(%d,%d): want %q in %v", tc.stalling, tc.playing, tc.want, got)
		}
	}
}

func TestQoECIRTBoundaries(t *testing.T) {
	// CIRT = stalling_ms/stalling_count vs 1000/2000. High playing keeps
	// CIRR silent so we isolate CIRT.
	const playing = 1_000_000_000
	cases := []struct {
		stalling, count uint32
		want            string
	}{
		{9990, 10, ""}, // 999 < 1000
		{10000, 10, qoeLabel(SevWarning, "qoe_cirt_concerning")}, // 1000
		{20000, 10, qoeLabel(SevCritical, "qoe_cirt_breach")},    // 2000
	}
	for _, tc := range cases {
		got := evalQoE(&row{Ts: tsBase, PlayerID: "p", PlayID: "x", PlaybackStatus: "in_progress", StallingTimeMs: tc.stalling, StallingCount: tc.count, PlayingTimeMs: playing})
		if tc.want == "" {
			if hasLabel(got, qoeLabel(SevWarning, "qoe_cirt_concerning")) || hasLabel(got, qoeLabel(SevCritical, "qoe_cirt_breach")) {
				t.Fatalf("cirt(%d/%d): expected none, got %v", tc.stalling, tc.count, got)
			}
			continue
		}
		if !hasLabel(got, tc.want) {
			t.Fatalf("cirt(%d/%d): want %q in %v", tc.stalling, tc.count, tc.want, got)
		}
	}
}

// --- Windowed: stall_burst & downshift_storm -------------------------

func TestQoEStallBurstWindowed(t *testing.T) {
	// threshold 3, window 60s: the 4th stall_start within the window trips.
	s := newLabelState()
	want := qoeLabel(SevCritical, "qoe_stall_burst")
	for i, ts := range []string{
		"2026-06-01 00:00:01.000",
		"2026-06-01 00:00:02.000",
		"2026-06-01 00:00:03.000",
		"2026-06-01 00:00:04.000",
	} {
		got := computeEventLabelsWithState(s, &row{Ts: ts, PlayerID: "p", PlayID: "x", PlaybackStatus: "in_progress", LastEvent: "stall_start"})
		if i < 3 && hasLabel(got, want) {
			t.Fatalf("stall #%d should not burst yet: %v", i+1, got)
		}
		if i == 3 && !hasLabel(got, want) {
			t.Fatalf("stall #4 should burst: %v", got)
		}
	}
}

func TestQoEStallBurstWindowExpiry(t *testing.T) {
	// Stalls spread > window apart never accumulate to a burst.
	s := newLabelState()
	want := qoeLabel(SevCritical, "qoe_stall_burst")
	for _, ts := range []string{
		"2026-06-01 00:00:00.000",
		"2026-06-01 00:02:00.000",
		"2026-06-01 00:04:00.000",
		"2026-06-01 00:06:00.000",
	} {
		got := computeEventLabelsWithState(s, &row{Ts: ts, PlayerID: "p", PlayID: "x", PlaybackStatus: "in_progress", LastEvent: "stall_start"})
		if hasLabel(got, want) {
			t.Fatalf("ts=%s: stalls 2min apart must not burst: %v", ts, got)
		}
	}
}

func TestQoEDownshiftStormWindowed(t *testing.T) {
	// threshold 3, window 30s: 4th rate_shift_down within window trips.
	s := newLabelState()
	want := qoeLabel(SevWarning, "qoe_downshift_storm")
	for i, ts := range []string{
		"2026-06-01 00:00:01.000",
		"2026-06-01 00:00:05.000",
		"2026-06-01 00:00:10.000",
		"2026-06-01 00:00:15.000",
	} {
		got := computeEventLabelsWithState(s, &row{Ts: ts, PlayerID: "p", PlayID: "x", PlaybackStatus: "in_progress", LastEvent: "rate_shift_down"})
		if i < 3 && hasLabel(got, want) {
			t.Fatalf("downshift #%d should not storm yet: %v", i+1, got)
		}
		if i == 3 && !hasLabel(got, want) {
			t.Fatalf("downshift #4 should storm: %v", got)
		}
	}
}

// --- ABR: ladder-aware bitrate cause labels --------------------------

func TestQoEBitrateCauseLabels(t *testing.T) {
	conservative := qoeLabel(SevWarning, "qoe_abr_conservative")
	ladderGap := qoeLabel(SevInfo, "qoe_ladder_gap")
	ladder3 := `[{"bandwidth":1000000},{"bandwidth":5000000},{"bandwidth":8000000}]`
	ladderGapLadder := `[{"bandwidth":1000000},{"bandwidth":9900000}]`

	cases := []struct {
		name              string
		cur, netbw, avgbw float32
		variants          string
		wantConservative  bool
		wantLadderGap     bool
	}{
		// cur 1, throughput 10 → 0.1 < 0.5 underutilized; next rung 5 ≤ 8.5 → conservative
		{"fitting rung", 1, 10, 0, ladder3, true, false},
		// next rung 9.9 > 8.5 → ladder gap
		{"no fitting rung", 1, 10, 0, ladderGapLadder, false, true},
		// at top rung → correctly ceilinged, neither
		{"top rung", 8, 20, 0, ladder3, false, false},
		// not underutilized (cur/throughput = 6/10 = 0.6 ≥ 0.5) → neither
		{"well utilized", 6, 10, 0, ladder3, false, false},
		// no ladder → can't attribute → neither
		{"no ladder", 1, 10, 0, "", false, false},
		// iOS fallback: network_bitrate 0, avg present → uses avg → conservative
		{"avg fallback", 1, 0, 10, ladder3, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := evalQoE(&row{
				Ts: tsBase, PlayerID: "p", PlayID: "x", PlaybackStatus: "in_progress",
				PlayingTimeMs:    10000, // past ABR startup grace (#595)
				VideoBitrateMbps: tc.cur, NetworkBitrateMbps: tc.netbw, AvgNetworkBitrateMbps: tc.avgbw,
				ManifestVariants: tc.variants,
			})
			if got1 := hasLabel(got, conservative); got1 != tc.wantConservative {
				t.Fatalf("conservative=%v want %v (%v)", got1, tc.wantConservative, got)
			}
			if got1 := hasLabel(got, ladderGap); got1 != tc.wantLadderGap {
				t.Fatalf("ladder_gap=%v want %v (%v)", got1, tc.wantLadderGap, got)
			}
		})
	}
}

func TestQoEBitrateMalformedLadder(t *testing.T) {
	// Malformed JSON must not panic and must emit no cause label.
	got := evalQoE(&row{Ts: tsBase, PlayerID: "p", PlayID: "x", PlaybackStatus: "in_progress", PlayingTimeMs: 10000,
		VideoBitrateMbps: 1, NetworkBitrateMbps: 10, ManifestVariants: "{not json"})
	if hasLabel(got, qoeLabel(SevWarning, "qoe_abr_conservative")) || hasLabel(got, qoeLabel(SevInfo, "qoe_ladder_gap")) {
		t.Fatalf("malformed ladder should emit no cause label: %v", got)
	}
}

func TestQoEMinVariantStuck(t *testing.T) {
	// Dwell ≥ 30s at the floor rung trips. Share state across two rows.
	s := newLabelState()
	want := qoeLabel(SevWarning, "qoe_min_variant_stuck")
	ladder := `[{"bandwidth":1000000},{"bandwidth":5000000}]`
	r1 := &row{Ts: "2026-06-01 00:00:00.000", PlayerID: "p", PlayID: "x", PlaybackStatus: "in_progress", VideoBitrateMbps: 1, ManifestVariants: ladder}
	if got := computeEventLabelsWithState(s, r1); hasLabel(got, want) {
		t.Fatalf("first floor sample should not trip: %v", got)
	}
	r2 := &row{Ts: "2026-06-01 00:00:31.000", PlayerID: "p", PlayID: "x", PlaybackStatus: "in_progress", VideoBitrateMbps: 1, ManifestVariants: ladder}
	if got := computeEventLabelsWithState(s, r2); !hasLabel(got, want) {
		t.Fatalf("31s at floor should trip: %v", got)
	}
}

func TestQoEFPSDip(t *testing.T) {
	// dropped/(displayed+dropped) ≥ 0.2.
	if got := evalQoE(&row{Ts: tsBase, PlayerID: "p", PlayID: "x", PlaybackStatus: "in_progress", FramesDisplayed: 81, FramesDropped: 19}); hasLabel(got, qoeLabel(SevWarning, "qoe_fps_dip")) {
		t.Fatalf("19%% drop should not dip: %v", got)
	}
	if got := evalQoE(&row{Ts: tsBase, PlayerID: "p", PlayID: "x", PlaybackStatus: "in_progress", FramesDisplayed: 80, FramesDropped: 20}); !hasLabel(got, qoeLabel(SevWarning, "qoe_fps_dip")) {
		t.Fatalf("20%% drop should dip: %v", got)
	}
}

// --- Network-tier (event-row inputs) ---------------------------------

func TestQoEThroughputDivergence(t *testing.T) {
	want := qoeLabel(SevWarning, "qoe_throughput_divergence")
	// nb matches server peak → no divergence.
	if got := evalQoE(&row{Ts: tsBase, PlayerID: "p", PlayID: "x", PlaybackStatus: "in_progress", PlayingTimeMs: 10000, NetworkBitrateMbps: 10, MbpsTransferRate: 10}); hasLabel(got, want) {
		t.Fatalf("matching throughput should not diverge: %v", got)
	}
	// nb 5 vs peak 10 → 50% > 15% → diverge.
	if got := evalQoE(&row{Ts: tsBase, PlayerID: "p", PlayID: "x", PlaybackStatus: "in_progress", PlayingTimeMs: 10000, NetworkBitrateMbps: 5, MbpsTransferRate: 10}); !hasLabel(got, want) {
		t.Fatalf("nb 5 vs peak 10 should diverge: %v", got)
	}
	// nb 5 vs cap 10 (no server peak) → diverge via limit branch.
	if got := evalQoE(&row{Ts: tsBase, PlayerID: "p", PlayID: "x", PlaybackStatus: "in_progress", PlayingTimeMs: 10000, NetworkBitrateMbps: 5, EffectiveRateLimitMbps: 10}); !hasLabel(got, want) {
		t.Fatalf("nb 5 vs cap 10 should diverge: %v", got)
	}
}

func TestQoEAbrStartupGate(t *testing.T) {
	// Inputs that trip BOTH qoe_abr_conservative (cur 1 under throughput 10,
	// next rung 5 fits) and qoe_throughput_divergence (nb 10 vs peak 20). But
	// during the startup ramp (playing time < grace) both are suppressed (#595).
	conservative := qoeLabel(SevWarning, "qoe_abr_conservative")
	divergence := qoeLabel(SevWarning, "qoe_throughput_divergence")
	ladder := `[{"bandwidth":1000000},{"bandwidth":5000000},{"bandwidth":8000000}]`
	mk := func(playingMs uint32) *row {
		return &row{
			Ts: tsBase, PlayerID: "p", PlayID: "x", PlaybackStatus: "in_progress",
			PlayingTimeMs: playingMs, VideoBitrateMbps: 1, NetworkBitrateMbps: 10,
			MbpsTransferRate: 20, ManifestVariants: ladder,
		}
	}
	// 5s in — still ramping → suppressed.
	if got := evalQoE(mk(5000)); hasLabel(got, conservative) || hasLabel(got, divergence) {
		t.Fatalf("startup ramp (5s playing) must suppress abr/throughput labels: %v", got)
	}
	// 10s in — settled → both fire.
	got := evalQoE(mk(10000))
	if !hasLabel(got, conservative) || !hasLabel(got, divergence) {
		t.Fatalf("settled play (10s) should emit both abr_conservative + throughput_divergence: %v", got)
	}
}

func TestQoERateCapBreach(t *testing.T) {
	want := qoeLabel(SevWarning, "qoe_rate_cap_breach")
	// 12 > 10*1.10=11 → breach.
	if got := evalQoE(&row{Ts: tsBase, PlayerID: "p", PlayID: "x", PlaybackStatus: "in_progress", NetworkBitrateMbps: 12, EffectiveRateLimitMbps: 10}); !hasLabel(got, want) {
		t.Fatalf("12 over cap 10 should breach: %v", got)
	}
	// 10.5 < 11 → no breach.
	if got := evalQoE(&row{Ts: tsBase, PlayerID: "p", PlayID: "x", PlaybackStatus: "in_progress", NetworkBitrateMbps: 10.5, EffectiveRateLimitMbps: 10}); hasLabel(got, want) {
		t.Fatalf("10.5 within factor should not breach: %v", got)
	}
}

func TestQoECMCDMTPDrift(t *testing.T) {
	want := qoeLabel(SevWarning, "qoe_cmcd_mtp_drift")
	// measured 10 vs actual 4 → 150% > 50% → drift.
	if got := evalQoE(&row{Ts: tsBase, PlayerID: "p", PlayID: "x", PlaybackStatus: "in_progress", MeasuredMbps: 10, MbpsTransferRate: 4}); !hasLabel(got, want) {
		t.Fatalf("measured/actual 10/4 should drift: %v", got)
	}
	// measured 5 vs actual 4 → 25% < 50% → no drift.
	if got := evalQoE(&row{Ts: tsBase, PlayerID: "p", PlayID: "x", PlaybackStatus: "in_progress", MeasuredMbps: 5, MbpsTransferRate: 4}); hasLabel(got, want) {
		t.Fatalf("measured/actual 5/4 within ratio should not drift: %v", got)
	}
}

// --- Network-tier (network-row labels) -------------------------------

func TestQoENetworkRowLabels(t *testing.T) {
	s := newLabelState()
	// clean 2xx row, TTFB 600 > 500, transfer 6000 > 5000 stall.
	got := computeNetworkLabelsWithState(s, &netRow{Ts: tsBase, Status: 200, TTFBMs: 600, TransferMs: 6000})
	if !hasLabel(got, qoeLabel(SevWarning, "qoe_ttfb_breach")) {
		t.Fatalf("TTFB 600 should breach: %v", got)
	}
	if !hasLabel(got, qoeLabel(SevWarning, "qoe_transfer_stall")) {
		t.Fatalf("transfer 6000 should stall: %v", got)
	}
	// healthy row → neither.
	got = computeNetworkLabelsWithState(s, &netRow{Ts: tsBase, Status: 200, TTFBMs: 100, TransferMs: 1000})
	if hasLabel(got, qoeLabel(SevWarning, "qoe_ttfb_breach")) || hasLabel(got, qoeLabel(SevWarning, "qoe_transfer_stall")) {
		t.Fatalf("healthy row should emit neither: %v", got)
	}
}

// --- Live tier -------------------------------------------------------

func TestQoELiveOffset(t *testing.T) {
	mk := func(off, cfgd float32) []string {
		return evalQoE(&row{Ts: tsBase, PlayerID: "p", PlayID: "x", PlaybackStatus: "in_progress",
			RecommendedOffsetS: 5, LiveOffsetS: off, ConfiguredOffsetS: cfgd})
	}
	concerning := qoeLabel(SevWarning, "qoe_live_offset_concerning")
	breach := qoeLabel(SevCritical, "qoe_live_offset_breach")
	holdback := qoeLabel(SevWarning, "qoe_holdback_deviation")
	// excess 2 < 3 → none; configured == recommended → no holdback.
	if got := mk(7, 5); hasLabel(got, concerning) || hasLabel(got, breach) || hasLabel(got, holdback) {
		t.Fatalf("excess 2 / no holdback should be clean: %v", got)
	}
	// excess 3 → concerning.
	if got := mk(8, 5); !hasLabel(got, concerning) {
		t.Fatalf("excess 3 should be concerning: %v", got)
	}
	// excess 10 → breach.
	if got := mk(15, 5); !hasLabel(got, breach) {
		t.Fatalf("excess 10 should breach: %v", got)
	}
	// configured 8 vs recommended 5 → dev 3 > 2 → holdback.
	if got := mk(5, 8); !hasLabel(got, holdback) {
		t.Fatalf("holdback dev 3 should fire: %v", got)
	}
}

func TestQoELiveOffsetSilentOnVOD(t *testing.T) {
	// No recommended offset (VOD) → no live labels.
	got := evalQoE(&row{Ts: tsBase, PlayerID: "p", PlayID: "x", PlaybackStatus: "in_progress", LiveOffsetS: 60})
	for _, l := range got {
		if l == qoeLabel(SevWarning, "qoe_live_offset_concerning") || l == qoeLabel(SevCritical, "qoe_live_offset_breach") {
			t.Fatalf("VOD row should emit no live-offset label: %v", got)
		}
	}
}

// --- Terminal gate + tier aggregate ----------------------------------

func TestQoETerminalGateAndTier(t *testing.T) {
	tierPremium := qoeLabel(SevInfo, "qoe_tier_premium")
	tierAcceptable := qoeLabel(SevWarning, "qoe_tier_acceptable")
	tierUnacceptable := qoeLabel(SevCritical, "qoe_tier_unacceptable")

	anyTier := func(ls []string) bool {
		return hasLabel(ls, tierPremium) || hasLabel(ls, tierAcceptable) || hasLabel(ls, tierUnacceptable)
	}

	// Sticky terminal STATUS without the terminal EVENT → NO tier chip.
	// This is the fix: post-failure heartbeats carry the status but aren't
	// the end event, so they must not be classified as the play outcome.
	got := evalQoE(&row{Ts: tsBase, PlayerID: "p", PlayID: "x", PlaybackStatus: "mid_stream_failure", VideoFirstFrameTimeMs: 100})
	if anyTier(got) {
		t.Fatalf("sticky terminal status w/o session_end must not get a tier chip: %v", got)
	}

	// session_end clean → premium.
	if got := evalQoE(&row{Ts: tsBase, PlayerID: "p", PlayID: "x", LastEvent: "session_end", PlaybackStatus: "completed"}); !hasLabel(got, tierPremium) {
		t.Fatalf("clean terminal → premium: %v", got)
	}
	// session_end with a warning (VST concerning) → acceptable.
	if got := evalQoE(&row{Ts: tsBase, PlayerID: "p", PlayID: "x", LastEvent: "session_end", PlaybackStatus: "completed", VideoStartTimeMs: 6000}); !hasLabel(got, tierAcceptable) {
		t.Fatalf("warning terminal → acceptable: %v", got)
	}
	// play_end with a critical (VST breach) → unacceptable. Also proves the
	// {session_end, play_end} terminal-event set both trigger.
	if got := evalQoE(&row{Ts: tsBase, PlayerID: "p", PlayID: "x", LastEvent: "play_end", PlaybackStatus: "completed", VideoStartTimeMs: 20000}); !hasLabel(got, tierUnacceptable) {
		t.Fatalf("critical terminal (play_end) → unacceptable: %v", got)
	}
}

func TestQoETerminalFailureLabels(t *testing.T) {
	// vsf: failed before first frame.
	if got := evalQoE(&row{Ts: tsBase, PlayerID: "p", PlayID: "x", LastEvent: "session_end", PlaybackStatus: "start_failure", VideoFirstFrameTimeMs: 0}); !hasLabel(got, qoeLabel(SevError, "qoe_vsf")) {
		t.Fatalf("start_failure pre-first-frame → vsf: %v", got)
	}
	// msf: failed after first frame.
	if got := evalQoE(&row{Ts: tsBase, PlayerID: "p", PlayID: "x", LastEvent: "session_end", PlaybackStatus: "mid_stream_failure", VideoFirstFrameTimeMs: 250}); !hasLabel(got, qoeLabel(SevError, "qoe_msf")) {
		t.Fatalf("mid_stream_failure post-first-frame → msf: %v", got)
	}
	// ebvs: abandoned start — warning (a user can bail during startup).
	if got := evalQoE(&row{Ts: tsBase, PlayerID: "p", PlayID: "x", LastEvent: "session_end", PlaybackStatus: "abandoned_start"}); !hasLabel(got, qoeLabel(SevWarning, "qoe_ebvs")) {
		t.Fatalf("abandoned_start → ebvs: %v", got)
	}
	// Negative: a failed status WITHOUT the terminal event → no outcome
	// label (it's a mid-play heartbeat carrying a sticky status).
	if got := evalQoE(&row{Ts: tsBase, PlayerID: "p", PlayID: "x", PlaybackStatus: "mid_stream_failure", VideoFirstFrameTimeMs: 250}); hasLabel(got, qoeLabel(SevError, "qoe_msf")) {
		t.Fatalf("failed status w/o session_end must not emit msf: %v", got)
	}
}

// TestQoETerminalEmitOnce — playback_status is sticky client pass-through,
// so a failed session re-sends the terminal status on every heartbeat. The
// terminal aggregates (msf + tier here) must fire only on the FIRST such
// row, never on the repeats.
func TestQoETerminalEmitOnce(t *testing.T) {
	s := newLabelState()
	msf := qoeLabel(SevError, "qoe_msf")
	tier := qoeLabel(SevCritical, "qoe_tier_unacceptable")
	mk := func(ts string) *row {
		return &row{Ts: ts, PlayerID: "p", PlayID: "x", LastEvent: "session_end", PlaybackStatus: "mid_stream_failure", VideoFirstFrameTimeMs: 250}
	}
	first := computeEventLabelsWithState(s, mk("2026-06-01 00:00:00.000"))
	if !hasLabel(first, msf) || !hasLabel(first, tier) {
		t.Fatalf("first terminal row should carry msf + tier: %v", first)
	}
	for _, ts := range []string{"2026-06-01 00:00:05.000", "2026-06-01 00:00:10.000"} {
		repeat := computeEventLabelsWithState(s, mk(ts))
		if hasLabel(repeat, msf) || hasLabel(repeat, tier) {
			t.Fatalf("sticky-status repeat at %s must NOT re-emit terminal labels: %v", ts, repeat)
		}
	}
}

// assertLabelsEqual compares label slices order-independently — the
// labeler's ordering is an implementation detail, the set is the
// contract.
func assertLabelsEqual(t *testing.T, got, want []string) {
	t.Helper()
	g := append([]string(nil), got...)
	w := append([]string(nil), want...)
	sort.Strings(g)
	sort.Strings(w)
	if len(g) == 0 && len(w) == 0 {
		return
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("labels mismatch\n got: %v\nwant: %v", got, want)
	}
}

// --- Testing tier: operator/test KV labels (#571) --------------------

func TestKVLabelsUseTestingSeverity(t *testing.T) {
	got := kvLabelsFromInfo(`{"run_id":"20260530T141942Z","test":"rampup"}`)
	want := map[string]bool{
		"testing=run_id_20260530T141942Z": true,
		"testing=test_rampup":             true,
	}
	if len(got) != len(want) {
		t.Fatalf("want %d testing labels, got %v", len(want), got)
	}
	for _, l := range got {
		if !want[l] {
			t.Fatalf("operator KV label must be testing= prefixed, got %q in %v", l, got)
		}
	}
}

func TestTestingSeverityUnranked(t *testing.T) {
	// Testing-only labels register no severity → no row tint, no
	// classification bump.
	if sev := worstSeverity([]string{"testing=run_id_x", "testing=test_y"}); sev != "" {
		t.Fatalf("testing labels must be unranked, got %q", sev)
	}
	// A real signal alongside still wins.
	if sev := worstSeverity([]string{"testing=run_id_x", SevWarning + "=timejump"}); sev != SevWarning {
		t.Fatalf("real signal must still rank past testing labels, got %q", sev)
	}
}

// --- Edge-triggering: rising edge + terminal summary (#595) ----------

func TestQoEStickyEmitsOnceNotEveryRow(t *testing.T) {
	s := newLabelState()
	concerning := qoeLabel(SevWarning, "qoe_vst_concerning")
	mk := func(ts string) *row {
		return &row{Ts: ts, PlayerID: "p", PlayID: "x", PlaybackStatus: "in_progress", VideoStartTimeMs: 6000}
	}
	// Row 1: first determination → emit.
	if got := computeEventLabelsWithState(s, mk("2026-06-03 00:00:01.000")); !hasLabel(got, concerning) {
		t.Fatalf("first determination should emit %s: %v", concerning, got)
	}
	// Rows 2-3: same sticky VST → suppressed, no per-heartbeat repeat.
	for _, ts := range []string{"2026-06-03 00:00:02.000", "2026-06-03 00:00:03.000"} {
		if got := computeEventLabelsWithState(s, mk(ts)); hasLabel(got, concerning) {
			t.Fatalf("sticky VST must not re-stamp %s on %s: %v", concerning, ts, got)
		}
	}
}

func TestQoEReFiresOnNewInstance(t *testing.T) {
	s := newLabelState()
	want := qoeLabel(SevWarning, "qoe_cirr_concerning")
	// CIRR = stalling/(stalling+playing). 0.002 = concerning threshold.
	hot := func(ts string) *row {
		return &row{Ts: ts, PlayerID: "p", PlayID: "x", PlaybackStatus: "in_progress", StallingTimeMs: 1000, PlayingTimeMs: 499000}
	}
	cold := func(ts string) *row {
		return &row{Ts: ts, PlayerID: "p", PlayID: "x", PlaybackStatus: "in_progress", StallingTimeMs: 1, PlayingTimeMs: 10_000_000}
	}
	if got := computeEventLabelsWithState(s, hot("2026-06-03 00:00:01.000")); !hasLabel(got, want) {
		t.Fatalf("rising edge should emit %s: %v", want, got)
	}
	if got := computeEventLabelsWithState(s, hot("2026-06-03 00:00:02.000")); hasLabel(got, want) {
		t.Fatalf("sustained must suppress %s: %v", want, got)
	}
	if got := computeEventLabelsWithState(s, cold("2026-06-03 00:00:03.000")); hasLabel(got, want) {
		t.Fatalf("cleared row must not emit %s: %v", want, got)
	}
	// Cleared then true again = a new instance → re-emit.
	if got := computeEventLabelsWithState(s, hot("2026-06-03 00:00:04.000")); !hasLabel(got, want) {
		t.Fatalf("recurrence after clearing should re-emit %s: %v", want, got)
	}
}

func TestQoETerminalSummaryForcesStillTrue(t *testing.T) {
	s := newLabelState()
	concerning := qoeLabel(SevWarning, "qoe_vst_concerning")
	mk := func(ts, ev, status string) *row {
		return &row{Ts: ts, PlayerID: "p", PlayID: "x", LastEvent: ev, PlaybackStatus: status, VideoStartTimeMs: 6000}
	}
	computeEventLabelsWithState(s, mk("2026-06-03 00:00:01.000", "", "in_progress")) // first determination (emits)
	computeEventLabelsWithState(s, mk("2026-06-03 00:00:02.000", "", "in_progress")) // sustained (suppressed)
	// Terminal row: summary must force-emit the still-true VST + the tier.
	got := computeEventLabelsWithState(s, mk("2026-06-03 00:00:03.000", "session_end", "completed"))
	if !hasLabel(got, concerning) {
		t.Fatalf("terminal summary must carry still-true %s: %v", concerning, got)
	}
	if !hasLabel(got, qoeLabel(SevWarning, "qoe_tier_acceptable")) {
		t.Fatalf("tier must reflect the summary (warning → acceptable): %v", got)
	}
}
