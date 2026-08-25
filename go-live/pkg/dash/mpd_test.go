package dash

import "testing"

// usesVirtualSegments gates the fragment-regrouping path. The base package is 6s;
// LL and 6s serve the base SegmentList directly, while 1s and 2s are synthesized
// by regrouping the base fmp4 fragments. 1s was added alongside 2s (#931).
func TestUsesVirtualSegments(t *testing.T) {
	cases := map[int]bool{
		1: true,  // regrouped sub-base variant (#931)
		2: true,  // regrouped sub-base variant
		4: false, // not a served variant; init-prefix special-cased elsewhere
		6: false, // base segments, served directly
	}
	for duration, want := range cases {
		if got := usesVirtualSegments(duration); got != want {
			t.Errorf("usesVirtualSegments(%d) = %v, want %v", duration, got, want)
		}
	}
}
