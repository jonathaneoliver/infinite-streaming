package main

import (
	"testing"
)

// Tests for the recovery chain store + buildIncidentContext (issue #281).

func TestRecoveryChain_AppendAndSnapshot(t *testing.T) {
	// Reset the store between tests by using unique keys.
	playerID := "player-rc-1"
	playID := "play-1"
	chain := recordRecoveryReason(playerID, playID, "frozen")
	if len(chain) != 1 || chain[0] != "frozen" {
		t.Errorf("first reason: got %+v, want [frozen]", chain)
	}
	chain = recordRecoveryReason(playerID, playID, "user_retry")
	if len(chain) != 2 || chain[1] != "user_retry" {
		t.Errorf("second reason: got %+v, want [frozen, user_retry]", chain)
	}
	snap := recoveryChainSnapshot(playerID, playID)
	if len(snap) != 2 {
		t.Errorf("snapshot length = %d, want 2", len(snap))
	}
}

func TestRecoveryChain_DifferentPlayIDsDoNotShare(t *testing.T) {
	// A new play_id starts a fresh chain — old play's reasons stay
	// in their own bucket.
	playerID := "player-rc-2"
	recordRecoveryReason(playerID, "play-A", "frozen")
	recordRecoveryReason(playerID, "play-A", "user_retry")
	chainB := recordRecoveryReason(playerID, "play-B", "segment_stall")
	if len(chainB) != 1 || chainB[0] != "segment_stall" {
		t.Errorf("play-B chain shouldn't include play-A reasons: %+v", chainB)
	}
	chainA := recoveryChainSnapshot(playerID, "play-A")
	if len(chainA) != 2 {
		t.Errorf("play-A chain mutated by play-B activity: %+v", chainA)
	}
}

func TestRecoveryChain_EmptyKeyReturnsNil(t *testing.T) {
	if got := recordRecoveryReason("", "", "frozen"); got != nil {
		t.Errorf("empty (player, play) should return nil, got %+v", got)
	}
	if got := recoveryChainSnapshot("", ""); got != nil {
		t.Errorf("empty (player, play) snapshot should return nil, got %+v", got)
	}
}

func TestRecoveryChain_EmptyReasonNoOp(t *testing.T) {
	playerID := "player-rc-3"
	playID := "play-1"
	recordRecoveryReason(playerID, playID, "frozen")
	chain := recordRecoveryReason(playerID, playID, "")
	// Empty reason should not add to the chain.
	if len(chain) != 0 {
		t.Errorf("empty reason returned non-empty chain: %+v", chain)
	}
	snap := recoveryChainSnapshot(playerID, playID)
	if len(snap) != 1 {
		t.Errorf("chain mutated by empty reason: %+v", snap)
	}
}

func TestBuildIncidentContext_PopulatesDeviceAndStream(t *testing.T) {
	a := &App{networkLogs: map[string]*NetworkLogRingBuffer{}}
	session := SessionData{"session_id": "s1", "player_id": "p1"}
	req := &SnapshotRequest{
		Reason: "frozen",
		Source: "player",
		Device: &SnapshotDeviceMeta{
			Model: "iPad Pro", OSVersion: "iOS 18", AppVersion: "1.0", NetworkType: "wifi",
		},
		Stream: &SnapshotStreamMeta{
			ContentID: "movie", Protocol: "hls", Codec: "h264",
		},
	}
	ctx := a.buildIncidentContext(session, req, "p1")
	if ctx == nil || ctx.Device == nil || ctx.Stream == nil {
		t.Fatalf("expected device + stream populated, got %+v", ctx)
	}
	if ctx.Device.Model != "iPad Pro" || ctx.Stream.ContentID != "movie" {
		t.Errorf("device/stream copy wrong: device=%+v stream=%+v", ctx.Device, ctx.Stream)
	}
}

func TestBuildIncidentContext_ScenarioFromSession(t *testing.T) {
	a := &App{networkLogs: map[string]*NetworkLogRingBuffer{}}
	session := SessionData{
		"session_id":                    "s1",
		"player_id":                     "p1",
		"segment_failure_type":          "404",
		"segment_consecutive_failures":  3,
		"transport_failure_type":        "none",        // should be filtered out
		"nftables_bandwidth_mbps":       5,
		"unrelated_session_field":       "should-not-appear",
	}
	req := &SnapshotRequest{Reason: "manual", Source: "rest"}
	ctx := a.buildIncidentContext(session, req, "p1")
	if ctx.Scenario == nil {
		t.Fatalf("expected scenario block")
	}
	if ctx.Scenario.FaultSettings["segment_failure_type"] != "404" {
		t.Errorf("fault_settings missing segment_failure_type: %+v", ctx.Scenario.FaultSettings)
	}
	if _, leaked := ctx.Scenario.FaultSettings["transport_failure_type"]; leaked {
		t.Errorf("'none' values should be filtered: %+v", ctx.Scenario.FaultSettings)
	}
	if _, leaked := ctx.Scenario.FaultSettings["unrelated_session_field"]; leaked {
		t.Errorf("non-fault keys leaked: %+v", ctx.Scenario.FaultSettings)
	}
	if ctx.Scenario.NftablesShape["nftables_bandwidth_mbps"] != 5 {
		t.Errorf("nftables_shape missing bandwidth: %+v", ctx.Scenario.NftablesShape)
	}
}

func TestBuildIncidentContext_RecordsForPlayerSource(t *testing.T) {
	a := &App{networkLogs: map[string]*NetworkLogRingBuffer{}}
	session := SessionData{"session_id": "s1", "player_id": "p-rec-1"}
	req := &SnapshotRequest{
		Reason: "frozen",
		Source: "player",
		PlayID: "play-rec-1",
	}
	ctx := a.buildIncidentContext(session, req, "p-rec-1")
	if len(ctx.RecoveryChain) != 1 || ctx.RecoveryChain[0] != "frozen" {
		t.Errorf("first recovery_chain: got %+v, want [frozen]", ctx.RecoveryChain)
	}
	// A second snapshot appends.
	req2 := &SnapshotRequest{Reason: "user_retry", Source: "player", PlayID: "play-rec-1"}
	ctx2 := a.buildIncidentContext(session, req2, "p-rec-1")
	if len(ctx2.RecoveryChain) != 2 || ctx2.RecoveryChain[1] != "user_retry" {
		t.Errorf("second recovery_chain: got %+v, want [frozen, user_retry]", ctx2.RecoveryChain)
	}
}

func TestBuildIncidentContext_DashboardSourceObservesWithoutRecording(t *testing.T) {
	a := &App{networkLogs: map[string]*NetworkLogRingBuffer{}}
	session := SessionData{"session_id": "s1", "player_id": "p-obs-1"}
	// Pre-populate via a player snapshot.
	a.buildIncidentContext(session, &SnapshotRequest{
		Reason: "frozen", Source: "player", PlayID: "play-obs-1",
	}, "p-obs-1")
	// Dashboard snapshot reads but doesn't append "manual".
	ctx := a.buildIncidentContext(session, &SnapshotRequest{
		Reason: "manual", Source: "dashboard", PlayID: "play-obs-1",
	}, "p-obs-1")
	if len(ctx.RecoveryChain) != 1 || ctx.RecoveryChain[0] != "frozen" {
		t.Errorf("dashboard read should not record: %+v", ctx.RecoveryChain)
	}
}
