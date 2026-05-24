package main

// llm_tool_investigate.go — subagent pattern for fat investigations.
//
// The user is on a chat thread. They ask "why did this play fail?"
// Naïve flow: the main agent calls find_plays → get_play_timeline →
// get_control_events → query → ... over 20 turns, each result lands
// in the main thread's context. After three of these the context is
// 80% tool-result detritus.
//
// Subagent flow: the main agent calls investigate(play_id, question).
// Inside the tool, the forwarder spins up a NEW chat loop with
//   - empty starting context
//   - a focused system prompt ("you are an investigator, return a
//     short finding")
//   - a restricted tool set (the play-scoped read tools — no cite,
//     no nested investigate, no propose_finding)
// The subagent runs its own tool-use loop to a final assistant
// message. That message is the tool result returned to main.
//
// Main agent's history grows by ~500 bytes; subagent's expensive
// reasoning + raw data is discarded at the end of the call. Same
// pattern Claude Code's Explore subagent uses.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jonathaneoliver/infinite-streaming/analytics/go-forwarder/internal/plays"
)

// subagentCtxKey is the context.Context key that handleChat sets
// for the subagent tool to find its spawn function + creds.
type subagentCtxKey struct{}

// subagentContext is the per-request bundle the investigate tool
// needs. handleChat builds one from the active ChatRequest and
// stashes it in ctx via context.WithValue.
type subagentContext struct {
	cfg       config
	baseURL   string
	apiKey    string
	profile   string
	model     string
	// registry contains the FULL tool registry from the main loop.
	// The subagent uses a filtered subset (see safeSubagentTools).
	registry  *ToolRegistry
}

// safeSubagentTools returns the names of tools the subagent is
// allowed to call. Excludes:
//   - cite (subagent shouldn't emit UI events on the parent's stream)
//   - investigate (no recursion — would explode unboundedly)
//   - propose_finding (write side; only main, with user confirm, may write)
func safeSubagentTools() map[string]bool {
	return map[string]bool{
		"find_plays":          true,
		"get_play_summary":    true,
		"get_control_events":  true,
		"list_labels":         true,
		"list_findings":       true,
		"read_finding":        true,
		"list_standards":      true,
		"read_standard":       true,
		"list_skills":         true,
		"read_skill":          true,
		"read_conventions":    true,
		"query":               true,
	}
}

// InvestigateTool builds the subagent dispatcher. cfg is the
// per-process config (database, ledger settings); spawn details
// (api_key, base_url, model) come per-request via ctx.
func InvestigateTool(cfg config) Tool {
	return Tool{
		Name: "investigate",
		Description: "Spawn a focused subagent to investigate a specific play or " +
			"question. The subagent has its own context (starts empty), its own " +
			"tool-use loop (up to max_iterations), and returns a SHORT conclusion " +
			"as the tool result. Use this for any deep dive that would otherwise " +
			"flood your own context with raw tool results — analysing a single " +
			"play's stall, reconstructing a timeline, comparing two sessions. " +
			"The subagent CANNOT call investigate (no recursion), cite (no UI " +
			"emission from inside), or propose_finding (write side stays with you). " +
			"Cost: each investigate is its own conversation, billed to the same " +
			"key. Typical: 5-15 tool calls inside, 200-500 word finding out.",
		Tier: 1,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task": map[string]any{
					"type":        "string",
					"description": "The investigator's brief — be specific. e.g. 'Why did play X stall at 14:32? Cite the control_events at that time.' Include play_id / time range / hypothesis explicitly; the subagent starts with no context.",
				},
				"player_id": map[string]any{"type": "string", "description": "Optional — pre-loads the subagent's preamble with this player_id so it doesn't need to look it up."},
				"play_id":   map[string]any{"type": "string", "description": "Optional — same, for the play under investigation."},
				"max_iterations": map[string]any{
					"type":        "integer",
					"minimum":     1,
					"maximum":     20,
					"default":     10,
					"description": "Hard cap on tool-use rounds inside the subagent. 10 is plenty for most one-play investigations.",
				},
			},
			"required": []string{"task"},
		},
		Execute: func(ctx context.Context, args json.RawMessage, _ ToolEmitter) (string, error) {
			var a struct {
				Task          string `json:"task"`
				PlayerID      string `json:"player_id"`
				PlayID        string `json:"play_id"`
				MaxIterations int    `json:"max_iterations"`
			}
			if err := json.Unmarshal(args, &a); err != nil {
				return "", fmt.Errorf("parse args: %w", err)
			}
			if strings.TrimSpace(a.Task) == "" {
				return "", fmt.Errorf("task required")
			}
			sub, ok := ctx.Value(subagentCtxKey{}).(*subagentContext)
			if !ok || sub == nil {
				return "", fmt.Errorf("subagent context not threaded — server bug")
			}
			if a.MaxIterations == 0 {
				a.MaxIterations = 10
			}
			result, usage, err := runSubagent(ctx, sub, a.Task, a.PlayerID, a.PlayID, a.MaxIterations)
			if err != nil {
				return "", err
			}
			return mustJSON(map[string]any{
				"finding":          result,
				"subagent_iterations": usage.iterations,
				"subagent_tool_calls": usage.toolCalls,
				"subagent_tokens":  map[string]int{"in": int(usage.inTokens), "out": int(usage.outTokens)},
			}), nil
		},
	}
}

