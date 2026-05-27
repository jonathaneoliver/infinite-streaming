package main

// llm_tools_tier1.go — typed domain tools (#497).
//
// Each tool is a thin adapter: parse the JSON args into the
// internal/plays typed parameter struct, call the domain function,
// JSON-encode the result. The HTTP handlers use the same domain
// functions — single source of truth.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jonathaneoliver/infinite-streaming/analytics/go-forwarder/internal/plays"
)

// Tier1Tools builds the typed-domain tool set for the chat backend.
// Pass the chat backend's config so the tools can call internal/plays.
func Tier1Tools(cfg config) []Tool {
	be := playsBackend(cfg)
	return []Tool{
		findPlaysTool(be),
		getPlaySummaryTool(be),
		getControlEventsTool(be),
	}
}

func findPlaysTool(be plays.Backend) Tool {
	return Tool{
		Name: "find_plays",
		Description: "List archived plays matching the filters. " +
			"Returns one row per play with aggregated facts (label set, error counts, " +
			"timing, classification). Use to find aberrant sessions, scope by player " +
			"or content, or get a fleet-wide answer to 'what's broken since X'. " +
			"Filter by labels[] (e.g. labels_has=['critical=frozen']) for the cheap " +
			"interestingness signal. **Default mode is 'summary'** (aggregates only — " +
			"count, classification breakdown, label histogram, time span). Switch to " +
			"mode='rows' when you need play_ids to drill into. Use top_k=N to cap " +
			"row mode at the N most interesting plays (highest issue count first).",
		Tier: 1,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"player_id":      map[string]any{"type": "string", "description": "Filter to one player (UUID)."},
				"play_id":        map[string]any{"type": "string", "description": "Filter to one play (UUID)."},
				"from":           map[string]any{"type": "string", "description": "ISO timestamp lower bound (e.g. 2026-05-23T09:00:00Z)."},
				"to":             map[string]any{"type": "string", "description": "ISO timestamp upper bound."},
				"classification": map[string]any{"type": "string", "enum": []string{"interesting", "other", "favourite"}},
				"labels_has":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "AND-required labels (e.g. ['critical=frozen'])."},
				"labels_not":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Labels that must NOT be present."},
				"mode":           map[string]any{"type": "string", "enum": []string{"summary", "rows"}, "default": "summary", "description": "summary = aggregates only (cheap, default); rows = full per-play data."},
				"top_k":          map[string]any{"type": "integer", "minimum": 1, "maximum": 200, "description": "In rows mode: keep only the N highest-issue-count plays. Default 20 if mode='rows' and unset."},
				"limit":          map[string]any{"type": "integer", "minimum": 1, "maximum": 5000, "default": 500, "description": "Upper bound on rows scanned from CH (before top_k narrowing)."},
			},
		},
		Execute: func(ctx context.Context, args json.RawMessage, _ ToolEmitter) (string, error) {
			var a struct {
				PlayerID       string   `json:"player_id"`
				PlayID         string   `json:"play_id"`
				From           string   `json:"from"`
				To             string   `json:"to"`
				Classification string   `json:"classification"`
				LabelsHas      []string `json:"labels_has"`
				LabelsNot      []string `json:"labels_not"`
				Mode           string   `json:"mode"`
				TopK           int      `json:"top_k"`
				Limit          int      `json:"limit"`
			}
			if len(args) > 0 {
				if err := json.Unmarshal(args, &a); err != nil {
					return "", fmt.Errorf("parse args: %w", err)
				}
			}
			if a.Limit == 0 {
				a.Limit = 500
			}
			rows, err := plays.FindPlays(ctx, be, plays.PlayFilter{
				PlayerID:       a.PlayerID,
				PlayID:         a.PlayID,
				From:           a.From,
				To:             a.To,
				Classification: a.Classification,
				Labels:         plays.LabelFilter{Has: a.LabelsHas, Not: a.LabelsNot},
				Limit:          a.Limit,
			})
			if err != nil {
				return "", err
			}
			if rows == nil {
				rows = []map[string]any{}
			}
			// Default mode='summary' — bot must explicitly ask for rows.
			// Saves ~30 KB per call on a typical 24h window.
			if a.Mode != "rows" {
				return mustJSON(summarisePlays(rows)), nil
			}
			// rows mode: cap to top_k by issue count so even a 500-row
			// scan returns the most useful subset, not the whole bag.
			if a.TopK == 0 {
				a.TopK = 20
			}
			rows = topKByIssues(rows, a.TopK)
			return mustJSON(map[string]any{
				"count": len(rows),
				"plays": rows,
				"mode":  "rows",
				"top_k": a.TopK,
			}), nil
		},
	}
}

