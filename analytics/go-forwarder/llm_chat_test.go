package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sashabaranov/go-openai"
)

// scriptedLLM serves chat-completion requests from a pre-loaded
// FIFO of responses. Each call pops the next entry; running out
// fails the test. Records the JSON body of each request so tests
// can verify ToolChoice / Tools / Messages were shaped correctly.
type scriptedLLM struct {
	srv       *httptest.Server
	mu        sync.Mutex
	responses []openai.ChatCompletionResponse
	bodies    [][]byte
}

func (s *scriptedLLM) URL() string { return s.srv.URL }

func (s *scriptedLLM) Bodies() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]byte, len(s.bodies))
	for i, b := range s.bodies {
		dup := make([]byte, len(b))
		copy(dup, b)
		out[i] = dup
	}
	return out
}

func newScriptedLLM(t *testing.T, responses ...openai.ChatCompletionResponse) *scriptedLLM {
	s := &scriptedLLM{responses: responses}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.bodies = append(s.bodies, body)
		if len(s.responses) == 0 {
			s.mu.Unlock()
			t.Errorf("scriptedLLM: out of scripted responses (call #%d)", len(s.bodies))
			http.Error(w, "out of scripted responses", 500)
			return
		}
		next := s.responses[0]
		s.responses = s.responses[1:]
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(next)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func textResp(s string, in, out int) openai.ChatCompletionResponse {
	return openai.ChatCompletionResponse{
		Choices: []openai.ChatCompletionChoice{{
			Index:        0,
			FinishReason: "stop",
			Message: openai.ChatCompletionMessage{
				Role:    openai.ChatMessageRoleAssistant,
				Content: s,
			},
		}},
		Usage: openai.Usage{PromptTokens: in, CompletionTokens: out},
	}
}

func toolCallResp(callID, fnName, fnArgs string, in, out int) openai.ChatCompletionResponse {
	return openai.ChatCompletionResponse{
		Choices: []openai.ChatCompletionChoice{{
			Index:        0,
			FinishReason: "tool_calls",
			Message: openai.ChatCompletionMessage{
				Role: openai.ChatMessageRoleAssistant,
				ToolCalls: []openai.ToolCall{{
					ID:   callID,
					Type: openai.ToolTypeFunction,
					Function: openai.FunctionCall{
						Name:      fnName,
						Arguments: fnArgs,
					},
				}},
			},
		}},
		Usage: openai.Usage{PromptTokens: in, CompletionTokens: out},
	}
}

func setupChatTest(t *testing.T, baseURL string) (*LLMClient, ChatTurnOptions, *int) {
	const envName = "LLM_CHAT_TEST_KEY"
	t.Setenv(envName, "test-key")
	profile := &LLMProfile{
		Name:      "test",
		BaseURL:   baseURL + "/",
		APIKeyEnv: envName,
		Model:     "test-model",
	}
	profiles := &LLMProfiles{
		Active:   "test",
		Profiles: map[string]*LLMProfile{"test": profile},
	}
	dispatchCount := 0
	tools := []openai.Tool{{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "echo",
			Description: "echo back the value field",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"value": map[string]any{"type": "string"}},
				"required":   []string{"value"},
			},
		},
	}}
	dispatcher := func(ctx context.Context, name, args string) string {
		dispatchCount++
		var parsed struct {
			Value string `json:"value"`
		}
		_ = json.Unmarshal([]byte(args), &parsed)
		return `{"echo":"` + parsed.Value + `"}`
	}
	return NewLLMClient(profiles), ChatTurnOptions{
		Tools:      tools,
		Dispatcher: dispatcher,
	}, &dispatchCount
}

func TestRunChatTurn_NoToolCalls_ReturnsImmediately(t *testing.T) {
	srv := newScriptedLLM(t, textResp("the answer is 42", 100, 7))
	client, opts, dispatched := setupChatTest(t, srv.URL())

	res, err := client.RunChatTurn(context.Background(), "test",
		[]openai.ChatCompletionMessage{{Role: "user", Content: "what's the answer"}}, opts)
	if err != nil {
		t.Fatalf("RunChatTurn: %v", err)
	}
	if res.FinalText != "the answer is 42" {
		t.Errorf("FinalText = %q", res.FinalText)
	}
	if res.ToolCallsCount != 0 {
		t.Errorf("ToolCallsCount = %d, want 0", res.ToolCallsCount)
	}
	if *dispatched != 0 {
		t.Errorf("dispatcher called %d times, want 0", *dispatched)
	}
	if res.StoppedReason != "complete" {
		t.Errorf("StoppedReason = %q, want complete", res.StoppedReason)
	}
	if res.InputTokens != 100 || res.OutputTokens != 7 {
		t.Errorf("usage = (%d in, %d out)", res.InputTokens, res.OutputTokens)
	}
}

