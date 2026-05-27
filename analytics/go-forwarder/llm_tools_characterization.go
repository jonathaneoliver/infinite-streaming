package main

// llm_tools_characterization.go — typed-domain tools over the
// characterization_runs table (#497). Same pattern as
// llm_tools_tier1.go: thin adapters that build a CH query, call
// the shared chQueryBytes + parseJSONEachRowItems helpers, and
// JSON-encode the result for the LLM.
//
// Three tools:
//   list_characterization_runs    — pick a run from the recent set
//   get_characterization_step     — drill into one step of one run
//   compare_characterization_runs — side-by-side step deltas
//
// All read from infinite_streaming.characterization_runs, which is
// one row per (run_id, test_name) — see 01-schema.sql:496.
// report_json is the runner.Report blob stored as a string; we
// parse the minimum we need (steps + summary) on the way out.

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// CharacterizationTools is the third typed-domain tool group.
// Registered in newChatHandler alongside Tier1Tools / Tier2Tools.
func CharacterizationTools(cfg config) []Tool {
	return []Tool{
		listCharacterizationRunsTool(cfg),
		getCharacterizationStepTool(cfg),
		compareCharacterizationRunsTool(cfg),
	}
}

func listCharacterizationRunsTool(cfg config) Tool {
	return Tool{
		Name: "list_characterization_runs",
		Description: "List recent characterization sweeps (one row per (run_id, test_name)). " +
			"Use to find a run to drill into, or to enumerate runs of a single test " +
			"mode for trend analysis. Each row carries the parsed summary_json — " +
			"pass/fail, max bitrate seen, total stalls — so a single call often " +
			"answers 'did last night's runs all pass'. To drill into a step, follow " +
			"with get_characterization_step(run_id, test_name, step_index).",
		Tier: 1,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"test_name":      map[string]any{"type": "string", "description": "Filter by test mode (smooth, steps, transient-shock, startup-caps, downshift-severity, hysteresis-gap, emergency-downshift, abort, retry-backoff, startup, rampup, rampdown, pyramid)."},
				"platform":       map[string]any{"type": "string", "description": "Filter by platform (apple, roku, web)."},
				"classification": map[string]any{"type": "string", "enum": []string{"interesting", "other", "favourite"}},
				"since":          map[string]any{"type": "string", "description": "ISO timestamp lower bound on started_at."},
				"limit":          map[string]any{"type": "integer", "minimum": 1, "maximum": 200, "default": 30},
			},
		},
		Execute: func(ctx context.Context, args json.RawMessage, _ ToolEmitter) (string, error) {
			var a struct {
				TestName       string `json:"test_name"`
				Platform       string `json:"platform"`
				Classification string `json:"classification"`
				Since          string `json:"since"`
				Limit          int    `json:"limit"`
			}
			if len(args) > 0 {
				if err := json.Unmarshal(args, &a); err != nil {
					return "", fmt.Errorf("parse args: %w", err)
				}
			}
			if a.Limit == 0 {
				a.Limit = 30
			}
			conds := []string{"1=1"}
			params := map[string]string{"limit": fmt.Sprintf("%d", a.Limit)}
			if a.TestName != "" {
				conds = append(conds, "test_name = {test_name:String}")
				params["test_name"] = a.TestName
			}
			if a.Platform != "" {
				conds = append(conds, "platform = {platform:String}")
				params["platform"] = a.Platform
			}
			if a.Classification != "" {
				conds = append(conds, "classification = {classification:String}")
				params["classification"] = a.Classification
			}
			if a.Since != "" {
				// parseDateTime64BestEffort returns Nullable(DateTime64),
				// and CH 24.8 refuses `DateTime64 >= Nullable(DateTime64)`
				// (No operation greaterOrEquals between String and
				// DateTime64). Use the *OrZero variant — returns a
				// concrete DateTime64 so the comparison binds. The bot
				// only ever passes a real ISO timestamp here, so the
				// "Zero" fallback (epoch) just disables the filter
				// rather than corrupting it.
				conds = append(conds, "started_at >= parseDateTime64BestEffortOrZero({since:String}, 3, 'UTC')")
				params["since"] = a.Since
			}
			q := fmt.Sprintf(`
				SELECT run_id, test_name, platform,
				       toString(started_at) AS started_at,
				       toString(ended_at)   AS ended_at,
				       player_id, play_ids, passed, classification, summary_json
				FROM infinite_streaming.characterization_runs
				WHERE %s
				ORDER BY started_at DESC
				LIMIT {limit:UInt32}
			`, strings.Join(conds, " AND "))
			body, err := chQueryBytes(ctx, cfg, q, params)
			if err != nil {
				return "", fmt.Errorf("clickhouse: %w", err)
			}
			items, err := parseJSONEachRowItems(body)
			if err != nil {
				return "", fmt.Errorf("decode rows: %w", err)
			}
			out := make([]map[string]any, 0, len(items))
			for _, raw := range items {
				out = append(out, decorateRunRow(raw))
			}
			return mustJSON(map[string]any{"count": len(out), "runs": out}), nil
		},
	}
}

