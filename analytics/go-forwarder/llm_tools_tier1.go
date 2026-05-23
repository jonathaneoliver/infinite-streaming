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
			"interestingness signal.",
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
				"limit":          map[string]any{"type": "integer", "minimum": 1, "maximum": 5000, "default": 100},
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
				Limit          int      `json:"limit"`
			}
			if len(args) > 0 {
				if err := json.Unmarshal(args, &a); err != nil {
					return "", fmt.Errorf("parse args: %w", err)
				}
			}
			if a.Limit == 0 {
				a.Limit = 100
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
			return mustJSON(map[string]any{
				"count": len(rows),
				"plays": rows,
			}), nil
		},
	}
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
