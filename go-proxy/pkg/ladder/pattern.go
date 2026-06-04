package ladder

import "sort"

// Pattern template names accepted by BuildPattern. These mirror the
// PatternTemplate enum in go-proxy's v2 OpenAPI surface and the
// dashboard's NetworkShapingPattern.vue.
const (
	RampUp     = "ramp_up"
	RampDown   = "ramp_down"
	Pyramid    = "pyramid"
	SquareWave = "square_wave"
)

// Step is one entry of a shape pattern: hold RateMbps for DurationSeconds.
type Step struct {
	RateMbps        float64 `json:"rate_mbps"`
	DurationSeconds int     `json:"duration_seconds"`
}

// BuildPattern orders a limit ladder into a shape-pattern step list:
//
//   - ramp_up     ascending caps (lowest → highest)
//   - ramp_down   descending caps (highest → lowest)
//   - pyramid     ascending then descending, without duplicating the apex
//   - square_wave just the lowest and highest cap, alternating
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
		down := reversedFloat(asc[:len(asc)-1]) // drop the apex so it isn't held twice
		seq = append(append([]float64{}, asc...), down...)
	case SquareWave:
		seq = []float64{asc[0], asc[len(asc)-1]}
	default:
		return nil
	}

	out := make([]Step, 0, len(seq))
	for _, r := range seq {
		out = append(out, Step{RateMbps: r, DurationSeconds: stepSecs})
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
