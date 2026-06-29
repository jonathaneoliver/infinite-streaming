package charplan

import (
	"path/filepath"
	"testing"
)

func TestParseBool(t *testing.T) {
	tr, fa := true, false
	cases := map[string]*bool{
		"true": &tr, "TRUE": &tr, " on ": &tr, "yes": &tr, "1": &tr,
		"false": &fa, "FALSE": &fa, "off": &fa, "no": &fa, "0": &fa,
		"": nil, "maybe": nil, "2": nil,
	}
	for in, want := range cases {
		got := ParseBool(in)
		switch {
		case want == nil && got != nil:
			t.Errorf("ParseBool(%q) = %v, want nil", in, *got)
		case want != nil && got == nil:
			t.Errorf("ParseBool(%q) = nil, want %v", in, *want)
		case want != nil && got != nil && *want != *got:
			t.Errorf("ParseBool(%q) = %v, want %v", in, *got, *want)
		}
	}
}

func TestWithDefaults(t *testing.T) {
	a := ArmConfig{} // all zero
	a.WithDefaults()
	if a.StepS != DefaultStepS || a.MarginPct != DefaultMarginPct {
		t.Errorf("step/margin = %d/%d, want %d/%d", a.StepS, a.MarginPct, DefaultStepS, DefaultMarginPct)
	}
	if Bool(a.LocalProxy, true) != false {
		t.Errorf("local_proxy default = true, want false")
	}
	if Bool(a.AutoRecovery, false) != true {
		t.Errorf("auto_recovery default = false, want true")
	}

	// Explicit values survive WithDefaults.
	f := false
	b := ArmConfig{StepS: 30, MarginPct: 2, AutoRecovery: &f}
	b.WithDefaults()
	if b.StepS != 30 || b.MarginPct != 2 || Bool(b.AutoRecovery, true) != false {
		t.Errorf("WithDefaults clobbered explicit values: %+v", b)
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	tr := true
	want := &RunPlan{
		BaseURL:    "https://dev.example:21000",
		FleetCount: 2,
		DurationS:  300,
		Arms: []ArmConfig{
			{PlayerID: "p0", Platform: "ipad-sim", Segment: "s2", Muted: &tr, PatternMaster: true},
			{PlayerID: "p1", Platform: "iphone", Segment: "s6", PeakBitrateMbps: 8},
		},
	}
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := Save(path, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if want.SchemaVersion != SchemaVersion {
		t.Errorf("Save did not stamp SchemaVersion: got %d", want.SchemaVersion)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.FleetCount != 2 || len(got.Arms) != 2 || got.Arms[0].Muted == nil || !*got.Arms[0].Muted {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.Arms[1].Muted != nil {
		t.Errorf("nil *bool should round-trip as nil, got %v", *got.Arms[1].Muted)
	}
}

func TestLoadRejectsBadSchema(t *testing.T) {
	if _, err := Load(""); err == nil {
		t.Error("Load(\"\") should error")
	}
	path := filepath.Join(t.TempDir(), "v999.json")
	if err := Save(path, &RunPlan{SchemaVersion: 999}); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("Load should reject a mismatched SchemaVersion")
	}
}
