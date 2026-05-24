package main

// llm_session_chat.go — the /api/v2/chat SSE endpoint (#497).
//
// Wire shape: POST with a JSON body carrying the user's chosen
// profile + model + api_key (BYO key, browser localStorage) + the
// chat history + an optional scope. Response: SSE stream of typed
// events the Vue chat panel consumes.
//
// Event types (the `event:` line in each SSE chunk; data is JSON):
//   text_delta   { delta: string }              — assistant token
//   tool_call    { id, name, args }             — model invoked a tool
//   tool_result  { id, ok, summary }            — tool returned
//   citation     { span_id, kind, ... }         — emitted by cite()
//   usage        { input_tokens, output_tokens,
//                  cost_usd, duration_ms,
//                  tool_calls_count }
//   done         {}
//   error        { kind, message }
//
// The loop:
//   1. Build full history: [system, ...user-supplied messages]
//   2. StreamChat → accumulate text + tool_calls
//   3. If finish_reason = tool_calls, dispatch each, append
//      assistant + tool messages, loop
//   4. Else break, compute usage, write ledger, send done

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

//go:embed prompts/chat.md
var embeddedSystemPrompt string

// ChatRequest is the wire body POSTed to /api/v2/chat.
type ChatRequest struct {
	// Conversation correlation. Generated client-side; the server
	// uses it as the chat_id on every ledger row. If empty, the
	// server mints one and returns it on the first SSE frame.
	ChatID string `json:"chat_id"`

	// Profile selection. Profile must be a template name from the
	// catalog (/api/v2/chat/profiles); Model can be any string —
	// it's passed to the upstream verbatim. If the (profile, model)
	// pair isn't in the catalog, cost tracking goes to "unknown"
	// (-1) but the call still runs.
	Profile string `json:"profile"`
	Model   string `json:"model"`
	APIKey  string `json:"api_key"`
	BaseURL string `json:"base_url"` // optional — overrides the template's default

	// Conversation history. The server prepends the system prompt;
	// the rest is whatever the dashboard's chat panel persisted.
	Messages []LLMMessage `json:"messages"`

	// Scope hint for the system prompt + ledger. Optional.
	Scope ChatScope `json:"scope"`

	// One-shot mode: a single user turn → single assistant turn.
	// Tool-use within that single turn is still allowed.
	OneShot bool `json:"one_shot"`

	// Temperature override (0.0–1.0). Default 0.2.
	Temperature float64 `json:"temperature"`
}

// ChatScope tells the system prompt + ledger what context the
// dashboard is asking from.
type ChatScope struct {
	Kind     string `json:"kind"`                // "fleet" | "play" | "range" | "characterization" | ""
	PlayerID string `json:"player_id,omitempty"` // for "play" / "range" — bot uses it to build citations
	PlayID   string `json:"play_id,omitempty"`
	From     string `json:"from,omitempty"`      // for "range"
	To       string `json:"to,omitempty"`
	RunID    string `json:"run_id,omitempty"`    // for "characterization"
	Cycle    int    `json:"cycle,omitempty"`
}

// chatHandler is the registered handler for POST /api/v2/chat.
// Holds the per-process tool registry + cached catalog. Built once
// at startup via newChatHandler.
type chatHandler struct {
	cfg        config
	registry   *ToolRegistry
	systemPrompt string
}

// newChatHandler builds the chat backend. Tools are registered
// once; the system prompt is read from cfg.llmPromptPath once.
// If cfg.llmPromptPath is empty a built-in minimal prompt is used.
func newChatHandler(cfg config) (*chatHandler, error) {
	reg := NewToolRegistry()
	reg.RegisterAll(Tier1Tools(cfg))
	reg.RegisterAll(Tier2Tools(cfg, cfg.claudeDir))
	reg.Register(CiteTool())
	reg.Register(QueryTool(cfg))
	reg.Register(InvestigateTool(cfg))
	reg.Register(ProposeFindingTool())

	prompt := embeddedSystemPrompt
	if cfg.llmPromptPath != "" {
		body, err := os.ReadFile(cfg.llmPromptPath)
		if err != nil {
			return nil, fmt.Errorf("read system prompt %s: %w", cfg.llmPromptPath, err)
		}
		prompt = string(body)
	}
	return &chatHandler{cfg: cfg, registry: reg, systemPrompt: prompt}, nil
}

