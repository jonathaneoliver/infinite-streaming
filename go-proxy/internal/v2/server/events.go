package server

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/go-proxy/internal/v2/oapigen"
)

// heartbeatInterval keeps idle SSE connections from being closed by
// load balancers / proxies. Spec lists ~15s.
const heartbeatInterval = 15 * time.Second

// EventSource owns the v2 SSE pipeline: subscribes to v1's session +
// network hubs, transforms events into v2 StreamEvent envelopes, and
// publishes them into the shared EventRing for fan-out.
//
// One Source is started per Server. The goroutines run for the lifetime
// of the process; Close() releases the v1 subscriptions cleanly.
//
// `player.updated` only fires when a player's `control_revision`
// changes — that is the canonical mutation cursor. v1 fields outside
// the v2 PlayerRecord projection (NAT-rebind on `origination_ip`, etc.)
// don't bump the revision and are silently absorbed; this matches the
// v2 spec's "control_revision is the source of truth" contract.
type EventSource struct {
	v1   V1Adapter
	ring *EventRing

	cancel context.CancelFunc
	done   chan struct{}

	// State carried across session-snapshot diffs to derive
	// player.created / player.updated / player.deleted, plus the
	// session_id → player_id index used by network-row lookups, plus
	// the per-player current play_id used to derive play.started /
	// play.ended on network-row arrival.
	stateMu        sync.Mutex
	prev           map[string]playerSnapshot // player_id → last seen
	sessionToPlayr map[string]string         // session_id → player_id
	currentPlay    map[string]string         // player_id → most recent play_id
}

// playerSnapshot tracks the per-player state used for diff detection.
//   - rev:   control_revision; flips on PATCH writes.
//   - hash:  fnv64a of the marshalled body; changes on ANY field
//            update including non-PATCH metric drains (RTT, buffer,
//            byte counters). A snapshot is "changed" when either
//            differs from the prior, ensuring chart-feeding fields
//            propagate even between PATCHes.
type playerSnapshot struct {
	rev  string
	hash uint64
}

// bodyHash returns a fast 64-bit fingerprint of a marshalled
// PlayerRecord body. fnv-1a is non-cryptographic but plenty for
// equality detection between consecutive snapshots of the same
// resource.
func bodyHash(body []byte) uint64 {
	const (
		offset64 uint64 = 14695981039346656037
		prime64  uint64 = 1099511628211
	)
	h := offset64
	for _, b := range body {
		h ^= uint64(b)
		h *= prime64
	}
	return h
}

// NewEventSource constructs and starts the source. Heartbeat ticker +
// session subscriber + network subscriber spin up immediately.
func NewEventSource(v1 V1Adapter, ring *EventRing) *EventSource {
	if ring == nil {
		ring = NewEventRing(0, 0)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &EventSource{
		v1:             v1,
		ring:           ring,
		cancel:         cancel,
		done:           make(chan struct{}),
		prev:           map[string]playerSnapshot{},
		sessionToPlayr: map[string]string{},
		currentPlay:    map[string]string{},
	}
	go s.run(ctx)
	return s
}

// Close terminates the source goroutines and unsubscribes from v1.
func (s *EventSource) Close() {
	if s == nil {
		return
	}
	s.cancel()
	<-s.done
}

func (s *EventSource) run(ctx context.Context) {
	defer close(s.done)

	if s.v1 == nil {
		// Nil adapter: only heartbeat fires (test mode).
		s.heartbeatLoop(ctx)
		return
	}

	sessionsCh, sessionsCancel := s.v1.SubscribeSessions(16)
	defer sessionsCancel()

	networkCh, networkCancel := s.v1.SubscribeNetwork(256)
	defer networkCancel()

	heartbeat := time.NewTicker(heartbeatInterval)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			s.publishHeartbeat()
		case snap, ok := <-sessionsCh:
			if !ok {
				return
			}
			s.handleSessionSnapshot(snap)
		case row, ok := <-networkCh:
			if !ok {
				return
			}
			s.handleNetworkRow(row)
		}
	}
}

