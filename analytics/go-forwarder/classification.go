// Tiered retention for archived sessions (issue #342).
//
// Each row of (session_id, play_id) carries a `classification`
// LowCardinality(String) in `session_events`, `network_requests`,
// and `control_events`. ClickHouse TTL evicts rows according to
// that value:
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
// occasions: session-end (auto-classifier), star, unstar. The actual
// SQL lives in internal/plays/classification.go — this file holds the
// ingest-time queue + drain loop that batches auto-classification
// requests so a long session doesn't fire a mutation per row.

package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/analytics/go-forwarder/internal/plays"
)

// hasInterestingSignal returns true if the snapshot map carries any
// of the last_event values that drive the picker's "Flags" chips OR
// (#550 Phase 2) a non-zero terminal_error_code, OR an explicit
// `failed_*` / `abandoned_start` playback_status. Matches
// plays.ReclassifySession's SQL predicate exactly so chip + filter +
// retention tier agree on what "interesting" means.
func hasInterestingSignal(s map[string]interface{}) bool {
	switch strings.ToLower(strings.TrimSpace(getStr(s, "player_metrics_last_event"))) {
	case "user_marked", "frozen", "segment_stall", "restart", "error":
		return true
	}
	// #550 Phase 2: any terminal failure marks the session interesting.
	// `terminal_error_code != 0` only on rows where iOS classifier set
	// it; equally `playback_status starts with 'failed_'` or equals
	// 'abandoned_start' (EBVS) is a terminal-failure signal.
	if code := getI64(s, "player_metrics_terminal_error_code"); code != 0 {
		return true
	}
	status := strings.ToLower(strings.TrimSpace(getStr(s, "player_metrics_playback_status")))
	if strings.HasPrefix(status, "failed_") || status == "abandoned_start" {
		return true
	}
	return false
}

// classifyQueue debounces auto-reclassification requests. A session
// snapshot carrying an interesting signal calls .mark(); a goroutine
// drains the queue every flushInterval and fires the domain
// reclassifier for each (session, play) pair, with `force=false` so
// existing 'favourite' classifications stay put.
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
// classifier ALTER UPDATEs via internal/plays. Single goroutine for
// the lifetime of the process.
func runClassifyLoop(ctx context.Context, cfg config, q *classifyQueue, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	be := playsBackend(cfg)
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			keys := q.drain()
			for _, k := range keys {
				if err := plays.ReclassifySession(ctx, be, k.sessionID, k.playID, false); err != nil {
					log.Printf("auto-classify failed sid=%s pid=%s: %v", k.sessionID, k.playID, err)
				}
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
