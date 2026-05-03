// Tiered retention for archived sessions (issue #342).
//
// Each row of (session_id, play_id) carries a `classification`
// LowCardinality(String) in both `session_snapshots` and
// `network_requests`. ClickHouse TTL evicts rows according to that
// value:
//
//   'other'        → 30 d  (default; the boring sessions)
//   'interesting'  → 90 d  (auto-classified at session-end if any of
//                            user_marked / frozen / segment_stall /
//                            restart / error / non-empty player_error
//                            / non-zero fault counters appear)
//   'favourite'    → forever (explicit user star — TTL has no clause
//                              that matches, so rows are never dropped)
//
// The forwarder mutates `classification` via ALTER UPDATE on three
// occasions: session-end (auto-classifier), star, unstar. ClickHouse
// mutations are async — they complete in seconds at our scale and
// settle long before the 30-day grace would matter.
//
// Per-row TTL evaluates each row's `ts` independently. For long
// sessions the front of the session may evict before the back over
// the session's wall-clock duration; #347 tracks the `session_end_ts`
// polish for strict all-or-nothing eviction once that becomes a real
// concern.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// hasInterestingSignal returns true if the snapshot map carries any
// of the bad-event signals that should auto-classify a session as
// 'interesting'. Mirrors the predicate inside reclassifySession but
// works on the in-memory map the SSE consumer hands us, before the
// row reaches ClickHouse.
func hasInterestingSignal(s map[string]interface{}) bool {
	switch strings.ToLower(strings.TrimSpace(getStr(s, "player_metrics_last_event"))) {
	case "user_marked", "frozen", "segment_stall", "restart", "error":
		return true
	}
	if getStr(s, "player_metrics_player_error") != "" {
		return true
	}
	for _, k := range []string{
		"master_manifest_consecutive_failures",
		"all_consecutive_failures",
		"manifest_consecutive_failures",
		"segment_consecutive_failures",
		"transport_consecutive_failures",
		"fault_count_transfer_active_timeout",
		"fault_count_transfer_idle_timeout",
	} {
		if v, ok := s[k]; ok {
			switch n := v.(type) {
			case float64:
				if n > 0 {
					return true
				}
			case int:
				if n > 0 {
					return true
				}
			case int64:
				if n > 0 {
					return true
				}
			}
		}
	}
	return false
}

// classifyQueue debounces auto-reclassification requests. A session
// snapshot carrying an interesting signal calls .mark(); a goroutine
// drains the queue every flushInterval and fires reclassifySession
// for each (session, play) pair, with `force=false` so existing
// 'favourite' classifications stay put.
//
// The queue is a set, not a list — repeated marks for the same key
// coalesce into one ALTER UPDATE per flush window.
type classifyQueue struct {
	mu      sync.Mutex
	pending map[classifyKey]struct{}
}

type classifyKey struct {
	sessionID string
	playID    string
}

func newClassifyQueue() *classifyQueue {
	return &classifyQueue{pending: make(map[classifyKey]struct{})}
}

func (q *classifyQueue) mark(sessionID, playID string) {
	if sessionID == "" {
		return
	}
	q.mu.Lock()
	q.pending[classifyKey{sessionID: sessionID, playID: playID}] = struct{}{}
	q.mu.Unlock()
}

func (q *classifyQueue) drain() []classifyKey {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]classifyKey, 0, len(q.pending))
	for k := range q.pending {
		out = append(out, k)
	}
	q.pending = make(map[classifyKey]struct{})
	return out
}

// runClassifyLoop drains the queue periodically and fires the auto-
// classifier ALTER UPDATEs. Designed to run as a single goroutine
// for the lifetime of the process.
func runClassifyLoop(ctx context.Context, cfg config, q *classifyQueue, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			keys := q.drain()
			for _, k := range keys {
				if err := reclassifySession(ctx, cfg, k.sessionID, k.playID, false); err != nil {
					log.Printf("auto-classify failed sid=%s pid=%s: %v", k.sessionID, k.playID, err)
				}
			}
		}
	}
}

// reclassifySession runs the auto-classifier predicate against
// `session_snapshots` and writes either 'interesting' or 'other' to
// every row of (session_id, play_id) in both data tables.
// `force` overrides any existing classification — used by the unstar
// path to reset a previously-favourited session back to whatever the
// auto-classifier would have said.
//
// `play_id` may be empty (legacy rows ingested before the proxy
// stamped play_id) — the WHERE clause matches the empty string in
// that case.
func reclassifySession(ctx context.Context, cfg config, sessionID, playID string, force bool) error {
	if sessionID == "" {
		return errors.New("session id required")
	}
	// 1. Determine whether the session is interesting. The signals
	// match the per-row "Flags" column on the picker so a user who
	// sees a 🚨 / ❄️ / ⛔ / ⏸ / 🔄 chip on a row knows that row will
	// auto-class as 'interesting' on session-end.
	probe := fmt.Sprintf(`
		SELECT count() FROM %s.%s
		WHERE session_id = {session:String} AND play_id = {play:String}
		  AND (
		    last_event IN ('user_marked', 'frozen', 'segment_stall', 'restart', 'error')
		    OR player_error != ''
		    OR master_manifest_consecutive_failures > 0
		    OR all_consecutive_failures > 0
		    OR manifest_consecutive_failures > 0
		    OR segment_consecutive_failures > 0
		    OR transport_consecutive_failures > 0
		    OR fault_count_transfer_active_timeout > 0
		    OR fault_count_transfer_idle_timeout > 0
		  )
		FORMAT TSV`, cfg.chDatabase, cfg.chTable)
	body, err := chQueryBytes(ctx, cfg, probe, map[string]string{
		"session": sessionID,
		"play":    playID,
	})
	if err != nil {
		return fmt.Errorf("auto-classifier probe: %w", err)
	}
	count := strings.TrimSpace(string(body))
	target := "other"
	if count != "" && count != "0" {
		target = "interesting"
	}
	return setClassification(ctx, cfg, sessionID, playID, target, force)
}

