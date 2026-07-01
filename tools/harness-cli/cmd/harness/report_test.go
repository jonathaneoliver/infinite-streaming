package main

import (
	"reflect"
	"testing"
)

// rptLabels builds a label_histogram in the wire shape: [ ["<sev>=<event>", n], … ].
func rptLabels(labels ...string) []any {
	out := make([]any, 0, len(labels))
	for _, l := range labels {
		out = append(out, []any{l, float64(1)})
	}
	return out
}

func TestRptVerdict(t *testing.T) {
	tests := []struct {
		name        string
		labels      []any
		wantVerdict string
		wantWorst   string
	}{
		{
			"acceptable tier; lifecycle excluded, worst QoE = stall_frozen",
			rptLabels("warning=*qoe_tier_acceptable", "critical=unexpected_fault", "critical=stall_frozen", "warning=*segment_failure"),
			"ok", "stall_frozen",
		},
		{
			"unacceptable tier wins the verdict",
			rptLabels("critical=*qoe_tier_unacceptable", "warning=*qoe_vst_concerning"),
			"BAD", "qoe_vst_concerning",
		},
		{
			"premium tier; only lifecycle labels → no worst_qoe",
			rptLabels("info=*qoe_tier_premium", "info=first_frame"),
			"premium", "",
		},
		{
			"no tier → fall back to worst QoE severity; lifecycle (unexpected_end) excluded",
			rptLabels("critical=unexpected_end", "warning=*segment_failure"),
			"warn", "segment_failure",
		},
		{
			"no tier, only lifecycle → ok",
			rptLabels("info=first_frame", "critical=unexpected_fault"),
			"ok", "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, w := rptVerdict(tt.labels)
			if v != tt.wantVerdict || w != tt.wantWorst {
				t.Errorf("rptVerdict = (%q, %q), want (%q, %q)", v, w, tt.wantVerdict, tt.wantWorst)
			}
		})
	}
}

func TestRptFloat(t *testing.T) {
	tests := []struct {
		in   any
		want float64
		ok   bool
	}{
		{float64(7307), 7307, true},
		{"7307", 7307, true}, // ClickHouse serializes UInt64 counts as strings
		{"5.06", 5.06, true},
		{0, 0, true},
		{"", 0, false},
		{nil, 0, false},
		{"n/a", 0, false},
	}
	for _, tt := range tests {
		got, ok := rptFloat(tt.in)
		if ok != tt.ok || (ok && got != tt.want) {
			t.Errorf("rptFloat(%#v) = (%v, %v), want (%v, %v)", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestRptDetectIV(t *testing.T) {
	rows := []map[string]any{
		{"scenario": map[string]any{"manifest_variant": "6s", "platform": "ipad-sim", "device_model": "arm64"}},
		{"scenario": map[string]any{"manifest_variant": "2s", "platform": "ipad-sim", "device_model": "arm64"}},
	}
	iv, constants := rptDetectIV(rows)
	if !reflect.DeepEqual(iv, []string{"manifest_variant"}) {
		t.Errorf("ivCols = %v, want [manifest_variant]", iv)
	}
	got := map[string]bool{}
	for _, c := range constants {
		got[c] = true
	}
	if len(constants) != 2 || !got["platform=ipad-sim"] || !got["device_model=arm64"] {
		t.Errorf("constants = %v, want {platform=ipad-sim, device_model=arm64}", constants)
	}
}

func TestRptMedAndRange(t *testing.T) {
	if got := rptMed([]float64{1, 3, 2}, 0); got != "2" {
		t.Errorf("median odd = %q, want 2", got)
	}
	if got := rptMed([]float64{1, 2, 3, 4}, 1); got != "2.5" {
		t.Errorf("median even = %q, want 2.5", got)
	}
	if got := rptMed(nil, 0); got != "-" {
		t.Errorf("median empty = %q, want -", got)
	}
	if got := rptRange([]float64{5.06, 6.16}, 2); got != "5.06–6.16" {
		t.Errorf("range = %q, want 5.06–6.16", got)
	}
	if got := rptRange([]float64{1.2}, 2); got != "-" {
		t.Errorf("range single = %q, want -", got)
	}
}

func TestRptWorstVerdict(t *testing.T) {
	if got := rptWorstVerdict([]string{"ok", "BAD", "premium"}); got != "BAD" {
		t.Errorf("worst = %q, want BAD", got)
	}
	if got := rptWorstVerdict([]string{"ok", "premium"}); got != "ok" {
		t.Errorf("worst = %q, want ok", got)
	}
	if got := rptWorstVerdict(nil); got != "-" {
		t.Errorf("worst empty = %q, want -", got)
	}
}
