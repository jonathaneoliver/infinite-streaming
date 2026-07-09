package main

import (
	"fmt"
	"testing"
	"time"
)

// The parity oracle for #919 step 2: prove the native fault_rules evaluator
// produces the SAME per-request decision as the v1 surface engine everywhere v1
// can express the config, and the CORRECT divergence where it can't (#917/#918).
//
// Both engines run over the same request sequence. They persist state under
// disjoint session keys (v1: `<surface>_failure_at`/`segments_count`; native:
// `_faultrule_state`), so a single session map drives both without collision.

// seedSegmentFault populates a session with a segment-scoped fault expressed
// BOTH ways: v1 surface fields (what translate_faults writes) and the v2
// `_v2_fault_rules` array — so runV1 and runNative see the same rule.
func seedSegmentFault(typ string, freq, consec int, mode string) SessionData {
	consecUnits, freqUnits := unitsForMode(mode)
	return SessionData{
		"manifest_variants": []PlaylistInfo{
			{URL: "720p/playlist.m3u8", Bandwidth: 4_000_000, Resolution: "1280x720"},
		},
		// v1 surface fields (as translate_faults.go writes them). The "All"
		// sentinel in _failure_urls is what an unscoped rule translates to —
		// without it v1's shouldApplyFailure([]) short-circuits to no-fault.
		"segment_failure_type":         typ,
		"segment_failure_frequency":    freq,
		"segment_consecutive_failures": consec,
		"segment_failure_mode":         mode,
		"segment_consecutive_units":    consecUnits,
		"segment_frequency_units":      freqUnits,
		"segment_failure_urls":         []string{"All"},
		// v2 stash (verbatim, as the PATCH stores it).
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

func runV1Segment(s SessionData, filename string) string {
	return NewRequestHandler(true, false, false, s).HandleRequest(filename)
}

func runNativeSegment(s SessionData, filename string, now time.Time) string {
	rc := classifyRequest(s, filename, true, false, false)
	return evaluateFaultRules(s, rc, now)
}

// TestParity_SegmentCountMode is the strong deterministic proof: mode=requests
// is pure count-space (no wall clock), so the two engines must agree on every
// single request across a long sequence and a range of (freq, consec).
func TestParity_SegmentCountMode(t *testing.T) {
	const seg = "/go-live/c/720p/segment_%d.m4s"
	now := time.Unix(1_700_000_000, 0)
	configs := []struct{ freq, consec int }{
		{5, 1}, {5, 2}, {6, 3}, {10, 4}, {3, 1}, {8, 8},
	}
	for _, cfg := range configs {
		v1s := seedSegmentFault("500", cfg.freq, cfg.consec, "requests")
		nats := seedSegmentFault("500", cfg.freq, cfg.consec, "requests")
		for i := 1; i <= 40; i++ {
			fn := fmt.Sprintf(seg, i)
			gotV1 := runV1Segment(v1s, fn)
			gotNat := runNativeSegment(nats, fn, now)
			if gotV1 != gotNat {
				t.Fatalf("freq=%d consec=%d req#%d: v1=%q native=%q (diverged)",
					cfg.freq, cfg.consec, i, gotV1, gotNat)
			}
		}
	}
}

// seedSegmentFaultV1Only mimics the v1 write API (server_behavior): it sets the
// surface fields and cadence UNITS but NO `_failure_mode` and NO
// `_v2_fault_rules`. The native engine must reconstruct the rule via the reverse
// bridge (synthRulesFromV1) and derive the mode from the units — the regression
// TestServerFault caught, where an absent _failure_mode wrongly defaulted a
// count-mode config to time-based.
func seedSegmentFaultV1Only(typ string, freq, consec int, consecUnits, freqUnits string) SessionData {
	return SessionData{
		"manifest_variants": []PlaylistInfo{
			{URL: "720p/playlist.m3u8", Bandwidth: 4_000_000, Resolution: "1280x720"},
		},
		"segment_failure_type":         typ,
		"segment_failure_frequency":    freq,
		"segment_consecutive_failures": consec,
		"segment_consecutive_units":    consecUnits,
		"segment_frequency_units":      freqUnits,
		"segment_failure_urls":         []string{"All"},
	}
}

// TestParity_ReverseBridge_CountMode exercises the v1-only path (no
// _v2_fault_rules → synthRulesFromV1) with count-mode units and no
// _failure_mode. Over 120 requests a "1-in-10" must fire ~12 times on BOTH
// engines — the exact live shape from TestServerFault.
func TestParity_ReverseBridge_CountMode(t *testing.T) {
	const seg = "/go-live/c/720p/segment_%d.m4s"
	now := time.Unix(1_700_000_000, 0)
	v1s := seedSegmentFaultV1Only("503", 10, 1, "requests", "requests")
	nats := seedSegmentFaultV1Only("503", 10, 1, "requests", "requests")
	v1Fires, natFires := 0, 0
	for i := 1; i <= 120; i++ {
		fn := fmt.Sprintf(seg, i)
		gv1 := runV1Segment(v1s, fn)
		gn := runNativeSegment(nats, fn, now)
		if gv1 != gn {
			t.Fatalf("reverse-bridge req#%d: v1=%q native=%q", i, gv1, gn)
		}
		if gv1 == "503" {
			v1Fires++
		}
		if gn == "503" {
			natFires++
		}
	}
	if natFires < 10 || natFires > 14 {
		t.Errorf("reverse-bridge 1-in-10 over 120 fired %d times, want ~12 (regression guard)", natFires)
	}
	if v1Fires != natFires {
		t.Errorf("fire counts diverged: v1=%d native=%d", v1Fires, natFires)
	}
}

// TestParity_SegmentOneShot: a one-shot (frequency 0) fires for its on-window
// then reverts to none forever, on both engines.
func TestParity_SegmentOneShot(t *testing.T) {
	const seg = "/go-live/c/720p/segment_%d.m4s"
	now := time.Unix(1_700_000_000, 0)
	v1s := seedSegmentFault("404", 0, 3, "requests")
	nats := seedSegmentFault("404", 0, 3, "requests")
	fired := 0
	for i := 1; i <= 20; i++ {
		fn := fmt.Sprintf(seg, i)
		gotV1 := runV1Segment(v1s, fn)
		gotNat := runNativeSegment(nats, fn, now)
		if gotV1 != gotNat {
			t.Fatalf("one-shot req#%d: v1=%q native=%q", i, gotV1, gotNat)
		}
		if gotNat == "404" {
			fired++
		}
	}
	if fired != 3 {
		t.Errorf("one-shot fired %d times, want 3", fired)
	}
}

// TestParity_TimeMode: mode=seconds is time-space. Both engines share the same
// FailureHandler time path but initialise `failureAt` from wall-clock nowISO()
// when unset — so to stay deterministic we PRE-SEED failureAt on both sides to a
// fixed base and advance a fake clock from there. Guards the units mapping and
// state threading for the seconds mode.
func TestParity_TimeMode(t *testing.T) {
	const seg = "/go-live/c/720p/segment_%d.m4s"
	const tsFmt = "2006-01-02T15:04:05.000" // v1's failureAt string format
	base := time.Unix(1_700_000_000, 0)
	baseStr := base.Format(tsFmt)

	v1s := seedSegmentFault("503", 10, 3, "seconds")
	v1s["segment_failure_at"] = baseStr
	nats := seedSegmentFault("503", 10, 3, "seconds")
	nats["_faultrule_state"] = map[string]any{
		"seg-rule": map[string]any{"count": 0, "failure_at": baseStr, "failure_recover_at": nil, "done": false},
	}

	for i := 1; i <= 30; i++ {
		now := base.Add(time.Duration(i) * time.Second)
		fn := fmt.Sprintf(seg, i)
		gotV1 := runV1Segment(v1s, fn)
		gotNat := runNativeSegment(nats, fn, now)
		if gotV1 != gotNat {
			t.Fatalf("time-mode t+%ds req#%d: v1=%q native=%q", i, i, gotV1, gotNat)
		}
	}
}

// TestParity_FailuresPerSeconds covers the dashboard's DEFAULT cadence mode:
// failures_per_seconds → consecutiveUnits=requests (count on-window),
// frequencyUnits=seconds (time gap). It's the mixed mode the other parity tests
// didn't exercise. Pre-seed failureAt (time) on both engines for determinism,
// then advance a shared clock and assert identical per-request decisions.
func TestParity_FailuresPerSeconds(t *testing.T) {
	const seg = "/go-live/c/720p/segment_%d.m4s"
	const tsFmt = "2006-01-02T15:04:05.000"
	base := time.Unix(1_700_000_000, 0)
	baseStr := base.Format(tsFmt)

	v1s := seedSegmentFault("500", 10, 1, "failures_per_seconds")
	v1s["segment_failure_at"] = baseStr
	nats := seedSegmentFault("500", 10, 1, "failures_per_seconds")
	nats["_faultrule_state"] = map[string]any{
		"seg-rule": map[string]any{"count": 0, "failure_at": baseStr, "failure_recover_at": nil, "done": false},
	}

	// Two segment requests per simulated second (a modest burst) over ~40s, so
	// the 10s gap is crossed repeatedly.
	for i := 1; i <= 80; i++ {
		now := base.Add(time.Duration(i*500) * time.Millisecond)
		fn := fmt.Sprintf(seg, i)
		gotV1 := runV1Segment(v1s, fn)
		gotNat := runNativeSegment(nats, fn, now)
		if gotV1 != gotNat {
			t.Fatalf("failures_per_seconds req#%d (t+%dms): v1=%q native=%q", i, i*500, gotV1, gotNat)
		}
	}
}

// TestParity_IntendedDivergence_AudioAndInit is the #917/#918 payoff: for a
// segment-scoped rule, v1 (which can't distinguish) faults audio AND init,
// while the native engine correctly scopes them OUT. This asserts the engines
// DIFFER, and in the right direction.
func TestParity_IntendedDivergence_AudioAndInit(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	for _, path := range []string{
		"/go-live/c/audio/segment_1.m4s",
		"/go-live/c/720p/init.mp4",
	} {
		s := seedSegmentFault("500", 1, 1, "requests") // every matching request faults
		gotV1 := runV1Segment(s, path)
		gotNat := runNativeSegment(s, path, now)
		if gotV1 != "500" {
			t.Errorf("%s: expected v1 to (wrongly) fault it; got %q", path, gotV1)
		}
		if gotNat != "none" {
			t.Errorf("%s: native must NOT fault a non-video request under a segment-scoped rule; got %q (#917/#918)", path, gotNat)
		}
	}
}
