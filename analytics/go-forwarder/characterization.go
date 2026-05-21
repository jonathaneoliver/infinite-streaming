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

// Characterization-test report ingestion + retrieval. The Go test
// framework (tests/characterization/runner/report.go § WriteReport)
// POSTs one of these per test at end-of-sweep via the harness CLI
// (`harness post characterization <path>`); this handler unpacks the
// body, canonicalises IDs, strips redundant sample data, and INSERTs
// into infinite_streaming.characterization_runs.
//
// Two GETs alongside the POST:
//   - GET /api/v2/characterization-runs        list view (summary_json + structured cols, no report_json)
//   - GET /api/v2/characterization-runs/{run_id}/{test_name} detail view (includes report_json)
//
// See plan ~/.claude/plans/characterization-run-report-server-ingest.md.

// characterizationPost is the wire shape the CLI sends.
type characterizationPost struct {
	RunID    string          `json:"run_id"`
	TestName string          `json:"test_name"`
	Platform string          `json:"platform"`
	// Report is the full runner.Report struct serialised. Decoded as
	// RawMessage so we can re-serialise without round-tripping the
	// embedded times/floats through interface{}.
	Report json.RawMessage `json:"report"`
}

// minimumReportFields are the fields the server extracts from the
// embedded report blob for indexing / display. Everything else stays in
// the report_json blob for the dashboard to render.
type minimumReportFields struct {
	PlayerID  string    `json:"player_id"`
	PlayIDs   []string  `json:"play_ids"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at"`
	Summary   struct {
		TotalStalls int `json:"total_stalls"`
	} `json:"summary"`
}

