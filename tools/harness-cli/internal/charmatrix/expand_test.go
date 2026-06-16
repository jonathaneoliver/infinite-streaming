package charmatrix

import (
	"strings"
	"testing"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/sweep"
)

func f64(v float64) *float64 { return &v }

func shapeRate(v float64) *sweep.Shape { return &sweep.Shape{RateMbps: &v} }

func TestExpand_CartesianCount(t *testing.T) {
	// segment{s2,s6} × live_offset{12,24,30} × lever{proxy,app} = 12 arms.
	spec := &Spec{
		Name: "m",
		Axes: map[string][]any{
			"segment":     {"s2", "s6"},
			"live_offset": {12.0, 24.0, 30.0},
			"lever":       {"proxy", "app"},
		},
	}
	arms, err := Expand(spec)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(arms) != 12 {
		t.Fatalf("got %d arms, want 12", len(arms))
	}
}

func TestExpand_ReproducibleSortedIDs(t *testing.T) {
	// Axes are swept in sorted-name order (lever, live_offset, segment), with the
	// rightmost axis advancing fastest — so ids are deterministic run-to-run.
	spec := &Spec{
		Name: "m",
		Axes: map[string][]any{
			"segment":     {"s6"},
			"lever":       {"proxy", "app"},
			"live_offset": {24.0},
		},
	}
	arms, err := Expand(spec)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	want := []string{
		"m/lever-proxy.live_offset-24.segment-s6",
		"m/lever-app.live_offset-24.segment-s6",
	}
	for i, w := range want {
		if arms[i].ID != w {
			t.Errorf("arm[%d].ID = %q, want %q", i, arms[i].ID, w)
		}
	}
}

func TestExpand_LeverRouting(t *testing.T) {
	spec := &Spec{
		Name: "m",
		Axes: map[string][]any{
			"live_offset": {24.0},
			"lever":       {"proxy", "app"},
		},
	}
	arms, err := Expand(spec)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	// arms sorted by (lever, live_offset): lever-proxy first, lever-app second.
	proxyArm, appArm := arms[0], arms[1]
	if proxyArm.Lever != "proxy" || appArm.Lever != "app" {
		t.Fatalf("unexpected lever order: %q, %q", proxyArm.Lever, appArm.Lever)
	}

	// proxy lever → server manifest hold-back on the experiment, client flag "0".
	pe := proxyArm.ToExperiment()
	if pe.ContentManipulation == nil || pe.ContentManipulation.LiveOffset == nil {
		t.Fatal("proxy lever: expected ContentManipulation.LiveOffset set")
	}
	if *pe.ContentManipulation.LiveOffset != 24 {
		t.Errorf("proxy lever: server live_offset = %g, want 24", *pe.ContentManipulation.LiveOffset)
	}
	if got := proxyArm.ClientLiveOffsetS(); got != "0" {
		t.Errorf("proxy lever: client offset = %q, want 0", got)
	}

	// app lever → NO server manipulation, client flag carries the value.
	ae := appArm.ToExperiment()
	if ae.ContentManipulation != nil && ae.ContentManipulation.LiveOffset != nil {
		t.Error("app lever: server ContentManipulation.LiveOffset must be nil")
	}
	if got := appArm.ClientLiveOffsetS(); got != "24" {
		t.Errorf("app lever: client offset = %q, want 24", got)
	}

	// Either lever reports the same intended offset for the measurement gate.
	if off, ok := appArm.IntendedLiveOffset(); !ok || off != 24 {
		t.Errorf("app lever IntendedLiveOffset = %g,%v, want 24,true", off, ok)
	}
}

func TestExpand_DefaultLeverIsProxy(t *testing.T) {
	spec := &Spec{
		Name: "m",
		Axes: map[string][]any{"live_offset": {18.0}},
	}
	arms, err := Expand(spec)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	e := arms[0].ToExperiment()
	if e.ContentManipulation == nil || e.ContentManipulation.LiveOffset == nil || *e.ContentManipulation.LiveOffset != 18 {
		t.Fatal("no lever ⇒ defaults to proxy (server-side live_offset)")
	}
}

func TestExpand_DefaultsLayered(t *testing.T) {
	spec := &Spec{
		Name:      "m",
		Class:     "config",
		DurationS: 90,
		Defaults:  &Arm{Platform: "ipad-sim", Content: "insane_new_p200_h264"},
		Axes:      map[string][]any{"segment": {"s2", "s6"}},
	}
	arms, err := Expand(spec)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	for _, a := range arms {
		if a.Platform != "ipad-sim" {
			t.Errorf("arm %s: platform = %q, want ipad-sim (from defaults)", a.ID, a.Platform)
		}
		if a.Content != "insane_new_p200_h264" {
			t.Errorf("arm %s: content not inherited from defaults", a.ID)
		}
		e := a.ToExperiment()
		if e.DurationS != 90 {
			t.Errorf("arm %s: duration_s = %d, want 90 (spec default)", a.ID, e.DurationS)
		}
		if e.Class != "config" {
			t.Errorf("arm %s: class = %q, want config (spec default)", a.ID, e.Class)
		}
	}
}