// ServeHTTP routes POST → chat, GET /profiles → catalog, GET /budget
// → budget. mountChatHandlers wires the actual paths.
func mountChatHandlers(mux *http.ServeMux, h *chatHandler) {
	mux.HandleFunc("/api/v2/chat", h.handleChat)
	mux.HandleFunc("/api/v2/chat/profiles", h.handleProfiles)
	mux.HandleFunc("/api/v2/chat/budget", h.handleBudget)
	mux.HandleFunc("/api/v2/chat/discover-models", h.handleDiscoverModels)
	mux.HandleFunc("/api/v2/chat/findings/save", h.handleSaveFinding)
}

// handleSaveFinding writes a proposed finding to disk. Called by
// the dashboard when the operator clicks Save on a finding_proposed
// card. The forwarder NEVER writes a finding from the propose
// tool directly — the operator's click is the commit signal.
//
// Body: { slug: "...", markdown: "..." }
// Returns: { ok: true, path: "..." } or { ok: false, error: "..." }
func (h *chatHandler) handleSaveFinding(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeProblemv2(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}
	var body struct {
		Slug     string `json:"slug"`
		Markdown string `json:"markdown"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeProblemv2(w, http.StatusBadRequest, "bad request", err.Error())
		return
	}
	path, err := saveFinding(h.cfg, body.Slug, body.Markdown)
	if err != nil {
		writeJSONv2(w, http.StatusBadRequest, map[string]any{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}
	writeJSONv2(w, http.StatusOK, map[string]any{
		"ok":   true,
		"path": path,
		"note": "file written to disk — commit to git when ready (the forwarder doesn't touch git)",
	})
}

// handleProfiles returns the catalog. No secrets — pure config.
func (h *chatHandler) handleProfiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeProblemv2(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}
	cat := LLMCatalogValue()
	if cat == nil {
		writeProblemv2(w, http.StatusServiceUnavailable, "catalog not loaded", "")
		return
	}
	writeJSONv2(w, http.StatusOK, cat)
}

// handleDiscoverModels proxies the OAI-compat GET /v1/models call
// to a user-supplied {base_url, api_key}. The browser can't always
// do this directly — hosted providers (Anthropic, OpenAI, HF) don't
// send CORS headers because they're not browser-facing APIs. The
// forwarder has no such restriction.
//
// Body: { base_url: "https://...", api_key: "..." }
// Returns: { models: ["id1", "id2", ...], source: "server" }
// or { error: "...", models: [], source: "server" } on upstream failure.
func (h *chatHandler) handleDiscoverModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeProblemv2(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}
	var body struct {
		BaseURL string `json:"base_url"`
		APIKey  string `json:"api_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeProblemv2(w, http.StatusBadRequest, "bad request", err.Error())
		return
	}
	if body.BaseURL == "" {
		writeProblemv2(w, http.StatusBadRequest, "bad request", "base_url required")
		return
	}
	url := strings.TrimRight(body.BaseURL, "/") + "/models"

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		writeJSONv2(w, http.StatusOK, map[string]any{
			"models": []string{},
			"source": "server",
			"error":  err.Error(),
		})
		return
	}
	req.Header.Set("Accept", "application/json")
	if body.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+body.APIKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		writeJSONv2(w, http.StatusOK, map[string]any{
			"models": []string{},
			"source": "server",
			"error":  err.Error(),
		})
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		writeJSONv2(w, http.StatusOK, map[string]any{
			"models": []string{},
			"source": "server",
			"error":  fmt.Sprintf("upstream %d", resp.StatusCode),
		})
		return
	}
	var upstream struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&upstream); err != nil {
		writeJSONv2(w, http.StatusOK, map[string]any{
			"models": []string{},
			"source": "server",
			"error":  fmt.Sprintf("parse upstream: %s", err.Error()),
		})
		return
	}
	ids := make([]string, 0, len(upstream.Data))
	for _, m := range upstream.Data {
		if m.ID != "" {
			ids = append(ids, m.ID)
		}
	}
	writeJSONv2(w, http.StatusOK, map[string]any{
		"models": ids,
		"source": "server",
	})
}

