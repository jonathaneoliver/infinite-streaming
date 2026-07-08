package main

import "testing"

// #911: a network row must attribute to a player_id even on a cold session→player
// map, by preferring the player_id the proxy stamped on the entry (off the
// request's query param) and learning the mapping so later param-less
// sub-requests for the same session also attribute instead of landing empty.
func TestResolveNetworkPlayerID_PrefersEntryAndLearns(t *testing.T) {
	m := newSessionPlayerMap()
	const want = "859dc6dc-2ac3-40ad-8fae-ba22d4493b4a"

	// Cold map + entry carries an uppercase player_id (iOS) → canonicalised.
	if got := resolveNetworkPlayerID(m, "sess-2", "859DC6DC-2AC3-40AD-8FAE-BA22D4493B4A"); got != want {
		t.Fatalf("entry pid: got %q want %q", got, want)
	}
	// ...and the mapping is LEARNED from it.
	if lk := m.lookup("sess-2"); lk != want {
		t.Fatalf("map not learned from entry: got %q want %q", lk, want)
	}
	// A later sub-request for the SAME session with NO player_id attributes via
	// the now-warm map — the crux of #911 (previously landed empty).
	if got := resolveNetworkPlayerID(m, "sess-2", ""); got != want {
		t.Fatalf("param-less sub-request via learned map: got %q want %q", got, want)
	}
	// A different session that's never carried a player_id stays empty.
	if got := resolveNetworkPlayerID(m, "sess-cold", ""); got != "" {
		t.Fatalf("cold unknown session: got %q want empty", got)
	}
}
