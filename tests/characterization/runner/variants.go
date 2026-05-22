package runner

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

// TCPOverheadPct is the framework's per-cap allowance for TCP+IP+TLS+
// HTTP framing overhead. Applied multiplicatively on top of any test
// margin so that "margin 0" means "cap = variant_avg × 1.07" — the
// just-barely-sustainable real-world cap. Empirically the on-wire
// overhead is 5-8% across the throughput range we test (per-packet TCP
// headers ≈ 2.7%, TLS framing ≈ 3-5%, HTTP/2 frames ≈ 1-2%); 7% is the
// midpoint and covers low-rate connection-setup amortization concerns
// without being wasteful at the top of the ladder.
const TCPOverheadPct = 7

// VariantRate is one rung of the current play's ladder, with the cap rate
// the characterization sweep should apply for it: the variant's
// AVERAGE-BANDWIDTH (preferred — long-term sustainable) or BANDWIDTH (peak
// per HLS spec) × (1 + marginPct/100) × (1 + TCPOverheadPct/100).
//
// Both AvgBps + PeakBps are recorded for diagnostics — Source tells you
// which one fed the cap calc. RawBps is kept as an alias of "the one we
// used" for backward compat with existing report JSON.
//
// Why both matter: a cap at avg×1.05 is enough for *sustained* playback
// at that variant, but the player's per-segment fetch peak can be 30-40%
// higher than avg for typical CBR. If the cap is below peak, the player
// can't keep up during peak segments and downshifts — which is exactly
// the "+5% isn't enough" finding the smooth sweep surfaces on AVPlayer.
type VariantRate struct {
	Resolution string  `json:"resolution"`
	URL        string  `json:"url"`
	AvgBps     int     `json:"avg_bps"`     // AVERAGE-BANDWIDTH from the master playlist, 0 if absent
	PeakBps    int     `json:"peak_bps"`    // BANDWIDTH (per HLS spec — peak segment rate)
	RawBps     int     `json:"raw_bps"`     // value used for cap calc (avg if present, else peak)
	Source     string  `json:"source"`      // "average" or "peak" — which one fed RawBps
	MarginPct  int     `json:"margin_pct"`  // headroom we applied
	CapMbps    float64 `json:"cap_mbps"`    // RawBps × (1+margin/100) / 1e6
}

// VariantRatesDesc reads the bound player's current manifest variants and
// returns one VariantRate per rung, sorted descending by cap rate. Margin
// is in percent (5 = 5% headroom). The descending order mirrors the
// server's `ramp_down` pattern and the dashboard's buildSteps shape.
//
// Errors when the player has no manifest variants yet (master playlist
// hasn't been fetched, or the v2 projection is missing them).
func VariantRatesDesc(rec *PlayerRecord, marginPct int) ([]VariantRate, error) {
	if rec == nil || rec.CurrentPlay == nil {
		return nil, errors.New("variant rates: player has no current play")
	}
	variants := rec.CurrentPlay.Manifest.Variants
	if len(variants) == 0 {
		return nil, errors.New("variant rates: manifest has no variants (master playlist not fetched yet?)")
	}
	if marginPct <= -100 {
		return nil, fmt.Errorf("variant rates: margin %d would zero or invert the cap", marginPct)
	}
	rates := make([]VariantRate, 0, len(variants))
	for _, v := range variants {
		bps := v.Bandwidth
		source := "peak"
		if v.AverageBandwidth > 0 {
			bps = v.AverageBandwidth
			source = "average"
		}
		if bps <= 0 {
			continue
		}
		// cap = raw × (1 + margin/100) × (1 + TCP_overhead/100) / 1e6
		// The TCP overhead factor is a framework constant — operators
		// only pick margin; the framework adds 7% on top so margin=0
		// produces a just-barely-sustainable cap (i.e. variant_avg
		// PLUS the on-wire framing tax).
		cap := float64(bps) *
			(1 + float64(marginPct)/100) *
			(1 + float64(TCPOverheadPct)/100) /
			1_000_000
		// Round to 3 decimal places — same precision the harness CLI uses
		// so the dashboard and the framework apply identical numbers.
		cap = math.Round(cap*1000) / 1000
		rates = append(rates, VariantRate{
			Resolution: v.Resolution,
			URL:        v.URL,
			AvgBps:     v.AverageBandwidth,
			PeakBps:    v.Bandwidth,
			RawBps:     bps,
			Source:     source,
			MarginPct:  marginPct,
			CapMbps:    cap,
		})
	}
	if len(rates) == 0 {
		return nil, errors.New("variant rates: all variants had zero bandwidth")
	}
	sort.Slice(rates, func(i, j int) bool { return rates[i].CapMbps > rates[j].CapMbps })
	return rates, nil
}

// StepsFromVariants converts a descending variant-rate list into a Step
// slice, one per rung, with the supplied hold duration. The framework
// applies each step's CapMbps in turn — at the right rate to keep that
// variant just-barely-playable on a clean network.
func StepsFromVariants(rates []VariantRate, hold time.Duration) []Step {
	out := make([]Step, 0, len(rates))
	for _, r := range rates {
		out = append(out, Step{RateMbps: r.CapMbps, Hold: hold})
	}
	return out
}

