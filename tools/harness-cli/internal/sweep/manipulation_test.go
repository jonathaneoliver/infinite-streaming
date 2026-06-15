package sweep

import "testing"

func TestIntendedLiveOffset(t *testing.T) {
	if _, ok := IntendedLiveOffset(&Experiment{}); ok {
		t.Error("no content_manipulation → not a live-offset experiment")
	}
	if _, ok := IntendedLiveOffset(&Experiment{ContentManipulation: &ContentManipulation{}}); ok {
		t.Error("nil LiveOffset → not a live-offset experiment")
	}
	zero := 0.0
	if _, ok := IntendedLiveOffset(&Experiment{ContentManipulation: &ContentManipulation{LiveOffset: &zero}}); ok {
		t.Error("zero LiveOffset → skip the gate")
	}
	v, ok := IntendedLiveOffset(&Experiment{ContentManipulation: &ContentManipulation{LiveOffset: floatPtr(6)}})
	if !ok || v != 6 {
		t.Errorf("want (6,true), got (%v,%v)", v, ok)
	}
}

func TestAchievedOffsetFromEvents(t *testing.T) {
	// player_metrics arrives nested (object) on some rows and string-wrapped on
	// others; the parser must handle both, ignore the null/startup row, and
	// take the median of the steady-state samples.
	body := []byte(`{"items":[
		{"player_metrics":{"recommended_offset_s":null,"true_offset_s":null}},
		{"player_metrics":{"recommended_offset_s":21.4,"true_offset_s":21.5}},
		{"player_metrics":"{\"recommended_offset_s\":21.6,\"true_offset_s\":21.7}"},
		{"player_metrics":{"recommended_offset_s":21.5,"true_offset_s":21.6}}
	]}`)
	a := AchievedOffsetFromEvents(body)
	if !a.HasData {
		t.Fatal("expected offset data")
	}
	if a.RecommendedS != 21.5 { // median of {21.4,21.5,21.6}
		t.Errorf("recommended median: got %v want 21.5", a.RecommendedS)
	}
	if a.TrueS != 21.6 { // median of {21.5,21.6,21.7}
		t.Errorf("true median: got %v want 21.6", a.TrueS)
	}
}

func TestAchievedOffsetEmpty(t *testing.T) {
	for _, body := range []string{`{"items":[]}`, `{}`, `{"items":[{"player_metrics":{}}]}`, `not json`} {
		if a := AchievedOffsetFromEvents([]byte(body)); a.HasData {
			t.Errorf("%s: expected no data", body)
		}
	}
}

func TestManipulationLanded(t *testing.T) {
	s6 := SegmentSlackS("s6") // 7
	s2 := SegmentSlackS("s2") // 3
	cases := []struct {
		name     string
		intended float64
		achieved AchievedOffset
		slack    float64
		want     bool
	}{
		// The androidtv run that motivated the gate: intended 6 on 6s segments,
		// achieved ~21.5 (clamped to the 3×7 floor). 15.5 > slack 7 → not landed.
		{"s6 sub-spec did not land (6 vs 21.5)", 6, AchievedOffset{RecommendedS: 21.5, HasData: true}, s6, false},
		// s6 cross arm: intended 12, clamped to ~21 → 9 > slack 7 → not landed.
		{"s6 cross (12 vs 21) not landed", 12, AchievedOffset{RecommendedS: 21, HasData: true}, s6, false},
		// s6 deep arm honoured: intended 36, achieved ~36 → landed.
		{"s6 deep honoured (36 vs 36)", 36, AchievedOffset{RecommendedS: 36, HasData: true}, s6, true},
		// s6 deep NOT honoured: stayed ~21 → 15 > slack 7 → not landed.
		{"s6 deep ignored (36 vs 21) not landed", 36, AchievedOffset{RecommendedS: 21, HasData: true}, s6, false},
		// s2 cross arm legal + honoured: intended 12, achieved ~12 → landed.
		{"s2 cross honoured (12 vs 12)", 12, AchievedOffset{RecommendedS: 12, HasData: true}, s2, true},
		// Segment slack absorbs boundary quantization: intended 12 on s6, got 18
		// (one 6-7s segment off) → 6 ≤ slack 7 → still counts as landed.
		{"s6 segment-boundary slack (12 vs 18)", 12, AchievedOffset{RecommendedS: 18, HasData: true}, s6, true},
		{"falls back to true offset", 6, AchievedOffset{TrueS: 6.5, HasData: true}, s2, true},
		{"no data → don't false-flag", 6, AchievedOffset{HasData: false}, s6, true},
		{"data present but zero → don't flag", 6, AchievedOffset{HasData: true}, s6, true},
	}
	for _, c := range cases {
		if got := ManipulationLanded(c.intended, c.achieved, c.slack); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestMarkInconclusive(t *testing.T) {
	e := &Experiment{Result: &Result{Verdict: VerdictAberration, Note: "label says bad"}}
	bucket := MarkInconclusive(e, "IV did not move: intended 6s, achieved 21.5s")
	if bucket != StatusReview {
		t.Errorf("bucket: got %s want %s", bucket, StatusReview)
	}
	if e.Result.Verdict != VerdictInconclusive {
		t.Errorf("verdict: got %s want %s", e.Result.Verdict, VerdictInconclusive)
	}
	if e.Result.Note == "label says bad" || e.Result.Note == "IV did not move: intended 6s, achieved 21.5s" {
		t.Errorf("note should append, not replace: %q", e.Result.Note)
	}
	// nil-Result path must not panic.
	e2 := &Experiment{}
	if MarkInconclusive(e2, "x") != StatusReview || e2.Result == nil {
		t.Error("nil-Result path failed")
	}
}
