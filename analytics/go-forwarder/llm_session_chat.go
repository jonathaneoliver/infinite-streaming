// SSE chat endpoint for the AI session-analysis path (epic #412).
//
// POST /api/session_chat — runs RunChatTurn with QueryTool wired up,
// streams progress to the browser as SSE events. The Discuss panel
// (#419) consumes this directly; the Analyze button uses one_shot=true
// for a single turn.
//
// GET /api/llm_profiles — list of available profiles, used by the
// model-picker dropdown.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sashabaranov/go-openai"
)

// heartbeatIntervalForTest is the SSE keepalive cadence. Production
// default is 15 s — well below nginx's 60 s `proxy_read_timeout` and
// most corporate intermediaries. Tests override to short intervals
// to verify the heartbeat fires within their deadlines.
var heartbeatIntervalForTest = 15 * time.Second

// chatRequest is the POST body shape. profile is optional (falls
// back to active); session_id and range get injected into the
// system message when present.
type chatRequest struct {
	Profile   string                            `json:"profile,omitempty"`
	Messages  []openai.ChatCompletionMessage    `json:"messages"`
	SessionID string                            `json:"session_id,omitempty"`
	Sessions  []string                          `json:"sessions,omitempty"`
	Range     *struct {
		From int64 `json:"from"`
		To   int64 `json:"to"`
	} `json:"range,omitempty"`
	OneShot bool `json:"one_shot,omitempty"`
}

// llmProfileSummary is the shape emitted to the UI. Mirrors most
// of LLMProfile but excludes the API key env name from the response
// since UI code never needs it.
type llmProfileSummary struct {
	Name            string  `json:"name"`
	Model           string  `json:"model"`
	BaseURL         string  `json:"base_url"`
	SupportsTools   bool    `json:"supports_tools"`
	SupportsCaching bool    `json:"supports_caching"`
	Available       bool    `json:"available"`
	Active          bool    `json:"active"`
	Pricing         Pricing `json:"pricing"`
}

// registerLLMHandlers wires the chat + profiles + budget handlers
// into the existing forwarder mux. Caller owns the mux. The
// `/api/llm_profiles` and `/api/llm_budget` endpoints are always
// registered so UIs can poll for the `enabled` / `cap` state;
// `/api/session_chat` is only registered when profiles loaded
// (otherwise it 404s, which is the right shape for "feature off").
func registerLLMHandlers(mux *http.ServeMux, cfg config) {
	ledger := NewLLMLedger(cfg.clickhouseURL, cfg.chDatabase)
	dailyCap := cfg.llmDailyBudgetUSD
	if dailyCap <= 0 {
		dailyCap = defaultDailyBudgetUSD
	}
	maxTok := cfg.llmMaxInputTokens
	if maxTok <= 0 {
		maxTok = defaultMaxInputTokensPerCall
	}

	mux.HandleFunc("/api/llm_profiles", serveLLMProfiles)
	mux.HandleFunc("/api/llm_budget", func(w http.ResponseWriter, r *http.Request) {
		serveLLMBudget(w, r, ledger, dailyCap)
	})
	if llmProfiles == nil {
		log.Printf("AI session_chat disabled (no profiles loaded); /api/llm_profiles + /api/llm_budget still serve")
		return
	}
	queryTool := NewQueryTool(cfg.clickhouseURL)
	client := NewLLMClient(llmProfiles)
	mux.HandleFunc("/api/session_chat", func(w http.ResponseWriter, r *http.Request) {
		handleSessionChat(w, r, client, queryTool, ledger, dailyCap, maxTok)
	})
}

// serveLLMProfiles returns the list (and the `enabled` flag) for the
// UI dropdown. Always 200; UIs check `enabled` rather than disambig-
// uating 404 vs. real network errors.
func serveLLMProfiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if llmProfiles == nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"enabled": false, "profiles": []any{}})
		return
	}
	out := []llmProfileSummary{}
	for _, prof := range llmProfiles.Profiles {
		out = append(out, llmProfileSummary{
			Name:            prof.Name,
			Model:           prof.Model,
			BaseURL:         prof.BaseURL,
			SupportsTools:   prof.SupportsTools,
			SupportsCaching: prof.SupportsCaching,
			Available:       prof.Available(),
			Active:          prof.Name == llmProfiles.Active,
			Pricing:         prof.Pricing,
		})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"enabled": true, "profiles": out})
}