// handleBudget returns the current global budget state.
func (h *chatHandler) handleBudget(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeProblemv2(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}
	status, err := ReadBudget(r.Context(), h.cfg, h.cfg.llmBudgetUSD)
	if err != nil {
		writeProblemv2(w, http.StatusBadGateway, "ledger read failed", err.Error())
		return
	}
	writeJSONv2(w, http.StatusOK, status)
}

// handleChat is the main entry. Streams an SSE response.
func (h *chatHandler) handleChat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeProblemv2(w, http.StatusMethodNotAllowed, "method not allowed", "")
		return
	}
	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeProblemv2(w, http.StatusBadRequest, "bad request", err.Error())
		return
	}
	if req.ChatID == "" {
		req.ChatID = newID(12)
	}
	if len(req.Messages) == 0 {
		writeProblemv2(w, http.StatusBadRequest, "bad request", "messages required")
		return
	}

	// Resolve profile + base_url.
	cat := LLMCatalogValue()
	if cat == nil {
		writeProblemv2(w, http.StatusServiceUnavailable, "catalog not loaded", "")
		return
	}
	tmpl := cat.FindTemplate(req.Profile)
	if tmpl == nil {
		writeProblemv2(w, http.StatusBadRequest, "unknown profile", req.Profile)
		return
	}
	baseURL := req.BaseURL
	if baseURL == "" {
		baseURL = tmpl.BaseURL
	}
	if tmpl.RequiresAPIKey && req.APIKey == "" {
		writeProblemv2(w, http.StatusBadRequest, "api_key required", tmpl.APIKeyHelp)
		return
	}
	if req.Model == "" && len(tmpl.Models) > 0 {
		req.Model = tmpl.Models[0].ID
	}

	// Sanitise inbound history — Anthropic (and OpenAI's strict
	// mode) reject assistant messages with no content AND no
	// tool_calls. Such messages slip into localStorage when a turn
	// errors mid-stream (e.g. the previous Anthropic 404 left
	// content=""). Drop them so the historic conversation is sendable
	// regardless of how it got into that state. Also drop any orphan
	// tool messages whose tool_call_id no longer points at an
	// assistant message — same shape of corruption from cancelled
	// mid-loop turns.
	req.Messages = sanitiseHistory(req.Messages)

	// Trim old fat tool results so the conversation doesn't grow
	// unboundedly. A typical find_plays call returns 20-50 KB of
	// JSON; carrying 10 of those in history blows past every
	// model's context window. Keep the most recent 3 tool results
	// verbatim, stub out older ones whose body is >2 KB.
	req.Messages = trimOldToolResults(req.Messages)

	// Pre-flight budget check.
	if h.cfg.llmBudgetUSD > 0 {
		spent, err := SpentTodayUSD(r.Context(), h.cfg)
		if err == nil && spent >= h.cfg.llmBudgetUSD {
			writeProblemv2(w, http.StatusTooManyRequests, "daily budget exceeded",
				fmt.Sprintf("$%.2f / $%.2f used today; resets at UTC midnight", spent, h.cfg.llmBudgetUSD))
			return
		}
	}

	// Stash subagent context for the investigate tool — see
	// llm_tool_investigate.go. The tool retrieves this via
	// ctx.Value(subagentCtxKey{}) and uses the same creds/profile
	// to spawn an inner chat loop with a restricted tool set.
	subCtx := &subagentContext{
		cfg:      h.cfg,
		baseURL:  baseURL,
		apiKey:   req.APIKey,
		profile:  req.Profile,
		model:    req.Model,
		registry: h.registry,
	}
	r = r.WithContext(context.WithValue(r.Context(), subagentCtxKey{}, subCtx))

	h.streamChat(w, r, req, tmpl, baseURL, cat)
}

