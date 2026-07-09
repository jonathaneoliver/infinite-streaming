package main

import "testing"

// rule is a tiny helper to build a JSON-decoded-shaped fault_rule (all numbers
// as float64, arrays as []any) the way _v2_fault_rules is stored.
func rule(id, typ string, filter map[string]any) map[string]any {
	r := map[string]any{"id": id, "type": typ, "frequency": float64(0), "consecutive": float64(1)}
	if filter != nil {
		r["filter"] = filter
	}
	return r
}

func video(rung int, resolution string, bw int) RequestClass {
	return RequestClass{Kind: "segment", RungIndex: rung, Resolution: resolution, BandwidthBps: bw, LadderSize: 5, Path: "/go-live/c/x/segment_1.m4s"}
}

func TestMatchFaultRule_NoFilterMatchesAll(t *testing.T) {
	rules := []any{rule("r1", "500", nil)}
	for _, rc := range []RequestClass{
		{Kind: "segment", RungIndex: 2, LadderSize: 5},
		{Kind: "audio_segment", RungIndex: -1, IsAudio: true},
		{Kind: "init", RungIndex: -1, IsInit: true},
		{Kind: "master_manifest", RungIndex: -1},
	} {
		mf, ok := matchFaultRule(rules, rc)
		if !ok || mf.Type != "500" {
			t.Errorf("kind %s: got (%+v,%v), want the 500 rule", rc.Kind, mf, ok)
		}
	}
}

func TestMatchFaultRule_RequestKindScopesAudioOut(t *testing.T) {
	// The #917 shape: a rule scoped to video segments must NOT hit audio.
	rules := []any{rule("vid", "404", map[string]any{"request_kind": []any{"segment"}})}

	if _, ok := matchFaultRule(rules, RequestClass{Kind: "segment", RungIndex: 0, LadderSize: 5}); !ok {
		t.Error("video segment should match request_kind=[segment]")
	}
	if _, ok := matchFaultRule(rules, RequestClass{Kind: "audio_segment", RungIndex: -1, IsAudio: true}); ok {
		t.Error("audio segment must NOT match request_kind=[segment] (#917)")
	}
	if _, ok := matchFaultRule(rules, RequestClass{Kind: "init", RungIndex: -1, IsInit: true}); ok {
		t.Error("init must NOT match request_kind=[segment] (#918)")
	}
}

func TestMatchFaultRule_InitAndAudioAreTargetable(t *testing.T) {
	// The other side of #917/#918: init and audio ARE independently scopable.
	initRule := []any{rule("i", "404", map[string]any{"request_kind": []any{"init"}})}
	if _, ok := matchFaultRule(initRule, RequestClass{Kind: "init", RungIndex: -1, IsInit: true}); !ok {
		t.Error("init request should match request_kind=[init]")
	}
	if _, ok := matchFaultRule(initRule, RequestClass{Kind: "segment", RungIndex: 0, LadderSize: 5}); ok {
		t.Error("video segment must NOT match request_kind=[init]")
	}
	audRule := []any{rule("a", "503", map[string]any{"request_kind": []any{"audio_segment", "audio_manifest"}})}
	if _, ok := matchFaultRule(audRule, RequestClass{Kind: "audio_segment", RungIndex: -1, IsAudio: true}); !ok {
		t.Error("audio segment should match request_kind=[audio_segment,audio_manifest]")
	}
}

func TestMatchFaultRule_VariantRungIndexes(t *testing.T) {
	rules := []any{rule("r", "500", map[string]any{"variant": map[string]any{"rung_indexes": []any{float64(0), float64(4)}}})}
	if _, ok := matchFaultRule(rules, video(0, "640x360", 800_000)); !ok {
		t.Error("rung 0 should match rung_indexes=[0,4]")
	}
	if _, ok := matchFaultRule(rules, video(4, "1920x1080", 8_000_000)); !ok {
		t.Error("rung 4 should match rung_indexes=[0,4]")
	}
	if _, ok := matchFaultRule(rules, video(2, "960x540", 2_200_000)); ok {
		t.Error("rung 2 must NOT match rung_indexes=[0,4]")
	}
	// A variant predicate never matches a non-video request (audio: RungIndex -1).
	if _, ok := matchFaultRule(rules, RequestClass{Kind: "audio_segment", RungIndex: -1, IsAudio: true}); ok {
		t.Error("audio must NOT match a variant predicate")
	}
}

func TestMatchFaultRule_VariantRungPositions(t *testing.T) {
	rules := []any{rule("r", "500", map[string]any{"variant": map[string]any{"rung_positions": []any{"top", "bottom"}}})}
	// LadderSize 5 → bottom=0, top=4.
	if _, ok := matchFaultRule(rules, video(0, "", 0)); !ok {
		t.Error("rung 0 should match position bottom")
	}
	if _, ok := matchFaultRule(rules, video(4, "", 0)); !ok {
		t.Error("rung 4 should match position top")
	}
	if _, ok := matchFaultRule(rules, video(2, "", 0)); ok {
		t.Error("rung 2 must NOT match positions top/bottom")
	}
}

