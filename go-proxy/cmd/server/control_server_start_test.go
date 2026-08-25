package main

import (
	"testing"
	"time"
)

// TestControlHubServerStartReplay verifies the boot marker is replayed to a
// client that subscribes AFTER it was broadcast. The restart that produces the
// marker is the same event that drops the forwarder's SSE subscription, so the
// forwarder always reconnects after the marker is emitted — without sticky
// replay the server_start would be lost and the restart unattributable. #671.
func TestControlHubServerStartReplay(t *testing.T) {
	h := NewControlEventHub()

	// Restart just dropped every subscriber: broadcast into zero clients.
	h.BroadcastServerStart(ControlEvent{
		Ts:     time.Now().UTC(),
		Source: "auto",
		Event:  "server_start",
		Info:   "restored=2;skipped=1;baseline_mbps=0",
	})

	// Forwarder reconnects afterwards — it must still receive the marker.
	_, ch := h.AddClient(8)
	select {
	case ev := <-ch:
		if ev.Event != "server_start" {
			t.Fatalf("replayed event = %q, want server_start", ev.Event)
		}
		if ev.Source != "auto" {
			t.Fatalf("replayed source = %q, want auto", ev.Source)
		}
		if ev.Info != "restored=2;skipped=1;baseline_mbps=0" {
			t.Fatalf("replayed info = %q, want the boot counts", ev.Info)
		}
	case <-time.After(time.Second):
		t.Fatal("late subscriber did not receive replayed server_start marker")
	}
}

// TestControlHubServerStartLiveBroadcast verifies an already-connected client
// also receives the marker via the normal fanout (not only the replay path).
func TestControlHubServerStartLiveBroadcast(t *testing.T) {
	h := NewControlEventHub()
	_, ch := h.AddClient(8)
	h.BroadcastServerStart(ControlEvent{Ts: time.Now().UTC(), Source: "auto", Event: "server_start"})
	select {
	case ev := <-ch:
		if ev.Event != "server_start" {
			t.Fatalf("event = %q, want server_start", ev.Event)
		}
	case <-time.After(time.Second):
		t.Fatal("connected client did not receive server_start")
	}
}

// TestControlHubOrdinaryNotSticky verifies ordinary control events are NOT
// replayed to later subscribers — only the boot marker is sticky, so a
// reconnecting forwarder doesn't get a stale flood of past session events.
func TestControlHubOrdinaryNotSticky(t *testing.T) {
	h := NewControlEventHub()
	h.Broadcast(ControlEvent{Ts: time.Now().UTC(), Source: "proxy", Event: "session_start"})
	_, ch := h.AddClient(8)
	select {
	case ev := <-ch:
		t.Fatalf("late subscriber unexpectedly received %q; ordinary events must not be sticky", ev.Event)
	case <-time.After(100 * time.Millisecond):
		// expected: nothing replayed
	}
}
