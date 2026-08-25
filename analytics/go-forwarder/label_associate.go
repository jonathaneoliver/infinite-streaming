package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
)

// Label↔dimension association (#783): Splunk-`associate`-style conditional
// probability lift. For a target label L, measure how disproportionately it
// occurs given each dimension value — answering "is this aberrant because of
// the content / app_version / device?" from the data alone (the observational
// complement to the sweep's experimental isolation).
//
//	GET /api/v2/label_associate?label=<L>&days=N&exclude_faulted=1
//	  → { label, baseline_pct, total, items: [ {dim,value,n,with_l,pct,lift,significant}, … ] }
//
// lift = P(L|dim=value) / P(L). A session = one play_id; a play "has L" if any
// of its rows (session_events + network_requests + control_events) carries L.
// Dimension values come from session_events columns (constant per play).

// associateDims are the per-play dimensions we associate against. The first
// group are session_events columns (no `platform` column — device_class /
// device_model proxy it, #783). The sweep_* group is joined from
// sweep_experiments (keyed by play_id) so labels can be associated against the
// KNOWN experiment parameters — pattern/recipe, mode, class. Because the sweep
// SET those, that association is near-causal, not just observational (the
// pattern idea). They're empty (→ filtered) for non-sweep plays.
var associateDims = []struct{ label, col string }{
	{"test", "test"}, // characterization mode (pyramid/rampup/…) from the testing=test_* tag on control_events
	{"content", "content_name"},
	{"app_version", "app_version"},
	{"platform", "platform"}, // derived: iphone / iphone-sim / ipad / ipad-sim / androidtv
	{"device_model", "device_model"},
	{"device_kind", "device_kind"}, // simulator vs real (derived from device_model)
	{"sweep_recipe", "sweep_recipe"},
	{"sweep_mode", "sweep_mode"},
	{"sweep_class", "sweep_class"},
}

func registerLabelAssociateHandler(mux *http.ServeMux, cfg config) {
	mux.HandleFunc("/api/v2/label_associate", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handleLabelAssociate(w, r, cfg)
	})
}

