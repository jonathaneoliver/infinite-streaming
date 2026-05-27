package server

import (
	"reflect"
	"sort"
	"testing"
)

func TestLeafPaths(t *testing.T) {
	tests := []struct {
		name  string
		patch map[string]any
		want  []string
	}{
		{"scalar at top", map[string]any{"display_id": 7}, []string{"display_id"}},
		{"null at top", map[string]any{"labels": nil}, []string{"labels"}},
		{"nested object", map[string]any{"shape": map[string]any{"rate_mbps": 5.0}}, []string{"shape.rate_mbps"}},
		{"array is opaque leaf", map[string]any{"fault_rules": []any{}}, []string{"fault_rules"}},
		{"sibling fields",
			map[string]any{"shape": map[string]any{"rate_mbps": 5.0, "loss_pct": 1.5}},
			[]string{"shape.loss_pct", "shape.rate_mbps"},
		},
		{"deep nesting",
			map[string]any{"a": map[string]any{"b": map[string]any{"c": "x"}}},
			[]string{"a.b.c"},
		},
		{"nulls inside object are leaves",
			map[string]any{"labels": map[string]any{"a": "x", "b": nil}},
			[]string{"labels.a", "labels.b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := LeafPaths(tt.patch)
			sort.Strings(got)
			sort.Strings(tt.want)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("LeafPaths = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestApplyMergePatch_Replace(t *testing.T) {
	target := map[string]any{
		"shape": map[string]any{"rate_mbps": 5.0, "loss_pct": 1.5},
	}
	patch := map[string]any{
		"shape": map[string]any{"rate_mbps": 10.0},
	}
	got := ApplyMergePatch(target, patch)
	want := map[string]any{
		"shape": map[string]any{"rate_mbps": 10.0, "loss_pct": 1.5},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("merge replace: got %v, want %v", got, want)
	}
}

func TestApplyMergePatch_NullDeletes(t *testing.T) {
	target := map[string]any{
		"labels": map[string]any{"a": "x", "b": "y"},
	}
	patch := map[string]any{
		"labels": map[string]any{"b": nil},
	}
	got := ApplyMergePatch(target, patch)
	want := map[string]any{
		"labels": map[string]any{"a": "x"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("null delete: got %v, want %v", got, want)
	}
}

func TestApplyMergePatch_NullAtTopWipesField(t *testing.T) {
	target := map[string]any{
		"labels": map[string]any{"a": "x"},
		"shape":  map[string]any{"rate_mbps": 5.0},
	}
	patch := map[string]any{"labels": nil}
	got := ApplyMergePatch(target, patch)
	if _, has := got["labels"]; has {
		t.Errorf("null at top should delete key, got %v", got)
	}
	if _, has := got["shape"]; !has {
		t.Errorf("sibling key should survive, got %v", got)
	}
}

func TestApplyMergePatch_ArrayReplacesWholesale(t *testing.T) {
	target := map[string]any{
		"fault_rules": []any{map[string]any{"id": "a"}, map[string]any{"id": "b"}},
	}
	patch := map[string]any{
		"fault_rules": []any{map[string]any{"id": "c"}},
	}
	got := ApplyMergePatch(target, patch)
	rules, _ := got["fault_rules"].([]any)
	if len(rules) != 1 {
		t.Errorf("array should replace wholesale, got %d entries: %v", len(rules), rules)
	}
}

func TestDecodePatch(t *testing.T) {
	body := []byte(`{"shape":{"rate_mbps":5}}`)
	patch, paths, err := DecodePatch(body)
	if err != nil {
		t.Fatalf("DecodePatch: %v", err)
	}
	if !reflect.DeepEqual(patch, map[string]any{"shape": map[string]any{"rate_mbps": 5.0}}) {
		t.Errorf("decoded patch %v unexpected", patch)
	}
	if !reflect.DeepEqual(paths, []string{"shape.rate_mbps"}) {
		t.Errorf("paths %v unexpected", paths)
	}
}

func TestDecodePatch_RejectsNonObject(t *testing.T) {
	for _, body := range []string{`"x"`, `123`, `null`, `[]`, ``, `   `} {
		_, _, err := DecodePatch([]byte(body))
		if err == nil {
			t.Errorf("DecodePatch(%q) expected error, got nil", body)
		}
	}
}