// decorateRunRow takes a CH row JSON and replaces summary_json
// (string) with summary (parsed object), so the LLM doesn't have
// to parse-inside-a-parse. Falls back to dropping the string on
// parse failure rather than failing the whole tool.
func decorateRunRow(raw json.RawMessage) map[string]any {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return map[string]any{"_raw_parse_error": err.Error()}
	}
	if s, ok := m["summary_json"].(string); ok {
		var inner any
		if err := json.Unmarshal([]byte(s), &inner); err == nil {
			m["summary"] = inner
		}
		delete(m, "summary_json")
	}
	return m
}

func getCharacterizationStepTool(cfg config) Tool {
	return Tool{
		Name: "get_characterization_step",
		Description: "Get one step of one characterization run. Returns the step's " +
			"buffer envelope (start/end/min/max), max bitrate the player picked, " +
			"max measured network throughput, stall delta, exit reason, and the " +
			"player_id + play_id covering that step. Pivot to forensics with " +
			"investigate(player_id=..., play_id=..., question=...) or with " +
			"get_play_summary if you want the play-level facts. When step_index " +
			"is omitted, returns just the run's step count + list of step indexes.",
		Tier: 1,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"run_id":     map[string]any{"type": "string", "description": "Run ID (UTC timestamp the harness stamps at sweep start)."},
				"test_name":  map[string]any{"type": "string", "description": "Test mode (smooth, steps, …)."},
				"step_index": map[string]any{"type": "integer", "minimum": 0, "description": "Step number (0-based). Omit to list the steps."},
			},
			"required": []string{"run_id", "test_name"},
		},
		Execute: func(ctx context.Context, args json.RawMessage, _ ToolEmitter) (string, error) {
			var a struct {
				RunID     string `json:"run_id"`
				TestName  string `json:"test_name"`
				StepIndex *int   `json:"step_index"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("parse args: %w", err)
			}
			if a.RunID == "" || a.TestName == "" {
				return "", fmt.Errorf("run_id and test_name required")
			}
			row, err := fetchCharacterizationDetail(ctx, cfg, a.RunID, a.TestName)
			if err != nil {
				return "", err
			}
			if row == nil {
				return mustJSON(map[string]any{"found": false, "run_id": a.RunID, "test_name": a.TestName}), nil
			}
			report := parseReportJSON(row)
			steps, _ := report["steps"].([]any)
			playerID, _ := row["player_id"].(string)
			playIDs, _ := row["play_ids"].([]any)
			base := map[string]any{
				"found":      true,
				"run_id":     a.RunID,
				"test_name":  a.TestName,
				"player_id":  playerID,
				"play_ids":   playIDs,
				"step_count": len(steps),
				"mode":       report["mode"],
				// Purpose doc — read this with read_standard() to learn
				// what the test was trying to verify before judging the
				// numbers. Hint follows the existing standards naming
				// convention; falsy hints (test_name doesn't map to a
				// standard) still work — the bot just gets a 404 it can
				// move past.
				"_purpose_doc_hint": a.TestName + "-characterization-test",
			}
			if a.StepIndex == nil {
				// Index mode — short shape for picking a step.
				summaries := make([]map[string]any, len(steps))
				for i, s := range steps {
					m, _ := s.(map[string]any)
					summaries[i] = map[string]any{
						"step_index": i,
						"rate_mbps":  m["rate_mbps"],
						"exit_reason": m["exit_reason"],
						"max_bitrate_mbps": m["max_bitrate_mbps"],
						"started_at": m["started_at"],
					}
				}
				base["steps"] = summaries
				base["_hint"] = "call again with step_index=N to get the full step row"
				return mustJSON(base), nil
			}
			i := *a.StepIndex
			if i < 0 || i >= len(steps) {
				return mustJSON(map[string]any{
					"found":      false,
					"reason":     fmt.Sprintf("step_index %d out of range [0, %d)", i, len(steps)),
					"step_count": len(steps),
				}), nil
			}
			base["step_index"] = i
			base["step"] = steps[i]
			// First entry in play_ids is the play active at sweep start
			// — usually correct for non-startup modes. For modes that
			// relaunch (startup-caps), the bot should grep the steps[].started_at
			// against the play windows to find the right play.
			if len(playIDs) > 0 {
				base["play_id"] = playIDs[0]
				if len(playIDs) > 1 {
					base["_note"] = "multiple play_ids — first one returned. For app-relaunch modes (startup-caps), match step.started_at against per-play windows."
				}
			}
			return mustJSON(base), nil
		},
	}
}

func compareCharacterizationRunsTool(cfg config) Tool {
	return Tool{
		Name: "compare_characterization_runs",
		Description: "Side-by-side compare of two characterization runs (same test mode). " +
			"Returns per-step deltas on max_bitrate_mbps, buffer_at_start_s, " +
			"buffer_at_end_s, max_network_bitrate_mbps, stalls_delta, exit_reason. " +
			"Use to spot regressions between nightly runs or between a baseline " +
			"and a candidate. If step counts differ, that's flagged in step_count_diff " +
			"and only the overlapping prefix is compared.",
		Tier: 1,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"run_id_a":    map[string]any{"type": "string"},
				"test_name_a": map[string]any{"type": "string"},
				"run_id_b":    map[string]any{"type": "string"},
				"test_name_b": map[string]any{"type": "string"},
			},
			"required": []string{"run_id_a", "test_name_a", "run_id_b", "test_name_b"},
		},
		Execute: func(ctx context.Context, args json.RawMessage, _ ToolEmitter) (string, error) {
			var a struct {
				RunIDA    string `json:"run_id_a"`
				TestNameA string `json:"test_name_a"`
				RunIDB    string `json:"run_id_b"`
				TestNameB string `json:"test_name_b"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("parse args: %w", err)
			}
			rowA, err := fetchCharacterizationDetail(ctx, cfg, a.RunIDA, a.TestNameA)
			if err != nil {
				return "", fmt.Errorf("fetch A: %w", err)
			}
			rowB, err := fetchCharacterizationDetail(ctx, cfg, a.RunIDB, a.TestNameB)
			if err != nil {
				return "", fmt.Errorf("fetch B: %w", err)
			}
			if rowA == nil || rowB == nil {
				return mustJSON(map[string]any{
					"found_a": rowA != nil,
					"found_b": rowB != nil,
				}), nil
			}
			repA := parseReportJSON(rowA)
			repB := parseReportJSON(rowB)
			stepsA, _ := repA["steps"].([]any)
			stepsB, _ := repB["steps"].([]any)
			overlap := len(stepsA)
			if len(stepsB) < overlap {
				overlap = len(stepsB)
			}
			deltas := make([]map[string]any, 0, overlap)
			for i := 0; i < overlap; i++ {
				ma, _ := stepsA[i].(map[string]any)
				mb, _ := stepsB[i].(map[string]any)
				deltas = append(deltas, map[string]any{
					"step_index": i,
					"rate_mbps":  ma["rate_mbps"],
					"a": stepDigest(ma),
					"b": stepDigest(mb),
					"delta": stepDelta(ma, mb),
				})
			}
			return mustJSON(map[string]any{
				"a": runDigest(rowA, repA),
				"b": runDigest(rowB, repB),
				"step_count_a": len(stepsA),
				"step_count_b": len(stepsB),
				"step_count_diff": len(stepsB) - len(stepsA),
				"overlapping_steps": overlap,
				"step_deltas": deltas,
				"summary_delta": summaryDelta(repA, repB),
			}), nil
		},
	}
}