// heartbeatLoop is the v1==nil fallback (e.g. unit tests with a stub
// Server). Just emits heartbeats so the channel doesn't go idle.
func (s *EventSource) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.publishHeartbeat()
		}
	}
}

// publishHeartbeat appends a `heartbeat` frame to the ring.
func (s *EventSource) publishHeartbeat() {
	body, _ := json.Marshal(map[string]any{
		"type": "heartbeat",
		"data": map[string]any{"ts": time.Now().UTC().Format(time.RFC3339Nano)},
	})
	s.ring.Publish("heartbeat", body)
}

// handleSessionSnapshot diffs the new snapshot against the previous
// one and emits player.created / player.updated / player.deleted
// frames. Only player_id-bearing sessions are considered.
//
// Side effect: updates the sessionToPlayr index used by lookupPlayerID
// to translate v1's session_id-keyed network events into v2's
// player_id-keyed envelope. Built here so both diffs run under the
// same lock and traversal.
func (s *EventSource) handleSessionSnapshot(snap SessionSnapshot) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()

	next := make(map[string]playerSnapshot, len(snap.Sessions))
	nextSession := make(map[string]string, len(snap.Sessions))
	for _, sess := range snap.Sessions {
		rec, ok := playerFromSession(sess)
		if !ok {
			continue
		}
		pid := rec.Id.String()
		// Marshal the typed record + the raw v1 session map under
		// `raw_session`. v2-compat consumers (the dashboards that
		// rely on the v1 chart engine) read RTT samples / byte
		// counters / etc. from raw_session because those fields
		// aren't on the typed shape. Cost: ~5KB per event per
		// session — fine for an internal harness.
		// DEBUG (keep these until issue resolved)
		_, hasWin := sess["_rttWindow"]
		rtt, _ := sess["client_rtt_ms"].(float64)
		log.Printf("V2_SSE_MARSHAL sess.rtt=%.2f sess.hasWin=%v sess.keys=%d", rtt, hasWin, len(sess))
		body, err := json.Marshal(struct {
			oapigen.PlayerRecord
			RawSession map[string]any `json:"raw_session,omitempty"`
		}{rec, sess})
		if err != nil {
			continue
		}
		// DEBUG: re-parse the body to confirm what actually ended up in JSON.
		var verify map[string]any
		_ = json.Unmarshal(body, &verify)
		vrs, _ := verify["raw_session"].(map[string]any)
		_, vHasWin := vrs["_rttWindow"]
		vRtt, _ := vrs["client_rtt_ms"].(float64)
		log.Printf("V2_SSE_VERIFY post-marshal raw_session: rtt=%.2f hasWin=%v keys=%d", vRtt, vHasWin, len(vrs))
		// Hash the marshalled body so we emit on ANY observable
		// field change, not just on control_revision (which only
		// flips on PATCH writes). RTT samples, buffer-depth updates,
		// byte counters etc. all need to reach the chart engine —
		// they're stamped onto the v1 session map by
		// normalizeSessionsForResponse but don't bump
		// control_revision. Without this, RTT/buffer charts stay
		// flat because no v2 event fires.
		h := bodyHash(body)
		next[pid] = playerSnapshot{rev: rec.ControlRevision, hash: h}
		if sid := getString(sess, "session_id"); sid != "" {
			nextSession[sid] = pid
		}

		prev, hadPrev := s.prev[pid]
		switch {
		case !hadPrev:
			s.publishPlayerEvent("player.created", body)
		case prev.rev != rec.ControlRevision || prev.hash != h:
			s.publishPlayerEvent("player.updated", body)
		}
	}

	for pid := range s.prev {
		if _, still := next[pid]; still {
			continue
		}
		// Fire play.ended for the player's active play (if any)
		// before player.deleted so subscribers maintain a clean
		// "this play ended" signal. Restore is best-effort — the
		// player session is gone, so kernel-state cleanup happens
		// via v1's port-cleanup helpers in DeletePlayer anyway.
		if activePlay := s.currentPlay[pid]; activePlay != "" {
			s.publishPlayEnded(pid, activePlay, "player_deleted")
			delete(s.currentPlay, pid)
		}
		// json.Marshal of a map with string keys + scalar values
		// can't fail in practice — ignore the error.
		body, _ := json.Marshal(map[string]any{
			"type": "player.deleted",
			"data": map[string]any{
				"player_id": pid,
				"reason":    "client_disconnect",
			},
		})
		s.ring.Publish("player.deleted", body)
	}

	s.prev = next
	s.sessionToPlayr = nextSession
}