// summarisePlays builds a compact aggregate view of a play set.
// Replaces ~30 KB of row JSON with ~1 KB of counts + histograms.
// Bot reads this, decides whether to drill (with mode='rows' +
// tighter filter) or proceed.
func summarisePlays(rows []map[string]any) map[string]any {
	totalIssues := 0
	classCount := map[string]int{}
	labelCount := map[string]int{}
	playerCount := map[string]int{}
	contentCount := map[string]int{}
	var earliest, latest string
	for _, r := range rows {
		if c, _ := r["classification"].(string); c != "" {
			classCount[c]++
		}
		if p, _ := r["player_id"].(string); p != "" {
			playerCount[p]++
		}
		if c, _ := r["content_id"].(string); c != "" {
			contentCount[c]++
		}
		// label_histogram is []any of [label, count] tuples
		if lh, ok := r["label_histogram"].([]any); ok {
			for _, pair := range lh {
				p, ok := pair.([]any)
				if !ok || len(p) < 2 {
					continue
				}
				label, _ := p[0].(string)
				n, _ := p[1].(float64)
				if label != "" {
					labelCount[label] += int(n)
				}
			}
		}
		totalIssues += asNumber(r["stalls"]) + asNumber(r["net_errors"]) +
			asNumber(r["net_faults"]) + asNumber(r["user_marked_count"]) +
			asNumber(r["frozen_count"]) + asNumber(r["segment_stall_count"])
		if s, _ := r["started_at"].(string); s != "" {
			if earliest == "" || s < earliest {
				earliest = s
			}
		}
		if s, _ := r["last_seen_at"].(string); s != "" {
			if latest == "" || s > latest {
				latest = s
			}
		}
	}
	return map[string]any{
		"count":                  len(rows),
		"mode":                   "summary",
		"total_issues":           totalIssues,
		"by_classification":      classCount,
		"by_player":              playerCount,
		"by_content":             contentCount,
		"label_histogram":        labelCount,
		"distinct_players":       len(playerCount),
		"distinct_content":       len(contentCount),
		"window_start":           earliest,
		"window_end":             latest,
		"_hint":                  "summary mode — call again with mode='rows' (optionally top_k=N, labels_has=[...]) to drill into specific plays.",
	}
}

// topKByIssues sorts plays by issue weight DESC and returns the
// first k. Stable for plays with equal issue counts so re-runs
// match.
func topKByIssues(rows []map[string]any, k int) []map[string]any {
	if len(rows) <= k {
		return rows
	}
	scored := make([]struct {
		row   map[string]any
		score int
	}, len(rows))
	for i, r := range rows {
		scored[i].row = r
		scored[i].score = asNumber(r["stalls"]) + asNumber(r["net_errors"]) +
			asNumber(r["net_faults"]) + asNumber(r["user_marked_count"]) +
			asNumber(r["frozen_count"]) + asNumber(r["segment_stall_count"])
	}
	// Sort DESC by score; stable so equal scores keep input order.
	for i := 1; i < len(scored); i++ {
		for j := i; j > 0 && scored[j].score > scored[j-1].score; j-- {
			scored[j], scored[j-1] = scored[j-1], scored[j]
		}
	}
	out := make([]map[string]any, k)
	for i := 0; i < k; i++ {
		out[i] = scored[i].row
	}
	return out
}

// asNumber coerces a CH-row field (could be float64 from json,
// string from UInt64-as-string, or nil) to int.
func asNumber(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		var n int
		_, _ = fmt.Sscanf(x, "%d", &n)
		return n
	}
	return 0
}

func getPlaySummaryTool(be plays.Backend) Tool {
	return Tool{
		Name: "get_play_summary",
		Description: "Get the full PlaySummary facts row for one play (the same shape " +
			"find_plays returns, but with no surrounding scope). Use before reaching " +
			"a conclusion about a specific play — anchors your reasoning in concrete " +
			"counters (stalls, error counts, label set).",
		Tier: 1,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"play_id": map[string]any{"type": "string", "description": "Play UUID."},
			},
			"required": []string{"play_id"},
		},
		Execute: func(ctx context.Context, args json.RawMessage, _ ToolEmitter) (string, error) {
			var a struct {
				PlayID string `json:"play_id"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("parse args: %w", err)
			}
			if a.PlayID == "" {
				return "", fmt.Errorf("play_id required")
			}
			row, err := plays.GetPlaySummary(ctx, be, a.PlayID)
			if err != nil {
				return "", err
			}
			if row == nil {
				return mustJSON(map[string]any{"found": false, "play_id": a.PlayID}), nil
			}
			return mustJSON(map[string]any{"found": true, "play": row}), nil
		},
	}
}

func getControlEventsTool(be plays.Backend) Tool {
	return Tool{
		Name: "get_control_events",
		Description: "Get the operator / proxy / harness action log for a player. " +
			"Rows include fault toggles, traffic-shape changes, pattern step advances, " +
			"and any harness mutation. Crucial for forensics — was a fault injected " +
			"at the time something broke?",
		Tier: 1,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"player_id":  map[string]any{"type": "string", "description": "Player UUID (required)."},
				"play_id":    map[string]any{"type": "string", "description": "Narrow to one play (optional)."},
				"from":       map[string]any{"type": "string", "description": "ISO timestamp lower bound (optional)."},
				"to":         map[string]any{"type": "string", "description": "ISO timestamp upper bound (optional)."},
				"labels_has": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"labels_not": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"limit":      map[string]any{"type": "integer", "minimum": 1, "maximum": 10000, "default": 1000},
			},
			"required": []string{"player_id"},
		},
		Execute: func(ctx context.Context, args json.RawMessage, _ ToolEmitter) (string, error) {
			var a struct {
				PlayerID  string   `json:"player_id"`
				PlayID    string   `json:"play_id"`
				From      string   `json:"from"`
				To        string   `json:"to"`
				LabelsHas []string `json:"labels_has"`
				LabelsNot []string `json:"labels_not"`
				Limit     int      `json:"limit"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("parse args: %w", err)
			}
			rows, err := plays.GetControlEvents(ctx, be, plays.ControlEventsFilter{
				PlayerID: a.PlayerID,
				PlayID:   a.PlayID,
				From:     a.From,
				To:       a.To,
				Labels:   plays.LabelFilter{Has: a.LabelsHas, Not: a.LabelsNot},
				Limit:    a.Limit,
			})
			if err != nil {
				return "", err
			}
			if rows == nil {
				rows = []map[string]any{}
			}
			return mustJSON(map[string]any{
				"count":  len(rows),
				"events": rows,
			}), nil
		},
	}
}
