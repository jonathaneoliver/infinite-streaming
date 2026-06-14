package sweep

import "testing"

// --- oracle (trichotomy) ---

func TestClassifyTrichotomy(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   Verdict
	}{
		{"clean", []string{"info=first_frame", "info=*qoe_tier_premium", "testing=test_pyramid"}, VerdictClean},
		{"notable-warning", []string{"info=first_frame", "warning=*qoe_downshift_overshoot"}, VerdictNotable},
		{"aberration-critical", []string{"warning=timejump", "critical=stall_frozen"}, VerdictAberration},
		{"aberration-error", []string{"error=*qoe_vsf"}, VerdictAberration},
		{"only-testing-is-clean", []string{"testing=run_id_x", "testing=platform_ipad"}, VerdictClean},
		{"empty-is-clean", nil, VerdictClean},
	}
	for _, c := range cases {
		if got := Classify(c.labels); got != c.want {
			t.Errorf("%s: Classify=%s want %s", c.name, got, c.want)
		}
	}
}

func TestPrimaryKind(t *testing.T) {
	if k := PrimaryKind([]string{"error=*qoe_vsf", "warning=timejump"}); k != "qoe_vsf" {
		t.Fatalf("want qoe_vsf, got %q", k)
	}
	if k := PrimaryKind([]string{"warning=*qoe_downshift_overshoot"}); k != "qoe_downshift_overshoot" {
		t.Fatalf("want qoe_downshift_overshoot, got %q", k)
	}
	if k := PrimaryKind([]string{"info=first_frame"}); k != "" {
		t.Fatalf("clean run must have empty kind, got %q", k)
	}
}

// --- labels + signature ---

func TestSlugEncodesForbiddenChars(t *testing.T) {
	// , and = would be silently dropped by the forwarder — must be encoded.
	if got := Slug("a,b=c d"); got != "a_b_c_d" {
		t.Fatalf("Slug must encode , = space → _, got %q", got)
	}
}

func TestRunLabelsWhatAndWhy(t *testing.T) {
	off := 6.0
	e := &Experiment{
		ID: "iso-abc123-platform", Platform: "androidtv", Protocol: "hls", Mode: "startup",
		Kind: KindIsolation, Arm: ArmVariant, Group: "iso-abc123", Parent: "seed-x", Depth: 0,
		Why: "startup vsf, 4k ladder", Fault: &Fault{Type: "timeout", RequestKind: "manifest"},
		ContentManipulation: &ContentManipulation{LiveOffset: &off},
	}
	m := RunLabels(e)
	if m["sweep"] != "1" || m["kind"] != "isolation" || m["arm"] != "variant" {
		t.Fatalf("missing core labels: %v", m)
	}
	if m["fault"] != "timeout_manifest" {
		t.Fatalf("fault label wrong: %q", m["fault"])
	}
	if m["why"] != "startup_vsf__4k_ladder" { // comma+spaces encoded
		t.Fatalf("why not slugged: %q", m["why"])
	}
}

func TestSignature(t *testing.T) {
	// fault-class: recipe slot is the fault family, class namespaces it
	e := &Experiment{Class: ClassFault, Protocol: "hls", Fault: &Fault{Type: "500", RequestKind: "segment"}}
	if s := Signature(e, "qoe_vsf", ""); s != "sig:fault-hls-500_segment-qoe_vsf" {
		t.Fatalf("sig wrong: %q", s)
	}
	if s := Signature(e, "qoe_vsf", "platform"); s != "sig:fault-hls-500_segment-qoe_vsf-platform" {
		t.Fatalf("sig+axis wrong: %q", s)
	}
	// config-class (default): no fault → recipe is the config knob, baseline here
	base := &Experiment{Protocol: "dash"}
	if s := Signature(base, "downshift_overshoot", ""); s != "sig:config-dash-baseline-downshift_overshoot" {
		t.Fatalf("sig baseline wrong: %q", s)
	}
	// config-class with a content knob → recipe reflects it
	strip := &Experiment{Protocol: "hls", ContentManipulation: &ContentManipulation{StripAvgBandwidth: true}}
	if s := Signature(strip, "downshift_overshoot", "ladder"); s != "sig:config-hls-strip_avgbw-downshift_overshoot-ladder" {
		t.Fatalf("sig config-knob wrong: %q", s)
	}
}