// publishPlayerEvent writes a player.* frame whose data is the
// supplied pre-marshaled PlayerRecord JSON.
func (s *EventSource) publishPlayerEvent(typ string, recordJSON []byte) {
	envelope := struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}{
		Type: typ,
		Data: recordJSON,
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		return
	}
	s.ring.Publish(typ, body)
}

// handleNetworkRow emits one play.network.entry per v1 NetworkEvent
// and detects play_id rotations to emit play.started / play.ended.
//
// Translation:
//   - the v1 event arrives keyed on session_id (v1 internal); v2
//     re-keys on player_id by looking up the bound session.
//   - play_id is read from the entry itself (v1 already captures it).
//   - the entry payload is reused as the data block; oapigen's
//     NetworkLogEntry shape lines up with the v1 row's keys.
//
// Play detection:
//   - The first time we see a non-empty play_id for a player_id, emit
//     play.started.
//   - When the play_id changes to a new non-empty value, emit
//     play.ended for the old play_id and play.started for the new.
//   - We don't observe a "play ended without a successor" signal here
//     — that's emitted from handleSessionSnapshot when the player
//     itself goes away (player.deleted).
func (s *EventSource) handleNetworkRow(row NetworkLogRow) {
	playerID := s.lookupPlayerID(row.SessionID)
	if playerID == "" {
		return
	}
	playID := getString(row.Entry, "play_id")
	s.detectPlayRotation(playerID, playID)

	body, err := json.Marshal(map[string]any{
		"type": "play.network.entry",
		"data": map[string]any{
			"player_id": playerID,
			"play_id":   playID,
			"entry":     row.Entry,
		},
	})
	if err != nil {
		return
	}
	s.ring.Publish("play.network.entry", body)
}

// detectPlayRotation maintains the per-player current play_id and
// emits play.started / play.ended frames on rotation.
//
// On rotation, the old play's `_v2_play_overrides` snapshot is
// restored to the player's session — play-scope mutations
// auto-clear when the play ends, per DESIGN.md § Player vs play
// scope precedence.
func (s *EventSource) detectPlayRotation(playerID, newPlayID string) {
	if newPlayID == "" {
		return
	}
	s.stateMu.Lock()
	prevPlayID := s.currentPlay[playerID]
	if prevPlayID == newPlayID {
		s.stateMu.Unlock()
		return
	}
	s.currentPlay[playerID] = newPlayID
	s.stateMu.Unlock()

	if prevPlayID != "" {
		s.restorePlayScope(playerID, prevPlayID, "rotated")
		s.publishPlayEnded(playerID, prevPlayID, "rotated")
	}
	s.publishPlayStarted(playerID, newPlayID)
}

