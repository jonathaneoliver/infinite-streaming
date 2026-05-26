package main

// Throwaway probe (test file so it doesn't ship in production builds)
// measuring the serialized size of a typical chat-backend payload.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestProbePromptSize(t *testing.T) {
	cfg := config{claudeDir: "/claude"}
	h := &chatHandler{cfg: cfg, registry: NewToolRegistry(), systemPrompt: embeddedSystemPrompt}
	h.registry.RegisterAll(Tier1Tools(cfg))
	h.registry.RegisterAll(CharacterizationTools(cfg))
	h.registry.RegisterAll(Tier2Tools(cfg, cfg.claudeDir))
	h.registry.Register(CiteTool())
	h.registry.Register(QueryTool(cfg))
	h.registry.Register(InvestigateTool(cfg))
	h.registry.Register(ProposeFindingTool())

	// Mimic a typical request: range scope on a known play
	scope := ChatScope{
		Kind: "range", PlayerID: "3bff77d6-56af-468b-a938-38a9467c447b",
		PlayID: "0a9c4308-21d8-4792-93cd-bfcf1784c6d2",
		From: "2026-05-24T15:42:00Z", To: "2026-05-24T15:44:00Z",
	}
	system := h.buildSystemPrompt(scope, "America/Los_Angeles")
	tools := h.registry.ToOpenAITools()
	toolsJSON, _ := json.Marshal(tools)

	// First user message
	firstUser := LLMMessage{Role: "user", Content: "do we know what caused the stall?"}
	firstUserJSON, _ := json.Marshal(firstUser)

	t.Logf("system prompt:      %d bytes (~%d tokens)", len(system), len(system)/4)
	t.Logf("  embedded chat.md: %d bytes", len(h.systemPrompt))
	t.Logf("  scope preamble:   %d bytes", len(system)-len(h.systemPrompt))
	t.Logf("tools block (JSON): %d bytes (~%d tokens)  [%d tools]", len(toolsJSON), len(toolsJSON)/4, len(tools))
	t.Logf("first user msg:     %d bytes (~%d tokens)", len(firstUserJSON), len(firstUserJSON)/4)

	totalBase := len(system) + len(toolsJSON) + len(firstUserJSON)
	t.Logf("TOTAL turn-1 input:  %d bytes (~%d tokens)", totalBase, totalBase/4)
	t.Logf("  cacheable prefix (system + tools): %d bytes (~%d tokens)", len(system)+len(toolsJSON), (len(system)+len(toolsJSON))/4)

	// Per-tool description sizes — find the worst offenders.
	t.Logf("")
	t.Logf("per-tool sizes (description + parameters JSON, sorted desc):")
	type row struct{ name string; desc, params int }
	rows := []row{}
	for _, tl := range h.registry.tools {
		p, _ := json.Marshal(tl.Parameters)
		rows = append(rows, row{tl.Name, len(tl.Description), len(p)})
	}
	// Sort by total desc
	for i := 1; i < len(rows); i++ {
		for j := i; j > 0 && rows[j].desc+rows[j].params > rows[j-1].desc+rows[j-1].params; j-- {
			rows[j], rows[j-1] = rows[j-1], rows[j]
		}
	}
	for _, r := range rows {
		t.Logf("  %-30s  desc=%4d  params=%4d  total=%4d", r.name, r.desc, r.params, r.desc+r.params)
	}

	_ = strings.NewReader // silence unused
}
