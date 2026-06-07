// #550 — QoE threshold config (Phase 2 + Phase 3 prereq).
//
// One layered JSON config covering both the outcome-classifier
// thresholds (Phase 2: EBVS, user_stopped_after) and the auto-label
// thresholds (Phase 3: VST, CIRR, CIRT, stall_burst, ...). Defaults
// compiled into Go so the forwarder boots cleanly without a config
// file; operator overrides via FORWARDER_QOE_THRESHOLDS_PATH env var.
//
// Loading is startup-only — restart the forwarder to pick up changes.
// Logs the resolved values at startup so operators can audit which
// tier their deployment is running at. Layered merge:
//   1. Hardcoded defaults (Conviva "good" tier)
//   2. Override file at FORWARDER_QOE_THRESHOLDS_PATH (if set)
//      → missing keys fall through to defaults
//
// Schema versioning: a `version` mismatch fails the load with a
// clear error so a binary + config mismatch never silently uses
// stale thresholds.

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
)

// QoEThresholds holds every threshold the forwarder uses to classify
// terminal outcomes (Phase 2) and to stamp auto-labels (Phase 3 —
// fields parsed but consumers not yet implemented).
type QoEThresholds struct {
	Version string `json:"version"`

	Outcomes struct {
		// EBVSThresholdMs — wait (in ms) before an in-progress
		// startup session is classified as abandoned_start (EBVS).
		// Conviva "good" = 10000; "best" = 8000.
		EBVSThresholdMs uint32 `json:"ebvs_threshold_ms"`

		// UserStoppedAfterThresholdMs — playing_time_ms above which
		// a user-stop counts as `user_stopped` (after-substantial-
		// watch) vs `user_stopped` (before-substantial-watch, i.e.
		// bounce).
		UserStoppedAfterThresholdMs uint32 `json:"user_stopped_after_threshold_ms"`
	} `json:"outcomes"`

	// Phase 3 — auto-labels. Parsed but consumed by a follow-up
	// labels.go expansion. Kept in the same config so a campaign
	// override file can tune both outcome thresholds AND label tiers
	// in one place.
	Startup struct {
		VSTConcerningMs  uint32  `json:"vst_concerning_ms"`
		VSTBreachMs      uint32  `json:"vst_breach_ms"`
		DRMDominantRatio float64 `json:"drm_dominant_ratio"`
	} `json:"startup"`

	Continuity struct {
		CIRRConcerning      float64 `json:"cirr_concerning"`
		CIRRBreach          float64 `json:"cirr_breach"`
		CIRTConcerningMs    uint32  `json:"cirt_concerning_ms"`
		CIRTBreachMs        uint32  `json:"cirt_breach_ms"`
		StallBurstThreshold uint32  `json:"stall_burst_threshold"`
		StallBurstWindowS   uint32  `json:"stall_burst_window_s"`
	} `json:"continuity"`

	Network struct {
		TTFBBreachMs        uint32  `json:"ttfb_breach_ms"`
		TransferStallMs     uint32  `json:"transfer_stall_ms"`
		RateCapBreachFactor float64 `json:"rate_cap_breach_factor"`
		CMCDMTPDriftRatio   float64 `json:"cmcd_mtp_drift_ratio"`
	} `json:"network"`

	// ABR / quality label thresholds (#553). The two bitrate cause
	// labels (qoe_abr_conservative / qoe_ladder_gap) share the
	// underutilization ratio + headroom margin; the storm/dwell labels
	// pair a count/duration with a window.
	ABR struct {
		// cur/throughput below this ⇒ underutilized (gate for the two
		// ladder-aware cause labels).
		BitrateUnderutilizedRatio float64 `json:"bitrate_underutilized_ratio"`
		// Next rung "fits" if next_rung_mbps ≤ throughput × this.
		AbrHeadroomMargin float64 `json:"abr_headroom_margin"`
		// network_bitrate diverging > this fraction from recent-peak
		// server throughput OR the cap trips qoe_throughput_divergence.
		ThroughputDivergenceFactor float64 `json:"throughput_divergence_factor"`
		// Window (s) over which the recent server-throughput peak is
		// taken for the divergence comparison.
		ThroughputPeakWindowS uint32 `json:"throughput_peak_window_s"`
		// > N rate_shift_down within window ⇒ qoe_downshift_storm.
		DownshiftStormThreshold uint32 `json:"downshift_storm_threshold"`
		DownshiftStormWindowS   uint32 `json:"downshift_storm_window_s"`
		// Dwell (s) pinned at the lowest rung ⇒ qoe_min_variant_stuck.
		MinVariantStuckS uint32 `json:"min_variant_stuck_s"`
		// Displayed fps below this fraction of nominal ⇒ qoe_fps_dip.
		FPSDipRatio float64 `json:"fps_dip_ratio"`
		// Playing-time (ms) a play must accumulate before qoe_abr_conservative
		// / qoe_ladder_gap / qoe_throughput_divergence are evaluated. Below
		// this the player is still in its startup ramp — sitting at a low
		// rung under high throughput is expected, not a defect — so those
		// labels are suppressed to avoid startup false positives. #595.
		StartupGraceMs uint32 `json:"startup_grace_ms"`
		// Selected variant sitting ≥ this many rungs below the rung the
		// applied cap supports ⇒ qoe_downshift_overshoot (#669). 1 rung
		// below is normal conservative ABR; 2+ is over-correction.
		DownshiftOvershootRungs int `json:"downshift_overshoot_rungs"`
	} `json:"abr"`

	// Live-edge label thresholds (#553). Margins are seconds BEYOND the
	// manifest-recommended offset.
	Live struct {
		OffsetConcerningMarginS float64 `json:"offset_concerning_margin_s"`
		OffsetBreachMarginS     float64 `json:"offset_breach_margin_s"`
		HoldbackDeviationS      float64 `json:"holdback_deviation_s"`
	} `json:"live"`
}

