package main

// llm_client.go — OpenAI-compatible streaming chat client (#497).
//
// The forwarder is a stateless proxy: every chat request brings its
// own {base_url, api_key, model} (BYO key, browser localStorage). No
// keyring; no env-var API keys. The client forwards the call to the
// upstream OAI-compat endpoint, parses the SSE response, and yields
// typed events (text deltas, tool-call assembly, usage, done) back
// to the caller.
//
// Streaming-only — every supported model supports streaming, and the
// tool-use loop relies on tool_calls arriving incrementally so the
// loop can interrupt to run the tool and re-feed.

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

// LLMMessage is one turn in the chat (OpenAI message shape).
type LLMMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	Name       string         `json:"name,omitempty"`
	ToolCalls  []LLMToolCall  `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// LLMTool advertises a callable function to the model.
type LLMTool struct {
	Type     string      `json:"type"` // always "function"
	Function LLMToolDef  `json:"function"`
}

// LLMToolDef is the JSON-Schema-shaped tool description.
type LLMToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// LLMToolCall is one tool-use the model wants the loop to run.
type LLMToolCall struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"` // always "function"
	Function LLMToolCallFunc `json:"function"`
}

// LLMToolCallFunc holds the parsed function name + raw JSON args.
type LLMToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// LLMUsage is the per-turn token + cost accounting.
type LLMUsage struct {
	InputTokens  uint32 `json:"input_tokens"`
	OutputTokens uint32 `json:"output_tokens"`
}

// LLMRequest is what the chat endpoint hands to the client. It maps
// 1:1 to an OAI chat-completion request, with the auth + endpoint
// info on top.
type LLMRequest struct {
	BaseURL string
	APIKey  string
	Model   string

	Messages    []LLMMessage
	Tools       []LLMTool
	Temperature float64
	MaxTokens   int
}

// LLMEvent is a streamed event yielded by Stream(). One of TextDelta,
// ToolCallDelta, Usage, or Done will be set.
type LLMEvent struct {
	TextDelta string

	// ToolCallDelta carries the incremental tool-call assembly. The
	// loop accumulates these per index until a Done event arrives,
	// at which point it has the full tool calls.
	ToolCallDelta *LLMToolCallDelta

	// Usage arrives once per turn (last event before Done) when
	// stream_options.include_usage is set on the request.
	Usage *LLMUsage

	// FinishReason is set on the final delta. "stop" | "tool_calls"
	// | "length" | "content_filter".
	FinishReason string

	Err error
}

// LLMToolCallDelta is one incremental piece of a tool-call assembly.
// Index identifies which tool call this delta belongs to (a turn
// can call multiple tools in parallel).
type LLMToolCallDelta struct {
	Index    int
	ID       string // set on the first delta of each call
	Name     string // set on the first delta of each call
	ArgsFrag string // appended across deltas
}

// StreamChat sends the request to the upstream and yields events
// over the returned channel. The channel closes when the upstream
// stream ends or the context is cancelled. Any error mid-stream
// arrives as an LLMEvent with Err set, followed by a channel close.
func StreamChat(ctx context.Context, req LLMRequest) (<-chan LLMEvent, error) {
	if req.BaseURL == "" {
		return nil, errors.New("llm: base_url required")
	}
	if req.Model == "" {
		return nil, errors.New("llm: model required")
	}
	if len(req.Messages) == 0 {
		return nil, errors.New("llm: at least one message required")
	}

	// Build the request body. We always set stream=true and ask for
	// usage in the final chunk — every provider in our catalog
	// supports both today.
	body := map[string]any{
		"model":    req.Model,
		"messages": req.Messages,
		"stream":   true,
		"stream_options": map[string]any{
			"include_usage": true,
		},
	}
	if len(req.Tools) > 0 {
		body["tools"] = req.Tools
	}
	if req.Temperature > 0 {
		body["temperature"] = req.Temperature
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}

	bodyBytes, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("llm: marshal request: %w", err)
	}

	url := strings.TrimRight(req.BaseURL, "/") + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("llm: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if req.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)
	}

	// 30 s connect; the body itself can take as long as the upstream
	// needs (tool-use loops can run for minutes legitimately).
	client := &http.Client{
		Timeout: 0, // no overall timeout — context handles cancellation
	}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("llm: upstream: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("llm: upstream %d: %s", resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	out := make(chan LLMEvent, 16)
	go func() {
		defer resp.Body.Close()
		defer close(out)
		streamLoop(ctx, resp.Body, out)
	}()
	return out, nil
}