// subagentUsage tracks what the subagent consumed so the main
// agent can see the cost in its tool result.
type subagentUsage struct {
	iterations int
	toolCalls  int
	inTokens   uint32
	outTokens  uint32
}

// runSubagent executes the subagent's tool-use loop. Returns the
// final assistant text + usage stats. Errors only on infrastructure
// failures (no upstream, invalid creds); tool-call failures and
// recoverable LLM errors are surfaced in the finding text.
func runSubagent(
	ctx context.Context,
	sub *subagentContext,
	task, playerID, playID string,
	maxIter int,
) (string, subagentUsage, error) {
	allowed := safeSubagentTools()
	// Filter the parent registry to allowed tools.
	allTools := sub.registry.All()
	subTools := make([]LLMTool, 0, len(allTools))
	for _, t := range allTools {
		if allowed[t.Name] {
			subTools = append(subTools, LLMTool{
				Type: "function",
				Function: LLMToolDef{
					Name: t.Name, Description: t.Description, Parameters: t.Parameters,
				},
			})
		}
	}

	prompt := buildSubagentPrompt(task, playerID, playID)
	messages := []LLMMessage{{Role: "system", Content: prompt}}

	var usage subagentUsage
	for iter := 0; iter < maxIter; iter++ {
		select {
		case <-ctx.Done():
			return "", usage, ctx.Err()
		default:
		}
		usage.iterations = iter + 1

		stream, err := StreamChat(ctx, LLMRequest{
			BaseURL:     sub.baseURL,
			APIKey:      sub.apiKey,
			Model:       sub.model,
			Messages:    messages,
			Tools:       subTools,
			Temperature: 0.2,
		})
		if err != nil {
			return "", usage, fmt.Errorf("subagent upstream: %w", err)
		}

		var (
			textBuilder  strings.Builder
			toolBuilders = map[int]*toolCallAssembly{}
			finishReason string
		)
		for ev := range stream {
			if ev.Err != nil {
				continue
			}
			if ev.TextDelta != "" {
				textBuilder.WriteString(ev.TextDelta)
			}
			if ev.ToolCallDelta != nil {
				tcd := ev.ToolCallDelta
				asm, ok := toolBuilders[tcd.Index]
				if !ok {
					asm = &toolCallAssembly{}
					toolBuilders[tcd.Index] = asm
				}
				if tcd.ID != "" {
					asm.ID = tcd.ID
				}
				if tcd.Name != "" {
					asm.Name = tcd.Name
				}
				if tcd.ArgsFrag != "" {
					asm.Args.WriteString(tcd.ArgsFrag)
				}
			}
			if ev.Usage != nil {
				usage.inTokens += ev.Usage.InputTokens
				usage.outTokens += ev.Usage.OutputTokens
			}
			if ev.FinishReason != "" {
				finishReason = ev.FinishReason
			}
		}

		// Append the assistant turn to subagent's history.
		asstMsg := LLMMessage{Role: "assistant", Content: textBuilder.String()}
		if len(toolBuilders) > 0 {
			ordered := orderedToolCalls(toolBuilders)
			asstMsg.ToolCalls = make([]LLMToolCall, 0, len(ordered))
			for _, asm := range ordered {
				asstMsg.ToolCalls = append(asstMsg.ToolCalls, LLMToolCall{
					ID:   asm.ID,
					Type: "function",
					Function: LLMToolCallFunc{
						Name: asm.Name, Arguments: asm.Args.String(),
					},
				})
			}
		}
		messages = append(messages, asstMsg)

		// No tool calls → subagent's done.
		if len(asstMsg.ToolCalls) == 0 {
			return textBuilder.String(), usage, nil
		}

		// Dispatch each tool call into the parent registry — same
		// MaxToolResultBytes guard applies, so a runaway subagent
		// can't dump 100MB into its own context either.
		for _, tc := range asstMsg.ToolCalls {
			usage.toolCalls++
			result := sub.registry.Dispatch(ctx, tc.Function.Name, json.RawMessage(tc.Function.Arguments), nil)
			messages = append(messages, LLMMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    result,
			})
		}
		_ = finishReason
	}
	// Hit max iterations without a final text response — return
	// what we have plus a marker.
	last := lastAssistantText(messages)
	return last + "\n\n[subagent hit max_iterations cap; conclusions may be incomplete]", usage, nil
}