func registerCharacterizationHandlers(mux *http.ServeMux, cfg config) {
	// Exact match: list + ingest live on the same path keyed by method.
	mux.HandleFunc("/api/v2/characterization-runs", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			handleCharacterizationPost(w, r, cfg)
		case http.MethodGet:
			handleCharacterizationList(w, r, cfg)
		default:
			w.Header().Set("Allow", "POST, GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
	// Detail view — trailing slash → path-prefix dispatch.
	mux.HandleFunc("/api/v2/characterization-runs/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handleCharacterizationDetail(w, r, cfg)
	})
}

func handleCharacterizationPost(w http.ResponseWriter, r *http.Request, cfg config) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
	if err != nil {
		http.Error(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var post characterizationPost
	if err := json.Unmarshal(body, &post); err != nil {
		http.Error(w, "decode body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if post.RunID == "" || post.TestName == "" || post.Platform == "" {
		http.Error(w, "run_id, test_name, platform are required", http.StatusBadRequest)
		return
	}
	if len(post.Report) == 0 {
		http.Error(w, "report payload is required", http.StatusBadRequest)
		return
	}

	// Extract the minimum indexing fields from the embedded report.
	var min minimumReportFields
	if err := json.Unmarshal(post.Report, &min); err != nil {
		http.Error(w, "decode report: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Canonicalise IDs — iOS emits uppercase UUIDs; CH stores lowercase.
	// Without this the dashboard's filters silently match zero rows.
	// See memory: case_sensitivity_ids.md.
	playerID := canonicalV2ID(min.PlayerID)
	playIDs := make([]string, 0, len(min.PlayIDs))
	for _, p := range min.PlayIDs {
		if c := canonicalV2ID(p); c != "" {
			playIDs = append(playIDs, c)
		}
	}

	// Strip the Samples field from report_json — per the plan, samples
	// are redundant with session_events and would bloat each row. Drop
	// in-place by re-marshalling without the field.
	report, summaryJSON, err := stripSamplesAndExtractSummary(post.Report)
	if err != nil {
		http.Error(w, "rewrite report: "+err.Error(), http.StatusBadRequest)
		return
	}

	passed := uint8(0)
	if min.Summary.TotalStalls == 0 {
		passed = 1
	}

	if err := insertCharacterizationRun(r.Context(), cfg, characterizationRow{
		RunID:       post.RunID,
		TestName:    post.TestName,
		Platform:    post.Platform,
		StartedAt:   min.StartedAt,
		EndedAt:     min.EndedAt,
		PlayerID:    playerID,
		PlayIDs:     playIDs,
		Passed:      passed,
		SummaryJSON: summaryJSON,
		ReportJSON:  report,
	}); err != nil {
		http.Error(w, "clickhouse insert: "+err.Error(), http.StatusBadGateway)
		return
	}

	writeJSON(w, map[string]any{
		"run_id":    post.RunID,
		"test_name": post.TestName,
		"platform":  post.Platform,
		"player_id": playerID,
		"play_ids":  playIDs,
		"passed":    passed == 1,
		"stored":    true,
	})
}

// stripSamplesAndExtractSummary returns the report JSON with the
// `samples` array replaced by an empty array, plus the marshalled
// `summary` object as a standalone JSON document. Operates on the
// decoded map so the rest of the report's structure is preserved
// byte-for-byte.
func stripSamplesAndExtractSummary(raw json.RawMessage) (report []byte, summary []byte, err error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, nil, err
	}
	if s, ok := m["summary"]; ok {
		summary = append(summary, s...)
	}
	m["samples"] = json.RawMessage("[]")
	out, err := json.Marshal(m)
	if err != nil {
		return nil, nil, err
	}
	return out, summary, nil
}

// characterizationRow mirrors the CH table columns.
type characterizationRow struct {
	RunID       string    `json:"run_id"`
	TestName    string    `json:"test_name"`
	Platform    string    `json:"platform"`
	StartedAt   time.Time `json:"started_at"`
	EndedAt     time.Time `json:"ended_at"`
	PlayerID    string    `json:"player_id"`
	PlayIDs     []string  `json:"play_ids"`
	Passed      uint8     `json:"passed"`
	SummaryJSON []byte    `json:"-"` // serialised separately
	ReportJSON  []byte    `json:"-"`
}

// insertCharacterizationRun writes one row using ClickHouse's
// JSONEachRow ingest format. Mirrors the shape used by the streaming
// ingest paths elsewhere in this binary but for a single-row sync POST.
func insertCharacterizationRun(ctx context.Context, cfg config, row characterizationRow) error {
	// Build the JSONEachRow line by hand so we can embed the already-
	// serialised summary_json / report_json without quoting them twice.
	line := map[string]any{
		"run_id":       row.RunID,
		"test_name":    row.TestName,
		"platform":     row.Platform,
		"started_at":   row.StartedAt.UTC().Format("2006-01-02 15:04:05.000"),
		"ended_at":     row.EndedAt.UTC().Format("2006-01-02 15:04:05.000"),
		"player_id":    row.PlayerID,
		"play_ids":     row.PlayIDs,
		"passed":       row.Passed,
		"summary_json": string(row.SummaryJSON),
		"report_json":  string(row.ReportJSON),
	}
	payload, err := json.Marshal(line)
	if err != nil {
		return err
	}

	u, err := url.Parse(cfg.clickhouseURL)
	if err != nil {
		return err
	}
	qs := u.Query()
	qs.Set("query", "INSERT INTO infinite_streaming.characterization_runs FORMAT JSONEachRow")
	u.RawQuery = qs.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), strings.NewReader(string(payload)+"\n"))
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
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("clickhouse %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return nil
}

func handleCharacterizationList(w http.ResponseWriter, r *http.Request, cfg config) {
	q := r.URL.Query()
	conditions := []string{"1=1"}
	params := map[string]string{}

	if v := strings.TrimSpace(q.Get("test_name")); v != "" {
		conditions = append(conditions, "test_name = {test_name:String}")
		params["test_name"] = v
	}
	if v := strings.TrimSpace(q.Get("platform")); v != "" {
		conditions = append(conditions, "platform = {platform:String}")
		params["platform"] = v
	}
	if v := strings.TrimSpace(q.Get("run_id")); v != "" {
		conditions = append(conditions, "run_id = {run_id:String}")
		params["run_id"] = v
	}
	if v := strings.TrimSpace(q.Get("player_id")); v != "" {
		conditions = append(conditions, "player_id = {player_id:String}")
		params["player_id"] = canonicalV2ID(v)
	}
	if v := strings.TrimSpace(q.Get("from")); v != "" {
		if ts, err := time.Parse(time.RFC3339, v); err == nil {
			conditions = append(conditions, "started_at >= {from:DateTime64(3)}")
			params["from"] = ts.UTC().Format("2006-01-02 15:04:05.000")
		}
	}
	if v := strings.TrimSpace(q.Get("to")); v != "" {
		if ts, err := time.Parse(time.RFC3339, v); err == nil {
			conditions = append(conditions, "started_at < {to:DateTime64(3)}")
			params["to"] = ts.UTC().Format("2006-01-02 15:04:05.000")
		}
	}

	limit := 50
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	params["limit"] = fmt.Sprintf("%d", limit)

	query := fmt.Sprintf(`
		SELECT
		    run_id,
		    test_name,
		    platform,
		    toString(started_at) AS started_at,
		    toString(ended_at)   AS ended_at,
		    player_id,
		    play_ids,
		    passed,
		    summary_json
		FROM infinite_streaming.characterization_runs
		WHERE %s
		ORDER BY started_at DESC
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

func handleCharacterizationDetail(w http.ResponseWriter, r *http.Request, cfg config) {
	path := strings.TrimPrefix(r.URL.Path, "/api/v2/characterization-runs/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		http.Error(w, "expected /api/v2/characterization-runs/{run_id}/{test_name}", http.StatusBadRequest)
		return
	}
	runID := parts[0]
	testName := parts[1]

	query := `
		SELECT
		    run_id,
		    test_name,
		    platform,
		    toString(started_at) AS started_at,
		    toString(ended_at)   AS ended_at,
		    player_id,
		    play_ids,
		    passed,
		    summary_json,
		    report_json
		FROM infinite_streaming.characterization_runs
		WHERE run_id = {run_id:String} AND test_name = {test_name:String}
		ORDER BY started_at DESC
		LIMIT 1
	`
	body, err := chQueryBytes(r.Context(), cfg, query, map[string]string{
		"run_id":    runID,
		"test_name": testName,
	})
	if err != nil {
		http.Error(w, "clickhouse query: "+err.Error(), http.StatusBadGateway)
		return
	}
	items, err := parseJSONEachRowItems(body)
	if err != nil {
		http.Error(w, "decode row: "+err.Error(), http.StatusBadGateway)
		return
	}
	if len(items) == 0 {
		http.Error(w, "no characterization_runs row matches", http.StatusNotFound)
		return
	}
	writeJSON(w, items[0])
}

// parseJSONEachRowItems splits a JSONEachRow response into per-row
// json.RawMessage entries. summary_json / report_json arrive as quoted
// strings; the dashboard parses them client-side on demand.
func parseJSONEachRowItems(body []byte) ([]json.RawMessage, error) {
	out := []json.RawMessage{}
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, json.RawMessage(line))
	}
	return out, nil
}
