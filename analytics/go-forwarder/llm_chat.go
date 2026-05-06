// Tool-use loop for the AI session-analysis path (epic #412).
//
// Implements the standard OpenAI tools loop: model emits tool_calls,
// the forwarder runs each tool, appends results as role:"tool"
// messages, re-invokes the model, repeats until the model returns
// without tool_calls (the final answer).
//
// This file is the non-streaming path used by tests + the Analyze
// button's one-shot mode. The /analytics/api/session_chat endpoint
// (#416) layers SSE streaming on top by emitting per-tool-call and
// per-token events as it walks the same loop.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/sashabaranov/go-openai"
)

const (
	defaultMaxToolCallsPerTurn = 20
	// Per-iteration deadline: one model call + its tool fan-out. The
	// 12 s ClickHouse timeout in QueryTool fits inside this comfortably.
	defaultIterationTimeout = 30 * time.Second
	// Aggregate deadline for the whole turn — model_call ⨯ N + tool_runs ⨯ M.
	// Cuts off pathological loops even when each iteration is healthy.
	defaultTurnTimeout = 120 * time.Second
)

// ToolDispatcher maps a tool name (the OpenAI Function.Name) to a
// runner that takes the JSON-string arguments the model emitted.
// Errors from the runner are not loop-fatal — they round-trip back
// to the model so it can self-correct. Only context cancellation /
// upstream model errors stop the loop.
type ToolDispatcher func(ctx context.Context, name, argumentsJSON string) (resultJSON string)

// ChatTurnResult is the per-turn outcome consumers see.
type ChatTurnResult struct {
	FinalText      string                         `json:"final_text"`
	Messages       []openai.ChatCompletionMessage `json:"messages"`
	ToolCallsCount int                            `json:"tool_calls_count"`
	InputTokens    int                            `json:"input_tokens"`
	OutputTokens   int                            `json:"output_tokens"`
	Iterations     int                            `json:"iterations"`
	StoppedReason  string                         `json:"stopped_reason"` // "complete" | "tool_budget" | "timeout" | "error"
	StoppedDetail  string                         `json:"stopped_detail,omitempty"`
}

// ChatTurnOptions configures a single chat-completion-with-tools
// turn. Zero values use sensible defaults.
type ChatTurnOptions struct {
	Tools             []openai.Tool
	Dispatcher        ToolDispatcher
	MaxToolCallsTotal int           // default 20 (LLM_MAX_TOOL_CALLS_PER_TURN env wins)
	IterationTimeout  time.Duration // per model+tools pass; default 30s
	TurnTimeout       time.Duration // whole turn; default 120s
	Temperature       float32       // default 0 (deterministic-ish)
	MaxTokens         int           // default 4096
}

