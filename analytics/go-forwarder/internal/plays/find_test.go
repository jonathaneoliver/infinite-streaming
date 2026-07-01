package plays

import (
	"strings"
	"testing"
)

func TestBuildPlaysFilter_Group(t *testing.T) {
	clauses, params, err := buildPlaysFilter(PlayFilter{
		GroupID: "seg-trio-valley/run1",
		From:    "2026-07-01T00:00:00Z",
	})
	if err != nil {
		t.Fatalf("buildPlaysFilter: %v", err)
	}
	joined := strings.Join(clauses, " AND ")
	if !strings.Contains(joined, "startsWith(group_id, {group:String})") {
		t.Errorf("missing group clause in %q", joined)
	}
	if params["group"] != "seg-trio-valley/run1" {
		t.Errorf("params[group] = %q, want seg-trio-valley/run1", params["group"])
	}
}

func TestBuildPlaysFilter_NoGroup(t *testing.T) {
	clauses, params, err := buildPlaysFilter(PlayFilter{From: "2026-07-01T00:00:00Z"})
	if err != nil {
		t.Fatalf("buildPlaysFilter: %v", err)
	}
	if _, ok := params["group"]; ok {
		t.Errorf("params must not carry group when GroupID is empty")
	}
	for _, c := range clauses {
		if strings.Contains(c, "group_id") {
			t.Errorf("unexpected group clause %q", c)
		}
	}
}
