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
// signal that produces a non-empty `labels[]` entry in the
// /api/sessions response. Matches reclassifySession's SQL predicate
// exactly so picker chips, multi-select filter, and retention tier
// agree on what "interesting" means.
//
// Mirrors every per-row label-emitting condition in /api/sessions:
// last_event in the five chip types, non-empty player_error, any
// non-empty fault-type configuration, transport_fault_active, or a
// non-zero transfer-timeout counter. Agg-level labels (high_abr_churn,
// startup_failed, startup_slow) aren't checked here — those require
// session-wide aggregation and only need to land at session-end
// reclassification, where the SQL probe in reclassifySession sees the
// full (session, play).
func hasInterestingSignal(s map[string]interface{}) bool {
	switch strings.ToLower(strings.TrimSpace(getStr(s, "player_metrics_last_event"))) {
	case "user_marked", "frozen", "segment_stall", "restart", "error":
		return true
	}
	// player_error is a free-text field stamped by the player; the
	// proxy never sentinelizes it with 'none' the way it does for
	// failure_type columns, so empty-string is sufficient.
	if strings.TrimSpace(getStr(s, "player_metrics_player_error")) != "" {
		return true
	}
	for _, k := range []string{
		"master_manifest_failure_type",
		"manifest_failure_type",
		"segment_failure_type",
		"all_failure_type",
		"transport_failure_type",
	} {
		v := strings.TrimSpace(getStr(s, k))
		if v != "" && v != "none" {
			return true
		}
	}
	// Per-snapshot scalar interest signals available in the same map.
	// Kept in sync with the SQL labels[] in main.go so a "transport
	// flap only" or "timeout only" session does light up retention.
	if getU64(s, "transport_fault_active") > 0 {
		return true
	}
	if getU64(s, "fault_count_transfer_active_timeout") > 0 {
		return true
	}
	if getU64(s, "fault_count_transfer_idle_timeout") > 0 {
		return true
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
	// 1. Determine whether the session is interesting. The predicate
	// matches the per-row label-emitting conditions used by
	// /api/sessions to synthesize `labels[]`, so the picker's chip
	// row, the multi-select label filter, and the retention tier all
	// land on the same answer for those signals. Agg-level startup
	// labels (startup_failed, startup_slow) also drive retention via
	// the duration / state_playing / first_frame_s clauses below.
	// `high_abr_churn` alone is NOT promoted to retention-class
	// "interesting" — it's a busy-but-fine signal that surfaces as a
	// chip but doesn't justify 90-day storage on its own.
	probe := fmt.Sprintf(`
		WITH agg AS (
		  SELECT
		    countIf(last_event IN ('user_marked','frozen','segment_stall','restart','error')) AS evt_hits,
		    countIf(player_error != '') AS err_hits,
		    countIf((master_manifest_failure_type != '' AND master_manifest_failure_type != 'none')
		         OR (manifest_failure_type        != '' AND manifest_failure_type        != 'none')
		         OR (segment_failure_type         != '' AND segment_failure_type         != 'none')
		         OR (all_failure_type             != '' AND all_failure_type             != 'none')
		         OR (transport_failure_type       != '' AND transport_failure_type       != 'none')
		         OR transport_fault_active = 1) AS fault_hits,
		    max(fault_count_transfer_active_timeout) AS active_timeouts,
		    max(fault_count_transfer_idle_timeout)  AS idle_timeouts,
		    countIf(video_bitrate_mbps > 0) AS bitrate_samples,
		    countIf(player_state = 'playing') AS state_playing,
		    max(video_first_frame_time_s) AS first_frame_s,
		    dateDiff('second', min(ts), max(ts)) AS duration_s
		  FROM %s.%s
		  WHERE session_id = {session:String} AND play_id = {play:String}
		)
		SELECT if(
		  evt_hits > 0 OR err_hits > 0 OR fault_hits > 0
		    OR active_timeouts > 0 OR idle_timeouts > 0
		    OR (state_playing = 0 AND duration_s >= 5)
		    OR first_frame_s > 3.0,
		  1, 0) FROM agg
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
	// Two ALTER UPDATEs run sequentially on different tables. If the
	// first succeeds but the second fails the tables are momentarily
	// out of sync (snapshots.classification != network_requests
	// .classification for the same session+play). At our scale this
	// is harmless — at most one tier-boundary's worth of premature
	// or delayed eviction — but log distinguishably so an operator
	// can see WHICH table failed and re-run by hand if needed.
	updates := []struct {
		label string
		query string
	}{
		{"session_snapshots", fmt.Sprintf("ALTER TABLE %s.%s UPDATE classification = {cls:String} %s", cfg.chDatabase, cfg.chTable, whereSafe)},
		{"network_requests", fmt.Sprintf("ALTER TABLE %s.network_requests UPDATE classification = {cls:String} %s", cfg.chDatabase, whereSafe)},
	}
	for _, u := range updates {
		if _, err := chQueryBytes(ctx, cfg, u.query, params); err != nil {
			log.Printf("ALTER UPDATE classification table=%s sid=%s pid=%s value=%s err=%v",
				u.label, sessionID, playID, value, err)
			return fmt.Errorf("ALTER UPDATE classification on %s: %w", u.label, err)
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