func handleSessionChat(w http.ResponseWriter, r *http.Request, client *LLMClient, queryTool *QueryTool, ledger *LLMLedger, dailyBudgetUSD float64, maxInputTokens int) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.Messages) == 0 {
		http.Error(w, "messages required", http.StatusBadRequest)
		return
	}

	// Resolve the profile early so pre-flight ledger entries carry
	// real model names; profile resolution failures are an early 4xx.
	prof, err := llmProfiles.Resolve(req.Profile)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Pre-flight token estimate. tiktoken-style accuracy is a future
	// upgrade; char/4 is conservative within ±25% which is enough
	// for "is this clearly too big?" gating.
	msgsForEstimate := make([]openaiMessageLike, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgsForEstimate = append(msgsForEstimate, openaiMessageLike{Role: m.Role, Content: m.Content})
	}
	if estTok := EstimateInputTokens(msgsForEstimate, ""); estTok > maxInputTokens {
		_ = ledger.WriteCall(r.Context(), LLMCallRecord{
			SessionID:   req.SessionID,
			Profile:     prof.Name,
			Model:       prof.Model,
			Status:      statusInputTooLarge,
			InputTokens: uint32(estTok),
			ErrorKind:   "estimated_input_tokens_over_cap",
			ErrorDetail: fmt.Sprintf("estimated %d tokens > limit %d", estTok, maxInputTokens),
		})
		http.Error(w, fmt.Sprintf("payload too large: estimated %d tokens > limit %d", estTok, maxInputTokens), http.StatusRequestEntityTooLarge)
		return
	}

	// Pre-flight daily budget gate. ClickHouse hiccup → don't refuse;
	// fail-open so a ledger blip never blocks paying users from
	// running a single call (the post-flight insert below will still
	// catch this call once ClickHouse recovers).
	if spent, err := ledger.TodaysSpendUSD(r.Context()); err == nil && spent >= dailyBudgetUSD {
		_ = ledger.WriteCall(r.Context(), LLMCallRecord{
			SessionID:   req.SessionID,
			Profile:     prof.Name,
			Model:       prof.Model,
			Status:      statusBudgetExceeded,
			ErrorKind:   "daily_budget_exhausted",
			ErrorDetail: fmt.Sprintf("today's spend $%.4f >= cap $%.4f", spent, dailyBudgetUSD),
		})
		retryAfter := SecondsUntilUTCMidnight(time.Now())
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retryAfter))
		http.Error(w, fmt.Sprintf("daily budget exhausted ($%.4f spent of $%.2f); resets at 00:00 UTC", spent, dailyBudgetUSD), http.StatusTooManyRequests)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// Disable nginx proxy buffering so events flush in real time.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	sw := newSSEWriter(w, flusher)

	// chatCtx is cancelled by either (a) request ctx cancellation
	// from a properly-detected client disconnect, or (b) the
	// CloseNotifier watchdog below — needed because Go's
	// http.Server only proactively detects HTTP/1.1 disconnect on
	// failed writes, and a long-running upstream LLM call has no
	// writes between heartbeat ticks. Without (b), a closed browser
	// tab would still bill tokens for the in-flight model call.
	chatCtx, chatCancel := context.WithCancel(r.Context())
	defer chatCancel()
	if cn, ok := w.(http.CloseNotifier); ok {
		closeCh := cn.CloseNotify()
		go func() {
			select {
			case <-closeCh:
				chatCancel()
			case <-chatCtx.Done():
			}
		}()
	}

	stopHeartbeat := startSSEHeartbeat(chatCtx, sw, heartbeatIntervalForTest)
	defer stopHeartbeat()

	messages := buildMessagesWithContext(req)
	tools := []openai.Tool{queryTool.OpenAITool()}
	dispatcher := MakeQueryDispatcher(queryTool)

	turnStart := time.Now()
	res, err := client.RunChatTurn(chatCtx, req.Profile, messages, ChatTurnOptions{
		Tools:      tools,
		Dispatcher: dispatcher,
		OnToolCall: func(name, args string) {
			sw.Event("tool_call", map[string]any{"name": name, "arguments": args})
		},
		OnToolResult: func(name, result string) {
			// Don't ship the full payload back through SSE — it's
			// already in res.Messages and would double the bytes
			// the browser handles. Just emit a summary.
			summary := summarizeToolResult(result)
			sw.Event("tool_result", map[string]any{
				"name":       name,
				"rows":       summary.Rows,
				"truncated":  summary.Truncated,
				"elapsed_ms": summary.ElapsedMS,
				"error":      summary.Error,
			})
		},
		OnAssistantText: func(text string) {
			// Single non-streamed final message — name reflects that
			// this is not a per-token delta. Token streaming is a
			// follow-up; UIs treat this as "set the message body"
			// rather than "append delta."
			sw.Event("assistant_message", map[string]any{"content": text})
		},
	})
	stopHeartbeat()

	// Always write a ledger row, even on disconnect / error / cancel.
	// res accumulates per-iteration usage as the loop runs, so a
	// cancellation mid-loop preserves usage from completed iterations.
	// One known limitation: tokens consumed by an in-flight upstream
	// call that gets aborted by ctx cancel are billed by the provider
	// but NOT reflected here — the upstream API doesn't return usage
	// on canceled requests. Acceptable v1 fidelity.
	dur := uint32(time.Since(turnStart).Milliseconds())
	rec := LLMCallRecord{
		SessionID:      req.SessionID,
		Profile:        prof.Name,
		Model:          prof.Model,
		OneShot:        boolToU8(req.OneShot),
		DurationMS:     dur,
		Iterations:     uint16(res.Iterations),
		ToolCallsCount: uint16(res.ToolCallsCount),
		InputTokens:    uint32(res.InputTokens),
		OutputTokens:   uint32(res.OutputTokens),
		CostUSD:        CostUSD(prof, res.InputTokens, res.OutputTokens),
	}
	switch {
	case chatCtx.Err() != nil:
		rec.Status = statusCancelled
	case err != nil:
		rec.Status = statusError
		rec.ErrorKind = "upstream"
		rec.ErrorDetail = truncateErrDetail(err.Error())
	case res.StoppedReason == "tool_budget" || res.StoppedReason == "timeout":
		rec.Status = res.StoppedReason
		rec.ErrorKind = res.StoppedReason
		rec.ErrorDetail = res.StoppedDetail
	default:
		rec.Status = statusOK
	}
	// Use a fresh detached context with the ledger's own timeout so
	// the post-flight write completes even after the client
	// disconnected (which canceled chatCtx and r.Context()). The
	// cancellation branch especially needs this to record real
	// billed usage instead of silently failing.
	writeCtx, writeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	if writeErr := ledger.WriteCall(writeCtx, rec); writeErr != nil {
		log.Printf("ledger write failed (status=%s session=%s): %v", rec.Status, req.SessionID, writeErr)
	}
	writeCancel()

	// If the client disconnected mid-turn, skip writing usage/done
	// into a closed pipe.
	if chatCtx.Err() != nil {
		return
	}

	if err != nil {
		sw.Event("error", map[string]any{"message": err.Error()})
		sw.Event("done", nil)
		return
	}

	sw.Event("usage", map[string]any{
		"input_tokens":     res.InputTokens,
		"output_tokens":    res.OutputTokens,
		"tool_calls_count": res.ToolCallsCount,
		"iterations":       res.Iterations,
		"stopped_reason":   res.StoppedReason,
		"cost_usd":         rec.CostUSD,
	})
	sw.Event("done", nil)
}

