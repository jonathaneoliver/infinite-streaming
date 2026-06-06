package main

import "testing"

// Covers terminalFrameForSession (#556) — the pure frame-builder behind
// the proxy's synthesized terminal session_events frame on inactive
// timeout. Verifies the playback_status mapping, reason carry, and the
// dedupe against a client's own play-terminal event (play_end / legacy
// session_end, #554).
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
		if got := getString(frame, "player_metrics_last_event"); got != "play_end" {
			t.Fatalf("last_event = %q, want play_end", got)
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
		if got := getString(frame, "player_metrics_last_event"); got != "play_end" {
			t.Fatalf("last_event = %q, want play_end", got)
		}
	})

	t.Run("client already sent play_end -> dedupe (no frame)", func(t *testing.T) {
		if _, ok := terminalFrameForSession(SessionData{
			"player_metrics_last_event":      "play_end",
			"player_metrics_playback_status": "completed",
		}, reason); ok {
			t.Fatal("expected no frame when client already ended cleanly with play_end")
		}
	})

	t.Run("client already sent legacy session_end -> dedupe (no frame)", func(t *testing.T) {
		if _, ok := terminalFrameForSession(SessionData{
			"player_metrics_last_event":      "session_end",
			"player_metrics_playback_status": "completed",
		}, reason); ok {
			t.Fatal("expected no frame when client already ended cleanly with legacy session_end")
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

	// #634 — the frame must NOT reuse the snapshot's event_time verbatim:
	// that ties with the final heartbeat row in session_events and the
	// plays aggregate's argMax(playback_status, ts) flaps. The synthetic
	// terminal row is stamped 1ms after the snapshot.
	t.Run("event_time is bumped 1ms past the snapshot's", func(t *testing.T) {
		src := SessionData{
			"player_metrics_last_event":                "heartbeat",
			"player_metrics_playback_status":           "in_progress",
			"player_metrics_video_first_frame_time_ms": 1200,
			"player_metrics_event_time":                "2026-06-06T16:46:38.903Z",
		}
		frame, ok := terminalFrameForSession(src, reason)
		if !ok {
			t.Fatal("expected a frame")
		}
		got, gok := parseEventTime(getString(frame, "player_metrics_event_time"))
		if !gok {
			t.Fatalf("frame event_time unparseable: %q", getString(frame, "player_metrics_event_time"))
		}
		want, _ := parseEventTime("2026-06-06T16:46:38.904Z")
		if !got.Equal(want) {
			t.Fatalf("frame event_time = %v, want %v (snapshot + 1ms)", got, want)
		}
		// Source stays untouched — the bump is on the clone only.
		if got := getString(src, "player_metrics_event_time"); got != "2026-06-06T16:46:38.903Z" {
			t.Fatalf("source event_time mutated: %q", got)
		}
	})

	t.Run("missing event_time is left for downstream wall-clock stamping", func(t *testing.T) {
		frame, ok := terminalFrameForSession(SessionData{
			"player_metrics_last_event":                "heartbeat",
			"player_metrics_playback_status":           "in_progress",
			"player_metrics_video_first_frame_time_ms": 1200,
		}, reason)
		if !ok {
			t.Fatal("expected a frame")
		}
		if got := getString(frame, "player_metrics_event_time"); got != "" {
			t.Fatalf("event_time = %q, want empty (no fabricated stamp)", got)
		}
	})
}