// fetchCharacterizationDetail reads one (run_id, test_name) row.
// Returns nil + nil-err when there's no match.
func fetchCharacterizationDetail(ctx context.Context, cfg config, runID, testName string) (map[string]any, error) {
	q := `
		SELECT run_id, test_name, platform,
		       toString(started_at) AS started_at,
		       toString(ended_at)   AS ended_at,
		       player_id, play_ids, passed, classification,
		       summary_json, report_json
		FROM infinite_streaming.characterization_runs
		WHERE run_id = {run_id:String} AND test_name = {test_name:String}
		ORDER BY started_at DESC
		LIMIT 1
	`
	body, err := chQueryBytes(ctx, cfg, q, map[string]string{
		"run_id":    runID,
		"test_name": testName,
	})
	if err != nil {
		return nil, err
	}
	items, err := parseJSONEachRowItems(body)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}
	var row map[string]any
	if err := json.Unmarshal(items[0], &row); err != nil {
		return nil, err
	}
	return row, nil
}

// parseReportJSON pulls the inner report_json string off a CH row
// and parses it. Returns an empty map (not nil) on parse failure
// so callers can `.(string)`/`.([]any)` without panic-checking.
func parseReportJSON(row map[string]any) map[string]any {
	s, _ := row["report_json"].(string)
	if s == "" {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return map[string]any{"_parse_error": err.Error()}
	}
	return out
}

