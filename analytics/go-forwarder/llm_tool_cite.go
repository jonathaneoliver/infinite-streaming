package main

// llm_tool_cite.go — the cite() tool (#497).
//
// Unlike every other tool, cite() has a side-channel: it emits a
// "citation" SSE event to the dashboard at execution time so the
// citation card renders immediately, while returning only a brief
// JSON confirmation to the LLM so the LLM's tool budget stays
// small.
//
// Citations come in a few flavours (kinds), each rendered by the
// dashboard's Vue layer as a deep-linkable card:
//   - play     {player_id, play_id, at}         → session viewer at time
//   - range    {player_id, play_id, from, to}   → session viewer with brush
//   - finding  {slug}                           → finding doc
//   - standard {name}                           → standard doc
//   - skill    {name}                           → skill SKILL.md
//   - run      {run_id, cycle?}                 → characterization run
//
// player_id is REQUIRED for play / range — session-viewer keys
// SSE + timeseries subscriptions by player_id and bails on the
// URL without it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// CitationTarget is one citation the LLM wants the dashboard to
// render. Validated server-side; invalid targets are dropped with
// the reason in the result's `dropped` list so the LLM can correct.
type CitationTarget struct {
	SpanID string `json:"span_id"`
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	// PlayerID is REQUIRED for play / range kinds — every
	// find_plays / get_play_summary row carries it.
	PlayerID string `json:"player_id,omitempty"`
	PlayID   string `json:"play_id,omitempty"`
	At       string `json:"at,omitempty"`
	From     string `json:"from,omitempty"`
	To       string `json:"to,omitempty"`
	Slug     string `json:"slug,omitempty"`
	Name     string `json:"name,omitempty"`
	RunID    string `json:"run_id,omitempty"`
	Cycle    int    `json:"cycle,omitempty"`
}

// CiteTool builds the cite() tool definition.
func CiteTool() Tool {
	return Tool{
		Name: "cite",
		Description: "Emit one or more citation cards the dashboard renders as " +
			"deep-linkable buttons. Use to anchor every non-trivial claim in " +
			"your response to a clickable artifact. Kinds:\n" +
			"  - play  — requires player_id + play_id + at\n" +
			"  - range — requires player_id + play_id + from + to\n" +
			"  - finding — requires slug\n" +
			"  - standard — requires name\n" +
			"  - skill — requires name\n" +
			"  - run — requires run_id + optional cycle\n" +
			"For play / range kinds you MUST include both player_id AND play_id " +
			"— the session-viewer page won't load without player_id. Both fields " +
			"are in every find_plays / get_play_summary result row. Each citation " +
			"must have a span_id you reference in the surrounding prose so the UI " +
			"can correlate the card with the right span.",
		Tier: 0, // Special — runs alongside any tier's reasoning.
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"targets": map[string]any{
					"type":     "array",
					"minItems": 1,
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"span_id":   map[string]any{"type": "string", "description": "Short id you reference in prose (e.g. c1, c2)."},
							"kind":      map[string]any{"type": "string", "enum": []string{"play", "range", "finding", "standard", "skill", "run"}},
							"label":     map[string]any{"type": "string", "description": "Human-readable button label."},
							"player_id": map[string]any{"type": "string", "description": "Player UUID. REQUIRED for play / range (session-viewer won't load without it)."},
							"play_id":   map[string]any{"type": "string", "description": "Play UUID. Required for play / range."},
							"at":        map[string]any{"type": "string", "description": "Time within the play (mm:ss.ms or ISO)."},
							"from":      map[string]any{"type": "string"},
							"to":        map[string]any{"type": "string"},
							"slug":      map[string]any{"type": "string"},
							"name":      map[string]any{"type": "string"},
							"run_id":    map[string]any{"type": "string"},
							"cycle":     map[string]any{"type": "integer"},
						},
						"required": []string{"span_id", "kind", "label"},
					},
				},
			},
			"required": []string{"targets"},
		},
		Execute: func(ctx context.Context, args json.RawMessage, emit ToolEmitter) (string, error) {
			var a struct {
				Targets []CitationTarget `json:"targets"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("parse args: %w", err)
			}
			if len(a.Targets) == 0 {
				return "", errors.New("at least one citation target required")
			}
			if emit == nil {
				return mustJSON(map[string]any{
					"emitted":  0,
					"received": len(a.Targets),
					"note":     "no SSE emitter; citations dropped",
				}), nil
			}
			emitted := 0
			dropped := []string{}
			for _, t := range a.Targets {
				if !validCitationKind(t.Kind) {
					dropped = append(dropped, fmt.Sprintf("%s: invalid kind %q", t.SpanID, t.Kind))
					continue
				}
				if t.SpanID == "" || t.Label == "" {
					dropped = append(dropped, fmt.Sprintf("%s: missing span_id or label", t.SpanID))
					continue
				}
				// Required-IDs check for deep-linking kinds so the
				// dashboard URL actually resolves. Surface the drop
				// reason to the LLM so it can correct + retry.
				if (t.Kind == "play" || t.Kind == "range") && (t.PlayerID == "" || t.PlayID == "") {
					dropped = append(dropped, fmt.Sprintf("%s: %s kind requires player_id and play_id", t.SpanID, t.Kind))
					continue
				}
				if err := emit("citation", t); err != nil {
					return mustJSON(map[string]any{
						"emitted":  emitted,
						"received": len(a.Targets),
						"dropped":  dropped,
						"error":    err.Error(),
					}), nil
				}
				emitted++
			}
			result := map[string]any{
				"emitted":  emitted,
				"received": len(a.Targets),
			}
			if len(dropped) > 0 {
				result["dropped"] = dropped
			}
			return mustJSON(result), nil
		},
	}
}

func validCitationKind(k string) bool {
	switch k {
	case "play", "range", "finding", "standard", "skill", "run":
		return true
	}
	return false
}
