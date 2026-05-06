package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeOpenAI minimally impersonates an OpenAI-compatible chat endpoint
// so we can test client wiring (URL routing, auth header, model field,
// usage parsing) without any live network call.
func fakeOpenAI(t *testing.T, expectModel, replyText string, inTok, outTok int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization header = %q, want Bearer test-key", got)
		}
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("decode req: %v (body=%s)", err, body)
		}
		if req.Model != expectModel {
			t.Errorf("Model = %q, want %q", req.Model, expectModel)
		}
		resp := map[string]any{
			"id":      "test-id",
			"object":  "chat.completion",
			"created": 0,
			"model":   expectModel,
			"choices": []any{
				map[string]any{
					"index": 0,
					"message": map[string]any{
						"role":    "assistant",
						"content": replyText,
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     inTok,
				"completion_tokens": outTok,
				"total_tokens":      inTok + outTok,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}))
}

func TestLLMClient_Ping_RoutesAndParses(t *testing.T) {
	srv := fakeOpenAI(t, "test-model", "pong", 7, 1)
	defer srv.Close()

	const envName = "LLM_TEST_PING_KEY"
	t.Setenv(envName, "test-key")

	profile := &LLMProfile{
		Name:      "fake",
		BaseURL:   srv.URL + "/",
		APIKeyEnv: envName,
		Model:     "test-model",
	}
	profiles := &LLMProfiles{
		Active:   "fake",
		Profiles: map[string]*LLMProfile{"fake": profile},
	}
	client := NewLLMClient(profiles)
	res, err := client.Ping(context.Background(), "fake")
	if err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if res.Reply != "pong" {
		t.Errorf("Reply = %q, want %q", res.Reply, "pong")
	}
	if res.InputTokens != 7 || res.OutputTokens != 1 {
		t.Errorf("usage = (%d in, %d out), want (7, 1)", res.InputTokens, res.OutputTokens)
	}
	if res.Model != "test-model" {
		t.Errorf("Model = %q, want test-model", res.Model)
	}
}

func TestLLMClient_Ping_UnavailableProfile(t *testing.T) {
	const envName = "LLM_TEST_PING_UNAVAIL"
	profile := &LLMProfile{
		Name:      "no-creds",
		BaseURL:   "http://unused/",
		APIKeyEnv: envName,
		Model:     "m",
	}
	profiles := &LLMProfiles{
		Active:   "no-creds",
		Profiles: map[string]*LLMProfile{"no-creds": profile},
	}
	_, err := NewLLMClient(profiles).Ping(context.Background(), "no-creds")
	if err == nil {
		t.Fatal("expected error when profile API key env missing")
	}
}

func TestLLMClient_Ping_PropagatesUpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `{"error":{"message":"boom"}}`)
	}))
	defer srv.Close()

	const envName = "LLM_TEST_PING_BOOM"
	t.Setenv(envName, "k")

	profile := &LLMProfile{
		Name:      "boom",
		BaseURL:   srv.URL + "/",
		APIKeyEnv: envName,
		Model:     "m",
	}
	profiles := &LLMProfiles{
		Active:   "boom",
		Profiles: map[string]*LLMProfile{"boom": profile},
	}
	_, err := NewLLMClient(profiles).Ping(context.Background(), "boom")
	if err == nil {
		t.Fatal("expected error when upstream returns 500")
	}
}