func TestRunChatTurn_OneToolCall_RoundTrips(t *testing.T) {
	srv := newScriptedLLM(t,
		toolCallResp("call-1", "echo", `{"value":"hello"}`, 50, 10),
		textResp("you said hello", 60, 5),
	)
	client, opts, dispatched := setupChatTest(t, srv.URL())

	res, err := client.RunChatTurn(context.Background(), "test",
		[]openai.ChatCompletionMessage{{Role: "user", Content: "echo hello"}}, opts)
	if err != nil {
		t.Fatalf("RunChatTurn: %v", err)
	}
	if res.FinalText != "you said hello" {
		t.Errorf("FinalText = %q", res.FinalText)
	}
	if res.ToolCallsCount != 1 {
		t.Errorf("ToolCallsCount = %d, want 1", res.ToolCallsCount)
	}
	if *dispatched != 1 {
		t.Errorf("dispatcher called %d times, want 1", *dispatched)
	}
	if res.StoppedReason != "complete" {
		t.Errorf("StoppedReason = %q, want complete", res.StoppedReason)
	}
	if res.Iterations != 2 {
		t.Errorf("Iterations = %d, want 2", res.Iterations)
	}
	// Token totals accumulate.
	if res.InputTokens != 110 || res.OutputTokens != 15 {
		t.Errorf("usage = (%d in, %d out), want (110, 15)", res.InputTokens, res.OutputTokens)
	}
	// Final messages should be: user, assistant(tool_call), tool, assistant(text).
	if got := len(res.Messages); got != 4 {
		t.Errorf("len(Messages) = %d, want 4", got)
	}
	if res.Messages[2].Role != openai.ChatMessageRoleTool {
		t.Errorf("Messages[2].Role = %q, want tool", res.Messages[2].Role)
	}
	if !strings.Contains(res.Messages[2].Content, "hello") {
		t.Errorf("tool result = %q, expected to contain echoed value", res.Messages[2].Content)
	}
}

func TestRunChatTurn_ToolBudgetExhaustion(t *testing.T) {
	const calls = 4
	resps := make([]openai.ChatCompletionResponse, 0, calls+2)
	for i := 0; i < calls+1; i++ {
		resps = append(resps, toolCallResp("c"+strconv.Itoa(i), "echo", `{"value":"x"}`, 10, 1))
	}
	resps = append(resps, textResp("ok, stopping with what I have", 5, 5))
	srv := newScriptedLLM(t, resps...)
	client, opts, _ := setupChatTest(t, srv.URL())
	opts.MaxToolCallsTotal = calls

	res, err := client.RunChatTurn(context.Background(), "test",
		[]openai.ChatCompletionMessage{{Role: "user", Content: "loop"}}, opts)
	if err != nil {
		t.Fatalf("RunChatTurn: %v", err)
	}
	if res.StoppedReason != "tool_budget" {
		t.Errorf("StoppedReason = %q, want tool_budget", res.StoppedReason)
	}
	if res.FinalText != "ok, stopping with what I have" {
		t.Errorf("FinalText = %q, want forced wrap-up text", res.FinalText)
	}
	if res.ToolCallsCount <= calls {
		t.Errorf("ToolCallsCount = %d, expected > %d (the over-budget call counts but synthesizes an error)", res.ToolCallsCount, calls)
	}
}

// When budget is blown, the next request must include tool_choice:"none"
// to force a wrap-up text. Verifies by inspecting the recorded bodies.
func TestRunChatTurn_ToolBudgetForcesToolChoiceNone(t *testing.T) {
	const calls = 1
	srv := newScriptedLLM(t,
		toolCallResp("c0", "echo", `{"value":"x"}`, 10, 1),
		toolCallResp("c1", "echo", `{"value":"y"}`, 10, 1),
		textResp("done", 5, 5),
	)
	client, opts, _ := setupChatTest(t, srv.URL())
	opts.MaxToolCallsTotal = calls

	_, err := client.RunChatTurn(context.Background(), "test",
		[]openai.ChatCompletionMessage{{Role: "user", Content: "loop"}}, opts)
	if err != nil {
		t.Fatalf("RunChatTurn: %v", err)
	}

	bodies := srv.Bodies()
	if len(bodies) < 3 {
		t.Fatalf("got %d request bodies, expected >= 3", len(bodies))
	}
	// First two requests: no tool_choice override (tools allowed).
	if bytesContains(bodies[0], `"tool_choice"`) {
		t.Errorf("first request unexpectedly carried tool_choice: %s", bodies[0])
	}
	if bytesContains(bodies[1], `"tool_choice"`) {
		t.Errorf("second request unexpectedly carried tool_choice: %s", bodies[1])
	}
	// Third request: budget blown on iter 2 (call #1 emitted by first
	// response, call #2 by second response → ToolCallsCount = 2 > 1).
	// The third call must force tool_choice="none".
	if !bytesContains(bodies[2], `"tool_choice":"none"`) {
		t.Errorf("third request did not carry tool_choice=\"none\"; body: %s", bodies[2])
	}
}