func (c *LLMClient) RunChatTurn(
	ctx context.Context,
	profileName string,
	messages []openai.ChatCompletionMessage,
	opts ChatTurnOptions,
) (*ChatTurnResult, error) {
	prof, err := c.profiles.Resolve(profileName)
	if err != nil {
		return nil, err
	}
	maxCalls := envIntPositive("LLM_MAX_TOOL_CALLS_PER_TURN", opts.MaxToolCallsTotal, defaultMaxToolCallsPerTurn)
	iterTO := opts.IterationTimeout
	if iterTO <= 0 {
		iterTO = defaultIterationTimeout
	}
	turnTO := opts.TurnTimeout
	if turnTO <= 0 {
		turnTO = defaultTurnTimeout
	}
	maxTok := opts.MaxTokens
	if maxTok <= 0 {
		maxTok = 4096
	}

	turnCtx, cancel := context.WithTimeout(ctx, turnTO)
	defer cancel()

	res := &ChatTurnResult{
		Messages: append([]openai.ChatCompletionMessage(nil), messages...),
	}
	client := c.clientFor(prof)

	// budgetBlown flips when ToolCallsCount exceeds maxCalls. Once
	// set, the next iteration sends ToolChoice="none" to force the
	// model to produce final text instead of more tool calls, and we
	// break unconditionally after consuming that response. Without
	// this gate the model can keep emitting tool_calls right up to
	// the iteration cap and we never get a wrap-up answer.
	budgetBlown := false
	// maxCalls + 2: maxCalls tool-emitting iterations, plus one to
	// receive the model's reply to the final tool result, plus one
	// forced-no-tool wrap-up if the budget gate fires.
	for iter := 0; iter < maxCalls+2; iter++ {
		if err := turnCtx.Err(); err != nil {
			res.StoppedReason = "timeout"
			res.StoppedDetail = err.Error()
			return res, nil
		}
		iterCtx, iterCancel := context.WithTimeout(turnCtx, iterTO)
		req := openai.ChatCompletionRequest{
			Model:       prof.Model,
			Messages:    res.Messages,
			Tools:       opts.Tools,
			Temperature: opts.Temperature,
			MaxTokens:   maxTok,
		}
		if budgetBlown {
			req.ToolChoice = "none"
		}
		resp, err := client.CreateChatCompletion(iterCtx, req)
		iterCancel()
		if err != nil {
			res.StoppedReason = "error"
			res.StoppedDetail = err.Error()
			res.Iterations = iter + 1
			return res, fmt.Errorf("chat completion (iter %d, profile %q): %w", iter, prof.Name, err)
		}
		res.InputTokens += resp.Usage.PromptTokens
		res.OutputTokens += resp.Usage.CompletionTokens
		res.Iterations = iter + 1

		if len(resp.Choices) == 0 {
			res.StoppedReason = "error"
			res.StoppedDetail = "no choices in response"
			return res, errors.New("no choices in chat completion response")
		}
		assistant := resp.Choices[0].Message
		res.Messages = append(res.Messages, assistant)

		// Defensive: some providers emit finish_reason="tool_calls"
		// with an empty array. Treat that the same as the no-tool-
		// calls branch but flag it so telemetry catches the oddity.
		if len(assistant.ToolCalls) == 0 && assistant.Content == "" {
			res.StoppedReason = "error"
			res.StoppedDetail = "empty assistant message"
			return res, nil
		}
		if len(assistant.ToolCalls) == 0 {
			res.FinalText = assistant.Content
			if budgetBlown {
				res.StoppedReason = "tool_budget"
			} else {
				res.StoppedReason = "complete"
			}
			return res, nil
		}

		// If the model returned ToolChoice="none" but still emitted
		// tool_calls (some providers ignore the directive), bail out
		// rather than running them — we asked for text, not actions.
		if budgetBlown {
			res.StoppedReason = "tool_budget"
			res.StoppedDetail = "model ignored ToolChoice=none after budget exhaustion"
			res.FinalText = assistant.Content
			return res, nil
		}

		// Note: assistant.Content alongside ToolCalls is preamble
		// (model "thinking out loud" before invoking tools). It's
		// preserved in res.Messages for #416's SSE endpoint to
		// surface; we don't promote it to res.FinalText since this
		// turn isn't done.
		for _, tc := range assistant.ToolCalls {
			res.ToolCallsCount++
			if res.ToolCallsCount > maxCalls {
				res.Messages = append(res.Messages, openai.ChatCompletionMessage{
					Role:       openai.ChatMessageRoleTool,
					ToolCallID: tc.ID,
					Content:    `{"error":"tool budget exhausted; reply with a final answer using what you have"}`,
				})
				budgetBlown = true
				continue
			}
			// turnCtx deadline dominates if it's sooner than the
			// 12 s ClickHouse cap inside QueryTool, so a confused
			// dispatcher can't outlive the request.
			result := opts.Dispatcher(turnCtx, tc.Function.Name, tc.Function.Arguments)
			res.Messages = append(res.Messages, openai.ChatCompletionMessage{
				Role:       openai.ChatMessageRoleTool,
				ToolCallID: tc.ID,
				Content:    result,
			})
		}
	}

	if res.StoppedReason == "" {
		res.StoppedReason = "tool_budget"
		res.StoppedDetail = "max iterations reached without final answer"
	}
	return res, nil
}

// envIntPositive prefers env over caller-supplied opt over default.
// Returns the default if both are absent or the env can't parse.
func envIntPositive(envName string, optVal, defVal int) int {
	if v := os.Getenv(envName); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	if optVal > 0 {
		return optVal
	}
	return defVal
}

// MakeQueryDispatcher returns a Dispatcher that routes the `query`
// tool to a QueryTool and rejects every other name. #424 will fold
// in the read-only control tools using the same pattern.
func MakeQueryDispatcher(qt *QueryTool) ToolDispatcher {
	return func(ctx context.Context, name, args string) string {
		switch name {
		case "query":
			var parsed struct {
				SQL string `json:"sql"`
			}
			if err := json.Unmarshal([]byte(args), &parsed); err != nil {
				return jsonErr(fmt.Sprintf("malformed args: %s", err.Error()))
			}
			if parsed.SQL == "" {
				return jsonErr("missing required argument: sql")
			}
			return qt.Run(ctx, parsed.SQL).MarshalForTool()
		default:
			return jsonErr(fmt.Sprintf("unknown tool: %s", name))
		}
	}
}

func jsonErr(msg string) string {
	b, _ := json.Marshal(map[string]string{"error": msg})
	return string(b)
}
