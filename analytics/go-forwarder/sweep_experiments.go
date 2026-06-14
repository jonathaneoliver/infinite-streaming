package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Sweep-experiment queue ingestion + retrieval (issue #772). The local .sweep/
// store lives on the runner, not the deploy, so `harness sweep publish` POSTs a
// snapshot of every experiment here; this stores them in
// infinite_streaming.sweep_experiments (ReplacingMergeTree by exp_id, so each
// publish upserts the latest state) and serves them back to the dashboard's
// Sweep tab.
//
//	POST /api/v2/sweep/experiments   bulk upsert ({"experiments":[…]})
//	GET  /api/v2/sweep/experiments   list latest state (optional ?class= ?status= ?limit=)

// sweepExperimentRow is the wire + storage shape (one experiment's current state).
type sweepExperimentRow struct {
	ExpID     string  `json:"exp_id"`
	Class     string  `json:"class"`
	Status    string  `json:"status"`
	Kind      string  `json:"kind"`
	Platform  string  `json:"platform"`
	Protocol  string  `json:"protocol"`
	Mode      string  `json:"mode"`
	Recipe    string  `json:"recipe"`
	Arm       string  `json:"arm"`
	GroupID   string  `json:"group_id"`
	Parent    string  `json:"parent"`
	Depth     int     `json:"depth"`
	Why       string  `json:"why"`
	WhyText   string  `json:"why_text"`
	Verdict   string  `json:"verdict"`
	PlayerID  string  `json:"player_id"`
	PlayID    string  `json:"play_id"`
	Score     float64 `json:"score"`
	CreatedAt string  `json:"created_at"` // RFC3339; "" → now
}

type sweepPublishPost struct {
	Experiments []sweepExperimentRow `json:"experiments"`
}

func registerSweepHandlers(mux *http.ServeMux, cfg config) {
	mux.HandleFunc("/api/v2/sweep/experiments", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			handleSweepPublish(w, r, cfg)
		case http.MethodGet:
			handleSweepList(w, r, cfg)
		default:
			w.Header().Set("Allow", "POST, GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

func handleSweepPublish(w http.ResponseWriter, r *http.Request, cfg config) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 8<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var post sweepPublishPost
	if err := json.Unmarshal(body, &post); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(post.Experiments) == 0 {
		writeJSON(w, map[string]any{"stored": 0})
		return
	}
	now := time.Now().UTC().Format("2006-01-02 15:04:05.000")
	var lines strings.Builder
	for _, e := range post.Experiments {
		if e.ExpID == "" {
			continue
		}
		created := now
		if e.CreatedAt != "" {
			if ts, perr := time.Parse(time.RFC3339, e.CreatedAt); perr == nil {
				created = ts.UTC().Format("2006-01-02 15:04:05.000")
			}
		}
		line, merr := json.Marshal(map[string]any{
			"exp_id":     e.ExpID,
			"class":      strOrDefault(e.Class, "config"),
			"status":     e.Status,
			"kind":       e.Kind,
			"platform":   e.Platform,
			"protocol":   e.Protocol,
			"mode":       e.Mode,
			"recipe":     e.Recipe,
			"arm":        e.Arm,
			"group_id":   e.GroupID,
			"parent":     e.Parent,
			"depth":      e.Depth,
			"why":        e.Why,
			"why_text":   e.WhyText,
			"verdict":    e.Verdict,
			"player_id":  canonicalV2ID(e.PlayerID),
			"play_id":    canonicalV2ID(e.PlayID),
			"score":      e.Score,
			"created_at": created,
			"updated_at": now,
		})
		if merr != nil {
			http.Error(w, "marshal row: "+merr.Error(), http.StatusBadRequest)
			return
		}
		lines.Write(line)
		lines.WriteByte('\n')
	}

	if err := chInsertJSONEachRow(r.Context(), cfg, "infinite_streaming.sweep_experiments", lines.String()); err != nil {
		http.Error(w, "clickhouse insert: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"stored": len(post.Experiments)})
}

func handleSweepList(w http.ResponseWriter, r *http.Request, cfg config) {
	q := r.URL.Query()
	conditions := []string{"1=1"}
	params := map[string]string{}
	if v := strings.TrimSpace(q.Get("class")); v != "" {
		conditions = append(conditions, "class = {class:String}")
		params["class"] = v
	}
	if v := strings.TrimSpace(q.Get("status")); v != "" {
		conditions = append(conditions, "status = {status:String}")
		params["status"] = v
	}
	limit := 500
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 && n <= 2000 {
			limit = n
		}
	}
	params["limit"] = fmt.Sprintf("%d", limit)

	// FINAL collapses the ReplacingMergeTree to the latest row per exp_id.
	query := fmt.Sprintf(`
		SELECT
		    exp_id, class, status, kind, platform, protocol, mode, recipe,
		    arm, group_id, parent, depth, why, why_text, verdict,
		    player_id, play_id, score,
		    toString(created_at) AS created_at,
		    toString(updated_at) AS updated_at
		FROM infinite_streaming.sweep_experiments FINAL
		WHERE %s
		ORDER BY updated_at DESC
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

// chInsertJSONEachRow POSTs newline-delimited JSON rows into a CH table.
func chInsertJSONEachRow(ctx context.Context, cfg config, table, rows string) error {
	if strings.TrimSpace(rows) == "" {
		return nil
	}
	u, err := url.Parse(cfg.clickhouseURL)
	if err != nil {
		return err
	}
	qs := u.Query()
	qs.Set("query", "INSERT INTO "+table+" FORMAT JSONEachRow")
	u.RawQuery = qs.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(rows))
	if err != nil {
		return err
	}
	if cfg.chUser != "" {
		req.SetBasicAuth(cfg.chUser, cfg.chPassword)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("clickhouse %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

func strOrDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