// restorePlayScope rolls back the player's session to its pre-play
// state for the named play_id. Reads `_v2_play_overrides[playID]`
// (a snapshot of the original field values when the play-scope
// PATCH was first applied), writes those values back, deletes the
// snapshot entry, and re-runs the kernel apply for the affected
// fields. No-op when no snapshot exists.
func (s *EventSource) restorePlayScope(playerID, playID, reason string) {
	if s.v1 == nil {
		return
	}
	var touchedShape, touchedTransport, touchedPattern bool
	_, _, _ = s.v1.MutatePlayer(playerID, func(sess map[string]any) error {
		overrides, _ := sess["_v2_play_overrides"].(map[string]any)
		if overrides == nil {
			return nil
		}
		snapshot, _ := overrides[playID].(map[string]any)
		if snapshot == nil {
			return nil
		}
		for k, v := range snapshot {
			if v == nil {
				delete(sess, k)
			} else {
				sess[k] = v
			}
			switch {
			case k == "_v2_shape_pattern":
				touchedPattern = true
			case k == "nftables_bandwidth_mbps", k == "nftables_delay_ms", k == "nftables_packet_loss":
				touchedShape = true
			case k == "transport_failure_type", k == "transport_fault_type",
				k == "transport_failure_frequency", k == "transport_consecutive_failures",
				k == "transport_failure_mode":
				touchedTransport = true
			}
		}
		delete(overrides, playID)
		if len(overrides) == 0 {
			delete(sess, "_v2_play_overrides")
		} else {
			sess["_v2_play_overrides"] = overrides
		}
		// Bump control_revision so SSE consumers see the rollback.
		sess["control_revision"] = newRevision()
		return nil
	})
	// Drive kernel apply outside the MutatePlayer fn so the lock
	// order matches PATCH (helpers may re-enter saveSessionList).
	if touchedPattern {
		if sess, ok := s.v1.SessionByPlayerID(playerID); ok {
			steps := extractPatternSteps(sess)
			delayMs := 0
			if f, ok := numericFloat(sess["nftables_delay_ms"]); ok {
				delayMs = int(f)
			}
			lossPct := 0.0
			if f, ok := numericFloat(sess["nftables_packet_loss"]); ok {
				lossPct = f
			}
			_ = s.v1.ApplyPatternToPlayer(playerID, steps, delayMs, lossPct)
		}
	}
	if touchedShape {
		_ = s.v1.ApplyShapeToPlayer(playerID)
	}
	if touchedTransport {
		if sess, ok := s.v1.SessionByPlayerID(playerID); ok {
			faultType, _ := sess["transport_failure_type"].(string)
			if faultType == "" {
				faultType = "none"
			}
			consec := 1
			if f, ok := numericFloat(sess["transport_consecutive_failures"]); ok && int(f) >= 1 {
				consec = int(f)
			}
			consecUnits, _ := sess["transport_consecutive_units"].(string)
			if consecUnits == "" {
				consecUnits = "seconds"
			}
			freq := 0
			if f, ok := numericFloat(sess["transport_failure_frequency"]); ok {
				freq = int(f)
			}
			_ = s.v1.ApplyTransportFaultToPlayer(playerID, faultType, consec, consecUnits, freq)
		}
	}
}

// PlayerForPlay resolves a play_id back to its player_id by reading
// the most-recent rotation state. Returns ("", false) when no active
// play matches.
func (s *EventSource) PlayerForPlay(playID string) (string, bool) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	for player, current := range s.currentPlay {
		if current == playID {
			return player, true
		}
	}
	return "", false
}

// publishPlayStarted writes a play.started frame.
func (s *EventSource) publishPlayStarted(playerID, playID string) {
	body, err := json.Marshal(map[string]any{
		"type": "play.started",
		"data": map[string]any{
			"player_id":  playerID,
			"play_id":    playID,
			"started_at": time.Now().UTC().Format(time.RFC3339Nano),
		},
	})
	if err != nil {
		return
	}
	s.ring.Publish("play.started", body)
}

// publishPlayEnded writes a play.ended frame.
func (s *EventSource) publishPlayEnded(playerID, playID, reason string) {
	body, err := json.Marshal(map[string]any{
		"type": "play.ended",
		"data": map[string]any{
			"player_id": playerID,
			"play_id":   playID,
			"ended_at":  time.Now().UTC().Format(time.RFC3339Nano),
			"reason":    reason,
		},
	})
	if err != nil {
		return
	}
	s.ring.Publish("play.ended", body)
}

// lookupPlayerID resolves a v1 session_id to the bound player_id via
// the index maintained by handleSessionSnapshot. Returns "" if the
// session isn't yet (or no longer) in the most-recent snapshot —
// expected for the race between the very first manifest request and
// the first session-broadcast tick.
func (s *EventSource) lookupPlayerID(sessionID string) string {
	if sessionID == "" {
		return ""
	}
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	return s.sessionToPlayr[sessionID]
}
