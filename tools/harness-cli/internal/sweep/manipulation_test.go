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
	cases := []struct {
		name     string
		intended float64
		achieved AchievedOffset
		want     bool
	}{
		// The androidtv run that motivated the gate: intended 6, achieved ~21.5.
		{"did not land (6 vs 21.5)", 6, AchievedOffset{RecommendedS: 21.5, HasData: true}, false},
		{"landed exactly", 6, AchievedOffset{RecommendedS: 6, HasData: true}, true},
		{"landed within abs tolerance", 6, AchievedOffset{RecommendedS: 7.5, HasData: true}, true},  // tol = max(2, 1.5) = 2
		{"landed within frac tolerance", 24, AchievedOffset{RecommendedS: 28, HasData: true}, true}, // tol = max(2, 6) = 6
		{"outside frac tolerance", 24, AchievedOffset{RecommendedS: 33, HasData: true}, false},      // 9 > 6
		{"falls back to true offset", 6, AchievedOffset{TrueS: 6.5, HasData: true}, true},           // recommended 0 → use true
		{"no data → don't false-flag", 6, AchievedOffset{HasData: false}, true},
		{"data present but zero → don't flag", 6, AchievedOffset{HasData: true}, true},
	}
	for _, c := range cases {
		if got := ManipulationLanded(c.intended, c.achieved); got != c.want {
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
