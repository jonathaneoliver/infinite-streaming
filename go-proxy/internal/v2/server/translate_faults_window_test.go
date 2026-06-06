package server

import "testing"

// #643 — the v2 fault_rules translator must clear the failure engine's
// persisted window cursor (<surface>_failure_at / _failure_recover_at)
// on every re-translation, or a re-armed rule resumes the previous
// arm's half-consumed window and silently under-delivers faults. This
// is the path the harness CLI arms through; the v1 PATCH path has the
// matching resetFailureWindowState (cmd/server/main.go).
func TestTranslateFaultRulesClearsWindowCursor(t *testing.T) {
	s := map[string]any{
		// Cursor state left behind by a previous arm's request handling.
		"master_manifest_failure_at":         8,
		"master_manifest_failure_recover_at": 18,
		"all_failure_at":                     2,
		"all_failure_recover_at":             12,
	}
	rules := []any{
		map[string]any{
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
	for _, key := range []string{
		"master_manifest_failure_at", "master_manifest_failure_recover_at",
		"all_failure_at", "all_failure_recover_at",
	} {
		if _, ok := s[key]; ok {
			t.Errorf("%s survived re-arm — stale window would resume", key)
		}
	}
}
