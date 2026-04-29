package main

import (
	"testing"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/go-proxy/internal/har"
)

// Tests for the play_id capture + filter plumbing introduced for
// issue #280.

func TestMostRecentPlayID_PrefersNewest(t *testing.T) {
	entries := []NetworkLogEntry{
		{Timestamp: time.Now().Add(-3 * time.Second), PlayID: "old"},
		{Timestamp: time.Now().Add(-2 * time.Second), PlayID: "middle"},
		{Timestamp: time.Now().Add(-1 * time.Second), PlayID: "newest"},
	}
	if got := mostRecentPlayID(entries); got != "newest" {
		t.Errorf("mostRecentPlayID = %q, want %q", got, "newest")
	}
}

func TestMostRecentPlayID_SkipsEmpty(t *testing.T) {
	// Older entries (pre-#280 players) may not carry play_id; the
	// resolver should walk past them to find the newest play_id that
	// actually exists.
	entries := []NetworkLogEntry{
		{Timestamp: time.Now().Add(-3 * time.Second), PlayID: "actual-play"},
		{Timestamp: time.Now().Add(-2 * time.Second), PlayID: ""},
		{Timestamp: time.Now().Add(-1 * time.Second), PlayID: ""},
	}
	if got := mostRecentPlayID(entries); got != "actual-play" {
		t.Errorf("mostRecentPlayID with trailing-empties = %q, want %q", got, "actual-play")
	}
}

func TestMostRecentPlayID_AllEmptyReturnsEmpty(t *testing.T) {
	entries := []NetworkLogEntry{
		{Timestamp: time.Now(), PlayID: ""},
		{Timestamp: time.Now(), PlayID: ""},
	}
	if got := mostRecentPlayID(entries); got != "" {
		t.Errorf("mostRecentPlayID with all-empty = %q, want empty string", got)
	}
}

func TestMostRecentPlayID_EmptySlice(t *testing.T) {
	if got := mostRecentPlayID(nil); got != "" {
		t.Errorf("mostRecentPlayID(nil) = %q, want empty", got)
	}
	if got := mostRecentPlayID([]NetworkLogEntry{}); got != "" {
		t.Errorf("mostRecentPlayID([]) = %q, want empty", got)
	}
}

// buildHARForSession's filter logic is exercised by reading through a
// mock NetworkLogRingBuffer. Set up an App with two plays in one
// session and verify each filter mode picks the right entries.
func TestBuildHARForSession_FiltersToMostRecentPlayByDefault(t *testing.T) {
	a := &App{
		networkLogs: map[string]*NetworkLogRingBuffer{},
	}
	rb := NewNetworkLogRingBuffer(100)
	now := time.Now()
	// Play A: 3 entries
	rb.Add(NetworkLogEntry{Timestamp: now.Add(-30 * time.Second), URL: "/a/1", PlayID: "play-a"})
	rb.Add(NetworkLogEntry{Timestamp: now.Add(-29 * time.Second), URL: "/a/2", PlayID: "play-a"})
	rb.Add(NetworkLogEntry{Timestamp: now.Add(-28 * time.Second), URL: "/a/3", PlayID: "play-a"})
	// Play B: 2 entries (more recent)
	rb.Add(NetworkLogEntry{Timestamp: now.Add(-5 * time.Second), URL: "/b/1", PlayID: "play-b"})
	rb.Add(NetworkLogEntry{Timestamp: now.Add(-4 * time.Second), URL: "/b/2", PlayID: "play-b"})
	a.networkLogs["sess-1"] = rb

	session := SessionData{"session_id": "sess-1", "player_id": "p1"}
	doc := a.buildHARForSession(session, nil, HARBuildFilter{})
	if got := len(doc.Log.Entries); got != 2 {
		t.Errorf("default filter (most-recent play) entry count = %d, want 2", got)
	}
	for _, e := range doc.Log.Entries {
		if e.Request.URL == "/a/1" || e.Request.URL == "/a/2" || e.Request.URL == "/a/3" {
			t.Errorf("default filter leaked play-a entries: %+v", e.Request.URL)
		}
	}
}