// streamLoop reads SSE `data:` lines from r, parses each as an OAI
// chat-completion chunk, and emits LLMEvents.
func streamLoop(ctx context.Context, r io.Reader, out chan<- LLMEvent) {
	scanner := bufio.NewScanner(r)
	// Some providers (Anthropic, Ollama with large context) emit
	// chunks well over the default 64KB cap on prompt-reflection
	// turns. 16MB token buffer keeps the read robust.
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)

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
		if bytes.Equal(payload, []byte("[DONE]")) {
			return
		}
		var chunk oaiStreamChunk
		if err := json.Unmarshal(payload, &chunk); err != nil {
			// Don't kill the stream on a malformed line — log via
			// the Err event and keep going.
			select {
			case out <- LLMEvent{Err: fmt.Errorf("parse stream chunk: %w (line: %s)", err, string(payload))}:
			case <-ctx.Done():
				return
			}
			continue
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Content != "" {
				select {
				case out <- LLMEvent{TextDelta: ch.Delta.Content}:
				case <-ctx.Done():
					return
				}
			}
			for _, tc := range ch.Delta.ToolCalls {
				select {
				case out <- LLMEvent{ToolCallDelta: &LLMToolCallDelta{
					Index:    tc.Index,
					ID:       tc.ID,
					Name:     tc.Function.Name,
					ArgsFrag: tc.Function.Arguments,
				}}:
				case <-ctx.Done():
					return
				}
			}
			if ch.FinishReason != "" {
				select {
				case out <- LLMEvent{FinishReason: ch.FinishReason}:
				case <-ctx.Done():
					return
				}
			}
		}
		if chunk.Usage != nil {
			select {
			case out <- LLMEvent{Usage: &LLMUsage{
				InputTokens:  chunk.Usage.PromptTokens,
				OutputTokens: chunk.Usage.CompletionTokens,
			}}:
			case <-ctx.Done():
				return
			}
		}
	}
	if err := scanner.Err(); err != nil {
		select {
		case out <- LLMEvent{Err: fmt.Errorf("stream truncated: %w", err)}:
		case <-ctx.Done():
		}
	}
}

// oaiStreamChunk mirrors the OAI streaming chat-completion shape.
type oaiStreamChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []oaiChoice    `json:"choices"`
	Usage   *oaiUsage      `json:"usage,omitempty"`
}

type oaiChoice struct {
	Index        int      `json:"index"`
	Delta        oaiDelta `json:"delta"`
	FinishReason string   `json:"finish_reason,omitempty"`
}

type oaiDelta struct {
	Role      string             `json:"role,omitempty"`
	Content   string             `json:"content,omitempty"`
	ToolCalls []oaiToolCallDelta `json:"tool_calls,omitempty"`
}

type oaiToolCallDelta struct {
	Index    int                `json:"index"`
	ID       string             `json:"id,omitempty"`
	Type     string             `json:"type,omitempty"`
	Function oaiToolCallFunc    `json:"function,omitempty"`
}

type oaiToolCallFunc struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type oaiUsage struct {
	PromptTokens     uint32 `json:"prompt_tokens"`
	CompletionTokens uint32 `json:"completion_tokens"`
	TotalTokens      uint32 `json:"total_tokens"`
}
