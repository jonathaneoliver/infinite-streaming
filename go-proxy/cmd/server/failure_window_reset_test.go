package main

import (
	"testing"
)

// Covers resetFailureWindowState (#643) — re-arming an HTTP fault must
// open a FRESH consecutive window, not resume the previous arm's
// half-consumed one. Reproduced live 2026-06-06: arm master_manifest
// 404 --consecutive 10, consume 4 requests, re-arm ×10 → only 6 more
// faults fired before the OLD recover point silenced the rule.
func TestResetFailureWindowState(t *testing.T) {
	t.Run("config change clears the touched surface's cursor", func(t *testing.T) {
		target := SessionData{
			"master_manifest_failure_at":         8,
			"master_manifest_failure_recover_at": 18,
			"segment_failure_at":                 3,
			"segment_failure_recover_at":         5,
		}
		payload := map[string]interface{}{
			"master_manifest_failure_type":         "404",
			"master_manifest_consecutive_failures": 10,
		}
		resetFailureWindowState(payload, target)
		if _, ok := target["master_manifest_failure_at"]; ok {
			t.Error("master_manifest_failure_at survived a re-arm")
		}
		if _, ok := target["master_manifest_failure_recover_at"]; ok {
			t.Error("master_manifest_failure_recover_at survived a re-arm")
		}
		// Untouched surface keeps its cursor.
		if got := intFromInterface(target["segment_failure_at"]); got != 3 {
			t.Errorf("segment_failure_at = %d, want 3 (untouched surface must keep state)", got)
		}
	})

	t.Run("all surface is covered", func(t *testing.T) {
		target := SessionData{
			"all_failure_at":         2,
			"all_failure_recover_at": 12,
		}
		resetFailureWindowState(map[string]interface{}{"all_failure_type": "500"}, target)
		if _, ok := target["all_failure_at"]; ok {
			t.Error("all_failure_at survived a re-arm")
		}
	})

	t.Run("non-fault payload leaves cursors alone", func(t *testing.T) {
		target := SessionData{
			"master_manifest_failure_at":         8,
			"master_manifest_failure_recover_at": 18,
		}
		resetFailureWindowState(map[string]interface{}{"nftables_bandwidth_mbps": 5.0}, target)
		if got := intFromInterface(target["master_manifest_failure_at"]); got != 8 {
			t.Errorf("failure_at = %d, want 8 (shape-only PATCH must not reset fault windows)", got)
		}
	})
}

// The native-engine end-to-end re-arm behaviour (#643) is covered by
// TestNativeReArmResetsWindow (fault_parity_test.go) and
// TestTranslateFaultRulesClearsRuleState (internal/v2/server) since the v1
// surface engine + NewFailureHandler were removed in #925.