func TestExpand_ExplicitArmsEscapeHatch(t *testing.T) {
	spec := &Spec{
		Name:     "m",
		Defaults: &Arm{Platform: "ipad-sim"},
		Arms: []*Arm{
			{Segment: "s6", LiveOffset: f64(24), Lever: "proxy"},
			{Segment: "s2", LiveOffset: f64(12), Lever: "app", Platform: "androidtv"},
		},
	}
	arms, err := Expand(spec)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(arms) != 2 {
		t.Fatalf("got %d arms, want 2", len(arms))
	}
	if arms[0].Platform != "ipad-sim" {
		t.Errorf("explicit arm 0 should inherit defaults platform, got %q", arms[0].Platform)
	}
	if arms[1].Platform != "androidtv" {
		t.Errorf("explicit arm 1 should override defaults platform, got %q", arms[1].Platform)
	}
	if arms[0].ID != "m/arm0" || arms[1].ID != "m/arm1" {
		t.Errorf("explicit-arm ids: %q, %q", arms[0].ID, arms[1].ID)
	}
}

func TestExpand_AxesAndExplicitArmsCombine(t *testing.T) {
	spec := &Spec{
		Name: "m",
		Axes: map[string][]any{"segment": {"s2", "s6"}},
		Arms: []*Arm{{Segment: "ll", Mode: "pyramid"}},
	}
	arms, err := Expand(spec)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(arms) != 3 {
		t.Fatalf("got %d arms, want 3 (2 cartesian + 1 explicit)", len(arms))
	}
}

func TestExpand_NoArmsIsError(t *testing.T) {
	if _, err := Expand(&Spec{Name: "m"}); err == nil {
		t.Fatal("expected error for a spec with no axes and no arms")
	}
}

func TestExpand_Validation(t *testing.T) {
	cases := []struct {
		name string
		spec *Spec
		want string
	}{
		{"unknown axis", &Spec{Name: "m", Axes: map[string][]any{"bogus": {"x"}}}, "unknown axis"},
		{"empty axis", &Spec{Name: "m", Axes: map[string][]any{"segment": {}}}, "no values"},
		{"bad lever", &Spec{Name: "m", Axes: map[string][]any{"lever": {"sideways"}, "live_offset": {24.0}}}, "lever"},
		{"bad segment", &Spec{Name: "m", Axes: map[string][]any{"segment": {"s9"}}}, "segment"},
		{"bad platform", &Spec{Name: "m", Axes: map[string][]any{"platform": {"toaster"}}}, "platform"},
		{"bad protocol", &Spec{Name: "m", Axes: map[string][]any{"protocol": {"quic"}}}, "protocol"},
		{"offset out of window", &Spec{Name: "m", Axes: map[string][]any{"live_offset": {7.0}}}, "supported window"},
		{"non-integral offset", &Spec{Name: "m", Axes: map[string][]any{"live_offset": {6.5}}}, "supported window"},
		{"bad class", &Spec{Name: "m", Class: "chaos", Axes: map[string][]any{"segment": {"s6"}}}, "class"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Expand(tc.spec)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.want)
			}
		})
	}
}

func TestExpand_SupportedWindowAccepted(t *testing.T) {
	for _, off := range []any{0.0, 2.0, 4.0, 6.0, 12.0, 18.0, 24.0, 30.0, 36.0, 42.0} {
		spec := &Spec{Name: "m", Axes: map[string][]any{"live_offset": {off}}}
		if _, err := Expand(spec); err != nil {
			t.Errorf("live_offset %v should be valid: %v", off, err)
		}
	}
}

func TestArm_ToExperimentNoSharedPointers(t *testing.T) {
	// Two arms layered over a defaults that carries a shape must not share the
	// shape pointer — mutating one must not bleed into the other.
	spec := &Spec{
		Name:     "m",
		Defaults: &Arm{Shape: shapeRate(5.0)},
		Axes:     map[string][]any{"segment": {"s2", "s6"}},
	}
	arms, err := Expand(spec)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	e0 := arms[0].ToExperiment()
	e1 := arms[1].ToExperiment()
	if e0.Shape == e1.Shape {
		t.Fatal("experiments share a Shape pointer")
	}
	if e0.Shape.RateMbps == e1.Shape.RateMbps {
		t.Fatal("experiments share a Shape.RateMbps pointer")
	}
}
