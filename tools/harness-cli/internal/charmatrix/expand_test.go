package charmatrix

import (
	"strings"
	"testing"

	"github.com/jonathaneoliver/infinite-streaming/tools/harness-cli/internal/sweep"
)

func f64(v float64) *float64 { return &v }

func shapeRate(v float64) *sweep.Shape { return &sweep.Shape{RateMbps: &v} }

func TestExpand_CartesianCount(t *testing.T) {
	// is.segment{s2,s6} × proxy.live_offset{12,24,30} × is.protocol{hls,dash} = 12 arms.
	spec := &Spec{
		Name: "m",
		Axes: map[string][]any{
			"is.segment":        {"s2", "s6"},
			"proxy.live_offset": {12.0, 24.0, 30.0},
			"is.protocol":       {"hls", "dash"},
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
	// Axes are swept in sorted-name order with the rightmost advancing fastest, so
	// ids are deterministic run-to-run.
	spec := &Spec{
		Name: "m",
		Axes: map[string][]any{
			"is.segment":        {"s6"},
			"proxy.live_offset": {24.0, 30.0},
		},
	}
	arms, err := Expand(spec)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	want := []string{
		"m/is.segment-s6.proxy.live_offset-24",
		"m/is.segment-s6.proxy.live_offset-30",
	}
	for i, w := range want {
		if arms[i].ID != w {
			t.Errorf("arm[%d].ID = %q, want %q", i, arms[i].ID, w)
		}
	}
}

func TestArm_OffsetRouting(t *testing.T) {
	// proxy.live_offset → server manifest hold-back, client flag "0".
	proxyArm := &Arm{ProxyLiveOffset: f64(24)}
	pe := proxyArm.ToExperiment()
	if pe.ContentManipulation == nil || pe.ContentManipulation.LiveOffset == nil || *pe.ContentManipulation.LiveOffset != 24 {
		t.Fatalf("proxy offset: expected server ContentManipulation.LiveOffset=24, got %+v", pe.ContentManipulation)
	}
	if got := proxyArm.ClientLiveOffsetS(); got != "0" {
		t.Errorf("proxy offset: client flag = %q, want 0", got)
	}

	// is.live_offset → NO server manipulation, client flag carries the value.
	appArm := &Arm{AppLiveOffset: f64(24)}
	ae := appArm.ToExperiment()
	if ae.ContentManipulation != nil && ae.ContentManipulation.LiveOffset != nil {
		t.Error("app offset: server ContentManipulation.LiveOffset must be nil")
	}
	if got := appArm.ClientLiveOffsetS(); got != "24" {
		t.Errorf("app offset: client flag = %q, want 24", got)
	}
	if off, ok := appArm.IntendedLiveOffset(); !ok || off != 24 {
		t.Errorf("app offset IntendedLiveOffset = %g,%v, want 24,true", off, ok)
	}
}

func TestArm_PrecedenceCell(t *testing.T) {
	// The cell the old lever model could never reach: both surfaces set at once.
	both := &Arm{ProxyLiveOffset: f64(24), AppLiveOffset: f64(18)}
	be := both.ToExperiment()
	if be.ContentManipulation == nil || be.ContentManipulation.LiveOffset == nil || *be.ContentManipulation.LiveOffset != 24 {
		t.Fatalf("precedence: server hold-back should be 24, got %+v", be.ContentManipulation)
	}
	if got := both.ClientLiveOffsetS(); got != "18" {
		t.Errorf("precedence: client flag = %q, want 18", got)
	}
	// IntendedLiveOffset prefers the server offset (the one that lands as a manifest change).
	if off, ok := both.IntendedLiveOffset(); !ok || off != 24 {
		t.Errorf("precedence IntendedLiveOffset = %g,%v, want 24,true", off, ok)
	}
}

func TestExpand_PrecedenceMatrix(t *testing.T) {
	// proxy.live_offset{0,24} × is.live_offset{0,18} → the 4 precedence cells.
	spec := &Spec{
		Name: "m",
		Axes: map[string][]any{
			"proxy.live_offset": {0.0, 24.0},
			"is.live_offset":    {0.0, 18.0},
		},
	}
	arms, err := Expand(spec)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(arms) != 4 {
		t.Fatalf("got %d arms, want 4", len(arms))
	}
	var foundBaseline, foundBoth bool
	for _, a := range arms {
		p := a.ProxyLiveOffset != nil && *a.ProxyLiveOffset > 0
		c := a.AppLiveOffset != nil && *a.AppLiveOffset > 0
		switch {
		case !p && !c:
			foundBaseline = true
		case p && c:
			foundBoth = true
			if got := a.ClientLiveOffsetS(); got != "18" {
				t.Errorf("both-set: client flag = %q, want 18", got)
			}
		}
	}
	if !foundBaseline {
		t.Error("expected a (0,0) baseline cell")
	}
	if !foundBoth {
		t.Error("expected a (24,18) precedence cell")
	}
}

func TestExpand_CompareGroups(t *testing.T) {
	// compare: is.protocol → hls=control, dash=variant within each (segment) group.
	spec := &Spec{
		Name:     "m",
		Parallel: true,
		Defaults: &Arm{Platform: "ipad-sim"},
		Compare:  "is.protocol",
		Axes: map[string][]any{
			"is.segment":  {"s2", "s6"},
			"is.protocol": {"hls", "dash"},
		},
	}
	arms, err := Expand(spec)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(arms) != 4 {
		t.Fatalf("got %d arms, want 4", len(arms))
	}
	byGroup := map[string][]*Arm{}
	for _, a := range arms {
		if a.Group == "" {
			t.Errorf("arm %s has no group", a.ID)
		}
		byGroup[a.Group] = append(byGroup[a.Group], a)
	}
	if len(byGroup) != 2 {
		t.Fatalf("got %d groups, want 2: %v", len(byGroup), byGroup)
	}
	for g, members := range byGroup {
		if len(members) != 2 {
			t.Fatalf("group %s has %d arms, want 2", g, len(members))
		}
		var controls, variants int
		for _, a := range members {
			switch a.Role {
			case string(sweep.ArmControl):
				controls++
				if a.Protocol != "hls" {
					t.Errorf("control in %s has protocol %q, want hls (first compare value)", g, a.Protocol)
				}
			case string(sweep.ArmVariant):
				variants++
				if a.Protocol != "dash" {
					t.Errorf("variant in %s has protocol %q, want dash", g, a.Protocol)
				}
			default:
				t.Errorf("arm %s has role %q", a.ID, a.Role)
			}
			// roles must survive onto the experiment for the dashboard pairing.
			if e := a.ToExperiment(); string(e.Arm) != a.Role || e.Group != a.Group {
				t.Errorf("arm %s: ToExperiment lost group/role (got group=%q arm=%q)", a.ID, e.Group, e.Arm)
			}
		}
		if controls != 1 || variants != 1 {
			t.Errorf("group %s: controls=%d variants=%d, want 1/1", g, controls, variants)
		}
	}
}

func TestExpandWithRunID_UniquePerRun(t *testing.T) {
	// Two runs of the same spec must NOT share a group_id (else the dashboard joins
	// the prior run's sessions); within one run, paired arms DO share it.
	mk := func() *Spec {
		return &Spec{
			Name: "m", Parallel: true, Defaults: &Arm{Platform: "ipad-sim"},
			Compare: "is.protocol",
			Axes:    map[string][]any{"is.protocol": {"hls", "dash"}},
		}
	}
	a1, err := ExpandWithRunID(mk(), "20260617T143052Z")
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	a2, err := ExpandWithRunID(mk(), "20260617T150000Z")
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	// Run id is in the group, with the spec name kept for readability.
	if !strings.Contains(a1[0].Group, "20260617T143052Z") || !strings.HasPrefix(a1[0].Group, "grp-m-") {
		t.Errorf("run id / spec name missing from group %q", a1[0].Group)
	}
	// Within one run, the two arms pair (same group).
	if a1[0].Group != a1[1].Group {
		t.Errorf("arms in one run must share a group: %q vs %q", a1[0].Group, a1[1].Group)
	}
	// Across runs, groups differ.
	if a1[0].Group == a2[0].Group {
		t.Errorf("two runs must not share a group_id: both %q", a1[0].Group)
	}
	// groups: block also gets the run id.
	gspec := &Spec{Name: "m", Groups: []*Group{{ID: "x", Control: &Arm{}, Variants: []*Arm{{Segment: "s6"}}}}}
	ga, _ := ExpandWithRunID(gspec, "RID")
	if ga[0].Group != "m/x-RID" {
		t.Errorf("groups-block run id: got %q, want m/x-RID", ga[0].Group)
	}
}

func TestExpand_GroupsBlock(t *testing.T) {
	spec := &Spec{
		Name:     "m",
		Defaults: &Arm{Platform: "ipad-sim", Content: "c"},
		Groups: []*Group{
			{
				ID:      "avgbw",
				Control: &Arm{Segment: "s6"},
				Variants: []*Arm{
					{Segment: "s6", ContentManipulation: &sweep.ContentManipulation{StripAvgBandwidth: true}},
				},
			},
		},
	}
	arms, err := Expand(spec)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(arms) != 2 {
		t.Fatalf("got %d arms, want 2", len(arms))
	}
	ctrl, variant := arms[0], arms[1]
	if ctrl.Role != string(sweep.ArmControl) || ctrl.ID != "m/avgbw/control" {
		t.Errorf("control: role=%q id=%q", ctrl.Role, ctrl.ID)
	}
	if variant.Role != string(sweep.ArmVariant) || variant.ID != "m/avgbw/var0" {
		t.Errorf("variant: role=%q id=%q", variant.Role, variant.ID)
	}
	if ctrl.Group != "m/avgbw" || variant.Group != "m/avgbw" {
		t.Errorf("group ids: %q, %q", ctrl.Group, variant.Group)
	}
	if ctrl.Platform != "ipad-sim" || variant.Platform != "ipad-sim" {
		t.Error("group members should inherit defaults platform")
	}
	if variant.ContentManipulation == nil || !variant.ContentManipulation.StripAvgBandwidth {
		t.Error("variant should carry strip_avg_bandwidth")
	}
}

func TestArm_FlatContentManipConveniences(t *testing.T) {
	// Flat proxy.* conveniences fold onto ContentManipulation.
	yaml := `
name: m
arms:
  - platform: ipad-sim
    proxy.strip_avg_bandwidth: true
    proxy.allowed_variants: drop-top-rung
    proxy.overstate_bandwidth: 2.0
    proxy.variant_order: descending
`
	spec, err := Load([]byte(yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	arms, err := Expand(spec)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	cm := arms[0].ToExperiment().ContentManipulation
	if cm == nil {
		t.Fatal("flat conveniences produced no ContentManipulation")
	}
	if !cm.StripAvgBandwidth {
		t.Error("strip_avg_bandwidth not folded")
	}
	if cm.AllowedVariants != "drop-top-rung" {
		t.Errorf("allowed_variants = %q", cm.AllowedVariants)
	}
	if cm.OverstateBandwidth == nil || *cm.OverstateBandwidth != 2.0 {
		t.Errorf("overstate_bandwidth not folded: %v", cm.OverstateBandwidth)
	}
	if cm.VariantOrder != "descending" {
		t.Errorf("variant_order = %q", cm.VariantOrder)
	}
}

func TestArm_NestedContentManipWinsOverFlat(t *testing.T) {
	// When both the nested block and a flat convenience set the same field, the
	// nested block wins (it is the explicit full form).
	tru := true
	arm := &Arm{
		AllowedVariants:     "drop-top-rung",                                              // flat
		ContentManipulation: &sweep.ContentManipulation{AllowedVariants: "keep-bottom-2"}, // nested
		StripResolution:     &tru,
	}
	cm := arm.ToExperiment().ContentManipulation
	if cm.AllowedVariants != "keep-bottom-2" {
		t.Errorf("nested should win: allowed_variants = %q, want keep-bottom-2", cm.AllowedVariants)
	}
	if !cm.StripResolution {
		t.Error("flat strip_resolution should still fold when nested leaves it unset")
	}
}

func TestExpand_BoolConvenienceCompare(t *testing.T) {
	// A bool convenience axis pairs cleanly: false=control, true=variant.
	spec := &Spec{
		Name:     "m",
		Parallel: true,
		Defaults: &Arm{Platform: "ipad-sim"},
		Compare:  "proxy.strip_avg_bandwidth",
		Axes:     map[string][]any{"proxy.strip_avg_bandwidth": {false, true}},
	}
	arms, err := Expand(spec)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(arms) != 2 {
		t.Fatalf("got %d arms, want 2", len(arms))
	}
	if arms[0].Role != "control" || arms[1].Role != "variant" {
		t.Errorf("roles: %q, %q", arms[0].Role, arms[1].Role)
	}
	if arms[0].Group == "" || arms[0].Group != arms[1].Group {
		t.Errorf("arms should share a group: %q, %q", arms[0].Group, arms[1].Group)
	}
}

func TestExpand_ObjectAxisHashIDs(t *testing.T) {
	// An object-valued axis sweeps whole proxy.shape blocks; unlabelled values get
	// a stable obj-<hash> id slug, and each block decodes onto the arm.
	yaml := `
name: m
defaults: { platform: ipad-sim }
axes:
  proxy.shape:
    - { pattern: pyramid, step_seconds: 12, rate_mbps: 1.5 }
    - { pattern: ramp_down, step_seconds: 12 }
`
	spec, err := Load([]byte(yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	arms, err := Expand(spec)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(arms) != 2 {
		t.Fatalf("got %d arms, want 2", len(arms))
	}
	for _, a := range arms {
		if a.Shape == nil || a.Shape.Pattern == "" {
			t.Errorf("arm %s: proxy.shape not decoded: %+v", a.ID, a.Shape)
		}
		if !strings.Contains(a.ID, "proxy.shape-obj-") {
			t.Errorf("arm %s: id missing obj-hash slug", a.ID)
		}
	}
	if arms[0].ID == arms[1].ID {
		t.Error("distinct shape blocks must hash to distinct ids")
	}
	// Hashing is stable run-to-run.
	again, _ := Expand(spec)
	if again[0].ID != arms[0].ID {
		t.Errorf("object-axis id not stable: %q vs %q", again[0].ID, arms[0].ID)
	}
}

func TestExpand_ObjectAxisLabel(t *testing.T) {
	// A reserved `label:` inside the object gives a readable id and is stripped
	// before the block decodes (so it doesn't trip strict decoding).
	yaml := `
name: m
defaults: { platform: ipad-sim }
axes:
  proxy.shape:
    - { label: gentle, pattern: pyramid, step_seconds: 12, rate_mbps: 1.5 }
`
	spec, err := Load([]byte(yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	arms, err := Expand(spec)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if arms[0].ID != "m/proxy.shape-gentle" {
		t.Errorf("id = %q, want m/proxy.shape-gentle", arms[0].ID)
	}
	if arms[0].Shape == nil || arms[0].Shape.Pattern != "pyramid" {
		t.Errorf("labelled shape not decoded: %+v", arms[0].Shape)
	}
}

func TestExpand_ObjectAxisCompare(t *testing.T) {
	// compare: works on an object axis — first block is control, rest variants.
	spec := &Spec{
		Name:     "m",
		Parallel: true,
		Defaults: &Arm{Platform: "ipad-sim"},
		Compare:  "proxy.fault",
		Axes: map[string][]any{
			"proxy.fault": {
				map[string]any{"label": "five-hundred", "type": "500", "request_kind": "segment"},
				map[string]any{"label": "timeout", "type": "timeout", "request_kind": "segment"},
			},
		},
	}
	arms, err := Expand(spec)
	if err != nil {
		t.Fatalf("Expand: %v", err)
	}
	if len(arms) != 2 || arms[0].Role != "control" || arms[1].Role != "variant" {
		t.Fatalf("roles: %d arms, %q/%q", len(arms), arms[0].Role, arms[1].Role)
	}
	if arms[0].Group == "" || arms[0].Group != arms[1].Group {
		t.Errorf("arms should share a group: %q vs %q", arms[0].Group, arms[1].Group)
	}
	if arms[0].Fault == nil || arms[0].Fault.Type != "500" {
		t.Errorf("control fault not decoded: %+v", arms[0].Fault)
	}
}

func TestExpand_ObjectAxisTypoRejected(t *testing.T) {
	// A typo'd key inside an object axis value is caught by strict decoding rather
	// than silently dropped.
	yaml := `
name: m
axes:
  proxy.shape:
    - { patern: pyramid }
`
	spec, err := Load([]byte(yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := Expand(spec); err == nil || !strings.Contains(err.Error(), "patern") {
		t.Fatalf("expected unknown-field error for object-internal typo, got %v", err)
	}
}

func TestExpand_DefaultsLayered(t *testing.T) {
	spec := &Spec{
		Name:      "m",
		Class:     "config",
		DurationS: 90,
		Defaults:  &Arm{Platform: "ipad-sim", Content: "insane_new_p200_h264"},
		Axes:      map[string][]any{"is.segment": {"s2", "s6"}},
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
			{Segment: "s6", ProxyLiveOffset: f64(24)},
			{Segment: "s2", AppLiveOffset: f64(12), Platform: "androidtv"},
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
		Axes: map[string][]any{"is.segment": {"s2", "s6"}},
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
		t.Fatal("expected error for a spec with no axes, arms, or groups")
	}
}

func TestExpand_Validation(t *testing.T) {
	cases := []struct {
		name string
		spec *Spec
		want string
	}{
		{"unknown axis", &Spec{Name: "m", Axes: map[string][]any{"bogus": {"x"}}}, "unknown axis"},
		{"empty axis", &Spec{Name: "m", Axes: map[string][]any{"is.segment": {}}}, "no values"},
		{"bad segment", &Spec{Name: "m", Axes: map[string][]any{"is.segment": {"s9"}}}, "segment"},
		{"bad platform", &Spec{Name: "m", Axes: map[string][]any{"platform": {"toaster"}}}, "platform"},
		{"bad protocol", &Spec{Name: "m", Axes: map[string][]any{"is.protocol": {"quic"}}}, "protocol"},
		{"offset out of window", &Spec{Name: "m", Axes: map[string][]any{"proxy.live_offset": {7.0}}}, "supported window"},
		{"non-integral offset", &Spec{Name: "m", Axes: map[string][]any{"proxy.live_offset": {6.5}}}, "supported window"},
		{"bad class", &Spec{Name: "m", Class: "chaos", Axes: map[string][]any{"is.segment": {"s6"}}}, "class"},
		{"compare not an axis", &Spec{Name: "m", Parallel: true, Compare: "is.protocol", Axes: map[string][]any{"is.segment": {"s6"}}}, "not one of the axes"},
		{"compare needs parallel", &Spec{Name: "m", Compare: "is.protocol", Axes: map[string][]any{"is.protocol": {"hls", "dash"}}}, "requires parallel"},
		{"compare too wide", &Spec{Name: "m", Parallel: true, Compare: "proxy.live_offset", Axes: map[string][]any{"proxy.live_offset": {0.0, 6.0, 12.0, 18.0, 24.0}}}, "at most"},
		{"group too large", &Spec{Name: "m", Groups: []*Group{{ID: "g", Control: &Arm{}, Variants: []*Arm{{Segment: "s2"}, {Segment: "s6"}, {Segment: "ll"}, {Mode: "steps"}}}}}, "at most"},
		{"group no variants", &Spec{Name: "m", Groups: []*Group{{ID: "g", Control: &Arm{}}}}, "at least one variant"},
		{"bad role on explicit arm", &Spec{Name: "m", Arms: []*Arm{{Role: "sideways"}}}, "role"},
		{"bad variant_order", &Spec{Name: "m", Arms: []*Arm{{VariantOrder: "shuffle"}}}, "variant_order"},
		{"pyramid needs initial cap", &Spec{Name: "m", Arms: []*Arm{{Shape: &sweep.Shape{Pattern: "pyramid"}}}}, "starts limited"},
		{"ramp_up needs initial cap", &Spec{Name: "m", Arms: []*Arm{{Shape: &sweep.Shape{Pattern: "ramp_up"}}}}, "starts limited"},
		{"bad step_seconds", &Spec{Name: "m", Arms: []*Arm{{Shape: &sweep.Shape{Pattern: "ramp_down", StepSeconds: 16}}}}, "step_seconds"},
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
		spec := &Spec{Name: "m", Axes: map[string][]any{"proxy.live_offset": {off}}}
		if _, err := Expand(spec); err != nil {
			t.Errorf("proxy.live_offset %v should be valid: %v", off, err)
		}
	}
}

func TestArm_ToExperimentNoSharedPointers(t *testing.T) {
	// Two arms layered over a defaults that carries a shape must not share the
	// shape pointer — mutating one must not bleed into the other.
	spec := &Spec{
		Name:     "m",
		Defaults: &Arm{Shape: shapeRate(5.0)},
		Axes:     map[string][]any{"is.segment": {"s2", "s6"}},
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