func bytesContains(haystack []byte, needle string) bool {
	return strings.Contains(string(haystack), needle)
}

func TestRunChatTurn_TurnTimeout(t *testing.T) {
	// LLM hangs forever on the first call.
	hangSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(hangSrv.Close)
	client, opts, _ := setupChatTest(t, hangSrv.URL)
	opts.TurnTimeout = 100 * time.Millisecond
	opts.IterationTimeout = 100 * time.Millisecond

	start := time.Now()
	res, err := client.RunChatTurn(context.Background(), "test",
		[]openai.ChatCompletionMessage{{Role: "user", Content: "hang"}}, opts)
	elapsed := time.Since(start)

	// We expect a non-nil error from the upstream timeout; the test
	// asserts we returned within a sane multiple of TurnTimeout
	// rather than waiting the full 5 s the fake server would have.
	if elapsed > 1*time.Second {
		t.Errorf("loop took %v; expected to stop near TurnTimeout", elapsed)
	}
	if err == nil && res.StoppedReason != "timeout" && res.StoppedReason != "error" {
		t.Errorf("expected timeout/error stop; got %q (err=%v)", res.StoppedReason, err)
	}
}

func TestRunChatTurn_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream boom"}}`))
	}))
	t.Cleanup(srv.Close)
	client, opts, _ := setupChatTest(t, srv.URL)

	_, err := client.RunChatTurn(context.Background(), "test",
		[]openai.ChatCompletionMessage{{Role: "user", Content: "x"}}, opts)
	if err == nil {
		t.Fatal("expected error when upstream returns 500")
	}
}

func TestRunChatTurn_DispatcherErrorRoundTrips(t *testing.T) {
	srv := newScriptedLLM(t,
		toolCallResp("call-1", "echo", `{}`, 5, 5), // missing required value
		textResp("ok I will not", 10, 3),
	)
	client, opts, _ := setupChatTest(t, srv.URL())

	res, err := client.RunChatTurn(context.Background(), "test",
		[]openai.ChatCompletionMessage{{Role: "user", Content: "do something"}}, opts)
	if err != nil {
		t.Fatalf("RunChatTurn: %v", err)
	}
	if res.StoppedReason != "complete" {
		t.Errorf("dispatcher errors should not stop the loop; got %q", res.StoppedReason)
	}
	if res.ToolCallsCount != 1 {
		t.Errorf("ToolCallsCount = %d, want 1", res.ToolCallsCount)
	}
}

func TestMakeQueryDispatcher_RejectsUnknownTool(t *testing.T) {
	qt := NewQueryTool("http://unused")
	d := MakeQueryDispatcher(qt)
	out := d(context.Background(), "not_query", `{}`)
	if !strings.Contains(out, "unknown tool") {
		t.Errorf("dispatcher should reject unknown tool; got %q", out)
	}
}

func TestMakeQueryDispatcher_RejectsMissingSQL(t *testing.T) {
	qt := NewQueryTool("http://unused")
	d := MakeQueryDispatcher(qt)
	out := d(context.Background(), "query", `{}`)
	if !strings.Contains(out, "missing required argument") {
		t.Errorf("dispatcher should reject missing sql; got %q", out)
	}
}

func TestMakeQueryDispatcher_RejectsMalformedArgs(t *testing.T) {
	qt := NewQueryTool("http://unused")
	d := MakeQueryDispatcher(qt)
	out := d(context.Background(), "query", `not json`)
	if !strings.Contains(out, "malformed args") {
		t.Errorf("dispatcher should reject malformed args; got %q", out)
	}
}

func TestEnvIntPositive(t *testing.T) {
	const envName = "LLM_CHAT_ENV_INT_TEST"
	os.Unsetenv(envName)
	t.Cleanup(func() { os.Unsetenv(envName) })
	if got := envIntPositive(envName, 0, 7); got != 7 {
		t.Errorf("default not used: %d", got)
	}
	if got := envIntPositive(envName, 11, 7); got != 11 {
		t.Errorf("opt not used: %d", got)
	}
	t.Setenv(envName, "42")
	if got := envIntPositive(envName, 11, 7); got != 42 {
		t.Errorf("env not used: %d", got)
	}
	t.Setenv(envName, "garbage")
	if got := envIntPositive(envName, 11, 7); got != 11 {
		t.Errorf("malformed env should fall through to opt: %d", got)
	}
}
