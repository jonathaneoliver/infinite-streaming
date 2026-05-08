package server

import (
	"reflect"
	"sort"
	"testing"
)

func TestParseIfMatch(t *testing.T) {
	tests := []struct {
		header string
		want   string
	}{
		{"", ""},
		{"*", ""},
		{`"42"`, "42"},
		{`  "42"  `, "42"},
		{`"42", "43"`, "42"},
		{`42`, "42"}, // tolerate unquoted
		{`"a"b"`, ""},
	}
	for _, tt := range tests {
		got := parseIfMatch(tt.header)
		if got != tt.want {
			t.Errorf("parseIfMatch(%q) = %q, want %q", tt.header, got, tt.want)
		}
	}
}

func TestFieldRevisions_TouchAndTop(t *testing.T) {
	fr := NewFieldRevisions()
	if got := fr.Top(); got != "" {
		t.Errorf("empty Top = %q, want empty", got)
	}
	r1 := fr.Touch([]string{"shape.rate_mbps", "labels.test"})
	if r1 == "" {
		t.Fatal("Touch returned empty")
	}
	if got := fr.Top(); got != r1 {
		t.Errorf("Top after one Touch = %q, want %q", got, r1)
	}
	r2 := fr.Touch([]string{"fault_rules"})
	if r2 == r1 {
		t.Errorf("second Touch returned same revision %q", r2)
	}
	if r2 < r1 {
		t.Errorf("second Touch went backward: %q < %q", r2, r1)
	}
	if got := fr.Top(); got != r2 {
		t.Errorf("Top after second Touch = %q, want %q", got, r2)
	}
}

func TestFieldRevisions_TouchEmptyIsNoOp(t *testing.T) {
	fr := NewFieldRevisions()
	if got := fr.Touch(nil); got != "" {
		t.Errorf("Touch(nil) = %q, want empty", got)
	}
	if got := fr.Touch([]string{}); got != "" {
		t.Errorf("Touch([]) = %q, want empty", got)
	}
	if got := fr.Top(); got != "" {
		t.Errorf("after no-op Touch, Top = %q, want empty", got)
	}
}

func TestFieldRevisions_TouchWithSharedRevision(t *testing.T) {
	fr := NewFieldRevisions()
	rev := "2026-05-08T17:00:00.000000000Z"
	if got := fr.TouchWith([]string{"a", "b"}, rev); got != rev {
		t.Errorf("TouchWith returned %q, want %q", got, rev)
	}
	if got := fr.Top(); got != rev {
		t.Errorf("Top after TouchWith = %q, want %q", got, rev)
	}
}

func TestNewRevision_FixedWidthLexOrders(t *testing.T) {
	// 1000 calls back-to-back; each must lex-sort strictly newer than
	// the previous. Catches the pre-fix bug where time.RFC3339Nano
	// stripped trailing zeros and `.9Z` lex-compared > `.10Z`.
	prev := newRevision()
	for i := 0; i < 1000; i++ {
		next := newRevision()
		if next <= prev {
			t.Fatalf("lex-order broken at iter %d: %q !> %q", i, next, prev)
		}
		prev = next
	}
}

func TestFieldRevisions_ConflictsDetection(t *testing.T) {
	fr := NewFieldRevisions()
	r1 := fr.Touch([]string{"shape.rate_mbps"})

	// Reading at exactly r1 → no conflict on that field.
	if c := fr.Conflicts(r1, []string{"shape.rate_mbps"}); len(c) != 0 {
		t.Errorf("If-Match=top → conflicts %v, want none", c)
	}

	// Untouched fields never conflict.
	if c := fr.Conflicts(r1, []string{"labels.test"}); len(c) != 0 {
		t.Errorf("untouched field reported as conflict: %v", c)
	}

	// Newer write → reading at the older revision conflicts.
	fr.Touch([]string{"shape.rate_mbps"})
	if c := fr.Conflicts(r1, []string{"shape.rate_mbps"}); !reflect.DeepEqual(c, []string{"shape.rate_mbps"}) {
		t.Errorf("stale If-Match → conflicts %v, want [shape.rate_mbps]", c)
	}

	// Multiple paths, mixed: only contended ones surface.
	fr.Touch([]string{"labels.run"}) // bumps top
	c := fr.Conflicts(r1, []string{"shape.rate_mbps", "labels.run", "labels.untouched"})
	sort.Strings(c)
	want := []string{"labels.run", "shape.rate_mbps"}
	if !reflect.DeepEqual(c, want) {
		t.Errorf("multi-path conflicts %v, want %v", c, want)
	}
}

func TestFieldRevisions_EmptyIfMatchIsConflictOnTouchedFields(t *testing.T) {
	fr := NewFieldRevisions()
	fr.Touch([]string{"shape.rate_mbps"})
	c := fr.Conflicts("", []string{"shape.rate_mbps"})
	if len(c) != 1 {
		t.Errorf("empty If-Match on touched field → conflicts %v, want 1", c)
	}
}

func TestFieldRevisions_HierarchicalConflicts(t *testing.T) {
	fr := NewFieldRevisions()
	r1 := fr.Touch([]string{"fault_rules.r1"})

	// Whole-array query at If-Match=top → no conflict (top is r1 itself).
	if c := fr.Conflicts(r1, []string{"fault_rules"}); len(c) != 0 {
		t.Errorf("whole-array at top revision → %v, want none", c)
	}

	// Someone else PATCHed rule r1 since I read.
	fr.Touch([]string{"fault_rules.r1"})
	// Whole-array PATCHer at the older revision → conflict.
	if c := fr.Conflicts(r1, []string{"fault_rules"}); len(c) != 1 || c[0] != "fault_rules" {
		t.Errorf("whole-array vs descendant write: %v, want [fault_rules]", c)
	}

	// Whole-array writer wins; per-rule reader at the now-stale rev →
	// conflict on rule r1 (parent path was bumped).
	fr.Touch([]string{"fault_rules"})
	if c := fr.Conflicts(r1, []string{"fault_rules.r1"}); len(c) != 1 || c[0] != "fault_rules.r1" {
		t.Errorf("per-rule vs ancestor write: %v, want [fault_rules.r1]", c)
	}

	// Sibling rules don't conflict at the current top.
	fr.Touch([]string{"fault_rules.r2"})
	top := fr.Top()
	if c := fr.Conflicts(top, []string{"fault_rules.r3"}); len(c) != 0 {
		// fault_rules ancestor revision is older than top, so won't conflict.
		t.Errorf("untouched sibling at top: %v, want none", c)
	}
}

func TestFieldRevisions_SnapshotRestoreRoundTrip(t *testing.T) {
	fr := NewFieldRevisions()
	fr.Touch([]string{"a.b", "a.c"})
	fr.Touch([]string{"d"})
	snap := fr.Snapshot()

	fr2 := NewFieldRevisions()
	fr2.Restore(snap)
	if !reflect.DeepEqual(fr.Snapshot(), fr2.Snapshot()) {
		t.Errorf("snapshot/restore lost data")
	}
}
