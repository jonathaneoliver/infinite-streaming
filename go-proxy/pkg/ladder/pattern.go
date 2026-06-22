package ladder

import "sort"

// Pattern template names accepted by BuildPattern. These mirror the
// PatternTemplate enum in go-proxy's v2 OpenAPI surface and the
// dashboard's NetworkShapingPattern.vue.
const (
	RampUp         = "ramp_up"
	RampDown       = "ramp_down"
	Pyramid        = "pyramid"
	SquareWave     = "square_wave"
	TransientShock = "transient_shock"
)

// Step is one entry of a shape pattern: hold RateMbps for DurationSeconds.
type Step struct {
	RateMbps        float64 `json:"rate_mbps"`
	DurationSeconds int     `json:"duration_seconds"`
}

// BuildPattern orders a limit ladder into a shape-pattern step list:
//
//   - ramp_up         ascending caps (lowest → highest)
//   - ramp_down       descending caps (highest → lowest)
//   - pyramid         ascending then descending, without duplicating the apex
//   - square_wave     just the lowest and highest cap, alternating
//   - transient_shock hold the top cap, then dip to each lower rung in turn
//     (deepening), returning to the top between dips so the buffer refills —
//     the deepening-drop staircase the transient_shock characterization mode
//     uses to find where the player breaks.
//
// rungs may arrive in any order (StandardLadder returns them descending);
// the ascending sequence is derived here. Every step holds for stepSecs.
// An unknown template or empty ladder returns nil (the slider-only path).
func BuildPattern(template string, rungs []Rung, stepSecs int) []Step {
	if len(rungs) == 0 {
		return nil
	}
	asc := make([]float64, len(rungs))
	for i, r := range rungs {
		asc[i] = r.Mbps
	}
	sort.Float64s(asc)

	var seq []float64
	switch template {
	case RampUp:
		seq = asc
	case RampDown:
		seq = reversedFloat(asc)
	case Pyramid:
		// Bottom out at the over-selection BOUNDARY — midway between the bottom
		// variant's peak cap and the next variant's avg cap — instead of the bare
		// bottom.peak cap. Both are already +bump network caps, so the midpoint
		// carries the same bump. Caps below that floor are dropped. #811.
		if fl := pyramidFloor(rungs); fl > 0 {
			asc = floorAsc(asc, fl)
		}
		down := reversedFloat(asc[:len(asc)-1]) // drop the apex so it isn't held twice
		seq = append(append([]float64{}, asc...), down...)
	case SquareWave:
		seq = []float64{asc[0], asc[len(asc)-1]}
	case TransientShock:
		// Deepening-dip staircase: hold the top cap, dip to each lower rung
		// shallowest-first down to the bottom, recovering to the top between
		// dips. seq = top, r[n-2], top, r[n-3], …, top, r[0], top. With a
		// single rung there are no dips, so it's just that one cap.
		top := asc[len(asc)-1]
		seq = append(seq, top)
		for i := len(asc) - 2; i >= 0; i-- {
			seq = append(seq, asc[i], top)
		}
	default:
		return nil
	}

	out := make([]Step, 0, len(seq))
	for _, r := range seq {
		out = append(out, Step{RateMbps: r, DurationSeconds: stepSecs})
	}
	return out
}

// pyramidFloor returns the over-selection-boundary floor for a pyramid: the
// midpoint of the lowest peak cap and the lowest avg cap. The lowest avg cap is
// the NEXT variant's avg, since AnchorCaps drops the bottom variant's avg. Both
// are already +bump network caps, so the midpoint carries the same bump. Fills /
// headroom rungs are ignored (only "peak"/"avg" kinds count). Returns 0 when
// either kind is absent (no boundary to floor at). #811.
func pyramidFloor(rungs []Rung) float64 {
	bottomPeak, nextAvg := 0.0, 0.0
	for _, r := range rungs {
		switch r.Kind {
		case "peak":
			if r.Mbps > 0 && (bottomPeak == 0 || r.Mbps < bottomPeak) {
				bottomPeak = r.Mbps
			}
		case "avg":
			if r.Mbps > 0 && (nextAvg == 0 || r.Mbps < nextAvg) {
				nextAvg = r.Mbps
			}
		}
	}
	if bottomPeak <= 0 || nextAvg <= 0 {
		return 0
	}
	return round3((bottomPeak + nextAvg) / 2)
}

// floorAsc drops ascending caps strictly below floor and makes floor the lowest
// cap. `asc` must be sorted ascending; the result stays ascending.
func floorAsc(asc []float64, floor float64) []float64 {
	out := []float64{floor}
	for _, v := range asc {
		if v > floor {
			out = append(out, v)
		}
	}
	return out
}

func reversedFloat(in []float64) []float64 {
	out := make([]float64, len(in))
	for i, v := range in {
		out[len(in)-1-i] = v
	}
	return out
}
