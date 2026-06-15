package sweep

import (
	"encoding/json"
	"math"
	"sort"
)

// The manipulation check (epic #793) is the foundational gate for the
// live-offset recipe: a live_offset run is only interpretable if changing the
// knob actually changed the player's ACHIEVED offset. If the achieved offset
// never moved to ~the intended value, the independent variable never varied —
// so no observed QoE/stall outcome can be attributed to live_offset, and the
// run is `inconclusive`, not a finding. (The androidtv run that motivated this:
// intended 6 s, achieved ~21.5 s — the manifest HOLD-BACK either didn't reach
// ExoPlayer or it rejected the sub-spec value; either way the IV didn't move.)

// Offset-match tolerance: achieved counts as "landed" within whichever is
// larger — a small absolute floor (so tiny intended offsets aren't held to an
// impossible fraction) or a fraction of the intended value.
const (
	OffsetToleranceAbsS = 2.0
	OffsetToleranceFrac = 0.25
)

// IntendedLiveOffset returns the offset the recipe meant to impose and whether
// this is a live-offset experiment at all. Non-live-offset experiments return
// false and skip the gate entirely.
func IntendedLiveOffset(e *Experiment) (float64, bool) {
	if e == nil || e.ContentManipulation == nil || e.ContentManipulation.LiveOffset == nil {
		return 0, false
	}
	v := *e.ContentManipulation.LiveOffset
	if v <= 0 {
		return 0, false
	}
	return v, true
}

// AchievedOffset is the player-reported live offset for a play, read from the
// events archive. RecommendedS is the manifest HOLD-BACK the player parsed (the
// most direct "did the knob land" signal); TrueS is the actual achieved
// distance from the live edge. HasData is false when no offset sample exists
// (e.g. a play that never reached steady state).
type AchievedOffset struct {
	RecommendedS float64
	TrueS        float64
	HasData      bool
}

// AchievedOffsetFromEvents extracts a representative achieved offset from a
// forwarder /api/v2/events response body. The offset fields live nested under
// each row's `player_metrics` (which the archive emits as either a JSON object
// or a JSON-encoded string). It takes the MEDIAN of the non-null samples so a
// startup transient or end-of-play drift doesn't skew the steady-state read.
func AchievedOffsetFromEvents(body []byte) AchievedOffset {
	var env struct {
		Items []struct {
			PlayerMetrics json.RawMessage `json:"player_metrics"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return AchievedOffset{}
	}
	var rec, tru []float64
	for _, it := range env.Items {
		pm, ok := decodeMaybeStringObject(it.PlayerMetrics)
		if !ok {
			continue
		}
		if v, ok := numField(pm, "recommended_offset_s"); ok {
			rec = append(rec, v)
		}
		if v, ok := numField(pm, "true_offset_s"); ok {
			tru = append(tru, v)
		}
	}
	out := AchievedOffset{}
	if len(rec) > 0 {
		out.RecommendedS = median(rec)
		out.HasData = true
	}
	if len(tru) > 0 {
		out.TrueS = median(tru)
		out.HasData = true
	}
	return out
}

// ManipulationLanded reports whether the achieved offset reflects the intended
// one within tolerance. It prefers the recommended (parsed manifest target) and
// falls back to the true offset. With no offset data it returns true — a query
// gap must not masquerade as "the IV didn't move" (the caller logs the gap).
func ManipulationLanded(intended float64, a AchievedOffset) bool {
	if !a.HasData {
		return true
	}
	got := a.RecommendedS
	if got <= 0 {
		got = a.TrueS
	}
	if got <= 0 {
		return true
	}
	tol := math.Max(OffsetToleranceAbsS, intended*OffsetToleranceFrac)
	return math.Abs(got-intended) <= tol
}

// MarkInconclusive overrides a run's verdict to inconclusive and routes it to
// review — a human/LLM looks, but it never becomes a finding. Used by the
// manipulation-check gate when the IV demonstrably didn't move.
func MarkInconclusive(e *Experiment, note string) Status {
	if e.Result == nil {
		e.Result = &Result{}
	}
	e.Result.Verdict = VerdictInconclusive
	if e.Result.Note != "" {
		e.Result.Note += " · "
	}
	e.Result.Note += note
	return StatusReview
}

// decodeMaybeStringObject unmarshals a RawMessage that is either a JSON object
// or a JSON-encoded string wrapping an object (the events archive emits
// player_metrics in both shapes across versions).
func decodeMaybeStringObject(raw json.RawMessage) (map[string]any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return nil, false
		}
		var m map[string]any
		if json.Unmarshal([]byte(s), &m) != nil {
			return nil, false
		}
		return m, true
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return nil, false
	}
	return m, true
}

// numField reads a numeric field from a decoded JSON object. JSON numbers
// decode to float64; a null/absent/non-numeric field returns ok=false.
func numField(m map[string]any, key string) (float64, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false
	}
	f, ok := v.(float64)
	return f, ok
}

func median(xs []float64) float64 {
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	n := len(s)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}
