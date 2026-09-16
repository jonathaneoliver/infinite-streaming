package main

import (
	"context"
	"testing"
)

// #910 per-session degraded-shaping toggle: pins the pure logic (mode
// normalisation, forced-cap resolution, effective-cap resolution, kernel-gate
// predicates) and the per-port kernel-apply gate short-circuit. The only
// degraded mode is http-only (network shaping off, HTTP faults still fire).

func TestNormalizeShapingMode(t *testing.T) {
	cases := map[string]string{
		"http-only": "http-only",
		"HTTP-ONLY": "http-only",
		"httponly":  "http-only",
		"http":      "http-only",
		"kernel":    "",
		"off":       "",
		"":          "",
		"userspace": "",
		"bogus":     "",
	}
	for in, want := range cases {
		if got := normalizeShapingMode(in); got != want {
			t.Errorf("normalizeShapingMode(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestForcedShapingCaps(t *testing.T) {
	httpOnly, ok := forcedShapingCaps("http-only")
	if !ok || httpOnly.mode != "http-only" || httpOnly.rate || httpOnly.delay || httpOnly.loss || httpOnly.transportFault {
		t.Fatalf("http-only caps wrong: %+v ok=%v", httpOnly, ok)
	}
	if !httpOnly.degraded() || httpOnly.kernelRateOn() || httpOnly.kernelNetemOn() {
		t.Fatalf("http-only should be degraded with kernel off: %+v", httpOnly)
	}
	if _, ok := forcedShapingCaps("kernel"); ok {
		t.Fatalf("'kernel' is not a forced mode")
	}
	if _, ok := forcedShapingCaps("userspace"); ok {
		t.Fatalf("'userspace' is no longer a forced mode")
	}
}

func kernelCaps() shapingCaps {
	return shapingCaps{rate: true, delay: true, loss: true, transportFault: true, mode: "kernel"}
}

func TestEffectiveShapingForSession(t *testing.T) {
	app := &App{shaping: kernelCaps()}

	// No forced mode → inherits the (kernel) host caps.
	if eff := app.effectiveShapingForSession(SessionData{}); eff.mode != "kernel" || !eff.kernelRateOn() {
		t.Fatalf("no forced mode should inherit kernel host caps: %+v", eff)
	}
	// Forced http-only on a kernel box → session is degraded, kernel gated.
	eff := app.effectiveShapingForSession(SessionData{"shaping_forced_mode": "http-only"})
	if eff.mode != "http-only" || eff.kernelRateOn() || eff.kernelNetemOn() || !eff.degraded() {
		t.Fatalf("forced http-only should degrade the session: %+v", eff)
	}
}

func TestPerPortShapingGate(t *testing.T) {
	nm := NewTcTrafficManager("lo", false)
	// Gates default open.
	if nm.portRateGated(30181) || nm.portNetemGated(30181) {
		t.Fatalf("gates should default open")
	}
	// Close the rate gate for one port only.
	nm.SetPortShapingGate(30181, true, false)
	if !nm.portRateGated(30181) || nm.portNetemGated(30181) {
		t.Fatalf("rate gate should be closed, netem open for 30181")
	}
	if nm.portRateGated(30281) {
		t.Fatalf("other ports must be unaffected (per-session, not global)")
	}
	// A gated port no-ops the kernel apply (returns nil WITHOUT shelling to tc).
	if err := nm.UpdateRateLimit(30181, 5); err != nil {
		t.Fatalf("gated UpdateRateLimit should no-op nil, got %v", err)
	}
	// Clearing re-opens.
	nm.SetPortShapingGate(30181, false, false)
	if nm.portRateGated(30181) {
		t.Fatalf("rate gate should re-open after clear")
	}
}

// #910 canonical-clear invariant. A pattern lives in THREE representations —
// the in-memory step loop, the v1 nftables_pattern_* session fields, and the v2
// `_v2_shape_pattern` stash the dashboard renders shape.pattern from. They must
// clear together; the phantom "pattern running" bug was the v2 stash surviving a
// stop. This pins the invariant so any future divergence fails here.
func TestDisablePatternClearsAllRepresentations(t *testing.T) {
	const port = 30181
	sess := SessionData{
		"player_id":        "aaaaaaaa-0000-0000-0000-000000000001",
		"session_id":       "sess-1",
		"x_forwarded_port": "30181",
		// (2) v1 session fields — a live pattern with runtime state.
		"nftables_pattern_enabled":           true,
		"nftables_pattern_steps":             []NftShapeStep{{RateMbps: 30, DurationSeconds: 6, Enabled: true}},
		"nftables_pattern_step":              2,
		"nftables_pattern_step_runtime":      2,
		"nftables_pattern_rate_runtime_mbps": 7.7,
		"nftables_pattern_step_runtime_at":   "2026-07-07T00:00:00Z",
		"nftables_pattern_template_mode":     "pyramid",
		// (3) v2 stash — what the dashboard's shape.pattern is built from.
		"_v2_shape_pattern": map[string]interface{}{
			"template": "pyramid",
			"steps":    []any{map[string]any{"rate_mbps": 30.0, "duration_seconds": 6.0, "enabled": true}},
		},
	}
	a := newFanoutTestApp([]SessionData{sess}, map[int]ShapeApplyState{})
	// (1) in-memory loop state.
	cancelled := false
	a.shapeStates = map[int]NftShapePattern{port: {
		Steps:          []NftShapeStep{{RateMbps: 30, DurationSeconds: 6, Enabled: true}},
		ActiveStep:     2,
		ActiveRateMbps: 7.7,
		ActiveAt:       "2026-07-07T00:00:00Z",
	}}
	a.shapeLoops = map[int]context.CancelFunc{port: func() { cancelled = true }}

	a.disablePatternForPort(port)

	// (1) in-memory representation gone + loop cancelled.
	if _, ok := a.getShapePattern(port); ok {
		t.Errorf("in-memory shapeStates still present after disable")
	}
	if _, ok := a.shapeLoops[port]; ok {
		t.Errorf("shapeLoops[%d] not deleted", port)
	}
	if !cancelled {
		t.Errorf("pattern loop context not cancelled")
	}

	// Read the mutated session back.
	var got SessionData
	for _, s := range a.sessionsView() {
		if getString(s, "x_forwarded_port") == "30181" {
			got = s
		}
	}
	if got == nil {
		t.Fatal("session not found after disable")
	}
	// (2) v1 fields cleared.
	if getBool(got, "nftables_pattern_enabled") {
		t.Errorf("nftables_pattern_enabled still true")
	}
	if steps, ok := got["nftables_pattern_steps"].([]NftShapeStep); !ok || len(steps) != 0 {
		t.Errorf("nftables_pattern_steps not empty: %v", got["nftables_pattern_steps"])
	}
	for _, k := range []string{
		"nftables_pattern_rate_runtime_mbps",
		"nftables_pattern_step_runtime",
		"nftables_pattern_step_runtime_at",
	} {
		if got[k] != nil {
			t.Errorf("%s not cleared: %v", k, got[k])
		}
	}
	if tm := getString(got, "nftables_pattern_template_mode"); tm != "" {
		t.Errorf("template_mode not cleared: %q", tm)
	}
	// (3) v2 stash cleared — the invariant that FAILED before the fix; this is
	// the source the dashboard's shape.pattern is built from.
	if got["_v2_shape_pattern"] != nil {
		t.Errorf("_v2_shape_pattern not cleared (v2 shape.pattern would still render): %v", got["_v2_shape_pattern"])
	}
}