// qoeDefaults returns the Conviva "good" tier baked-in defaults.
// Override values in qoe_thresholds.json or via env var.
func qoeDefaults() *QoEThresholds {
	t := &QoEThresholds{Version: "1.0"}
	t.Outcomes.EBVSThresholdMs = 10000            // Conviva "good"
	t.Outcomes.UserStoppedAfterThresholdMs = 5000 // 5s = substantial-watch threshold
	t.Startup.VSTConcerningMs = 5000              // Conviva "best"
	t.Startup.VSTBreachMs = 10000                 // Conviva "good"
	t.Startup.DRMDominantRatio = 0.5
	t.Continuity.CIRRConcerning = 0.002 // Conviva "best"
	t.Continuity.CIRRBreach = 0.004     // Conviva "good"
	t.Continuity.CIRTConcerningMs = 1000
	t.Continuity.CIRTBreachMs = 2000
	t.Continuity.StallBurstThreshold = 3
	t.Continuity.StallBurstWindowS = 60
	t.Network.TTFBBreachMs = 500
	t.Network.TransferStallMs = 5000
	t.Network.RateCapBreachFactor = 1.10
	t.Network.CMCDMTPDriftRatio = 0.5
	t.ABR.BitrateUnderutilizedRatio = 0.5   // variant using < half the link
	t.ABR.AbrHeadroomMargin = 0.85          // need ~15% headroom to sustain a rung
	t.ABR.ThroughputDivergenceFactor = 0.15 // >15% client/server disagreement
	t.ABR.ThroughputPeakWindowS = 30
	t.ABR.DownshiftStormThreshold = 3
	t.ABR.DownshiftStormWindowS = 30
	t.ABR.MinVariantStuckS = 30
	t.ABR.FPSDipRatio = 0.2           // displayed fps < 80% of nominal
	t.ABR.StartupGraceMs = 10000      // suppress abr/throughput labels for the first 10s of playback
	t.ABR.DownshiftOvershootRungs = 2 // ≥2 rungs below the cap-supported ceiling ⇒ overshoot (#669)
	t.Live.OffsetConcerningMarginS = 3
	t.Live.OffsetBreachMarginS = 10
	t.Live.HoldbackDeviationS = 2
	return t
}