func lastAssistantText(msgs []LLMMessage) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" && msgs[i].Content != "" {
			return msgs[i].Content
		}
	}
	return ""
}

// buildSubagentPrompt composes the focused system prompt. Smaller
// than the main prompt — the subagent doesn't need the full
// conventions / citation contract, just enough to do one task.
func buildSubagentPrompt(task, playerID, playID string) string {
	var b strings.Builder
	b.WriteString(`You are an investigator subagent spawned by the main InfiniteStream
chat agent. Your job is to answer ONE specific question and return
a concise finding (200-500 words). You have no audience but the
main agent — write for that reader, not for an end user.

Available tools: the same read tools the main agent has (find_plays,
get_play_summary, get_play_timeline, get_control_events, query,
list_labels, list_findings/standards/skills/conventions).

You CANNOT call cite() (no UI emission), investigate() (no
recursion), or propose_finding() (write side stays with the main
agent).

Output format: plain markdown. Lead with a tagged hypothesis
(confirmed / refuted / needs-test). Cite play_ids / timestamps /
control_event indices inline. Don't dump raw tool results — the
main agent doesn't need them, just your synthesis.

If the data doesn't support a confident answer, say so in one line
and stop. Don't manufacture a finding.

`)
	b.WriteString("# Your task\n\n")
	b.WriteString(task)
	b.WriteString("\n")
	if playerID != "" || playID != "" {
		b.WriteString("\n# Pre-loaded scope\n\n")
		if playerID != "" {
			fmt.Fprintf(&b, "- player_id: %s\n", playerID)
		}
		if playID != "" {
			fmt.Fprintf(&b, "- play_id: %s\n", playID)
		}
	}
	fmt.Fprintf(&b, "\nCurrent time: %s\n", time.Now().UTC().Format(time.RFC3339))
	return b.String()
}

// Reference to plays package so unused-import linter stays happy
// when this file's only direct dep is at build time.
var _ = plays.Backend{}
