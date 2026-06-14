package sweep

import (
	"strings"
	"testing"
	"time"
)

func TestLabelsFromPlayHistogram(t *testing.T) {
	// Mirrors the live /api/v2/plays shape: label_histogram is [[label,count],…]
	// (count is a string), unioned across the three source tables.
	body := []byte(`{"items":[{"label_histogram":[
		["critical=unexpected_startup","1"],
		["warning=*qoe_live_offset_concerning","1"],
		["info=first_frame","1"],
		["warning=*qoe_live_offset_concerning","1"]
	]}]}`)
	got, err := LabelsFromPlayHistogram(body)
	if err != nil {
		t.Fatal(err)
	}
	// distinct, histogram order; the duplicate collapses
	want := []string{"critical=unexpected_startup", "warning=*qoe_live_offset_concerning", "info=first_frame"}
	if len(got) != len(want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("at %d got %q want %q (%v)", i, got[i], want[i], got)
		}
	}
	// the critical label drives an aberration verdict
	if Classify(got) != VerdictAberration {
		t.Fatalf("critical=unexpected_startup should be an aberration")
	}
}

func TestLabelsFromEmptyPlays(t *testing.T) {
	for _, body := range []string{`{"items":[]}`, `{}`, `{"items":[{"label_histogram":null}]}`} {
		got, err := LabelsFromPlayHistogram([]byte(body))
		if err != nil || len(got) != 0 {
			t.Fatalf("%s: want empty/no-err, got %v err=%v", body, got, err)
		}
	}
}

func TestAnalyzeBucketing(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   Status
		verd   Verdict
	}{
		{"clean→done", []string{"info=first_frame"}, StatusDone, VerdictClean},
		{"notable→found", []string{"warning=*qoe_downshift_overshoot"}, StatusFound, VerdictNotable},
		{"aberration→found", []string{"error=*qoe_vsf"}, StatusFound, VerdictAberration},
		{"empty→done", nil, StatusDone, VerdictClean},
	}
	for _, c := range cases {
		e := &Experiment{ID: "x"}
		got := Analyze(e, "play-123", c.labels)
		if got != c.want {
			t.Errorf("%s: bucket=%s want %s", c.name, got, c.want)
		}
		if e.Result == nil || e.Result.Verdict != c.verd {
			t.Errorf("%s: verdict=%v want %s", c.name, e.Result, c.verd)
		}
		if e.PlayID != "play-123" {
			t.Errorf("%s: play_id not stamped", c.name)
		}
	}
}

func TestNeedsConfirmation(t *testing.T) {
	mk := func(v Verdict, reps int, repGroup string) *Experiment {
		return &Experiment{Reps: reps, RepGroup: repGroup, Result: &Result{Verdict: v}}
	}
	if !NeedsConfirmation(mk(VerdictAberration, 1, "")) {
		t.Error("single-rep aberration should need confirmation")
	}
	if !NeedsConfirmation(mk(VerdictNotable, 0, "")) {
		t.Error("single-rep notable should need confirmation")
	}
	if NeedsConfirmation(mk(VerdictClean, 1, "")) {
		t.Error("clean never needs confirmation")
	}
	if NeedsConfirmation(mk(VerdictAberration, 1, "rep-abc")) {
		t.Error("a rep must not spawn another rep batch (no recursion)")
	}
	if NeedsConfirmation(mk(VerdictAberration, 3, "")) {
		t.Error("already multi-rep should not re-confirm")
	}
	if NeedsConfirmation(&Experiment{}) {
		t.Error("unanalysed experiment needs nothing")
	}
}

func TestConfirmationReps(t *testing.T) {
	rate := 0.4
	parent := &Experiment{
		ID: "seed-x", Platform: "ipad-sim", Protocol: "hls", Mode: "pyramid",
		Kind: KindSeed, Shape: &Shape{RateMbps: &rate}, Reps: 1,
		Result: &Result{Verdict: VerdictAberration},
	}
	reps := ConfirmationReps(parent, 3, "2026-06-13T00:00:00Z")
	if len(reps) != 3 {
		t.Fatalf("want 3 reps, got %d", len(reps))
	}
	group := reps[0].RepGroup
	if group == "" {
		t.Fatal("reps must share a rep_group")
	}
	for i, r := range reps {
		if r.RepGroup != group {
			t.Errorf("rep %d not in shared group", i)
		}
		if r.Parent != parent.ID || r.Reps != 1 || r.Result != nil || r.ClaimedAt != "" {
			t.Errorf("rep %d wiring wrong: %+v", i, r)
		}
		if r.Shape == nil || r.Shape.RateMbps == nil || *r.Shape.RateMbps != 0.4 {
			t.Errorf("rep %d lost recipe shape", i)
		}
		// a rep must not itself trigger another confirmation batch
		r.Result = &Result{Verdict: VerdictAberration}
		if NeedsConfirmation(r) {
			t.Errorf("rep %d would recurse", i)
		}
	}
}

func TestReapStale(t *testing.T) {
	now := "2026-06-13T12:00:00Z"
	running := []*Experiment{
		{ID: "fresh", ClaimedAt: "2026-06-13T11:55:00Z"}, // 5 min ago — alive
		{ID: "stale", ClaimedAt: "2026-06-13T11:00:00Z"}, // 60 min ago — dead
		{ID: "noclaim"},                                   // missing stamp — reap
		{ID: "bad", ClaimedAt: "not-a-time"},              // unparseable — reap
		{ID: "future", ClaimedAt: "2026-06-13T12:30:00Z"}, // clock skew — not stale
	}
	stale := ReapStale(running, now, 30*time.Minute)
	got := map[string]bool{}
	for _, e := range stale {
		got[e.ID] = true
	}
	if !got["stale"] || !got["noclaim"] || !got["bad"] {
		t.Fatalf("missing expected stale: %v", got)
	}
	if got["fresh"] || got["future"] {
		t.Fatalf("reaped a live/skewed claim: %v", got)
	}
}

func TestRequeueResetsRuntime(t *testing.T) {
	e := &Experiment{Owner: "r1", ClaimedAt: "t", PlayID: "p", Result: &Result{Verdict: VerdictClean}}
	Requeue(e)
	if e.Owner != "" || e.ClaimedAt != "" || e.PlayID != "" || e.Result != nil {
		t.Fatalf("runtime not cleared: %+v", e)
	}
}

func TestIssueArtifacts(t *testing.T) {
	e := &Experiment{
		ID: "iso-h-platform", Class: ClassFault, Platform: "androidtv", Protocol: "hls", Content: "insane_new",
		Mode: "pyramid", Kind: KindIsolation, Fault: &Fault{Type: "500", RequestKind: "segment"},
		PlayID: "play-abc", WhyText: "startup VSF on 4k ladder",
		Result: &Result{Verdict: VerdictAberration, Labels: []string{"error=*qoe_vsf"}},
	}
	sig := Signature(e, "qoe_vsf", "platform")
	labels := IssueLabels(sig, VerdictAberration)
	if labels[0] != "sweep" || labels[1] != sig || labels[2] != "bug" {
		t.Fatalf("aberration labels wrong: %v", labels)
	}
	if nl := IssueLabels(sig, VerdictNotable); nl[2] != "notable" {
		t.Fatalf("notable should not be a bug: %v", nl)
	}
	body := IssueBody(e, sig, "platform")
	for _, want := range []string{"qoe_vsf", "androidtv", "500_segment", "play-abc", sig, "startup VSF on 4k ladder"} {
		if !strings.Contains(body, want) {
			t.Fatalf("issue body missing %q:\n%s", want, body)
		}
	}
}