// loadQoEThresholds merges defaults with an optional override file at
// `path`. Empty path → defaults-only (no log noise; boots clean).
// Malformed override → logs the error and returns defaults so the
// forwarder doesn't fail to start on a bad config push.
func loadQoEThresholds(path string) *QoEThresholds {
	cfg := qoeDefaults()
	if path == "" {
		log.Printf("[QoE] using compiled-in defaults (set FORWARDER_QOE_THRESHOLDS_PATH to override)")
		logQoEResolved(cfg)
		return cfg
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("[QoE] override file %s unreadable (%v) — using compiled defaults", path, err)
		logQoEResolved(cfg)
		return cfg
	}
	// Decode into a shadow struct then field-by-field merge so a
	// missing key in the override falls through to the default
	// rather than zeroing.
	var override QoEThresholds
	if err := json.Unmarshal(data, &override); err != nil {
		log.Printf("[QoE] override file %s malformed (%v) — using compiled defaults", path, err)
		logQoEResolved(cfg)
		return cfg
	}
	if override.Version != "" && override.Version != cfg.Version {
		log.Printf("[QoE] override version mismatch (got %q, expected %q) — using compiled defaults", override.Version, cfg.Version)
		logQoEResolved(cfg)
		return cfg
	}
	if override.Outcomes.EBVSThresholdMs != 0 {
		cfg.Outcomes.EBVSThresholdMs = override.Outcomes.EBVSThresholdMs
	}
	if override.Outcomes.UserStoppedAfterThresholdMs != 0 {
		cfg.Outcomes.UserStoppedAfterThresholdMs = override.Outcomes.UserStoppedAfterThresholdMs
	}
	if override.Startup.VSTConcerningMs != 0 {
		cfg.Startup.VSTConcerningMs = override.Startup.VSTConcerningMs
	}
	if override.Startup.VSTBreachMs != 0 {
		cfg.Startup.VSTBreachMs = override.Startup.VSTBreachMs
	}
	if override.Startup.DRMDominantRatio != 0 {
		cfg.Startup.DRMDominantRatio = override.Startup.DRMDominantRatio
	}
	if override.Continuity.CIRRConcerning != 0 {
		cfg.Continuity.CIRRConcerning = override.Continuity.CIRRConcerning
	}
	if override.Continuity.CIRRBreach != 0 {
		cfg.Continuity.CIRRBreach = override.Continuity.CIRRBreach
	}
	if override.Continuity.CIRTConcerningMs != 0 {
		cfg.Continuity.CIRTConcerningMs = override.Continuity.CIRTConcerningMs
	}
	if override.Continuity.CIRTBreachMs != 0 {
		cfg.Continuity.CIRTBreachMs = override.Continuity.CIRTBreachMs
	}
	if override.Continuity.StallBurstThreshold != 0 {
		cfg.Continuity.StallBurstThreshold = override.Continuity.StallBurstThreshold
	}
	if override.Continuity.StallBurstWindowS != 0 {
		cfg.Continuity.StallBurstWindowS = override.Continuity.StallBurstWindowS
	}
	if override.Network.TTFBBreachMs != 0 {
		cfg.Network.TTFBBreachMs = override.Network.TTFBBreachMs
	}
	if override.Network.TransferStallMs != 0 {
		cfg.Network.TransferStallMs = override.Network.TransferStallMs
	}
	if override.Network.RateCapBreachFactor != 0 {
		cfg.Network.RateCapBreachFactor = override.Network.RateCapBreachFactor
	}
	if override.Network.CMCDMTPDriftRatio != 0 {
		cfg.Network.CMCDMTPDriftRatio = override.Network.CMCDMTPDriftRatio
	}
	if override.ABR.BitrateUnderutilizedRatio != 0 {
		cfg.ABR.BitrateUnderutilizedRatio = override.ABR.BitrateUnderutilizedRatio
	}
	if override.ABR.AbrHeadroomMargin != 0 {
		cfg.ABR.AbrHeadroomMargin = override.ABR.AbrHeadroomMargin
	}
	if override.ABR.ThroughputDivergenceFactor != 0 {
		cfg.ABR.ThroughputDivergenceFactor = override.ABR.ThroughputDivergenceFactor
	}
	if override.ABR.ThroughputPeakWindowS != 0 {
		cfg.ABR.ThroughputPeakWindowS = override.ABR.ThroughputPeakWindowS
	}
	if override.ABR.DownshiftStormThreshold != 0 {
		cfg.ABR.DownshiftStormThreshold = override.ABR.DownshiftStormThreshold
	}
	if override.ABR.DownshiftStormWindowS != 0 {
		cfg.ABR.DownshiftStormWindowS = override.ABR.DownshiftStormWindowS
	}
	if override.ABR.MinVariantStuckS != 0 {
		cfg.ABR.MinVariantStuckS = override.ABR.MinVariantStuckS
	}
	if override.ABR.FPSDipRatio != 0 {
		cfg.ABR.FPSDipRatio = override.ABR.FPSDipRatio
	}
	if override.ABR.StartupGraceMs != 0 {
		cfg.ABR.StartupGraceMs = override.ABR.StartupGraceMs
	}
	if override.Live.OffsetConcerningMarginS != 0 {
		cfg.Live.OffsetConcerningMarginS = override.Live.OffsetConcerningMarginS
	}
	if override.Live.OffsetBreachMarginS != 0 {
		cfg.Live.OffsetBreachMarginS = override.Live.OffsetBreachMarginS
	}
	if override.Live.HoldbackDeviationS != 0 {
		cfg.Live.HoldbackDeviationS = override.Live.HoldbackDeviationS
	}
	log.Printf("[QoE] loaded overrides from %s", path)
	logQoEResolved(cfg)
	return cfg
}

