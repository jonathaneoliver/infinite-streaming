package server

import "testing"

// #643 — re-arming a fault must open a FRESH cadence window, not resume the
// previous arm's half-consumed one. Under the native engine (#925) the window
// cursors live in `_faultrule_state`, so translateFaultRules must clear that on
// every re-translation. (Pre-#925 this cleared the v1 <surface>_failure_at
// fields; same intent, native storage.)
func TestTranslateFaultRulesClearsRuleState(t *testing.T) {
	s := map[string]any{
		// Cadence state left behind by a previous arm.
		"_faultrule_state": map[string]any{
			"sb-master_manifest": map[string]any{"count": 4, "failure_recover_at": 11, "done": false},
		},
	}
	rules := []any{
		map[string]any{
			"id":          "sb-master_manifest",
			"type":        "404",
			"frequency":   float64(0),
			"consecutive": float64(10),
			"mode":        "requests",
			"filter":      map[string]any{"request_kind": []any{"master_manifest"}},
		},
	}
	if err := translateFaultRules(s, rules); err != nil {
		t.Fatalf("translateFaultRules: %v", err)
	}
	if _, ok := s["_faultrule_state"]; ok {
		t.Error("_faultrule_state survived re-arm — the stale window would resume (#643)")
	}
	if _, ok := s["_v2_fault_rules"].([]any); !ok {
		t.Error("_v2_fault_rules not stored")
	}
}
