package main

import (
	"testing"
	"time"
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

// End-to-end through the engine: a fresh window after re-arm delivers
// the FULL consecutive count, mirroring the live curl reproduction.
func TestReArmDeliversFullWindow(t *testing.T) {
	session := SessionData{
		"master_manifest_failure_type":         "404",
		"master_manifest_failure_frequency":    0,
		"master_manifest_consecutive_failures": 10,
		"master_manifest_failure_units":        "requests",
		"master_manifest_consecutive_units":    "requests",
		"master_manifest_frequency_units":      "requests",
		"master_manifest_failure_mode":         "requests",
		"master_manifest_requests_count":       0,
	}
	now := time.Now()

	fire := func(count int) string {
		h := NewFailureHandler("master_manifest", session)
		ft := h.HandleFailure(count, now)
		session["master_manifest_failure_at"] = h.failureAt
		session["master_manifest_failure_recover_at"] = h.failureRecoverAt
		return ft
	}

	// First arm: consume 4 of the 10-request window.
	for i := 1; i <= 4; i++ {
		if got := fire(i); got != "404" {
			t.Fatalf("first arm req %d: failureType = %q, want 404", i, got)
		}
	}

	// Re-arm (same config PATCH) — must clear the cursor.
	resetFailureWindowState(map[string]interface{}{
		"master_manifest_failure_type":         "404",
		"master_manifest_consecutive_failures": 10,
	}, session)

	// Second arm must deliver a FULL 10-request window. Pre-fix, requests
	// 5..10 faulted (old recoverAt=11) and 11+ passed clean — only 6 of 10.
	for i := 5; i <= 14; i++ {
		if got := fire(i); got != "404" {
			t.Fatalf("re-arm req %d: failureType = %q, want 404 (stale window resumed?)", i, got)
		}
	}
	// And the window must still CLOSE: request 15 is past the fresh
	// recover point (5 + 10), so it recovers to none.
	if got := fire(15); got != "none" {
		t.Fatalf("req 15: failureType = %q, want none (window must still close)", got)
	}
}
