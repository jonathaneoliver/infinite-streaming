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
// of the 5 last_event values that drive the picker's "Flags" chips.
// Matches reclassifySession's SQL predicate exactly so chip + filter
// + retention tier agree on what "interesting" means.
func hasInterestingSignal(s map[string]interface{}) bool {
	switch strings.ToLower(strings.TrimSpace(getStr(s, "player_metrics_last_event"))) {
	case "user_marked", "frozen", "segment_stall", "restart", "error":
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
	// 1. Determine whether the session is interesting. The signals
	// match the per-row "Flags" column on the picker exactly — any of
	// the 5 chip types appearing as a last_event value. Keeping the
	// predicate narrow (just last_event) means the filter chip on
	// the picker, the auto-classifier, and the retention tier all
	// agree on what "interesting" means.
	probe := fmt.Sprintf(`
		SELECT count() FROM %s.%s
		WHERE session_id = {session:String} AND play_id = {play:String}
		  AND last_event IN ('user_marked', 'frozen', 'segment_stall', 'restart', 'error')
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

// reclassifyPlay is the play-scoped twin of reclassifySession: same
// auto-classifier predicate, but the WHERE clause keys on play_id
// alone (no session_id). Used by PATCH /api/v2/plays/{id}, where the
// caller has a play_id but no session_id.
func reclassifyPlay(ctx context.Context, cfg config, playID string, force bool) error {
	if playID == "" {
		return errors.New("play id required")
	}
	probe := fmt.Sprintf(`
		SELECT count() FROM %s.%s
		WHERE play_id = {play:String}
		  AND last_event IN ('user_marked', 'frozen', 'segment_stall', 'restart', 'error')
		FORMAT TSV`, cfg.chDatabase, cfg.chTable)
	body, err := chQueryBytes(ctx, cfg, probe, map[string]string{"play": playID})
	if err != nil {
		return fmt.Errorf("auto-classifier probe: %w", err)
	}
	target := "other"
	if c := strings.TrimSpace(string(body)); c != "" && c != "0" {
		target = "interesting"
	}
	return setPlayClassification(ctx, cfg, playID, target, force)
}

// setPlayClassification is the play-scoped twin of setClassification.
// Same three-table ALTER UPDATE pattern but keyed on play_id only, so
// the v2 PATCH endpoint doesn't have to round-trip the session_id.
//
// `SETTINGS mutations_sync = 2` makes the ALTER UPDATE wait for the
// mutation to be applied before returning. Without this CH's default
// (mutations_sync = 0) returns immediately and the SELECT v2PlayPatchHandler
// runs right after still reads the pre-mutation classification — the PATCH
// response then carries a stale value and the dashboard's optimistic flip
// visibly reverts before the next 5s refetch reconciles. Per-play volumes
// are tiny so the wait is sub-second in practice.
func setPlayClassification(ctx context.Context, cfg config, playID, value string, force bool) error {
	if playID == "" {
		return errors.New("play id required")
	}
	whereSafe := "WHERE play_id = {play:String}"
	if !force {
		whereSafe += " AND classification != 'favourite'"
	}
	params := map[string]string{"play": playID, "cls": value}
	const syncSuffix = " SETTINGS mutations_sync = 2"
	updates := []struct {
		label string
		query string
	}{
		{"session_events", fmt.Sprintf("ALTER TABLE %s.%s UPDATE classification = {cls:String} %s"+syncSuffix, cfg.chDatabase, cfg.chTable, whereSafe)},
		{"network_requests", fmt.Sprintf("ALTER TABLE %s.network_requests UPDATE classification = {cls:String} %s"+syncSuffix, cfg.chDatabase, whereSafe)},
		{"control_events", fmt.Sprintf("ALTER TABLE %s.control_events UPDATE classification = {cls:String} %s"+syncSuffix, cfg.chDatabase, whereSafe)},
	}
	for _, u := range updates {
		if _, err := chQueryBytes(ctx, cfg, u.query, params); err != nil {
			log.Printf("ALTER UPDATE classification table=%s pid=%s value=%s err=%v",
				u.label, playID, value, err)
			return fmt.Errorf("ALTER UPDATE classification on %s: %w", u.label, err)
		}
	}
	return nil
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
		{"session_events", fmt.Sprintf("ALTER TABLE %s.%s UPDATE classification = {cls:String} %s", cfg.chDatabase, cfg.chTable, whereSafe)},
		{"network_requests", fmt.Sprintf("ALTER TABLE %s.network_requests UPDATE classification = {cls:String} %s", cfg.chDatabase, whereSafe)},
		// Mirror onto control_events so the proxy/harness action log
		// ages out on the same TTL tier as the parent session.
		{"control_events", fmt.Sprintf("ALTER TABLE %s.control_events UPDATE classification = {cls:String} %s", cfg.chDatabase, whereSafe)},
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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
