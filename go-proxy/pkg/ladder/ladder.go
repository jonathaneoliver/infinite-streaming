// Package ladder builds the network "limit ladder" the test rig shapes
// throughput down to characterize a player's ABR behaviour, and the
// step lists the built-in shape patterns (ramp_up / ramp_down / pyramid
// / square_wave) walk. It lives outside go-proxy/internal/ so the three
// runtimes that used to each carry their own copy share one source of
// truth: the characterization Go harness (tests/characterization), the
// harness CLI (tools/harness-cli), and — mirrored in JS, kept in sync
// against ladder_test.go's golden vectors — the dashboard's
// NetworkShapingPattern.vue.
//
// Issue #551 — the ladder carries BOTH scalars per variant: the peak
// (HLS BANDWIDTH) and the average (AVERAGE-BANDWIDTH). A source audit of
// hls.js / ExoPlayer / Shaka showed rendition selection keys on peak for
// startup + down-switch, and average only in hls.js's full-buffer
// up-switch; AVPlayer is closed-source and unknown. Carrying both means
// a sweep probes both regimes regardless of which scalar a player keys
// on, and each variant's avg->peak band becomes a discriminating zone
// (a cap parked inside it makes a peak-keyed player drop while an
// avg-keyed player holds).
//
// All functions are pure — they never mutate their inputs.
package ladder

import (
	"math"
	"sort"
)

// Default headroom + fill density. DefaultBumpPct is the flat percentage
// added on top of a variant's published bitrate so a cap at that rung is
// just sustainable on a clean network — 5% covers TCP/IP + TLS + HTTP
// framing overhead (#551 replaced the old margin × 1.07-TCP two-factor
// with this single flat bump across all three runtimes). DefaultMaxStep
// is the largest ratio allowed between two consecutive caps before a
// geometric fill is inserted, so a downward sweep can't skip the rung at
// which a player actually switches.
const (
	DefaultBumpPct = 5.0
	DefaultMaxStep = 1.15
	// DefaultTopHeadroomPct prepends a starting rung at the top variant's
	// peak × (1 + this/100), above the +bump top anchor, so a sweep settles
	// the player comfortably at the top variant before constraining it.
	// Over the RAW top peak, not compounded with the bump. 50% gives
	// AVPlayer's conservative upshift enough room to actually reach the top
	// (4K) rung — at +25% it lagged on the rung below until the apex.
	DefaultTopHeadroomPct = 50.0
)

// Variant is one rung of a player's published manifest ladder, reduced to
// the two scalars the limit ladder cares about. Callers adapt their own
// variant structs (the harness PlayerRecord, the CLI's generated proxy
// client, etc.) into this neutral shape.
type Variant struct {
	AvgBps     int    // AVERAGE-BANDWIDTH, 0 when the playlist omits it
	PeakBps    int    // BANDWIDTH (per HLS spec — the peak segment rate)
	Resolution string // e.g. "1920x1080" — label only, not used in math
}

// Rung is one cap in the limit ladder, in Mbps. Kind is "peak", "avg"
// (an anchor derived from a variant's BANDWIDTH / AVERAGE-BANDWIDTH),
// "fill" (a geometric interpolation inserted between two anchors), or
// "headroom" (the optional top start rung above the top variant's peak).
// Anchors carry their source variant in Variant; fills carry the two
// anchors they sit between in HiVar/LoVar (a fill between a variant's own
// avg and peak is that variant's discriminating band).
type Rung struct {
	Mbps    float64 `json:"mbps"`
	Variant string  `json:"variant,omitempty"`
	Kind    string  `json:"kind"`
	HiVar   string  `json:"hi_var,omitempty"`
	LoVar   string  `json:"lo_var,omitempty"`
}

// Label renders a short descriptor for a rung, e.g. "1920x1080 peak" for
// an anchor or "fill" for an interpolated step.
func (r Rung) Label() string {
	if r.Kind == "fill" || r.Variant == "" {
		return r.Kind
	}
	return r.Variant + " " + r.Kind
}

// AnchorCaps emits up to two anchor rungs per variant — peak×(1+bump) and
// avg×(1+bump) — skipping the avg rung when AVERAGE-BANDWIDTH is absent
// and any rung whose source bitrate is non-positive. The result is sorted
// descending by Mbps. bumpPct is a percentage (5 => ×1.05).
func AnchorCaps(vs []Variant, bumpPct float64) []Rung {
	f := 1 + bumpPct/100
	out := make([]Rung, 0, len(vs)*2)
	for _, v := range vs {
		if v.PeakBps > 0 {
			out = append(out, Rung{Mbps: round3(float64(v.PeakBps) * f / 1e6), Variant: v.Resolution, Kind: "peak"})
		}
		if v.AvgBps > 0 {
			out = append(out, Rung{Mbps: round3(float64(v.AvgBps) * f / 1e6), Variant: v.Resolution, Kind: "avg"})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Mbps > out[j].Mbps })
	return out
}

// FillLadder inserts geometric fills between adjacent (descending) anchors
// so no consecutive pair exceeds maxStep. Between hi and lo it adds
// ceil(log(hi/lo)/log(maxStep)) - 1 evenly-ratioed steps. Insertion
// preserves the descending order; the anchors themselves are kept. maxStep
// <= 1 (or fewer than two anchors) is a no-op.
func FillLadder(anchors []Rung, maxStep float64) []Rung {
	if maxStep <= 1 || len(anchors) < 2 {
		return anchors
	}
	out := make([]Rung, 0, len(anchors)*3)
	for i, a := range anchors {
		out = append(out, a)
		if i+1 >= len(anchors) {
			break
		}
		hi, lo := a.Mbps, anchors[i+1].Mbps
		if lo <= 0 || hi <= lo {
			continue
		}
		n := int(math.Ceil(math.Log(hi/lo)/math.Log(maxStep))) - 1
		for k := 1; k <= n; k++ {
			v := hi * math.Pow(lo/hi, float64(k)/float64(n+1))
			out = append(out, Rung{
				Mbps:  round3(v),
				Kind:  "fill",
				HiVar: a.Label(),
				LoVar: anchors[i+1].Label(),
			})
		}
	}
	return out
}

// StandardLadder is the one entry point callers use: dual-rung anchors at
// the given flat bump, optionally a top-headroom start rung at the top
// variant's peak × (1 + topHeadroomPct/100), geometrically filled to
// maxStep, sorted descending. topHeadroomPct <= 0 disables the start rung.
func StandardLadder(vs []Variant, bumpPct, maxStep, topHeadroomPct float64) []Rung {
	anchors := AnchorCaps(vs, bumpPct)
	if topHeadroomPct > 0 {
		maxPeak := 0
		topRes := ""
		for _, v := range vs {
			if v.PeakBps > maxPeak {
				maxPeak = v.PeakBps
				topRes = v.Resolution
			}
		}
		if maxPeak > 0 {
			hr := Rung{
				Mbps:    round3(float64(maxPeak) * (1 + topHeadroomPct/100) / 1e6),
				Variant: topRes,
				Kind:    "headroom",
			}
			anchors = append([]Rung{hr}, anchors...)
			sort.SliceStable(anchors, func(i, j int) bool { return anchors[i].Mbps > anchors[j].Mbps })
		}
	}
	return FillLadder(anchors, maxStep)
}

func round3(v float64) float64 { return math.Round(v*1000) / 1000 }
