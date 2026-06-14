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
	Owner     string  `json:"owner"`      // runner holding a running claim
	ClaimedAt string  `json:"claimed_at"` // RFC3339; "" → epoch
	RawJSON   string  `json:"raw_json"`   // full serialized Experiment (recipe of record)
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
	// Concurrency-safe claim (#772 CH-master): atomically move the top-scored
	// eligible backlog experiment to running for one owner.
	mux.HandleFunc("/api/v2/sweep/claim", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handleSweepClaim(w, r, cfg)
	})
	// Soft-delete (tombstone) an experiment — list/claim ignore status='deleted'.
	mux.HandleFunc("/api/v2/sweep/delete", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handleSweepDelete(w, r, cfg)
	})
	// Control-plane scope (#772 dashboard buttons): GET the enable/disable map,
	// POST a single {dimension,value,enabled} toggle.
	mux.HandleFunc("/api/v2/sweep/scope", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			handleSweepScopeGet(w, r, cfg)
		case http.MethodPost:
			handleSweepScopeSet(w, r, cfg)
		default:
			w.Header().Set("Allow", "GET, POST")
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
		claimed := "1970-01-01 00:00:00.000"
		if e.ClaimedAt != "" {
			if ts, perr := time.Parse(time.RFC3339, e.ClaimedAt); perr == nil {
				claimed = ts.UTC().Format("2006-01-02 15:04:05.000")
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
			"owner":      e.Owner,
			"claimed_at": claimed,
			"raw_json":   e.RawJSON,
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
		    player_id, play_id, owner, raw_json, score,
		    toString(claimed_at) AS claimed_at,
		    toString(created_at) AS created_at,
		    toString(updated_at) AS updated_at
		FROM infinite_streaming.sweep_experiments FINAL
		WHERE %s AND status != 'deleted'
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

// sweepCols names the columns explicitly so the promote/tombstone INSERT…SELECT
// is matched by NAME, not position — robust to the physical order differing
// between a fresh schema and an ALTER-appended live table.
const sweepCols = `exp_id, class, status, kind, platform, protocol, mode, recipe, ` +
	`arm, group_id, parent, depth, why, why_text, verdict, player_id, play_id, ` +
	`owner, claimed_at, raw_json, score, created_at, updated_at`

// sweepRowSelect projects those columns in the same order, with status / owner /
// claimed_at / updated_at overridden (%s is the status literal expression).
const sweepRowSelect = `exp_id, class, %s AS status, kind, platform, protocol, mode, recipe, ` +
	`arm, group_id, parent, depth, why, why_text, verdict, player_id, play_id, ` +
	`{owner:String} AS owner, toDateTime64({now:String},3) AS claimed_at, raw_json, score, ` +
	`created_at, toDateTime64({now:String},3) AS updated_at`

// handleSweepClaim atomically moves the top-scored eligible backlog experiment to
// running for one owner. CH has no row lock, so the winner is arbitrated
// deterministically: append a claim row, settle, then argMin(owner) over
// (claim_ts, claim_token). The candidate query already excludes exp_ids present
// in sweep_claims, so contention only arises on two runners grabbing the same top
// candidate simultaneously — the loser just retries the (now smaller) backlog.
func handleSweepClaim(w http.ResponseWriter, r *http.Request, cfg config) {
	var post struct {
		Owner string `json:"owner"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&post); err != nil && err != io.EOF {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	owner := strings.TrimSpace(post.Owner)
	if owner == "" {
		http.Error(w, "owner is required", http.StatusBadRequest)
		return
	}
	ctx := r.Context()
	for try := 0; try < 6; try++ {
		candID, raw, err := sweepSelectCandidate(ctx, cfg)
		if err != nil {
			http.Error(w, "select candidate: "+err.Error(), http.StatusBadGateway)
			return
		}
		if candID == "" {
			writeJSON(w, map[string]any{"experiment": nil})
			return
		}
		nowT := time.Now().UTC()
		nowStr := nowT.Format("2006-01-02 15:04:05.000")
		token := fmt.Sprintf("%d-%s", nowT.UnixNano(), owner)
		claimLine, _ := json.Marshal(map[string]any{"exp_id": candID, "owner": owner, "claim_ts": nowStr, "claim_token": token})
		if err := chInsertJSONEachRow(ctx, cfg, "infinite_streaming.sweep_claims", string(claimLine)+"\n"); err != nil {
			http.Error(w, "insert claim: "+err.Error(), http.StatusBadGateway)
			return
		}
		time.Sleep(250 * time.Millisecond) // settle: let concurrent claims become visible
		winner, err := sweepClaimWinner(ctx, cfg, candID)
		if err != nil {
			http.Error(w, "resolve winner: "+err.Error(), http.StatusBadGateway)
			return
		}
		if winner == owner {
			if err := sweepSetStatus(ctx, cfg, candID, "'running'", owner, nowStr); err != nil {
				http.Error(w, "promote: "+err.Error(), http.StatusBadGateway)
				return
			}
			writeJSON(w, map[string]any{
				"experiment": json.RawMessage(raw),
				"owner":      owner,
				"claimed_at": nowT.Format(time.RFC3339),
			})
			return
		}
		// lost the race — candidate is now in sweep_claims and excluded next pass
	}
	writeJSON(w, map[string]any{"experiment": nil, "note": "claim contended; retry"})
}

// sweepSelectCandidate returns the top-scored backlog experiment that isn't
// already claimed and whose platform/protocol/class/mode aren't disabled in
// sweep_scope (the dashboard control plane).
func sweepSelectCandidate(ctx context.Context, cfg config) (id, raw string, err error) {
	q := `SELECT exp_id, raw_json FROM infinite_streaming.sweep_experiments FINAL
		WHERE status = 'backlog'
		  AND exp_id NOT IN (SELECT exp_id FROM infinite_streaming.sweep_claims)
		  AND platform NOT IN (SELECT value FROM infinite_streaming.sweep_scope FINAL WHERE dimension='platform' AND enabled=0)
		  AND protocol NOT IN (SELECT value FROM infinite_streaming.sweep_scope FINAL WHERE dimension='protocol' AND enabled=0)
		  AND class    NOT IN (SELECT value FROM infinite_streaming.sweep_scope FINAL WHERE dimension='class'    AND enabled=0)
		  AND mode     NOT IN (SELECT value FROM infinite_streaming.sweep_scope FINAL WHERE dimension='mode'     AND enabled=0)
		ORDER BY score DESC, exp_id
		LIMIT 1`
	body, err := chQueryBytes(ctx, cfg, q, nil)
	if err != nil {
		return "", "", err
	}
	items, err := parseJSONEachRowItems(body)
	if err != nil || len(items) == 0 {
		return "", "", err
	}
	var row struct {
		ExpID   string `json:"exp_id"`
		RawJSON string `json:"raw_json"`
	}
	if err := json.Unmarshal(items[0], &row); err != nil {
		return "", "", err
	}
	return row.ExpID, row.RawJSON, nil
}

func sweepClaimWinner(ctx context.Context, cfg config, expID string) (string, error) {
	q := `SELECT argMin(owner, (claim_ts, claim_token)) AS w FROM infinite_streaming.sweep_claims WHERE exp_id = {exp:String}`
	body, err := chQueryBytes(ctx, cfg, q, map[string]string{"exp": expID})
	if err != nil {
		return "", err
	}
	items, err := parseJSONEachRowItems(body)
	if err != nil || len(items) == 0 {
		return "", err
	}
	var row struct {
		W string `json:"w"`
	}
	if err := json.Unmarshal(items[0], &row); err != nil {
		return "", err
	}
	return row.W, nil
}

// sweepSetStatus re-projects an experiment row with a new status (+ owner /
// claimed_at / updated_at), preserving every other field via INSERT…SELECT.
func sweepSetStatus(ctx context.Context, cfg config, expID, statusExpr, owner, nowStr string) error {
	q := "INSERT INTO infinite_streaming.sweep_experiments (" + sweepCols + ") SELECT " +
		fmt.Sprintf(sweepRowSelect, statusExpr) +
		" FROM infinite_streaming.sweep_experiments FINAL WHERE exp_id = {exp:String}"
	_, err := chQueryBytes(ctx, cfg, q, map[string]string{"owner": owner, "now": nowStr, "exp": expID})
	return err
}

// handleSweepDelete tombstones an experiment (status='deleted'); list/claim skip it.
func handleSweepDelete(w http.ResponseWriter, r *http.Request, cfg config) {
	var post struct {
		ExpID string `json:"exp_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&post); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(post.ExpID) == "" {
		http.Error(w, "exp_id is required", http.StatusBadRequest)
		return
	}
	now := time.Now().UTC().Format("2006-01-02 15:04:05.000")
	if err := sweepSetStatus(r.Context(), cfg, post.ExpID, "'deleted'", "", now); err != nil {
		http.Error(w, "delete: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"deleted": post.ExpID})
}

func handleSweepScopeGet(w http.ResponseWriter, r *http.Request, cfg config) {
	q := `SELECT dimension, value, enabled FROM infinite_streaming.sweep_scope FINAL ORDER BY dimension, value`
	body, err := chQueryBytes(r.Context(), cfg, q, nil)
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

func handleSweepScopeSet(w http.ResponseWriter, r *http.Request, cfg config) {
	var post struct {
		Dimension string `json:"dimension"`
		Value     string `json:"value"`
		Enabled   *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&post); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if post.Dimension == "" || post.Value == "" || post.Enabled == nil {
		http.Error(w, "dimension, value, enabled are required", http.StatusBadRequest)
		return
	}
	enabled := 0
	if *post.Enabled {
		enabled = 1
	}
	now := time.Now().UTC().Format("2006-01-02 15:04:05.000")
	line, _ := json.Marshal(map[string]any{"dimension": post.Dimension, "value": post.Value, "enabled": enabled, "updated_at": now})
	if err := chInsertJSONEachRow(r.Context(), cfg, "infinite_streaming.sweep_scope", string(line)+"\n"); err != nil {
		http.Error(w, "clickhouse insert: "+err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"dimension": post.Dimension, "value": post.Value, "enabled": enabled})
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