// VariantSweep produces a fine-grained descending sweep over every
// (variant, margin) pair, then prunes any cap that doesn't strictly
// decrease against the previous emitted cap. Used by the smooth mode to
// characterize each rung at several headroom levels:
//
//	margins := []int{50, 25, 10, 5, 0, -5}
//	→ +50% (comfortable) … +5% (operational default) … -5% (forced downshift)
//
// The "strictly decrease" prune is the safety net the operator asked for:
// on tight ladders where a high margin on a low rung would overshoot a
// low margin on the next-higher rung (e.g. var_high×0.95 < var_low×1.50),
// we drop the offending step so the cap series remains monotonically
// non-increasing. On the test-dev ladder this never fires because
// adjacent rungs are ≥2× apart — every candidate survives.
//
// Returned slice is the cap series to apply, in order; each VariantRate
// carries the variant identity + the specific margin used at that step.
func VariantSweep(rec *PlayerRecord, margins []int) ([]VariantRate, error) {
	if len(margins) == 0 {
		return nil, errors.New("variant sweep: no margins supplied")
	}
	// Build per-variant candidates at every margin.
	all := []VariantRate{}
	for _, m := range margins {
		rates, err := VariantRatesDesc(rec, m)
		if err != nil {
			return nil, err
		}
		all = append(all, rates...)
	}
	// Sort all candidates descending by cap.
	sort.Slice(all, func(i, j int) bool { return all[i].CapMbps > all[j].CapMbps })
	// Walk through, emitting only strict-decreases vs. the last emitted.
	out := make([]VariantRate, 0, len(all))
	last := math.Inf(1)
	for _, c := range all {
		if c.CapMbps < last {
			out = append(out, c)
			last = c.CapMbps
		}
	}
	return out, nil
}

// VariantBandwidth captures the per-rung bandwidth from the manifest
// (avg + peak in Mbps, both rounded to 3 decimals for log brevity).
// Keyed by resolution string (e.g. "3840x2160").
type VariantBandwidth struct {
	AvgMbps  float64 `json:"avg_mbps"`
	PeakMbps float64 `json:"peak_mbps"`
}

// VariantBandwidthByResolution returns a map of resolution →
// {avg,peak} Mbps for every variant in the bound player's manifest.
// Used by tests that want to annotate log lines with the bandwidth
// context for a variant they're discussing — operator decisions read
// better when "settled=1440p" is followed by "(avg=10.845 peak=15.364)"
// instead of forcing the reader to remember the ladder.
//
// Audio entries (resolution=="") are dropped. Returns an empty map
// when the manifest hasn't been fetched yet.
func VariantBandwidthByResolution(rec *PlayerRecord) map[string]VariantBandwidth {
	out := map[string]VariantBandwidth{}
	if rec == nil || rec.CurrentPlay == nil {
		return out
	}
	for _, v := range rec.CurrentPlay.Manifest.Variants {
		res := strings.TrimSpace(v.Resolution)
		if res == "" {
			continue
		}
		out[res] = VariantBandwidth{
			AvgMbps:  math.Round(float64(v.AverageBandwidth)/1000) / 1000,
			PeakMbps: math.Round(float64(v.Bandwidth)/1000) / 1000,
		}
	}
	return out
}

// AnnotateVariant renders a one-liner "(avg=X peak=Y cap=Z)" string
// for log lines that mention a variant resolution. Empty resolution
// → empty string. Missing capMbps (≤0) → omit the cap portion.
// Missing variant in the lookup → still emit the cap if present.
func AnnotateVariant(bws map[string]VariantBandwidth, resolution string, capMbps float64) string {
	bw, ok := bws[resolution]
	switch {
	case ok && capMbps > 0:
		return fmt.Sprintf("(avg=%.3f peak=%.3f cap=%.3f)", bw.AvgMbps, bw.PeakMbps, capMbps)
	case ok:
		return fmt.Sprintf("(avg=%.3f peak=%.3f)", bw.AvgMbps, bw.PeakMbps)
	case capMbps > 0:
		return fmt.Sprintf("(cap=%.3f)", capMbps)
	}
	return ""
}

// videoVariantDirRE extracts the variant id from a per-variant playlist
// URL like "playlist_6s_2160p.m3u8" → "2160p". The id is also the
// segment-directory name under the content root (segments live in
// /<content>/<id>/segment_NN.m4s).
//
// Heuristic — works for the test-dev encoder's filename convention.
// Different encoders may use different schemes; for those cases the
// caller can override by passing url substrings directly.
var videoVariantDirRE = regexp.MustCompile(`(?:^|/)playlist[_-]?[0-9]*s?[_-]?([A-Za-z0-9]+)\.m3u8$`)

// VideoVariantDirs returns the segment-directory names for every video
// variant in the bound player's current manifest. Audio variants (and
// any entry with an empty Resolution) are excluded.
//
// Used by the abort characterization test to scope fault injection to
// only the video portion of the stream — without that scope, faults
// fire on whichever segment hits first (audio or video), and audio
// hits dominate because audio segments are smaller / more frequent.
//
// Returns an error when the manifest hasn't been fetched yet.
func VideoVariantDirs(rec *PlayerRecord) ([]string, error) {
	if rec == nil || rec.CurrentPlay == nil {
		return nil, errors.New("video variant dirs: player has no current play")
	}
	variants := rec.CurrentPlay.Manifest.Variants
	if len(variants) == 0 {
		return nil, errors.New("video variant dirs: manifest has no variants")
	}
	seen := map[string]bool{}
	out := []string{}
	for _, v := range variants {
		if strings.TrimSpace(v.Resolution) == "" {
			continue // audio-only / unresolved — skip
		}
		dir := variantDirFromPlaylistURL(v.URL)
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		out = append(out, dir)
	}
	if len(out) == 0 {
		return nil, errors.New("video variant dirs: no video entries derived from manifest")
	}
	return out, nil
}

func variantDirFromPlaylistURL(url string) string {
	if m := videoVariantDirRE.FindStringSubmatch(url); m != nil {
		return m[1]
	}
	// Fallback: strip ".m3u8", take part after final underscore or slash.
	stem := strings.TrimSuffix(url, ".m3u8")
	if i := strings.LastIndexAny(stem, "_/-"); i >= 0 {
		return stem[i+1:]
	}
	return stem
}