func boolToU8(b bool) uint8 {
	if b {
		return 1
	}
	return 0
}

func truncateErrDetail(s string) string {
	const max = 200
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// serveLLMBudget reports today's spend versus the daily cap and the
// number of calls today. Used by the dashboard's budget meter (#419)
// and by automation that wants to predict refusal before posting.
func serveLLMBudget(w http.ResponseWriter, r *http.Request, ledger *LLMLedger, capUSD float64) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	now := time.Now().UTC()
	resetsAt := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	out := map[string]any{
		"cap_usd":         capUSD,
		"resets_at":       resetsAt.Format(time.RFC3339),
		"resets_in_secs":  SecondsUntilUTCMidnight(now),
	}
	// Soft-fail on ClickHouse blips — the meter is informational
	// and should never block UI rendering.
	spent, err := ledger.TodaysSpendUSD(r.Context())
	if err != nil {
		out["error"] = err.Error()
		out["spent_usd"] = 0.0
		out["calls_today"] = 0
	} else {
		out["spent_usd"] = spent
		count, _ := ledger.CallsTodayCount(r.Context())
		out["calls_today"] = count
	}
	_ = json.NewEncoder(w).Encode(out)
}

// buildMessagesWithContext prepends a focus-context system message
// when session_id / sessions / range are set. Contract for multi-turn
// chat: the client must NOT echo system messages from prior turns
// back in `messages` — it should send the user/assistant/tool history
// only and re-supply session_id / range / sessions on each turn. We
// always re-inject so prompt-cacheable backends see a byte-identical
// prefix (sessions slice is sorted for that reason).
func buildMessagesWithContext(req chatRequest) []openai.ChatCompletionMessage {
	messages := req.Messages
	if req.SessionID == "" && len(req.Sessions) == 0 && req.Range == nil {
		return messages
	}
	parts := []string{}
	if req.SessionID != "" {
		parts = append(parts, fmt.Sprintf("Focus session_id: %s", req.SessionID))
	}
	if len(req.Sessions) > 0 {
		// Sort for stable cache key — Anthropic prefix caching needs
		// a byte-identical prefix across calls.
		sorted := append([]string(nil), req.Sessions...)
		sort.Strings(sorted)
		parts = append(parts, fmt.Sprintf("Compare sessions: %v", sorted))
	}
	if req.Range != nil {
		parts = append(parts, fmt.Sprintf("Focus range (ms): %d to %d", req.Range.From, req.Range.To))
	}
	preamble := openai.ChatCompletionMessage{
		Role:    openai.ChatMessageRoleSystem,
		Content: "Session-chat preamble:\n- " + strings.Join(parts, "\n- "),
	}
	out := make([]openai.ChatCompletionMessage, 0, len(messages)+1)
	out = append(out, preamble)
	out = append(out, messages...)
	return out
}

