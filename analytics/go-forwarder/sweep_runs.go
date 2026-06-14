package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/analytics/go-forwarder/internal/plays"
)

// Sweep run HISTORY (#772 CH-master, group D). sweep_experiments is the queue
// (current state, keyed by exp_id — re-runs overwrite); sweep_runs is the
// append-only log of every RUN (keyed by play_id), so "everything we've ever
// run" survives. The harness POSTs here on analyze; this also marks the play
// `interesting` so its rich archive is kept 90 days (D1 retention).
//
//	POST /api/v2/sweep/runs   record one run ({play_id, exp_id, …, verdict, note})
//	GET  /api/v2/sweep/runs   list run history (?limit= ?exp_id=), newest first

type sweepRunRow struct {
	PlayID   string `json:"play_id"`
	ExpID    string `json:"exp_id"`
	Class    string `json:"class"`
	Kind     string `json:"kind"`
	Platform string `json:"platform"`
	Protocol string `json:"protocol"`
	Mode     string `json:"mode"`
	Recipe   string `json:"recipe"`
	Verdict  string `json:"verdict"`
	Why      string `json:"why"`
	WhyText  string `json:"why_text"`
	Note     string `json:"note"`
	PlayerID string `json:"player_id"`
}

func registerSweepRunsHandlers(mux *http.ServeMux, cfg config) {
	mux.HandleFunc("/api/v2/sweep/runs", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			handleSweepRunRecord(w, r, cfg)
		case http.MethodGet:
			handleSweepRunsList(w, r, cfg)
		default:
			w.Header().Set("Allow", "POST, GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

func handleSweepRunRecord(w http.ResponseWriter, r *http.Request, cfg config) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var run sweepRunRow
	if err := json.Unmarshal(body, &run); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if run.PlayID == "" {
		http.Error(w, "play_id is required", http.StatusBadRequest)
		return
	}
	now := time.Now().UTC().Format("2006-01-02 15:04:05.000")
	line, _ := json.Marshal(map[string]any{
		"play_id": canonicalV2ID(run.PlayID), "exp_id": run.ExpID,
		"class": strOrDefault(run.Class, "config"), "kind": run.Kind,
		"platform": run.Platform, "protocol": run.Protocol, "mode": run.Mode, "recipe": run.Recipe,
		"verdict": run.Verdict, "why": run.Why, "why_text": run.WhyText, "note": run.Note,
		"player_id": canonicalV2ID(run.PlayerID), "run_at": now,
	})
	if err := chInsertJSONEachRow(r.Context(), cfg, "infinite_streaming.sweep_runs", string(line)+"\n"); err != nil {
		http.Error(w, "clickhouse insert: "+err.Error(), http.StatusBadGateway)
		return
	}
	// D1 retention: keep the sweep's plays — mark the play `interesting` (90d) so
	// even clean baselines survive for regression comparison (force=false leaves
	// any existing `favourite` star alone).
	marked := false
	if be := playsBackend(cfg); be.ClickHouseURL != "" {
		if err := plays.SetPlayClassification(r.Context(), be, canonicalV2ID(run.PlayID), plays.ClassificationInteresting, false); err == nil {
			marked = true
		}
	}
	writeJSON(w, map[string]any{"recorded": run.PlayID, "marked_interesting": marked})
}

func handleSweepRunsList(w http.ResponseWriter, r *http.Request, cfg config) {
	q := r.URL.Query()
	conditions := []string{"1=1"}
	params := map[string]string{}
	if v := strings.TrimSpace(q.Get("exp_id")); v != "" {
		conditions = append(conditions, "exp_id = {exp_id:String}")
		params["exp_id"] = v
	}
	limit := 500
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 && n <= 5000 {
			limit = n
		}
	}
	params["limit"] = fmt.Sprintf("%d", limit)
	query := fmt.Sprintf(`
		SELECT play_id, exp_id, class, kind, platform, protocol, mode, recipe,
		       verdict, why, why_text, note, player_id, toString(run_at) AS run_at
		FROM infinite_streaming.sweep_runs
		WHERE %s
		ORDER BY run_at DESC
		LIMIT {limit:UInt32}
	`, strings.Join(conditions, " AND "))
	body, err := chQueryBytes(r.Context(), cfg, query, params)
	if err != nil {
		http.Error(w, "clickhouse query: "+err.Error(), http.StatusBadGateway)
		return
	}
	items, err := parseJSONEachRowItems(body)
	if err != nil {
		http.Error(w, "decode rows: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"items": items})
}