// setClassification writes `value` to every row of (session_id,
// play_id) in both data tables via parallel ALTER UPDATE statements.
// When `force=false`, doesn't overwrite an existing 'favourite' (the
// auto-classifier path shouldn't quietly demote a starred session).
// When `force=true`, overrides any existing value (used by star and
// unstar paths).
func setClassification(ctx context.Context, cfg config, sessionID, playID, value string, force bool) error {
	whereSafe := "WHERE session_id = {session:String} AND play_id = {play:String}"
	if !force {
		// Don't trample 'favourite' — those are user-starred and the
		// star always wins over the auto-classifier.
		whereSafe += " AND classification != 'favourite'"
	}
	params := map[string]string{
		"session": sessionID,
		"play":    playID,
		"cls":     value,
	}
	updates := []string{
		fmt.Sprintf("ALTER TABLE %s.%s UPDATE classification = {cls:String} %s", cfg.chDatabase, cfg.chTable, whereSafe),
		fmt.Sprintf("ALTER TABLE %s.network_requests UPDATE classification = {cls:String} %s", cfg.chDatabase, whereSafe),
	}
	for _, q := range updates {
		if _, err := chQueryBytes(ctx, cfg, q, params); err != nil {
			return fmt.Errorf("ALTER UPDATE classification: %w", err)
		}
	}
	return nil
}

// registerClassificationHandlers wires up the star / unstar / reclassify
// endpoints. URLs:
//
//   POST   /api/sessions/{session_id}/{play_id}/star
//   DELETE /api/sessions/{session_id}/{play_id}/star
//   POST   /api/sessions/{session_id}/{play_id}/reclassify
//
// `play_id` in the URL accepts the sentinel `—` for rows ingested
// before the proxy stamped play_id (the picker uses the same
// sentinel everywhere).
//
// We use net/http path parsing rather than gorilla/mux because the
// rest of the forwarder is mux-free; small enough to roll our own.
func registerClassificationHandlers(mux *http.ServeMux, cfg config) {
	mux.HandleFunc("/api/sessions/", func(w http.ResponseWriter, r *http.Request) {
		// Paths handled here:
		//   /api/sessions/{sid}/{pid}/star
		//   /api/sessions/{sid}/{pid}/reclassify
		// Anything else under /api/sessions/ is delegated to the
		// existing /api/sessions handler via 404.
		path := strings.TrimPrefix(r.URL.Path, "/api/sessions/")
		parts := strings.Split(path, "/")
		if len(parts) != 3 {
			http.NotFound(w, r)
			return
		}
		sessionID := parts[0]
		playID := parts[1]
		action := parts[2]
		if playID == "—" {
			playID = ""
		}
		if sessionID == "" {
			http.Error(w, "session id required", http.StatusBadRequest)
			return
		}
		switch action {
		case "star":
			handleStar(w, r, cfg, sessionID, playID)
		case "reclassify":
			handleReclassify(w, r, cfg, sessionID, playID)
		default:
			http.NotFound(w, r)
		}
	})
}

func handleStar(w http.ResponseWriter, r *http.Request, cfg config, sessionID, playID string) {
	switch r.Method {
	case http.MethodPost:
		if err := setClassification(r.Context(), cfg, sessionID, playID, "favourite", true); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]any{
			"session_id":     sessionID,
			"play_id":        playID,
			"classification": "favourite",
		})
	case http.MethodDelete:
		// Unstar = drop back to whatever the auto-classifier would
		// say. force=true so we override the current 'favourite'.
		if err := reclassifySession(r.Context(), cfg, sessionID, playID, true); err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, map[string]any{
			"session_id": sessionID,
			"play_id":    playID,
			"unstarred":  true,
		})
	default:
		w.Header().Set("Allow", "POST, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func handleReclassify(w http.ResponseWriter, r *http.Request, cfg config, sessionID, playID string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// force=false so a session that's currently 'favourite' stays
	// 'favourite' — explicit user intent always wins over the
	// auto-classifier.
	if err := reclassifySession(r.Context(), cfg, sessionID, playID, false); err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{
		"session_id":   sessionID,
		"play_id":      playID,
		"reclassified": true,
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
