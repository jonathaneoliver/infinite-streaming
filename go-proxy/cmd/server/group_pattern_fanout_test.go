package main

import "testing"

// Single-owner group shaping (issue: single-master group shaping). The pattern
// loop runs on ONE master; each tick fanPatternRateToGroup mirrors the master's
// cap onto the group's other members and stamps the "driven by master" markers
// the slave UI reads — without arming a per-member pattern. These tests exercise
// that fan-out + the owner-label resolution with an in-memory App (no tc): a
// zero-value portMap makes sessionPortToInternal an identity, and pre-seeding the
// member's shape-apply state to the fanned rate makes applyShapeIfChanged
// short-circuit as "unchanged" so it never shells out to the kernel.

func newFanoutTestApp(sessions []SessionData, shapeApply map[int]ShapeApplyState) *App {
	a := &App{shapeApply: shapeApply}
	a.sessionsSnap.Store(&sessions)
	return a
}

func TestPatternOwnerLabels_PlayerIDFallbackAndTemplate(t *testing.T) {
	master := SessionData{
		"player_id":         "abb13111-9e46-4230-bd48-031b92154b63",
		"group_id":          "pyramid-2sim/pairX",
		"x_forwarded_port":  "30181",
		"_v2_shape_pattern": map[string]interface{}{"template": "pyramid"},
	}
	slave := SessionData{
		"player_id":        "c929ba15-a846-4606-84fe-54c33d9e5f48",
		"group_id":         "pyramid-2sim/pairX",
		"x_forwarded_port": "30281",
	}
	a := newFanoutTestApp([]SessionData{master, slave}, map[int]ShapeApplyState{})

	by, tmpl := a.patternOwnerLabels(30181)
	// No display_id → short (8-char) player_id.
	if by != "abb13111" {
		t.Errorf("drivenBy: want short player_id %q, got %q", "abb13111", by)
	}
	// Template read from the master's _v2_shape_pattern.
	if tmpl != "pyramid" {
		t.Errorf("drivenTemplate: want %q, got %q", "pyramid", tmpl)
	}
}

func TestPatternOwnerLabels_DisplayIDPreferred(t *testing.T) {
	master := SessionData{
		"player_id":        "abb13111-9e46-4230-bd48-031b92154b63",
		"display_id":       "lab-master-7",
		"group_id":         "G",
		"x_forwarded_port": "30181",
	}
	a := newFanoutTestApp([]SessionData{master}, map[int]ShapeApplyState{})
	by, _ := a.patternOwnerLabels(30181)
	if by != "lab-master-7" {
		t.Errorf("drivenBy: want display_id %q, got %q", "lab-master-7", by)
	}
}

func TestFanPatternRateToGroup_StampsSlaveOnly(t *testing.T) {
	master := SessionData{
		"player_id":        "abb13111-9e46-4230-bd48-031b92154b63",
		"group_id":         "G",
		"x_forwarded_port": "30181",
	}
	slave := SessionData{
		"player_id":        "c929ba15-a846-4606-84fe-54c33d9e5f48",
		"group_id":         "G",
		"x_forwarded_port": "30281",
	}
	// Pre-seed the slave's apply-state to the rate we'll fan so applyShapeIfChanged
	// short-circuits ("unchanged") and never touches tc — lets the test run off-Linux.
	a := newFanoutTestApp(
		[]SessionData{master, slave},
		map[int]ShapeApplyState{30281: {rate: 2.5, delay: 0, loss: 0}},
	)

	a.fanPatternRateToGroup(30181, 2.5, NetemParams{}, "M1", "pyramid")

	var ms, sv SessionData
	for _, s := range a.sessionsView() {
		switch getString(s, "x_forwarded_port") {
		case "30181":
			ms = s
		case "30281":
			sv = s
		}
	}
	if sv == nil {
		t.Fatal("slave session missing after fan-out")
	}
	// Slave gets the fanned rate + the driven markers.
	if got := getFloat(sv, "nftables_pattern_rate_runtime_mbps"); got != 2.5 {
		t.Errorf("slave rate_runtime: want 2.5, got %v", got)
	}
	if got := getString(sv, "nftables_pattern_driven_by"); got != "M1" {
		t.Errorf("slave driven_by: want %q, got %q", "M1", got)
	}
	if got := getString(sv, "nftables_pattern_driven_template"); got != "pyramid" {
		t.Errorf("slave driven_template: want %q, got %q", "pyramid", got)
	}
	// Master (the origin) is the single owner — it must NOT be fanned onto as a
	// driven slave (it carries its own pattern). The UI also guards on this.
	if got := getString(ms, "nftables_pattern_driven_by"); got != "" {
		t.Errorf("master should not be stamped driven_by, got %q", got)
	}
}

func TestFanPatternRateToGroup_NoGroupIsNoop(t *testing.T) {
	// A lone (ungrouped) session must not panic or stamp anything.
	solo := SessionData{
		"player_id":        "abb13111-9e46-4230-bd48-031b92154b63",
		"x_forwarded_port": "30181",
	}
	a := newFanoutTestApp([]SessionData{solo}, map[int]ShapeApplyState{})
	a.fanPatternRateToGroup(30181, 2.5, NetemParams{}, "M1", "pyramid")
	for _, s := range a.sessionsView() {
		if got := getString(s, "nftables_pattern_driven_by"); got != "" {
			t.Errorf("ungrouped session should not be stamped, got %q", got)
		}
	}
}
