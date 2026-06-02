package main

import "testing"

// Covers terminalFrameForSession (#556) — the pure frame-builder behind
// the proxy's synthesized terminal session_events frame on inactive
// timeout. Verifies the playback_status mapping, reason carry, and the
// dedupe against a client's own session_end.
func TestTerminalFrameForSession(t *testing.T) {
	const reason = "inactive_timeout"

	t.Run("pre-first-frame timeout -> abandoned_start", func(t *testing.T) {
		frame, ok := terminalFrameForSession(SessionData{
			"player_metrics_last_event":                "playing",
			"player_metrics_playback_status":           "in_progress",
			"player_metrics_video_first_frame_time_ms": 0,
		}, reason)
		if !ok {
			t.Fatal("expected a frame")
		}
		if got := getString(frame, "player_metrics_last_event"); got != "session_end" {
			t.Fatalf("last_event = %q, want session_end", got)
		}
		if got := getString(frame, "player_metrics_playback_status"); got != "abandoned_start" {
			t.Fatalf("playback_status = %q, want abandoned_start", got)
		}
		if got := getString(frame, "player_metrics_playback_reason"); got != reason {
			t.Fatalf("playback_reason = %q, want %q", got, reason)
		}
	})

	t.Run("post-first-frame timeout -> user_stopped", func(t *testing.T) {
		frame, ok := terminalFrameForSession(SessionData{
			"player_metrics_last_event":                "heartbeat",
			"player_metrics_playback_status":           "in_progress",
			"player_metrics_video_first_frame_time_ms": 1200,
		}, reason)
		if !ok {
			t.Fatal("expected a frame")
		}
		if got := getString(frame, "player_metrics_playback_status"); got != "user_stopped" {
			t.Fatalf("playback_status = %q, want user_stopped", got)
		}
	})

	t.Run("client already set a terminal status -> respected, no reason override", func(t *testing.T) {
		frame, ok := terminalFrameForSession(SessionData{
			"player_metrics_last_event":                "error",
			"player_metrics_playback_status":           "mid_stream_failure",
			"player_metrics_playback_reason":           "decoder_init",
			"player_metrics_video_first_frame_time_ms": 800,
		}, reason)
		if !ok {
			t.Fatal("expected a frame")
		}
		if got := getString(frame, "player_metrics_playback_status"); got != "mid_stream_failure" {
			t.Fatalf("playback_status = %q, want mid_stream_failure (client's own)", got)
		}
		if got := getString(frame, "player_metrics_playback_reason"); got != "decoder_init" {
			t.Fatalf("playback_reason = %q, want decoder_init (not overwritten)", got)
		}
		if got := getString(frame, "player_metrics_last_event"); got != "session_end" {
			t.Fatalf("last_event = %q, want session_end", got)
		}
	})

	t.Run("client already sent session_end -> dedupe (no frame)", func(t *testing.T) {
		if _, ok := terminalFrameForSession(SessionData{
			"player_metrics_last_event":      "session_end",
			"player_metrics_playback_status": "completed",
		}, reason); ok {
			t.Fatal("expected no frame when client already ended cleanly")
		}
	})

	t.Run("nil session -> no frame", func(t *testing.T) {
		if _, ok := terminalFrameForSession(nil, reason); ok {
			t.Fatal("expected no frame for nil session")
		}
	})

	t.Run("does not mutate the source session", func(t *testing.T) {
		src := SessionData{
			"player_metrics_last_event":      "playing",
			"player_metrics_playback_status": "in_progress",
		}
		if _, ok := terminalFrameForSession(src, reason); !ok {
			t.Fatal("expected a frame")
		}
		if got := getString(src, "player_metrics_last_event"); got != "playing" {
			t.Fatalf("source mutated: last_event = %q, want playing", got)
		}
	})
}