// stepDigest returns the comparable subset of a Step row. Keep the
// surface small — these are diffed across runs and full Step rows
// would balloon the response.
func stepDigest(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	return map[string]any{
		"max_bitrate_mbps":         m["max_bitrate_mbps"],
		"mean_bitrate_mbps":        m["mean_bitrate_mbps"],
		"buffer_at_start_s":        m["buffer_at_start_s"],
		"buffer_at_end_s":          m["buffer_at_end_s"],
		"min_buffer_s":             m["min_buffer_s"],
		"max_network_bitrate_mbps": m["max_network_bitrate_mbps"],
		"stalls_delta":             m["stalls_delta"],
		"profile_shifts_delta":     m["profile_shifts_delta"],
		"exit_reason":              m["exit_reason"],
	}
}

func stepDelta(a, b map[string]any) map[string]any {
	keys := []string{"max_bitrate_mbps", "mean_bitrate_mbps", "buffer_at_start_s",
		"buffer_at_end_s", "min_buffer_s", "max_network_bitrate_mbps"}
	out := map[string]any{}
	for _, k := range keys {
		va, vb := asFloat(a[k]), asFloat(b[k])
		if math.IsNaN(va) && math.IsNaN(vb) {
			continue
		}
		out[k] = round3(vb - va)
	}
	// stalls_delta + profile_shifts_delta are counts; report as int diff.
	out["stalls_delta"] = asInt(b["stalls_delta"]) - asInt(a["stalls_delta"])
	out["profile_shifts_delta"] = asInt(b["profile_shifts_delta"]) - asInt(a["profile_shifts_delta"])
	if asStr(a["exit_reason"]) != asStr(b["exit_reason"]) {
		out["exit_reason_change"] = fmt.Sprintf("%s → %s", asStr(a["exit_reason"]), asStr(b["exit_reason"]))
	}
	return out
}

func runDigest(row, report map[string]any) map[string]any {
	return map[string]any{
		"run_id":     row["run_id"],
		"test_name":  row["test_name"],
		"platform":   row["platform"],
		"started_at": row["started_at"],
		"passed":     row["passed"],
		"player_id":  row["player_id"],
		"summary":    report["summary"],
	}
}

func summaryDelta(a, b map[string]any) map[string]any {
	sa, _ := a["summary"].(map[string]any)
	sb, _ := b["summary"].(map[string]any)
	if sa == nil || sb == nil {
		return nil
	}
	out := map[string]any{}
	for k := range sa {
		if _, ok := sb[k]; !ok {
			continue
		}
		va, vb := asFloat(sa[k]), asFloat(sb[k])
		if math.IsNaN(va) || math.IsNaN(vb) {
			continue
		}
		out[k] = round3(vb - va)
	}
	return out
}

func asFloat(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case int:
		return float64(x)
	case string:
		var f float64
		if _, err := fmt.Sscanf(x, "%f", &f); err == nil {
			return f
		}
	}
	return math.NaN()
}

func asInt(v any) int {
	return asNumber(v) // shared with llm_tools_tier1.go
}

func asStr(v any) string {
	s, _ := v.(string)
	return s
}

func round3(f float64) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	return math.Round(f*1000) / 1000
}