func handleLabelAssociate(w http.ResponseWriter, r *http.Request, cfg config) {
	label := strings.TrimSpace(r.URL.Query().Get("label"))
	if label == "" {
		http.Error(w, "label param is required", http.StatusBadRequest)
		return
	}
	days := 7
	if v := strings.TrimSpace(r.URL.Query().Get("days")); v != "" {
		var n int
		if _, err := fmt.Sscanf(v, "%d", &n); err == nil && n > 0 && n <= 90 {
			days = n
		}
	}
	params := map[string]string{"days": fmt.Sprintf("%d", days), "label": label}

	excludeFaulted := false
	if v := strings.TrimSpace(r.URL.Query().Get("exclude_faulted")); v == "1" || v == "true" {
		excludeFaulted = true
	}

	// within=<dim>:<value> stratifies the population to one dimension value, so
	// the lift of the OTHER dimensions is computed WITHIN that stratum —
	// holding e.g. recipe=pyramid fixed removes the test-mix confound when
	// comparing platforms (#783). The held dimension is dropped from the output.
	withinDim, withinVal, withinCol := "", "", ""
	if v := strings.TrimSpace(r.URL.Query().Get("within")); v != "" {
		if i := strings.Index(v, ":"); i > 0 {
			d, val := v[:i], v[i+1:]
			for _, dim := range associateDims {
				if dim.label == d {
					withinDim, withinVal, withinCol = d, val, dim.col
					break
				}
			}
			if withinCol == "" {
				http.Error(w, "within: unknown dimension "+d, http.StatusBadRequest)
				return
			}
		}
	}

	// Per-play CTE: dimension values (constant per play) + whether the play
	// carries the target label. `labeled` and `faulted` are play-id sets built
	// by unioning labels across all three source tables (like label_frequency).
	win := "ts >= now() - INTERVAL {days:UInt32} DAY AND play_id != ''"
	unionLabels := fmt.Sprintf(`
		SELECT play_id, labels FROM infinite_streaming.session_events WHERE %[1]s
		UNION ALL SELECT play_id, labels FROM infinite_streaming.network_requests WHERE %[1]s
		UNION ALL SELECT play_id, labels FROM infinite_streaming.control_events WHERE %[1]s`, win)

	faultFilter := ""
	faultedCTE := ""
	if excludeFaulted {
		faultedCTE = fmt.Sprintf(`, faulted AS (
			SELECT DISTINCT play_id FROM (%s) ARRAY JOIN labels AS lab WHERE position(lab, 'fault') > 0
		)`, unionLabels)
		faultFilter = "AND se.play_id NOT IN faulted"
	}

	// One UNION-ALL arm per dimension over the shared per_play set. The held
	// (within) dimension is dropped — associating against a fixed value is moot.
	var arms []string
	for _, d := range associateDims {
		if d.label == withinDim {
			continue
		}
		arms = append(arms, fmt.Sprintf(
			"SELECT '%s' AS dim, toString(%s) AS value, is_labeled FROM per_play", d.label, d.col))
	}

	// HAVING on the per_play group restricts to the within stratum.
	having := ""
	if withinCol != "" {
		having = "HAVING " + withinCol + " = {within_val:String}"
		params["within_val"] = withinVal
	}

	query := fmt.Sprintf(`
		WITH labeled AS (
		    SELECT DISTINCT play_id FROM (%s) ARRAY JOIN labels AS lab WHERE lab = {label:String}
		)%s,
		tests AS (
		    SELECT play_id, replaceOne(arrayFirst(x -> startsWith(x, 'testing=test_'), labs), 'testing=test_', '') AS test
		    FROM (
		        SELECT play_id, groupUniqArrayArray(labels) AS labs
		        FROM infinite_streaming.control_events
		        WHERE ts >= now() - INTERVAL {days:UInt32} DAY AND play_id != ''
		        GROUP BY play_id
		    )
		),
		per_play AS (
		    SELECT se.play_id AS play_id,
		        any(t.test) AS test,
		        any(se.content_name) AS content_name,
		        any(se.app_version)  AS app_version,
		        any(se.device_model) AS device_model,
		        any(se.device_class) AS device_class,
		        -- iOS/tvOS simulators report the host CPU arch (arm64/x86_64) as
		        -- device_model instead of a real hardware id (iPhone15,4 etc.) (#783).
		        any(if(se.device_model IN ('arm64','x86_64','i386'), 'simulator', 'real')) AS device_kind,
		        -- Derived platform: device_class (phone/tablet/tv) × sim-vs-real.
		        -- iPhone-sim=phone+arm64, iPad-sim=tablet+arm64, real iPhone=phone+iPhone*,
		        -- real iPad=tablet+iPad, Android TV=tv+Google TV Streamer.
		        any(multiIf(
		            se.device_model IN ('arm64','x86_64','i386') AND se.device_class = 'tablet', 'ipad-sim',
		            se.device_model IN ('arm64','x86_64','i386') AND se.device_class = 'phone',  'iphone-sim',
		            se.device_model IN ('arm64','x86_64','i386'), 'sim-other',
		            se.device_class = 'tv',     'androidtv',
		            se.device_class = 'tablet', 'ipad',
		            se.device_class = 'phone',  'iphone',
		            se.device_class)) AS platform,
		        any(sx.recipe) AS sweep_recipe,
		        any(sx.mode)   AS sweep_mode,
		        any(sx.class)  AS sweep_class,
		        (se.play_id IN labeled) AS is_labeled
		    FROM infinite_streaming.session_events se
		    LEFT JOIN (SELECT play_id, recipe, mode, class FROM infinite_streaming.sweep_experiments FINAL) sx
		      ON sx.play_id = se.play_id
		    LEFT JOIN tests t ON t.play_id = se.play_id
		    WHERE se.ts >= now() - INTERVAL {days:UInt32} DAY AND se.play_id != '' %s
		    GROUP BY se.play_id
		    %s
		)
		SELECT dim, value, count() AS n, countIf(is_labeled) AS with_l
		FROM (%s)
		WHERE value != ''
		GROUP BY dim, value
		HAVING n > 0
		ORDER BY dim, n DESC
	`, unionLabels, faultedCTE, faultFilter, having, strings.Join(arms, " UNION ALL "))

	body, err := chQueryBytes(r.Context(), cfg, query, params)
	if err != nil {
		http.Error(w, "clickhouse query: "+err.Error(), http.StatusBadGateway)
		return
	}
	rawItems, err := parseJSONEachRowItems(body)
	if err != nil {
		http.Error(w, "decode rows: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Baseline P(L) = total labeled plays / total plays. Each dimension's rows
	// sum to the same totals (every play has one value per dim), so derive from
	// the first dimension we see.
	type rawRow struct {
		Dim   string      `json:"dim"`
		Value string      `json:"value"`
		N     json.Number `json:"n"`
		WithL json.Number `json:"with_l"`
	}
	rows := make([]rawRow, 0, len(rawItems))
	dimTotN := map[string]int{}
	dimTotL := map[string]int{}
	for _, it := range rawItems {
		var rr rawRow
		if json.Unmarshal(it, &rr) != nil {
			continue
		}
		rows = append(rows, rr)
		n, _ := strconv.Atoi(rr.N.String())
		l, _ := strconv.Atoi(rr.WithL.String())
		dimTotN[rr.Dim] += n
		dimTotL[rr.Dim] += l
	}
	totalPlays, totalLabeled := 0, 0
	for d, n := range dimTotN {
		totalPlays, totalLabeled = n, dimTotL[d] // any dimension's totals
		break
	}
	baseline := 0.0
	if totalPlays > 0 {
		baseline = float64(totalLabeled) / float64(totalPlays)
	}

	type item struct {
		Dim         string  `json:"dim"`
		Value       string  `json:"value"`
		N           int     `json:"n"`
		WithL       int     `json:"with_l"`
		Pct         float64 `json:"pct"`
		Lift        float64 `json:"lift"`
		Significant bool    `json:"significant"`
	}
	out := make([]item, 0, len(rows))
	for _, rr := range rows {
		n, _ := strconv.Atoi(rr.N.String())
		l, _ := strconv.Atoi(rr.WithL.String())
		pCond := 0.0
		if n > 0 {
			pCond = float64(l) / float64(n)
		}
		lift := 0.0
		if baseline > 0 {
			lift = pCond / baseline
		}
		out = append(out, item{
			Dim: rr.Dim, Value: rr.Value, N: n, WithL: l,
			Pct:  math.Round(pCond*1000) / 10,
			Lift: math.Round(lift*100) / 100,
			// Significance gate: enough sessions + enough label hits + a real
			// effect, so we never surface 10× lift off a handful of plays.
			Significant: n >= 20 && l >= 5 && (lift >= 1.5 || lift <= 0.67),
		})
	}

	resp := map[string]any{
		"label":        label,
		"days":         days,
		"total":        totalPlays,
		"baseline_pct": math.Round(baseline*1000) / 10,
		"items":        out,
	}
	if withinDim != "" {
		resp["within"] = withinDim + "=" + withinVal
	}
	writeJSON(w, resp)
}