// streamChat does the actual SSE response + tool-use loop. Split
// from handleChat to keep that function under "router-shaped."
func (h *chatHandler) streamChat(
	w http.ResponseWriter,
	r *http.Request,
	req ChatRequest,
	tmpl *LLMTemplate,
	baseURL string,
	cat *LLMCatalog,
) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	flusher, _ := w.(http.Flusher)

	// Emitter shared by streaming path + tool side-channel.
	var emitMu sync.Mutex
	emit := func(eventType string, payload any) error {
		emitMu.Lock()
		defer emitMu.Unlock()
		buf, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, string(buf)); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}

	// First frame: chat_id + request_id so the client correlates.
	requestID := newID(8)
	_ = emit("meta", map[string]any{"chat_id": req.ChatID, "request_id": requestID})

	// Build initial history: system prompt + user-supplied messages.
	history := make([]LLMMessage, 0, len(req.Messages)+1)
	history = append(history, LLMMessage{
		Role:    "system",
		Content: h.buildSystemPrompt(req.Scope),
	})
	history = append(history, req.Messages...)

	startedAt := time.Now()
	usage := LLMUsage{}
	toolCallsCount := 0
	status := LLMStatusOK
	errKind := ""

	// Track tool budget across the loop. Each call increments by 1;
	// once the cap is hit we stop dispatching but let the LLM
	// finish whatever it was streaming.
	maxToolCalls := h.cfg.llmMaxToolCalls
	if maxToolCalls <= 0 {
		maxToolCalls = 20
	}

loop:
	for iter := 0; iter < 50; iter++ { // hard ceiling on loop iterations
		select {
		case <-r.Context().Done():
			status = LLMStatusCancelled
			break loop
		default:
		}

		stream, err := StreamChat(r.Context(), LLMRequest{
			BaseURL:     baseURL,
			APIKey:      req.APIKey,
			Model:       req.Model,
			Messages:    history,
			Tools:       h.registry.ToOpenAITools(),
			Temperature: orDefault(req.Temperature, 0.2),
		})
		if err != nil {
			status = LLMStatusError
			errKind = "upstream_open"
			_ = emit("error", map[string]any{"kind": errKind, "message": err.Error()})
			break loop
		}

		// Accumulate this iteration's outputs.
		var (
			textBuilder  strings.Builder
			toolBuilders = map[int]*toolCallAssembly{}
			finishReason = ""
		)

		for ev := range stream {
			if ev.Err != nil {
				_ = emit("error", map[string]any{"kind": "stream", "message": ev.Err.Error()})
				continue
			}
			if ev.TextDelta != "" {
				textBuilder.WriteString(ev.TextDelta)
				_ = emit("text_delta", map[string]any{"delta": ev.TextDelta})
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
				usage.InputTokens += ev.Usage.InputTokens
				usage.OutputTokens += ev.Usage.OutputTokens
			}
			if ev.FinishReason != "" {
				finishReason = ev.FinishReason
			}
		}

		// Append the assistant message that produced this iteration.
		asstMsg := LLMMessage{Role: "assistant", Content: textBuilder.String()}
		if len(toolBuilders) > 0 {
			ordered := orderedToolCalls(toolBuilders)
			asstMsg.ToolCalls = make([]LLMToolCall, 0, len(ordered))
			for _, asm := range ordered {
				asstMsg.ToolCalls = append(asstMsg.ToolCalls, LLMToolCall{
					ID:   asm.ID,
					Type: "function",
					Function: LLMToolCallFunc{
						Name:      asm.Name,
						Arguments: asm.Args.String(),
					},
				})
			}
		}
		history = append(history, asstMsg)

		// If no tool calls, we're done.
		if len(asstMsg.ToolCalls) == 0 {
			break loop
		}

		// Dispatch each tool call. Append a tool message per call.
		for _, tc := range asstMsg.ToolCalls {
			if toolCallsCount >= maxToolCalls {
				_ = emit("error", map[string]any{
					"kind":    "tool_budget",
					"message": fmt.Sprintf("tool call budget %d exhausted; stopping", maxToolCalls),
				})
				break loop
			}
			toolCallsCount++
			_ = emit("tool_call", map[string]any{
				"id":   tc.ID,
				"name": tc.Function.Name,
				"args": tc.Function.Arguments,
			})
			result := h.registry.Dispatch(
				r.Context(),
				tc.Function.Name,
				json.RawMessage(tc.Function.Arguments),
				emit,
			)
			_ = emit("tool_result", map[string]any{
				"id":      tc.ID,
				"ok":      !strings.HasPrefix(result, `{"error"`),
				"summary": summarize(result, 200),
			})
			history = append(history, LLMMessage{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    result,
			})
		}

		// In one-shot mode, the iteration after tool dispatch is the
		// LLM's final answer — we let it complete, then break on
		// its next finish_reason=stop. Multi-turn mode is the same;
		// the loop continues until the LLM stops calling tools.
		_ = finishReason
		_ = req.OneShot
	}

	// Compute cost, write ledger, emit usage + done.
	durationMs := uint32(time.Since(startedAt).Milliseconds())
	costUSD := cat.CostUSD(req.Profile, req.Model, usage.InputTokens, usage.OutputTokens)

	_ = emit("usage", map[string]any{
		"input_tokens":     usage.InputTokens,
		"output_tokens":    usage.OutputTokens,
		"cost_usd":         costUSD,
		"duration_ms":      durationMs,
		"tool_calls_count": toolCallsCount,
	})

	// Ledger write — best effort.
	row := LLMCallRow{
		Ts:             time.Now().UTC().Format("2006-01-02 15:04:05.000"),
		ChatID:         req.ChatID,
		RequestID:      requestID,
		KeyHash:        HashAPIKey(req.APIKey),
		Profile:        req.Profile,
		BaseURL:        baseURL,
		Model:          req.Model,
		OneShot:        bool2u8(req.OneShot),
		ScopeKind:      req.Scope.Kind,
		ScopePlayID:    req.Scope.PlayID,
		ScopeRunID:     req.Scope.RunID,
		InputTokens:    usage.InputTokens,
		OutputTokens:   usage.OutputTokens,
		CostUSD:        costUSD,
		DurationMs:     durationMs,
		ToolCallsCount: uint16(toolCallsCount),
		Status:         status,
		ErrorKind:      errKind,
		PromptVersion:  promptVersion(h.systemPrompt),
	}
	if err := InsertLLMCall(context.Background(), h.cfg, row); err != nil {
		log.Printf("llm_calls insert failed: %v", err)
	}

	_ = emit("done", map[string]any{})
}

