// OpenAI-compatible chat client for the AI session-analysis path
// (epic #412). The same client talks to every backend the profiles
// support — HF Inference Providers, Anthropic OpenAI-compat, OpenAI,
// local Ollama — by overriding base URL + API key per call.
//
// This file deliberately exposes a thin Chat() surface that's enough
// for the smoke-test acceptance of #414. Streaming + tool_calls
// support arrives in #415 (tool-use loop) and #416 (SSE endpoint).

package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/sashabaranov/go-openai"
)

type LLMClient struct {
	profiles *LLMProfiles
}

func NewLLMClient(profiles *LLMProfiles) *LLMClient {
	return &LLMClient{profiles: profiles}
}

// clientFor builds a per-profile go-openai client. We construct it
// per call rather than caching: profiles are few, and the cost is a
// struct allocation. Caching would force us to invalidate when env
// vars change in tests, which isn't worth it.
func (c *LLMClient) clientFor(prof *LLMProfile) *openai.Client {
	cfg := openai.DefaultConfig(prof.APIKey())
	cfg.BaseURL = prof.BaseURL
	return openai.NewClientWithConfig(cfg)
}

// PingResult is the smoke-test return shape — what the issue
// acceptance criteria need to verify ("returns a non-empty response
// with usage populated").
type PingResult struct {
	Profile      string
	Model        string
	Reply        string
	InputTokens  int
	OutputTokens int
}

// Ping sends a one-shot "say 'pong'" message and returns the reply
// plus token counts. Used by the smoke test in CI / local; not on the
// session_chat hot path.
func (c *LLMClient) Ping(ctx context.Context, profileName string) (*PingResult, error) {
	prof, err := c.profiles.Resolve(profileName)
	if err != nil {
		return nil, err
	}
	resp, err := c.clientFor(prof).CreateChatCompletion(ctx, openai.ChatCompletionRequest{
		Model: prof.Model,
		Messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleUser,
				Content: "Reply with exactly the single word: pong",
			},
		},
		MaxTokens: 16,
	})
	if err != nil {
		return nil, fmt.Errorf("chat completion against profile %q (%s): %w", prof.Name, prof.Model, err)
	}
	if len(resp.Choices) == 0 {
		return nil, errors.New("no choices in chat completion response")
	}
	return &PingResult{
		Profile:      prof.Name,
		Model:        prof.Model,
		Reply:        resp.Choices[0].Message.Content,
		InputTokens:  resp.Usage.PromptTokens,
		OutputTokens: resp.Usage.CompletionTokens,
	}, nil
}