// --- isolation fan (the OFAT job-insertion invariant) ---

func reproParent() *Experiment {
	rate := 0.4
	return &Experiment{
		ID: "seed-ipad-sim-hls-abr-pyramid", Platform: "ipad-sim", Protocol: "hls",
		Content: "insane_new", Mode: "pyramid", Kind: KindSeed, Depth: 0,
		Shape: &Shape{RateMbps: &rate}, CreatedAt: "t0",
	}
}

func TestIsolationFanEachVariantFlipsExactlyOneAxis(t *testing.T) {
	parent := reproParent()
	flips := []Flip{
		{Axis: AxisPlatform, Value: "androidtv"},
		{Axis: AxisProtocol, Value: "dash"},
		{Axis: AxisLadder, Value: "drop-top-rung"},
		{Axis: AxisLiveOffset, Value: "6"},
	}
	fan, err := IsolationFan(parent, flips, "now")
	if err != nil {
		t.Fatal(err)
	}
	// 1 control + 4 variants
	if len(fan) != 5 {
		t.Fatalf("want 5 (control+4), got %d", len(fan))
	}
	control := fan[0]
	if control.Arm != ArmControl || control.Kind != KindIsolation {
		t.Fatalf("first must be control/isolation: %+v", control)
	}
	group := control.Group
	for _, v := range fan[1:] {
		if v.Arm != ArmVariant || v.Group != group || v.Parent != parent.ID {
			t.Fatalf("variant wiring wrong: %+v", v)
		}
		axis, ok := OneAxisDiff(control, v)
		if !ok {
			t.Fatalf("variant %s differs in !=1 axis", v.ID)
		}
		_ = axis
	}
}

func TestIsolationFanControlMatchesParent(t *testing.T) {
	parent := reproParent()
	fan, _ := IsolationFan(parent, []Flip{{Axis: AxisProtocol, Value: "dash"}}, "now")
	if _, ok := OneAxisDiff(parent, fan[0]); ok {
		t.Fatal("control should be identical to parent (zero-axis diff), got a 1-axis diff")
	}
}

func TestIsolationFanRejectsTooMany(t *testing.T) {
	parent := reproParent()
	flips := make([]Flip, MaxIsolationAxes+1)
	for i := range flips {
		flips[i] = Flip{Axis: AxisPlatform, Value: "iphone"}
	}
	if _, err := IsolationFan(parent, flips, "now"); err == nil {
		t.Fatal("want error exceeding MaxIsolationAxes")
	}
}

func TestIsolationFanRejectsBadAxis(t *testing.T) {
	parent := reproParent()
	if _, err := IsolationFan(parent, []Flip{{Axis: "bogus", Value: "x"}}, "now"); err == nil {
		t.Fatal("want error for unknown axis")
	}
	if _, err := IsolationFan(parent, []Flip{{Axis: AxisLiveOffset, Value: "not-a-number"}}, "now"); err == nil {
		t.Fatal("want error for unparseable liveoffset")
	}
}

// --- bisect (recursive depth bound) ---

func TestBisectRateChildren(t *testing.T) {
	parent := reproParent()
	parent.Kind = KindIsolation
	parent.Depth = 0
	kids := BisectRate(parent, []float64{1.0, 2.0}, "now")
	if len(kids) != 2 {
		t.Fatalf("want 2 bisect kids, got %d", len(kids))
	}
	for _, k := range kids {
		if k.Kind != KindBisect || k.Depth != 1 || k.Parent != parent.ID {
			t.Fatalf("bisect child wiring wrong: %+v", k)
		}
		if k.Shape == nil || k.Shape.RateMbps == nil {
			t.Fatalf("bisect child missing rate: %+v", k.Shape)
		}
	}
}

func TestBisectStopsAtDepthCap(t *testing.T) {
	parent := reproParent()
	parent.Depth = 3
	if kids := BisectRate(parent, []float64{1.0}, "now"); kids != nil {
		t.Fatalf("depth cap should stop bisection, got %d kids", len(kids))
	}
}

func TestBisectCapsAtTwo(t *testing.T) {
	parent := reproParent()
	kids := BisectRate(parent, []float64{1, 2, 3, 4}, "now")
	if len(kids) != 2 {
		t.Fatalf("bisect must cap at 2, got %d", len(kids))
	}
}