// Tool-result trimming bounds. These are conservative defaults —
// the bot still gets recent verbatim results to chain reasoning,
// while older ones shrink to a stub so the conversation doesn't
// drift into context-overflow territory after ~10 tool-heavy turns.
const (
	toolKeepRecent     = 3    // last N tool messages stay verbatim
	toolTrimThreshold  = 2000 // older messages > this byte count get stubbed
)

// trimOldToolResults replaces the content of old tool messages
// with a tiny JSON stub describing what was there. Only fires on
// messages older than toolKeepRecent AND larger than the
// threshold — small results (e.g. cite() acknowledgements) pass
// through untouched even when far back in history.
//
// The stub format teaches the LLM that data was here but is no
// longer in context — so it can either ask the user, re-call the
// tool, or proceed with reasoning from earlier text it kept.
func trimOldToolResults(in []LLMMessage) []LLMMessage {
	// Find all tool-message indices in reverse so we can split
	// "recent N" from "older candidates".
	toolIndices := []int{}
	for i := len(in) - 1; i >= 0; i-- {
		if in[i].Role == "tool" {
			toolIndices = append(toolIndices, i)
		}
	}
	if len(toolIndices) <= toolKeepRecent {
		return in
	}
	out := make([]LLMMessage, len(in))
	copy(out, in)
	for _, idx := range toolIndices[toolKeepRecent:] {
		msg := out[idx]
		if len(msg.Content) <= toolTrimThreshold {
			continue
		}
		stub := fmt.Sprintf(
			`{"_truncated":true,"_tool":%q,"_orig_bytes":%d,"_note":"older tool result trimmed for context budget; re-call the tool or ask the user if you need this data"}`,
			msg.Name, len(msg.Content),
		)
		out[idx] = LLMMessage{
			Role:       msg.Role,
			ToolCallID: msg.ToolCallID,
			Name:       msg.Name,
			Content:    stub,
		}
	}
	return out
}