// sseWriter serializes writes to the SSE response across the main
// chat goroutine + the heartbeat ticker so events are framed cleanly.
type sseWriter struct {
	mu      sync.Mutex
	w       http.ResponseWriter
	flusher http.Flusher
}

func newSSEWriter(w http.ResponseWriter, flusher http.Flusher) *sseWriter {
	return &sseWriter{w: w, flusher: flusher}
}

// Event writes one named SSE event with a JSON-encoded data payload.
func (s *sseWriter) Event(event string, data any) {
	var payload []byte
	if data == nil {
		payload = []byte("{}")
	} else {
		b, err := json.Marshal(data)
		if err != nil {
			b = []byte(fmt.Sprintf(`{"error":%q}`, err.Error()))
		}
		payload = b
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", event, payload)
	s.flusher.Flush()
}

// Comment writes a `:` line — valid SSE that browsers ignore. Used
// for keepalive against nginx's default 60s proxy_read_timeout.
func (s *sseWriter) Comment(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprintf(s.w, ": %s\n\n", text)
	s.flusher.Flush()
}

// startSSEHeartbeat fires a `: keepalive` comment every `interval`
// to keep nginx and other intermediates from killing the connection
// during long tool runs. Returns a stop function safe to call
// multiple times — the heartbeat also exits when ctx cancels.
func startSSEHeartbeat(ctx context.Context, sw *sseWriter, interval time.Duration) func() {
	stopCh := make(chan struct{})
	var stopOnce sync.Once
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				sw.Comment("keepalive")
			case <-stopCh:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return func() {
		stopOnce.Do(func() { close(stopCh) })
	}
}

// toolResultSummary is the compact shape we ship over SSE so the
// UI can render "model ran query, got 23 rows in 145ms" without
// double-shipping the rows.
type toolResultSummary struct {
	Rows      int    `json:"rows"`
	Truncated bool   `json:"truncated"`
	ElapsedMS int    `json:"elapsed_ms"`
	Error     string `json:"error,omitempty"`
}

func summarizeToolResult(resultJSON string) toolResultSummary {
	var parsed QueryToolResult
	if err := json.Unmarshal([]byte(resultJSON), &parsed); err != nil {
		// Surface the actual payload prefix so SSE-tailing dev tools
		// can debug a non-query tool whose response shape we don't
		// know yet (#424's control tools, future LLM hallucinations).
		max := 200
		if len(resultJSON) < max {
			max = len(resultJSON)
		}
		return toolResultSummary{Error: "unparseable result: " + resultJSON[:max]}
	}
	return toolResultSummary{
		Rows:      len(parsed.Rows),
		Truncated: parsed.Truncated,
		ElapsedMS: parsed.ElapsedMS,
		Error:     parsed.Error,
	}
}
