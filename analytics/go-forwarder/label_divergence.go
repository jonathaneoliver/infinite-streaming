package main

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// Label divergence / "skew" (#783): for EVERY label, the magnitude of its
// strongest disproportion across dimensions — max significant lift over
// platform / content / app_version. A label uniform across dimensions scores
// ~1; one that spikes 4× on some platform scores ~4. Surfaced as a top-level
// column next to impact/anomaly so a strongly dimension-driven label is
// obvious at a glance (without opening each drill-down).
//
//	GET /api/v2/label_divergence?days=N&exclude_faulted=1
//	  → { items: [ {label, skew, top}, … ] }   (top = "dim=value" driving the skew)
//
// Everything runs on per-play CTEs (collapsed to ~one row per play), so the
// label×dimension cross-tab is cheap despite the raw row count.

func registerLabelDivergenceHandler(mux *http.ServeMux, cfg config) {
	mux.HandleFunc("/api/v2/label_divergence", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handleLabelDivergence(w, r, cfg)
	})
}

func handleLabelDivergence(w http.ResponseWriter, r *http.Request, cfg config) {
	days := 7
	if v := strings.TrimSpace(r.URL.Query().Get("days")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 90 {
			days = n
		}
	}
	params := map[string]string{"days": strconv.Itoa(days)}

	win := "ts >= now() - INTERVAL {days:UInt32} DAY AND play_id != ''"
	unionLabels := "SELECT play_id, labels FROM infinite_streaming.session_events WHERE " + win +
		" UNION ALL SELECT play_id, labels FROM infinite_streaming.network_requests WHERE " + win +
		" UNION ALL SELECT play_id, labels FROM infinite_streaming.control_events WHERE " + win

	faultFilterSE := ""
	faultedCTE := ""
	if v := strings.TrimSpace(r.URL.Query().Get("exclude_faulted")); v == "1" || v == "true" {
		// drop plays carrying any fault marker (consistent with label_frequency)
		faultedCTE = "faulted AS (SELECT DISTINCT play_id FROM (" + unionLabels +
			") ARRAY JOIN labels AS lab WHERE position(lab,'fault')>0),"
		faultFilterSE = "AND se.play_id NOT IN faulted"
	}

	// pl: per-play dimensions (collapsed). labset: per-play distinct labels
	// across all three tables. tests: the characterization mode (pyramid/ramp/…)
	// per play, from the testing=test_* tag on control_events — so a label's
	// skew can be driven by WHICH TEST ran, not only platform/content (#783).
	// Explode label × dimension and cross-tab into a significant lift per
	// (label,dim,value); take the max per label.
	query := `WITH ` + faultedCTE + `
		tests AS (
		    SELECT play_id, replaceOne(arrayFirst(x -> startsWith(x, 'testing=test_'), labs), 'testing=test_', '') AS test
		    FROM (
		        SELECT play_id, groupUniqArrayArray(labels) AS labs
		        FROM infinite_streaming.control_events
		        WHERE ` + win + `
		        GROUP BY play_id
		    )
		),
		pl AS (
		    SELECT se.play_id AS play_id,
		        any(multiIf(
		            se.device_model IN ('arm64','x86_64','i386') AND se.device_class='tablet','ipad-sim',
		            se.device_model IN ('arm64','x86_64','i386') AND se.device_class='phone','iphone-sim',
		            se.device_model IN ('arm64','x86_64','i386'),'sim-other',
		            se.device_class='tv','androidtv', se.device_class='tablet','ipad', se.device_class='phone','iphone',
		            se.device_class)) AS platform,
		        any(se.content_name) AS content,
		        any(se.app_version)  AS app_version,
		        any(t.test)          AS test
		    FROM infinite_streaming.session_events se
		    LEFT JOIN tests t ON t.play_id = se.play_id
		    WHERE se.ts >= now() - INTERVAL {days:UInt32} DAY AND se.play_id != '' ` + faultFilterSE + `
		    GROUP BY se.play_id
		),
		labset AS (
		    SELECT play_id, groupUniqArrayArray(labels) AS labs
		    FROM (` + unionLabels + `) GROUP BY play_id
		),
		total AS (SELECT count() AS t FROM pl),
		withl AS (
		    SELECT label, count() AS wl FROM (
		        SELECT pl.play_id AS pid, arrayJoin(ls.labs) AS label
		        FROM pl LEFT JOIN labset ls ON ls.play_id = pl.play_id
		    ) GROUP BY label
		),
		nv AS (
		    SELECT dim, value, count() AS n FROM (
		        SELECT play_id, dv.1 AS dim, dv.2 AS value
		        FROM pl ARRAY JOIN [('test',test),('platform',platform),('content',content),('app_version',app_version)] AS dv
		        WHERE dv.2 != ''
		    ) GROUP BY dim, value
		),
		cond AS (
		    SELECT label, dim, value, count() AS cnt FROM (
		        SELECT arrayJoin(ls.labs) AS label, dv.1 AS dim, dv.2 AS value
		        FROM pl LEFT JOIN labset ls ON ls.play_id = pl.play_id
		        ARRAY JOIN [('test',pl.test),('platform',pl.platform),('content',pl.content),('app_version',pl.app_version)] AS dv
		        WHERE dv.2 != ''
		    ) GROUP BY label, dim, value
		)
		SELECT
		    c.label AS label,
		    round(max(if(c.cnt >= 5 AND nv.n >= 20,
		        (c.cnt * (SELECT t FROM total)) / (nv.n * wl.wl), 1.0)), 2) AS skew,
		    argMax(concat(c.dim,'=',c.value), if(c.cnt >= 5 AND nv.n >= 20,
		        (c.cnt * (SELECT t FROM total)) / (nv.n * wl.wl), 1.0)) AS top
		FROM cond c
		INNER JOIN nv ON nv.dim = c.dim AND nv.value = c.value
		INNER JOIN withl wl ON wl.label = c.label
		GROUP BY c.label
	`

	body, err := chQueryBytes(r.Context(), cfg, query, params)
	if err != nil {
		http.Error(w, "clickhouse query: "+err.Error(), http.StatusBadGateway)
		return
	}
	raw, err := parseJSONEachRowItems(body)
	if err != nil {
		http.Error(w, "decode rows: "+err.Error(), http.StatusBadGateway)
		return
	}
	type item struct {
		Label string  `json:"label"`
		Skew  float64 `json:"skew"`
		Top   string  `json:"top"`
	}
	out := make([]item, 0, len(raw))
	for _, it := range raw {
		var rr struct {
			Label string      `json:"label"`
			Skew  json.Number `json:"skew"`
			Top   string      `json:"top"`
		}
		if json.Unmarshal(it, &rr) != nil {
			continue
		}
		sk, _ := rr.Skew.Float64()
		out = append(out, item{Label: rr.Label, Skew: math.Round(sk*100) / 100, Top: rr.Top})
	}
	writeJSON(w, map[string]any{"items": out})
}
