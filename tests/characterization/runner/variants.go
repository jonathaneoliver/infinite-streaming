package runner

import (
	"errors"
	"fmt"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/go-proxy/pkg/ladder"
)

// VariantRate is one rung of the current play's limit ladder, with the
// cap rate the characterization sweep applies for it. Since #551 the
// ladder is built by the shared go-proxy/pkg/ladder package: it carries
// BOTH a peak (BANDWIDTH) and an average (AVERAGE-BANDWIDTH) anchor per
// variant, each ×(1+bump) at a flat bump (default 5%), plus geometric
// fills so no two consecutive caps differ by more than CHAR_LADDER_MAX_STEP.
//
// Each rung is attributed to the manifest variant a PEAK-keyed player
// should sustain at that cap (the confirmed model: hls.js/ExoPlayer/Shaka
// all key down-switch on peak; AVPlayer is unknown so we carry both
// scalars). Resolution/AvgBps/PeakBps therefore describe that expected
// variant; Source records the rung's provenance ("peak", "avg" or
// "fill"). RawBps mirrors PeakBps for backward-compat with report JSON
// and stepOnTarget's expected-bitrate check. MarginPct is retained for
// the report schema but is 0 under the flat-bump model.
type VariantRate struct {
	Resolution string  `json:"resolution"`
	URL        string  `json:"url"`
	AvgBps     int     `json:"avg_bps"`    // expected variant's AVERAGE-BANDWIDTH, 0 if absent
	PeakBps    int     `json:"peak_bps"`   // expected variant's BANDWIDTH (peak per HLS spec)
	RawBps     int     `json:"raw_bps"`    // == PeakBps (peak-anchored matching)
	Source     string  `json:"source"`     // "peak" | "avg" | "fill" — rung provenance
	MarginPct  int     `json:"margin_pct"` // 0 under the flat-bump ladder
	CapMbps    float64 `json:"cap_mbps"`   // the rate to shape to for this rung
}

// LadderBumpPct / LadderMaxStep / LadderTopHeadroomPct read the optional
// operator overrides, falling back to the shared package defaults (5% flat
// bump, 1.15× steps, 25% top-headroom start rung).
func LadderBumpPct() float64 { return envFloat("CHAR_LADDER_BUMP_PCT", ladder.DefaultBumpPct) }
func LadderMaxStep() float64 { return envFloat("CHAR_LADDER_MAX_STEP", ladder.DefaultMaxStep) }
func LadderTopHeadroomPct() float64 {
	return envFloat("CHAR_LADDER_TOP_HEADROOM_PCT", ladder.DefaultTopHeadroomPct)
}

func envFloat(key string, def float64) float64 {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f >= 0 {
			return f
		}
	}
	return def
}

// StandardLadderRates builds the dual-rung + geometrically-filled limit
// ladder for the bound player's current manifest, sorted descending by
// cap. Every rung (peak/avg anchor or interpolated fill) is attributed to
// the manifest variant a peak-keyed player should sustain at that cap, so
// the variant-aware sweep (stepOnTarget / per-variant pass-fail / Finalize)
// works unchanged. Errors when the master playlist hasn't been fetched.
func StandardLadderRates(rec *PlayerRecord) ([]VariantRate, error) {
	if rec == nil || rec.CurrentPlay == nil {
		return nil, errors.New("ladder: player has no current play")
	}
	variants := rec.CurrentPlay.Manifest.Variants
	if len(variants) == 0 {
		return nil, errors.New("ladder: manifest has no variants (master playlist not fetched yet?)")
	}
	lv := make([]ladder.Variant, 0, len(variants))
	for _, v := range variants {
		lv = append(lv, ladder.Variant{AvgBps: v.AverageBandwidth, PeakBps: v.Bandwidth, Resolution: v.Resolution})
	}
	bump := LadderBumpPct()
	rungs := ladder.StandardLadder(lv, bump, LadderMaxStep(), LadderTopHeadroomPct())
	if len(rungs) == 0 {
		return nil, errors.New("ladder: all variants had zero bandwidth")
	}
	// Manifest variants sorted descending by peak, for cap→variant attribution.
	manifest := make([]struct {
		res        string
		url        string
		avg, peak  int
		peakCapMbp float64
	}, 0, len(variants))
	f := 1 + bump/100
	for _, v := range variants {
		if v.Bandwidth <= 0 {
			continue
		}
		manifest = append(manifest, struct {
			res        string
			url        string
			avg, peak  int
			peakCapMbp float64
		}{v.Resolution, v.URL, v.AverageBandwidth, v.Bandwidth, math.Round(float64(v.Bandwidth)*f/1e6*1000) / 1000})
	}
	sort.Slice(manifest, func(i, j int) bool { return manifest[i].peak > manifest[j].peak })

	out := make([]VariantRate, 0, len(rungs))
	for _, r := range rungs {
		// Highest variant whose peak cap is still affordable at this rung's
		// rate is the one a peak-keyed player should sustain; below them all
		// it's the bottom variant.
		ev := manifest[len(manifest)-1]
		for _, m := range manifest {
			if m.peakCapMbp <= r.Mbps {
				ev = m
				break
			}
		}
		out = append(out, VariantRate{
			Resolution: ev.res,
			URL:        ev.url,
			AvgBps:     ev.avg,
			PeakBps:    ev.peak,
			RawBps:     ev.peak,
			Source:     r.Kind,
			CapMbps:    r.Mbps,
		})
	}
	return out, nil
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
