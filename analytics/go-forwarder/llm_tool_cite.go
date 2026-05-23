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
//   - play     {play_id, at}             → opens play viewer at time
//   - range    {play_id, from, to}       → opens play viewer with brush set
//   - finding  {slug}                    → opens finding doc
//   - standard {name}                    → opens standard doc
//   - skill    {name}                    → opens skill SKILL.md
//   - run      {run_id, cycle?}          → opens characterization run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// CitationTarget is one citation the LLM wants the dashboard to
// render. Validated server-side; invalid targets are dropped silently
// (the LLM gets a count in the result so it can detect them).
type CitationTarget struct {
	SpanID string `json:"span_id"`
	Kind   string `json:"kind"`
	Label  string `json:"label"`
	// Kind-specific fields. Only those relevant to Kind are emitted
	// — the dashboard's CitationCard component switches on Kind.
	PlayID string `json:"play_id,omitempty"`
	At     string `json:"at,omitempty"`
	From   string `json:"from,omitempty"`
	To     string `json:"to,omitempty"`
	Slug   string `json:"slug,omitempty"`
	Name   string `json:"name,omitempty"`
	RunID  string `json:"run_id,omitempty"`
	Cycle  int    `json:"cycle,omitempty"`
}

// CiteTool builds the cite() tool definition.
func CiteTool() Tool {
	return Tool{
		Name: "cite",
		Description: "Emit one or more citation cards the dashboard renders as " +
			"deep-linkable buttons. Use to anchor every non-trivial claim in " +
			"your response to a clickable artifact. Kinds: play (play_id+at), " +
			"range (play_id+from+to), finding (slug), standard (name), " +
			"skill (name), run (run_id+optional cycle). Each citation must " +
			"have a span_id you reference in the surrounding prose so the UI " +
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
							"span_id": map[string]any{"type": "string", "description": "Short id you reference in prose (e.g. c1, c2)."},
							"kind":    map[string]any{"type": "string", "enum": []string{"play", "range", "finding", "standard", "skill", "run"}},
							"label":   map[string]any{"type": "string", "description": "Human-readable button label."},
							"play_id": map[string]any{"type": "string"},
							"at":      map[string]any{"type": "string", "description": "Time within the play (mm:ss.ms or ISO)."},
							"from":    map[string]any{"type": "string"},
							"to":      map[string]any{"type": "string"},
							"slug":    map[string]any{"type": "string"},
							"name":    map[string]any{"type": "string"},
							"run_id":  map[string]any{"type": "string"},
							"cycle":   map[string]any{"type": "integer"},
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
				// Side-channel unavailable — return success but note
				// that no SSE was emitted (dashboard won't see cards).
				return mustJSON(map[string]any{
					"emitted":  0,
					"received": len(a.Targets),
					"note":     "no SSE emitter; citations dropped",
				}), nil
			}
			emitted := 0
			for _, t := range a.Targets {
				if !validCitationKind(t.Kind) {
					continue
				}
				if t.SpanID == "" || t.Label == "" {
					continue
				}
				if err := emit("citation", t); err != nil {
					// Client likely disconnected — stop here.
					return mustJSON(map[string]any{
						"emitted":  emitted,
						"received": len(a.Targets),
						"error":    err.Error(),
					}), nil
				}
				emitted++
			}
			return mustJSON(map[string]any{
				"emitted":  emitted,
				"received": len(a.Targets),
			}), nil
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
