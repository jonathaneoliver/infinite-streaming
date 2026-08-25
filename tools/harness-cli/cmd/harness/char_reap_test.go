package main

import (
	"testing"

	"github.com/jonathaneoliver/infinite-streaming/go-proxy/pkg/charplan"
)

// TestPlanPlayerIDs guards the crash-reap targeting (#938): only sessions that were
// actually bootstrapped (non-empty player_id) get released. Skipped/bootstrap-failed
// arms carry a zero-value ArmConfig and MUST be dropped, or a stray "" would try to
// delete a non-existent session (and, worse, mask which arms really leaked).
func TestPlanPlayerIDs(t *testing.T) {
	tests := []struct {
		name string
		arms []charplan.ArmConfig
		want []string
	}{
		{"nil plan", nil, nil},
		{"all bootstrapped", []charplan.ArmConfig{{PlayerID: "a"}, {PlayerID: "b"}}, []string{"a", "b"}},
		{
			"skipped arms (empty) dropped",
			[]charplan.ArmConfig{{PlayerID: "a"}, {}, {PlayerID: "c"}},
			[]string{"a", "c"},
		},
		{"whitespace-only is empty", []charplan.ArmConfig{{PlayerID: "  "}, {PlayerID: "d"}}, []string{"d"}},
		{"all skipped → nothing to reap", []charplan.ArmConfig{{}, {}}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := planPlayerIDs(tt.arms)
			if len(got) != len(tt.want) {
				t.Fatalf("planPlayerIDs = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("planPlayerIDs[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}