func logQoEResolved(cfg *QoEThresholds) {
	log.Printf("[QoE]   outcomes.ebvs_threshold_ms=%d", cfg.Outcomes.EBVSThresholdMs)
	log.Printf("[QoE]   outcomes.user_stopped_after_threshold_ms=%d", cfg.Outcomes.UserStoppedAfterThresholdMs)
	log.Printf("[QoE]   startup.vst_concerning_ms=%d vst_breach_ms=%d", cfg.Startup.VSTConcerningMs, cfg.Startup.VSTBreachMs)
	log.Printf("[QoE]   continuity.cirr_breach=%.4f cirt_breach_ms=%d stall_burst=%d/%ds",
		cfg.Continuity.CIRRBreach, cfg.Continuity.CIRTBreachMs,
		cfg.Continuity.StallBurstThreshold, cfg.Continuity.StallBurstWindowS)
	log.Printf("[QoE]   network.ttfb_breach_ms=%d transfer_stall_ms=%d rate_cap_breach_factor=%.2f cmcd_mtp_drift_ratio=%.2f",
		cfg.Network.TTFBBreachMs, cfg.Network.TransferStallMs,
		cfg.Network.RateCapBreachFactor, cfg.Network.CMCDMTPDriftRatio)
	log.Printf("[QoE]   abr.bitrate_underutilized_ratio=%.2f abr_headroom_margin=%.2f throughput_divergence_factor=%.2f downshift_storm=%d/%ds min_variant_stuck_s=%d fps_dip_ratio=%.2f startup_grace_ms=%d",
		cfg.ABR.BitrateUnderutilizedRatio, cfg.ABR.AbrHeadroomMargin, cfg.ABR.ThroughputDivergenceFactor,
		cfg.ABR.DownshiftStormThreshold, cfg.ABR.DownshiftStormWindowS, cfg.ABR.MinVariantStuckS, cfg.ABR.FPSDipRatio, cfg.ABR.StartupGraceMs)
	log.Printf("[QoE]   live.offset_concerning_margin_s=%.1f offset_breach_margin_s=%.1f holdback_deviation_s=%.1f",
		cfg.Live.OffsetConcerningMarginS, cfg.Live.OffsetBreachMarginS, cfg.Live.HoldbackDeviationS)
}

// Suppress the unused-import error in case fmt isn't otherwise
// referenced once parts of this file get trimmed. Harmless no-op
// referenced by the build pipeline.
var _ = fmt.Sprintf
