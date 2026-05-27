package main

// llm_client_anthropic.go — Anthropic native /v1/messages streaming
// client with prompt-caching support (#511).
//
// Why this exists despite llm_client.go already handling OAI-compat:
// Anthropic's OpenAI-compatibility layer (api.anthropic.com/v1/) does
// NOT support cache_control. See:
//
//   https://platform.claude.com/docs/en/api/openai-sdk
//   > Prompt caching is not supported [via OAI-compat], but it IS
//   > supported in the Anthropic SDK.
//
// To get cache_control on system + tools, we have to drop OAI-compat
// for the anthropic-claude profile only and speak Anthropic's native
// /v1/messages shape directly.
//
// Translation layer between our LLMRequest (OAI-shaped) and Anthropic's
// native shape is the bulk of this file. Streaming SSE parsing for
// the (substantially different) Anthropic stream shape follows.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// AnthropicVersion is the API version header value. Pinned to the
// latest stable as of writing; revisit when Anthropic publishes a
// new version that requires migration. The native API requires this
// header on every request; OAI-compat hides it.
const AnthropicVersion = "2023-06-01"

// anthropicSystemBlock is one text block in the system field. Using
// the array form (not the plain string form) so we can attach
// cache_control to specific blocks — only the embedded chat.md
// portion is cacheable; the dynamic scope preamble + tz preamble
// changes per request and must NOT be cached (a cached-but-wrong
// scope would be worse than no cache).
type anthropicSystemBlock struct {
	Type         string                 `json:"type"` // always "text"
	Text         string                 `json:"text"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

// anthropicCacheControl marks a block as cacheable. `ephemeral`
// gives a 5-minute TTL (default); `1h` is a separately-billed
// extended cache we're not using yet (#511 explicitly defers it).
type anthropicCacheControl struct {
	Type string `json:"type"` // "ephemeral" for our default
}

// anthropicTool is one tool definition. Note `input_schema` (NOT
// the OAI-compat `parameters`) and the absence of a `function`
// wrapper. cache_control on the last tool caches the entire tools
// array.
type anthropicTool struct {
	Name         string                 `json:"name"`
	Description  string                 `json:"description"`
	InputSchema  map[string]any         `json:"input_schema"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

// anthropicMessage is one message in the messages array. Anthropic
// only has user/assistant roles (system is hoisted out). content
// is always an array of blocks (text / tool_use / tool_result).
type anthropicMessage struct {
	Role    string                  `json:"role"` // "user" | "assistant"
	Content []anthropicContentBlock `json:"content"`
}

// anthropicContentBlock is one piece of a message's content. The
// type field discriminates which other fields are populated.
type anthropicContentBlock struct {
	Type string `json:"type"` // "text" | "tool_use" | "tool_result"

	// type=text
	Text string `json:"text,omitempty"`

	// type=tool_use (assistant invoking a tool)
	ID    string         `json:"id,omitempty"`
	Name  string         `json:"name,omitempty"`
	Input map[string]any `json:"input,omitempty"`

	// type=tool_result (user replying with a tool's output)
	ToolUseID string `json:"tool_use_id,omitempty"`
	// Content can be a plain string or array; we always send string.
	ToolContent string `json:"content,omitempty"`
	IsError     bool   `json:"is_error,omitempty"`
}

// anthropicRequest is the full /v1/messages POST body.
type anthropicRequest struct {
	Model       string                 `json:"model"`
	MaxTokens   int                    `json:"max_tokens"`
	System      []anthropicSystemBlock `json:"system,omitempty"`
	Messages    []anthropicMessage     `json:"messages"`
	Tools       []anthropicTool        `json:"tools,omitempty"`
	Temperature float64                `json:"temperature,omitempty"`
	Stream      bool                   `json:"stream"`
}

// StreamChatAnthropic is the native-API counterpart of StreamChat
// for the anthropic-claude profile. Yields the same LLMEvent type
// the OAI-compat client does so the chat handler's loop is identical.
// The Profile field on LLMRequest is what routes here; see
// StreamChatRouted (llm_client.go) for the dispatch.
func StreamChatAnthropic(ctx context.Context, req LLMRequest) (<-chan LLMEvent, error) {
	if req.BaseURL == "" {
		return nil, errors.New("llm-anthropic: base_url required")
	}
	if req.Model == "" {
		return nil, errors.New("llm-anthropic: model required")
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("llm-anthropic: at least one message required")
	}
	if req.APIKey == "" {
		return nil, errors.New("llm-anthropic: api_key required")
	}

	body, err := translateToAnthropic(req)
	if err != nil {
		return nil, fmt.Errorf("llm-anthropic: translate: %w", err)
	}
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("llm-anthropic: marshal: %w", err)
	}

	// Anthropic native is /v1/messages (not /v1/chat/completions).
	url := strings.TrimRight(req.BaseURL, "/") + "/messages"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("llm-anthropic: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("x-api-key", req.APIKey)
	httpReq.Header.Set("anthropic-version", AnthropicVersion)

	client := &http.Client{Timeout: 0} // context handles cancellation
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("llm-anthropic: upstream: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("llm-anthropic: upstream %d: %s",
			resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	out := make(chan LLMEvent, 16)
	go func() {
		defer resp.Body.Close()
		defer close(out)
		streamLoopAnthropic(ctx, resp.Body, out)
	}()
	return out, nil
}

// translateToAnthropic converts our OAI-shaped LLMRequest into the
// Anthropic native shape. Three non-trivial moves:
//
//  1. System messages get hoisted into the system field, split into
//     two blocks: a CACHEABLE block holding the embedded chat.md
//     content and a non-cacheable block holding the dynamic scope
//     preamble + operator_tz preamble. The split point is the line
//     starting with "\n\n# Current scope\n\n" which buildSystemPrompt
//     uses for the per-request preamble. If the marker isn't found
//     we treat the whole system as cacheable — losing the dynamic
//     part to cache TTL still works (just less optimal).
//
//  2. Tool calls live inside assistant messages as `tool_use`
//     content blocks (not at the message level as in OAI). Tool
//     results live as `tool_result` blocks INSIDE A USER MESSAGE
//     (one user message can carry multiple tool_result blocks for
//     parallel tool calls). Consecutive OAI tool messages are
//     batched into one user message.
//
//  3. Tools translate from {function:{name,description,parameters}}
//     to {name,description,input_schema}. The LAST tool gets
//     cache_control so the whole tools array hashes together.
func translateToAnthropic(req LLMRequest) (*anthropicRequest, error) {
	body := &anthropicRequest{
		Model:     req.Model,
		MaxTokens: maxTokensOrDefault(req.MaxTokens),
		Stream:    true,
		// Temperature intentionally omitted. Newer Anthropic models
		// (Opus 4+) reject `temperature` as deprecated; older ones
		// accept the server-side default (~1.0) just fine. The chat
		// handler used to push 0.2 here via orDefault, which broke
		// the moment the user switched to Opus. If a future caller
		// needs to tune temperature, add a "set explicitly" guard
		// (different from Go's zero value) and an allow-list of
		// model prefixes that still accept the field. Same for
		// top_p / top_k.
	}

	// --- System: split + mark cacheable prefix
	var systemAccum strings.Builder
	for _, m := range req.Messages {
		if m.Role == "system" {
			if systemAccum.Len() > 0 {
				systemAccum.WriteString("\n")
			}
			systemAccum.WriteString(m.Content)
		}
	}
	if systemAccum.Len() > 0 {
		body.System = splitSystemForCaching(systemAccum.String())
	}

	// --- Messages: user/assistant/tool → user/assistant with content blocks
	body.Messages = translateMessages(req.Messages)

	// --- Tools: OAI shape → Anthropic shape, last tool cacheable
	if len(req.Tools) > 0 {
		body.Tools = make([]anthropicTool, len(req.Tools))
		for i, t := range req.Tools {
			body.Tools[i] = anthropicTool{
				Name:        t.Function.Name,
				Description: t.Function.Description,
				InputSchema: t.Function.Parameters,
			}
		}
		// Cache the whole tools array via cache_control on the last.
		body.Tools[len(body.Tools)-1].CacheControl = &anthropicCacheControl{Type: "ephemeral"}
	}

	return body, nil
}

// splitSystemForCaching takes the full system prompt (embedded
// chat.md + dynamic preambles) and returns a two-block system array:
// first block is the static, cacheable prefix; second block is the
// dynamic suffix. The marker is the "# Current scope" or
// "# Operator timezone" header that buildSystemPrompt prepends.
// Whichever marker appears first is the split point.
func splitSystemForCaching(full string) []anthropicSystemBlock {
	// Try both markers; pick the earlier one.
	idxScope := strings.Index(full, "\n\n# Current scope\n\n")
	idxTZ := strings.Index(full, "\n\n# Operator timezone\n\n")
	split := -1
	switch {
	case idxScope < 0 && idxTZ < 0:
		split = -1
	case idxScope < 0:
		split = idxTZ
	case idxTZ < 0:
		split = idxScope
	case idxScope < idxTZ:
		split = idxScope
	default:
		split = idxTZ
	}
	if split < 0 {
		// Whole prompt is static — single cacheable block.
		return []anthropicSystemBlock{{
			Type: "text", Text: full,
			CacheControl: &anthropicCacheControl{Type: "ephemeral"},
		}}
	}
	staticPart := full[:split]
	dynamicPart := full[split:]
	return []anthropicSystemBlock{
		{Type: "text", Text: staticPart, CacheControl: &anthropicCacheControl{Type: "ephemeral"}},
		{Type: "text", Text: dynamicPart}, // no cache_control — varies per request
	}
}

// translateMessages walks the OAI messages and emits Anthropic
// alternating-role user/assistant messages. Consecutive tool
// messages are batched into one user message with one tool_result
// content block per source tool message.
func translateMessages(msgs []LLMMessage) []anthropicMessage {
	out := []anthropicMessage{}
	var pendingToolResults []anthropicContentBlock

	flushPendingToolResults := func() {
		if len(pendingToolResults) == 0 {
			return
		}
		out = append(out, anthropicMessage{
			Role:    "user",
			Content: pendingToolResults,
		})
		pendingToolResults = nil
	}

	for _, m := range msgs {
		switch m.Role {
		case "system":
			// Already hoisted; skip.
			continue

		case "user":
			flushPendingToolResults()
			content := []anthropicContentBlock{}
			if strings.TrimSpace(m.Content) != "" {
				content = append(content, anthropicContentBlock{
					Type: "text", Text: m.Content,
				})
			}
			if len(content) == 0 {
				// Empty user message — skip (Anthropic rejects empty content).
				continue
			}
			out = append(out, anthropicMessage{Role: "user", Content: content})

		case "assistant":
			flushPendingToolResults()
			content := []anthropicContentBlock{}
			if strings.TrimSpace(m.Content) != "" {
				content = append(content, anthropicContentBlock{
					Type: "text", Text: m.Content,
				})
			}
			for _, tc := range m.ToolCalls {
				// OAI arguments is a string; Anthropic input is a map.
				var argMap map[string]any
				if tc.Function.Arguments != "" {
					_ = json.Unmarshal([]byte(tc.Function.Arguments), &argMap)
				}
				if argMap == nil {
					argMap = map[string]any{}
				}
				content = append(content, anthropicContentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: argMap,
				})
			}
			if len(content) == 0 {
				// Empty assistant turn — skip (Anthropic rejects).
				continue
			}
			out = append(out, anthropicMessage{Role: "assistant", Content: content})

		case "tool":
			// Batch with any consecutive tool messages.
			pendingToolResults = append(pendingToolResults, anthropicContentBlock{
				Type:        "tool_result",
				ToolUseID:   m.ToolCallID,
				ToolContent: m.Content,
			})
		}
	}
	flushPendingToolResults()
	return out
}

func maxTokensOrDefault(n int) int {
	if n > 0 {
		return n
	}
	// Anthropic requires max_tokens. 8192 is a reasonable ceiling
	// for assistant replies in chat usage. The actual reply will
	// usually be much smaller; this just guards a runaway gen.
	return 8192
}

// --- SSE parsing ----------------------------------------------------

// streamLoopAnthropic reads Anthropic's native SSE stream and emits
// LLMEvents the same way the OAI loop does. Anthropic's event
// shape is substantially different:
//
//	event: message_start
//	data: {"type":"message_start","message":{...,"usage":{...}}}
//
//	event: content_block_start
//	data: {"type":"content_block_start","index":0,
//	        "content_block":{"type":"text","text":""}}
//
//	event: content_block_delta
//	data: {"type":"content_block_delta","index":0,
//	        "delta":{"type":"text_delta","text":"Hello"}}
//
//	event: content_block_stop
//	data: {"type":"content_block_stop","index":0}
//
//	event: message_delta
//	data: {"type":"message_delta",
//	        "delta":{"stop_reason":"end_turn","stop_sequence":null},
//	        "usage":{"output_tokens":12}}
//
//	event: message_stop
//	data: {"type":"message_stop"}
//
// Cache token counts live in the message_start event's usage:
//   - cache_creation_input_tokens: tokens we just WROTE to the cache
//   - cache_read_input_tokens: tokens we READ from the cache (free-ish)
//   - input_tokens: regular billable input (NOT including cache_creation
//     or cache_read tokens — they're billed separately)
//
// message_delta carries the FINAL output_tokens count (cumulative,
// not a delta).
//
// Tool use content blocks arrive as:
//
//	content_block_start { content_block: { type: "tool_use", id, name, input: {} } }
//	content_block_delta { delta: { type: "input_json_delta", partial_json: "{\"" } }
//	...
//	content_block_stop
func streamLoopAnthropic(ctx context.Context, r io.Reader, out chan<- LLMEvent) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)

	// Per-block state: which content block index we're on, what
	// kind it is, and (for tool_use blocks) the assembled args.
	type blockState struct {
		kind         string // "text" | "tool_use"
		toolCallID   string
		toolCallName string
		toolCallIdx  int
	}
	blocks := map[int]*blockState{}
	finalUsage := LLMUsage{}

	for scanner.Scan() {
		select {
		case <-ctx.Done():
			return
		default:
		}
		line := scanner.Bytes()
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		payload := bytes.TrimPrefix(line, []byte("data: "))

		// Parse the type discriminator first.
		var hdr struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(payload, &hdr); err != nil {
			select {
			case out <- LLMEvent{Err: fmt.Errorf("parse anthropic chunk: %w", err)}:
			case <-ctx.Done():
				return
			}
			continue
		}

		switch hdr.Type {

		case "message_start":
			var ev struct {
				Message struct {
					Usage struct {
						InputTokens              uint32 `json:"input_tokens"`
						OutputTokens             uint32 `json:"output_tokens"`
						CacheCreationInputTokens uint32 `json:"cache_creation_input_tokens"`
						CacheReadInputTokens     uint32 `json:"cache_read_input_tokens"`
					} `json:"usage"`
				} `json:"message"`
			}
			if err := json.Unmarshal(payload, &ev); err != nil {
				continue
			}
			finalUsage.InputTokens = ev.Message.Usage.InputTokens
			finalUsage.OutputTokens = ev.Message.Usage.OutputTokens
			finalUsage.CacheCreationInputTokens = ev.Message.Usage.CacheCreationInputTokens
			finalUsage.CacheReadInputTokens = ev.Message.Usage.CacheReadInputTokens

		case "content_block_start":
			var ev struct {
				Index        int `json:"index"`
				ContentBlock struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"content_block"`
			}
			if err := json.Unmarshal(payload, &ev); err != nil {
				continue
			}
			st := &blockState{
				kind:         ev.ContentBlock.Type,
				toolCallID:   ev.ContentBlock.ID,
				toolCallName: ev.ContentBlock.Name,
				toolCallIdx:  ev.Index,
			}
			blocks[ev.Index] = st
			// For tool_use blocks, emit the first delta with name+ID
			// so the chat handler's assembly map records this index.
			if st.kind == "tool_use" {
				select {
				case out <- LLMEvent{ToolCallDelta: &LLMToolCallDelta{
					Index: ev.Index, ID: st.toolCallID, Name: st.toolCallName,
				}}:
				case <-ctx.Done():
					return
				}
			}

		case "content_block_delta":
			var ev struct {
				Index int `json:"index"`
				Delta struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			}
			if err := json.Unmarshal(payload, &ev); err != nil {
				continue
			}
			st := blocks[ev.Index]
			if st == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text != "" {
					select {
					case out <- LLMEvent{TextDelta: ev.Delta.Text}:
					case <-ctx.Done():
						return
					}
				}
			case "input_json_delta":
				if ev.Delta.PartialJSON != "" {
					select {
					case out <- LLMEvent{ToolCallDelta: &LLMToolCallDelta{
						Index:    ev.Index,
						ArgsFrag: ev.Delta.PartialJSON,
					}}:
					case <-ctx.Done():
						return
					}
				}
			}

		case "content_block_stop":
			// No-op; the chat handler tracks tool-call completion via
			// the loop's FinishReason check.

		case "message_delta":
			var ev struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
				Usage struct {
					OutputTokens uint32 `json:"output_tokens"`
				} `json:"usage"`
			}
			if err := json.Unmarshal(payload, &ev); err != nil {
				continue
			}
			// Output tokens here OVERRIDES the message_start value
			// (which was 0 then; this is the cumulative final count).
			if ev.Usage.OutputTokens > 0 {
				finalUsage.OutputTokens = ev.Usage.OutputTokens
			}
			finishReason := translateAnthropicStopReason(ev.Delta.StopReason)
			if finishReason != "" {
				select {
				case out <- LLMEvent{FinishReason: finishReason}:
				case <-ctx.Done():
					return
				}
			}

		case "message_stop":
			// Emit the final usage as the last event before close.
			select {
			case out <- LLMEvent{Usage: &finalUsage}:
			case <-ctx.Done():
				return
			}
			return

		case "ping":
			// Anthropic sends periodic ping events to keep connections
			// alive on long streams. No-op.

		case "error":
			var ev struct {
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			_ = json.Unmarshal(payload, &ev)
			select {
			case out <- LLMEvent{Err: fmt.Errorf("anthropic error: %s: %s",
				ev.Error.Type, ev.Error.Message)}:
			case <-ctx.Done():
			}
			return
		}
	}
	if err := scanner.Err(); err != nil {
		select {
		case out <- LLMEvent{Err: fmt.Errorf("stream truncated: %w", err)}:
		case <-ctx.Done():
		}
	}
}

// translateAnthropicStopReason maps Anthropic's stop_reason values
// to the OAI-shaped finish_reason the chat handler's loop expects.
//   anthropic "end_turn"      → "stop"
//   anthropic "tool_use"      → "tool_calls"
//   anthropic "max_tokens"    → "length"
//   anthropic "stop_sequence" → "stop"
//   anthropic "pause_turn"    → "stop"  (rare; treat as final for now)
func translateAnthropicStopReason(r string) string {
	switch r {
	case "end_turn", "stop_sequence", "pause_turn":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	}
	return r
}