func TestBuildHARForSession_IncludeAllPlays(t *testing.T) {
	a := &App{networkLogs: map[string]*NetworkLogRingBuffer{}}
	rb := NewNetworkLogRingBuffer(100)
	now := time.Now()
	rb.Add(NetworkLogEntry{Timestamp: now.Add(-10 * time.Second), URL: "/a/1", PlayID: "play-a"})
	rb.Add(NetworkLogEntry{Timestamp: now.Add(-1 * time.Second), URL: "/b/1", PlayID: "play-b"})
	a.networkLogs["sess-1"] = rb
	session := SessionData{"session_id": "sess-1"}

	doc := a.buildHARForSession(session, nil, HARBuildFilter{IncludeAllPlays: true})
	if got := len(doc.Log.Entries); got != 2 {
		t.Errorf("IncludeAllPlays entry count = %d, want 2 (both plays)", got)
	}
}

func TestBuildHARForSession_ExplicitPlayIDOverride(t *testing.T) {
	a := &App{networkLogs: map[string]*NetworkLogRingBuffer{}}
	rb := NewNetworkLogRingBuffer(100)
	now := time.Now()
	rb.Add(NetworkLogEntry{Timestamp: now.Add(-10 * time.Second), URL: "/a/1", PlayID: "play-a"})
	rb.Add(NetworkLogEntry{Timestamp: now.Add(-9 * time.Second), URL: "/a/2", PlayID: "play-a"})
	rb.Add(NetworkLogEntry{Timestamp: now.Add(-1 * time.Second), URL: "/b/1", PlayID: "play-b"})
	a.networkLogs["sess-1"] = rb
	session := SessionData{"session_id": "sess-1"}

	// Default (no PlayID, no IncludeAllPlays) → most recent = play-b → 1 entry
	doc := a.buildHARForSession(session, nil, HARBuildFilter{})
	if got := len(doc.Log.Entries); got != 1 {
		t.Errorf("default = play-b entry count = %d, want 1", got)
	}

	// Explicit override to play-a → 2 entries
	doc = a.buildHARForSession(session, nil, HARBuildFilter{PlayID: "play-a"})
	if got := len(doc.Log.Entries); got != 2 {
		t.Errorf("explicit play-a entry count = %d, want 2", got)
	}
}

func TestBuildHARForSession_IncidentMetadataCarriesPlayID(t *testing.T) {
	a := &App{networkLogs: map[string]*NetworkLogRingBuffer{}}
	rb := NewNetworkLogRingBuffer(100)
	now := time.Now()
	rb.Add(NetworkLogEntry{Timestamp: now.Add(-2 * time.Second), URL: "/x", PlayID: "play-zz"})
	a.networkLogs["sess-1"] = rb
	session := SessionData{"session_id": "sess-1"}

	incident := &har.Incident{Reason: "manual"}
	a.buildHARForSession(session, incident, HARBuildFilter{})
	if incident.Metadata == nil {
		t.Fatalf("expected metadata populated by builder")
	}
	if got := incident.Metadata["play_id"]; got != "play-zz" {
		t.Errorf("metadata.play_id = %v, want play-zz", got)
	}
	if _, has := incident.Metadata["play_started_at"]; !has {
		t.Errorf("metadata.play_started_at missing: %+v", incident.Metadata)
	}
}

func TestBuildHARForSession_IncidentMetadataMarksIncludeAllPlays(t *testing.T) {
	a := &App{networkLogs: map[string]*NetworkLogRingBuffer{}}
	rb := NewNetworkLogRingBuffer(100)
	rb.Add(NetworkLogEntry{Timestamp: time.Now(), URL: "/x", PlayID: "p1"})
	rb.Add(NetworkLogEntry{Timestamp: time.Now(), URL: "/y", PlayID: "p2"})
	a.networkLogs["sess-1"] = rb
	session := SessionData{"session_id": "sess-1"}

	incident := &har.Incident{Reason: "forensic"}
	a.buildHARForSession(session, incident, HARBuildFilter{IncludeAllPlays: true})
	if v, _ := incident.Metadata["include_all_plays"].(bool); !v {
		t.Errorf("expected metadata.include_all_plays=true, got %+v", incident.Metadata)
	}
}
