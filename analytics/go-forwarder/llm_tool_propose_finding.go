package main

// llm_tool_propose_finding.go — write-side knowledge-base capture.
//
// The bot reaches a tagged hypothesis worth keeping. Calling
// propose_finding emits a "finding_proposed" SSE event with the
// full markdown payload to the dashboard, which renders a
// Save / Discard card under the assistant turn. The operator
// reviews + clicks Save → dashboard POSTs to
// /api/v2/chat/findings/save → forwarder writes the .md file to
// cfg.claudeDir/findings/<slug>.md.
//
// Two-step (propose → confirm-then-save) keeps the trust model the
// design picked: the LLM is never the principal writing to the
// knowledge base; the operator's click is the commit signal.
// File lands on disk → operator commits via git when ready
// (matches the "Write to mount, operator commits" decision).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
)

var findingSlugPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,128}$`)

// ProposeFindingTool builds the proposal tool. Returns a Tool that
// validates the proposal shape, emits a finding_proposed SSE
// event with the full payload, and returns a brief ack to the LLM.
func ProposeFindingTool() Tool {
	return Tool{
		Name: "propose_finding",
		Description: "Propose a new finding to add to .claude/findings/. " +
			"You CANNOT write directly — the operator must click Save in " +
			"the dashboard to commit the file. Use after reaching a " +
			"confirmed or strongly-suspected hypothesis that isn't already " +
			"in the findings library. Output a finding only when it would " +
			"be useful across sessions; don't propose for one-off conclusions. " +
			"Follow the finding template: a tagged-hypothesis Summary, a " +
			"Timeline, and Evidence with concrete play_ids / timestamps.",
		Tier: 2,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"slug": map[string]any{
					"type":        "string",
					"description": "Filename slug (will become <slug>.md). Format: <player-shortid-or-topic>-<symptom>-<YYYY-MM-DD>. e.g. ipad-262s-stall-2026-05-17. Only [a-zA-Z0-9._-] allowed.",
				},
				"markdown": map[string]any{
					"type":        "string",
					"description": "The finding body. Lead with `# <Title>`. Sections: ## Summary (1-3 sentences ending with `Tag: confirmed|refuted|needs-test.`), ## Timeline (bullet list with absolute UTC timestamps), ## Evidence (citations, harness output snippets, links).",
				},
				"tags": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Optional categorical tags (e.g. ['stall','ipad','wedge']). The slug already encodes most categorisation; tags are search-helpers.",
				},
			},
			"required": []string{"slug", "markdown"},
		},
		Execute: func(ctx context.Context, args json.RawMessage, emit ToolEmitter) (string, error) {
			var a struct {
				Slug     string   `json:"slug"`
				Markdown string   `json:"markdown"`
				Tags     []string `json:"tags"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("parse args: %w", err)
			}
			if !findingSlugPattern.MatchString(a.Slug) {
				return "", fmt.Errorf("invalid slug %q — must match %s", a.Slug, findingSlugPattern.String())
			}
			if len(a.Markdown) < 40 {
				return "", errors.New("markdown too short — a useful finding needs at least a summary section")
			}
			if emit == nil {
				return mustJSON(map[string]any{
					"proposed": false,
					"note":     "no SSE emitter; proposal dropped (call must come from a chat session)",
				}), nil
			}
			if err := emit("finding_proposed", map[string]any{
				"slug":     a.Slug,
				"markdown": a.Markdown,
				"tags":     a.Tags,
			}); err != nil {
				return "", fmt.Errorf("emit finding_proposed: %w", err)
			}
			return mustJSON(map[string]any{
				"proposed":      true,
				"slug":          a.Slug,
				"_note":         "queued for operator review in the dashboard; not yet written to disk",
				"_user_action":  "the user will see a Save / Discard card under this turn",
			}), nil
		},
	}
}

// saveFinding writes the markdown to <claudeDir>/findings/<slug>.md.
// Refuses to overwrite an existing file — the operator can rename
// the slug or commit + delete the old one if they want a refresh.
// Path traversal: validateSlug equivalent inline (the slug pattern
// already enforces this; double-check anyway).
func saveFinding(cfg config, slug, markdown string) (string, error) {
	if cfg.claudeDir == "" {
		return "", errors.New("FORWARDER_CLAUDE_DIR not configured; cannot write findings")
	}
	if !findingSlugPattern.MatchString(slug) {
		return "", fmt.Errorf("invalid slug %q", slug)
	}
	path := cfg.claudeDir + "/findings/" + slug + ".md"
	// Refuse overwrite — easier for operator to see "this was new"
	// vs "this clobbered something I had".
	if _, err := osStat(path); err == nil {
		return "", fmt.Errorf("finding %q already exists at %s — pick a different slug or delete the existing file", slug, path)
	}
	if err := osMkdirAll(cfg.claudeDir+"/findings", 0o755); err != nil {
		return "", fmt.Errorf("mkdir findings dir: %w", err)
	}
	if err := osWriteFile(path, []byte(markdown), 0o644); err != nil {
		return "", fmt.Errorf("write finding: %w", err)
	}
	return path, nil
}