// sanitiseHistory drops messages that would make the upstream
// reject the whole turn:
//   - assistant messages with no content AND no tool_calls (e.g.
//     left behind by a previous turn that errored mid-stream)
//   - tool messages whose tool_call_id doesn't match a tool_call
//     in any preceding assistant message (orphan from cancelled
//     mid-loop turn)
//
// Single forward pass so we can build the valid tool_call_id set
// as we go.
func sanitiseHistory(in []LLMMessage) []LLMMessage {
	out := make([]LLMMessage, 0, len(in))
	validToolCallIDs := map[string]struct{}{}
	for _, m := range in {
		switch m.Role {
		case "assistant":
			if m.Content == "" && len(m.ToolCalls) == 0 {
				// Empty assistant turn — drop.
				continue
			}
			for _, tc := range m.ToolCalls {
				validToolCallIDs[tc.ID] = struct{}{}
			}
		case "tool":
			if _, ok := validToolCallIDs[m.ToolCallID]; !ok {
				// Orphan tool response — drop.
				continue
			}
		}
		out = append(out, m)
	}
	return out
}

// toolCallAssembly accumulates a single tool call's incremental
// arrival.
type toolCallAssembly struct {
	ID   string
	Name string
	Args strings.Builder
}

func orderedToolCalls(m map[int]*toolCallAssembly) []*toolCallAssembly {
	// OAI's tool_calls[].index defines submit order; we sort by it.
	keys := make([]int, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	out := make([]*toolCallAssembly, len(keys))
	for i, k := range keys {
		out[i] = m[k]
	}
	return out
}

func newID(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return hex.EncodeToString(buf)
}

func orDefault(v, def float64) float64 {
	if v <= 0 {
		return def
	}
	return v
}

func bool2u8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

func summarize(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// promptVersion peeks the prompt's YAML frontmatter for a
// `prompt_version:` line and returns its value, or "v1" by default.
func promptVersion(prompt string) string {
	for _, line := range strings.Split(prompt, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "prompt_version:") {
			v := strings.TrimSpace(strings.TrimPrefix(line, "prompt_version:"))
			v = strings.Trim(v, `"'`)
			if v != "" {
				return v
			}
		}
		if line == "---" || line == "" {
			continue
		}
		// Bail at first non-frontmatter line.
		if !strings.Contains(line, ":") {
			break
		}
	}
	return "v1"
}

// buildSystemPrompt prepends a small scope preamble to the configured
// system prompt so the model knows what the dashboard is asking about.
func (h *chatHandler) buildSystemPrompt(scope ChatScope) string {
	var b strings.Builder
	b.WriteString(h.systemPrompt)
	if scope.Kind != "" {
		b.WriteString("\n\n# Current scope\n\n")
		switch scope.Kind {
		case "play":
			fmt.Fprintf(&b, "The user is looking at a single play. "+
				"player_id=%s, play_id=%s. Anchor your reasoning to this play; "+
				"use get_play_summary first. When you cite this play, you already "+
				"have both IDs — pass them straight to cite() without re-fetching.\n",
				scope.PlayerID, scope.PlayID)
		case "range":
			fmt.Fprintf(&b, "The user has brushed a time range on a play. "+
				"player_id=%s, play_id=%s, from=%s, to=%s. Focus on events inside "+
				"this window; use surrounding context as needed. Both IDs are in "+
				"hand for cite() — no get_play_summary needed just to deep-link.\n",
				scope.PlayerID, scope.PlayID, scope.From, scope.To)
		case "fleet":
			b.WriteString("The user is on the plays picker — fleet-wide question. " +
				"Use find_plays to find candidates; cite each clickable result.\n")
		case "characterization":
			fmt.Fprintf(&b, "The user is on a characterization run. run_id=%s, cycle=%d.\n",
				scope.RunID, scope.Cycle)
		}
	}
	return b.String()
}

