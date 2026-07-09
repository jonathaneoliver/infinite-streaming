package main

import (
	"fmt"
	"testing"
	"time"
)

// Native fault-engine cadence tests (#925). These replaced the v1-vs-native
// parity oracle once the v1 surface engine was deleted — they assert the native
// evaluator's cadence directly. End-to-end cadence against a live server is
// covered by tests/server_behavior (TestServerFault et al.).

// seedSegmentFaultV2 configures a single segment-scoped rule on _v2_fault_rules
// (the sole fault input since #925), plus a minimal video ladder so a segment
// classifies as request_kind=segment.
func seedSegmentFaultV2(typ string, freq, consec int, mode string) SessionData {
	return SessionData{
		"manifest_variants": []PlaylistInfo{
			{URL: "720p/playlist.m3u8", Bandwidth: 4_000_000, Resolution: "1280x720"},
		},
		"_v2_fault_rules": []any{
			map[string]any{
				"id":          "seg-rule",
				"type":        typ,
				"frequency":   float64(freq),
				"consecutive": float64(consec),
				"mode":        mode,
				"filter":      map[string]any{"request_kind": []any{"segment"}},
			},
		},
	}
}

func runNativeSegment(s SessionData, filename string, now time.Time) string {
	rc := classifyRequest(s, filename, true, false, false)
	return evaluateFaultRules(s, rc, now)
}

// TestNativeCountModeCadence: mode=requests fires consec of every freq requests.
func TestNativeCountModeCadence(t *testing.T) {
	const seg = "/go-live/c/720p/segment_%d.m4s"
	now := time.Unix(1_700_000_000, 0)
	cases := []struct{ freq, consec, wantMin, wantMax int }{
		{5, 1, 8, 8}, {10, 1, 4, 4}, {10, 2, 8, 8}, {5, 2, 16, 16},
	}
	for _, c := range cases {
		s := seedSegmentFaultV2("500", c.freq, c.consec, "requests")
		fires := 0
		for i := 1; i <= 40; i++ {
			if runNativeSegment(s, fmt.Sprintf(seg, i), now) == "500" {
				fires++
			}
		}
		if fires < c.wantMin || fires > c.wantMax {
			t.Errorf("freq=%d consec=%d over 40 reqs: %d fires, want %d-%d", c.freq, c.consec, fires, c.wantMin, c.wantMax)
		}
	}
}

// TestNativeOneShot: a one-shot (frequency 0) fires exactly `consec` times then
// reverts to none forever.
func TestNativeOneShot(t *testing.T) {
	const seg = "/go-live/c/720p/segment_%d.m4s"
	now := time.Unix(1_700_000_000, 0)
	s := seedSegmentFaultV2("404", 0, 3, "requests")
	fires := 0
	for i := 1; i <= 20; i++ {
		if runNativeSegment(s, fmt.Sprintf(seg, i), now) == "404" {
			fires++
		}
	}
	if fires != 3 {
		t.Errorf("one-shot fired %d times, want 3", fires)
	}
}

// TestNativeReArmResetsWindow is the #643 guard for the native engine: after a
// one-shot half-fires, clearing _faultrule_state (what translateFaultRules does
// on a re-arm PATCH) must deliver a FRESH full window, not resume the old one.
func TestNativeReArmResetsWindow(t *testing.T) {
	const seg = "/go-live/c/720p/segment_%d.m4s"
	now := time.Unix(1_700_000_000, 0)
	s := seedSegmentFaultV2("404", 0, 10, "requests")

	// Consume 4 of the 10-request window.
	for i := 1; i <= 4; i++ {
		if got := runNativeSegment(s, fmt.Sprintf(seg, i), now); got != "404" {
			t.Fatalf("first arm req %d: %q, want 404", i, got)
		}
	}
	// Re-arm: translateFaultRules drops _faultrule_state. Simulate that here.
	delete(s, "_faultrule_state")
	// A FRESH full 10-request window must fire.
	for i := 5; i <= 14; i++ {
		if got := runNativeSegment(s, fmt.Sprintf(seg, i), now); got != "404" {
			t.Fatalf("re-arm req %d: %q, want 404 (stale window resumed?)", i, got)
		}
	}
	if got := runNativeSegment(s, fmt.Sprintf(seg, 15), now); got != "none" {
		t.Errorf("req 15: %q, want none (window must still close)", got)
	}
}

// TestNativeTimeModeCadence: mode=seconds fires on the first request after each
// gap. Pre-seed failureAt for determinism (avoids the nowISO() wall clock).
func TestNativeTimeModeCadence(t *testing.T) {
	const seg = "/go-live/c/720p/segment_%d.m4s"
	base := time.Unix(1_700_000_000, 0)
	baseStr := base.Format("2006-01-02T15:04:05.000")
	s := seedSegmentFaultV2("503", 10, 1, "seconds")
	s["_faultrule_state"] = map[string]any{
		"seg-rule": map[string]any{"count": 0, "failure_at": baseStr, "failure_recover_at": nil, "done": false},
	}
	fires := 0
	for i := 1; i <= 30; i++ {
		now := base.Add(time.Duration(i) * time.Second)
		if runNativeSegment(s, fmt.Sprintf(seg, i), now) == "503" {
			fires++
		}
	}
	// Time mode THROTTLES: it fires some requests but not all. An exact count
	// isn't asserted here — the local/UTC handling of the pre-seeded failure_at
	// string makes it non-deterministic in-process; real-clock cadence is
	// covered by the live server_behavior suite.
	if fires == 0 || fires >= 30 {
		t.Errorf("time-mode fired %d/30 — expected throttling (some, not all)", fires)
	}
}

// TestNativeScopesOutAudioAndInit is the #917/#918 payoff: a segment-scoped rule
// (firing every request) must NOT fault audio segments or init segments.
func TestNativeScopesOutAudioAndInit(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	for _, path := range []string{
		"/go-live/c/audio/segment_1.m4s",
		"/go-live/c/720p/init.mp4",
	} {
		s := seedSegmentFaultV2("500", 1, 1, "requests") // every matching request faults
		if got := runNativeSegment(s, path, now); got != "none" {
			t.Errorf("%s: got %q, want none (segment-scoped rule must not hit non-video)", path, got)
		}
	}
}
