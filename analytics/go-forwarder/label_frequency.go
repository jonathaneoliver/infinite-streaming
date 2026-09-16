package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// Label-frequency baseline (#772): how often each severity-tagged label appears
// across recent sessions, so triage can distinguish ambient labels (fire on
// most streams → likely threshold noise OR high-impact) from rare ones (a
// specific stream is broken). The dashboard derives an "anomaly" score
// (severity×rarity — what the sweep should chase) and an "impact" score
// (severity×frequency — what product should fix) from this.
//
//	GET /api/v2/label_frequency?days=7&exclude_faulted=1
//	  → { total, days, items: [ {label, severity, sessions, pct}, … ] }
//
// A "session" is one play_id. A label counts once per play (distinct), unioned
// across session_events + network_requests + control_events. exclude_faulted
// drops plays that had a fault rule armed, for a clean (non-injected) baseline.

func registerLabelFrequencyHandler(mux *http.ServeMux, cfg config) {
	mux.HandleFunc("/api/v2/label_frequency", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handleLabelFrequency(w, r, cfg)
	})
}

func handleLabelFrequency(w http.ResponseWriter, r *http.Request, cfg config) {
	days := 7
	if v := strings.TrimSpace(r.URL.Query().Get("days")); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 && n <= 90 {
			days = n
		}
	}
	params := map[string]string{"days": fmt.Sprintf("%d", days)}

	// Filter applied to the per-play CTE so both the per-label count and the
	// total use the same denominator. A faulted play is one whose label set
	// contains the fault-rule marker.
	faultFilter := ""
	if v := strings.TrimSpace(r.URL.Query().Get("exclude_faulted")); v == "1" || v == "true" {
		// A session is "faulted" if it carries ANY fault marker — not just the
		// *fault_rule_enabled control event (which only fires for faults armed
		// via the live PATCH API). config-on-connect-armed faults (the sweep's
		// bootstrap path) emit the per-request fault_* labels (fault_timeout /
		// fault_incomplete / fault_other, set when the proxy flags faulted=1)
		// but NO rule-armed marker, so match the whole `fault` family.
		faultFilter = "WHERE NOT arrayExists(x -> position(x, 'fault') > 0, labs)"
	}

	query := fmt.Sprintf(`
		WITH play_labels AS (
		    SELECT play_id, groupUniqArrayArray(labels) AS labs
		    FROM (
		        SELECT play_id, labels FROM infinite_streaming.session_events
		            WHERE ts >= now() - INTERVAL {days:UInt32} DAY AND play_id != ''
		        UNION ALL
		        SELECT play_id, labels FROM infinite_streaming.network_requests
		            WHERE ts >= now() - INTERVAL {days:UInt32} DAY AND play_id != ''
		        UNION ALL
		        SELECT play_id, labels FROM infinite_streaming.control_events
		            WHERE ts >= now() - INTERVAL {days:UInt32} DAY AND play_id != ''
		    )
		    GROUP BY play_id
		),
		filtered AS ( SELECT * FROM play_labels %s )
		SELECT
		    label,
		    splitByChar('=', label)[1] AS severity,
		    count() AS sessions,
		    (SELECT count() FROM filtered) AS total,
		    round(count() / (SELECT count() FROM filtered) * 100, 1) AS pct
		FROM filtered
		ARRAY JOIN labs AS label
		GROUP BY label
		ORDER BY sessions DESC
		LIMIT 500
	`, faultFilter)

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
	total := 0
	if len(items) > 0 {
		// Every row carries the same total; pull it off the first. ClickHouse
		// renders UInt64 as a quoted string in JSON, so strip quotes then parse.
		var first map[string]json.RawMessage
		if json.Unmarshal(items[0], &first) == nil {
			if n, err := strconv.Atoi(strings.Trim(string(first["total"]), `"`)); err == nil {
				total = n
			}
		}
	}
	writeJSON(w, map[string]any{"total": total, "days": days, "items": items})
}