func TestMatchFaultRule_VariantResolutionAndBandwidth(t *testing.T) {
	resRule := []any{rule("r", "500", map[string]any{"variant": map[string]any{"resolutions": []any{"1920x1080"}}})}
	if _, ok := matchFaultRule(resRule, video(4, "1920x1080", 8_000_000)); !ok {
		t.Error("1080p should match resolutions=[1920x1080]")
	}
	if _, ok := matchFaultRule(resRule, video(0, "640x360", 800_000)); ok {
		t.Error("360p must NOT match resolutions=[1920x1080]")
	}
	aboveRule := []any{rule("r", "500", map[string]any{"variant": map[string]any{"bandwidth_above": float64(5_000_000)}})}
	if _, ok := matchFaultRule(aboveRule, video(4, "1920x1080", 8_000_000)); !ok {
		t.Error("8Mbps should match bandwidth_above=5M")
	}
	if _, ok := matchFaultRule(aboveRule, video(0, "640x360", 800_000)); ok {
		t.Error("0.8Mbps must NOT match bandwidth_above=5M")
	}
	belowRule := []any{rule("r", "500", map[string]any{"variant": map[string]any{"bandwidth_below": float64(1_000_000)}})}
	if _, ok := matchFaultRule(belowRule, video(0, "640x360", 800_000)); !ok {
		t.Error("0.8Mbps should match bandwidth_below=1M")
	}
	if _, ok := matchFaultRule(belowRule, video(4, "1920x1080", 8_000_000)); ok {
		t.Error("8Mbps must NOT match bandwidth_below=1M")
	}
}

func TestMatchFaultRule_UrlMatchModes(t *testing.T) {
	rc := RequestClass{Kind: "segment", RungIndex: 0, LadderSize: 5, Path: "/go-live/c/720p/segment_00017.m4s"}
	cases := []struct {
		mode, pattern string
		want          bool
	}{
		{"substring", "720p", true},
		{"substring", "1080p", false},
		{"basename", "segment_00017.m4s", true},
		{"basename", "720p", false},
		{"exact", "/go-live/c/720p/segment_00017.m4s", true},
		{"exact", "segment_00017.m4s", false},
		{"regex", `segment_\d+\.m4s$`, true},
		{"regex", `^audio/`, false},
	}
	for _, tc := range cases {
		rules := []any{rule("r", "500", map[string]any{"url_match": map[string]any{"mode": tc.mode, "patterns": []any{tc.pattern}}})}
		if _, ok := matchFaultRule(rules, rc); ok != tc.want {
			t.Errorf("url_match mode=%s pattern=%q: got %v, want %v", tc.mode, tc.pattern, ok, tc.want)
		}
	}
}

func TestMatchFaultRule_FirstMatchWins(t *testing.T) {
	// An ordered array: the audio rule is first, the catch-all second. An audio
	// request takes the audio rule; a video request falls through to catch-all.
	rules := []any{
		rule("audio", "503", map[string]any{"request_kind": []any{"audio_segment"}}),
		rule("all", "500", nil),
	}
	if mf, ok := matchFaultRule(rules, RequestClass{Kind: "audio_segment", RungIndex: -1, IsAudio: true}); !ok || mf.RuleID != "audio" {
		t.Errorf("audio should hit the audio rule first; got %+v %v", mf, ok)
	}
	if mf, ok := matchFaultRule(rules, video(2, "960x540", 2_200_000)); !ok || mf.RuleID != "all" {
		t.Errorf("video should fall through to catch-all; got %+v %v", mf, ok)
	}
}

func TestMatchFaultRule_NoneShadows(t *testing.T) {
	// A leading type:none rule scoped to rung 0 shadows the catch-all for rung 0
	// only — first-match-wins returns the none rule (caller = no fault), while
	// other rungs still fall through to the 500.
	rules := []any{
		rule("shield", "none", map[string]any{"variant": map[string]any{"rung_indexes": []any{float64(0)}}}),
		rule("all", "500", nil),
	}
	if mf, ok := matchFaultRule(rules, video(0, "640x360", 800_000)); !ok || mf.Type != "none" {
		t.Errorf("rung 0 should hit the none shield; got %+v %v", mf, ok)
	}
	if mf, ok := matchFaultRule(rules, video(3, "1280x720", 4_000_000)); !ok || mf.Type != "500" {
		t.Errorf("rung 3 should fall through to 500; got %+v %v", mf, ok)
	}
}

func TestMatchFaultRule_NoRulesNoMatch(t *testing.T) {
	if _, ok := matchFaultRule(nil, video(0, "", 0)); ok {
		t.Error("empty rule set must not match")
	}
}
